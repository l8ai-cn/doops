package server

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

func validateAttestedReconciliationResult(result map[string]interface{}, execution agentPromptExecutionContext) error {
	if stringField(result, "apiVersion") != "doops.sh/v2" || stringField(result, "kind") != "ReconciliationResult" {
		return fmt.Errorf("invalid reconciliation result type")
	}
	if stringField(result, "planDigest") != execution.PlanDigest {
		return fmt.Errorf("reconciliation result plan digest mismatch")
	}
	attempts, ok := integerField(result, "attempts")
	if !ok || attempts <= 0 {
		return fmt.Errorf("reconciliation result attempts must be positive")
	}
	noProgress, ok := integerField(result, "noProgressAttempts")
	if !ok || noProgress < 0 || noProgress > attempts {
		return fmt.Errorf("reconciliation result noProgressAttempts must be between 0 and attempts")
	}
	status := stringField(result, "status")
	toolCalls, traceDigest, err := validateReconciliationExecutionObject(result, execution, status)
	if err != nil {
		return fmt.Errorf("validate reconciliation execution evidence: %w", err)
	}
	evidence, err := validateReconciliationEvidenceObjects(result["evidence"], toolCalls, traceDigest)
	if err != nil {
		return fmt.Errorf("validate reconciliation evidence: %w", err)
	}
	failureEvidence, err := validateReconciliationEvidenceObjects(result["failureEvidence"], toolCalls, traceDigest)
	if err != nil {
		return fmt.Errorf("validate reconciliation failure evidence: %w", err)
	}

	switch status {
	case "converged":
		present := make(map[string]bool, len(evidence))
		for _, kind := range evidence {
			present[kind] = true
		}
		for _, kind := range normalizeRequiredEvidence(execution.RequiredEvidence) {
			if !present[kind] {
				return fmt.Errorf("required reconciliation evidence %q is missing", kind)
			}
		}
		return nil
	case "blocked", "failed":
		if len(failureEvidence) == 0 {
			return fmt.Errorf("blocked or failed reconciliation requires observed failure evidence")
		}
		return nil
	default:
		return fmt.Errorf("unsupported reconciliation result status %q", status)
	}
}

func validateReconciliationExecutionObject(
	result map[string]interface{},
	execution agentPromptExecutionContext,
	status string,
) (map[string]doagentToolTraceRecord, string, error) {
	raw, _ := result["executionEvidence"].(map[string]interface{})
	if raw == nil {
		return nil, "", fmt.Errorf("executionEvidence is required")
	}
	turnID := stringField(raw, "turnId")
	if turnID == "" {
		return nil, "", fmt.Errorf("turnId is required")
	}
	if stringField(raw, "workspaceCommit") != execution.WorkspaceCommit {
		return nil, "", fmt.Errorf("workspaceCommit does not match the pushed workspace")
	}
	if stringField(raw, "sourceRevision") != execution.SourceRevision {
		return nil, "", fmt.Errorf("sourceRevision does not match the immutable release source")
	}
	traceDigest := stringField(raw, "traceDigest")
	if !validAgentPlanDigest(traceDigest) {
		return nil, "", fmt.Errorf("traceDigest must be a sha256 digest")
	}
	var items []doagentToolTraceRecord
	switch value := raw["toolCalls"].(type) {
	case []doagentToolTraceRecord:
		items = value
	case []interface{}:
		items = make([]doagentToolTraceRecord, 0, len(value))
		for _, item := range value {
			object, _ := item.(map[string]interface{})
			if object == nil {
				return nil, "", fmt.Errorf("toolCalls items must be objects")
			}
			items = append(items, doagentToolTraceRecord{
				CallID:      stringField(object, "callId"),
				Tool:        stringField(object, "tool"),
				Status:      stringField(object, "status"),
				Observation: boolField(object, "observation"),
				Digest:      stringField(object, "digest"),
			})
		}
	default:
		return nil, "", fmt.Errorf("toolCalls must be an array")
	}
	records := make(map[string]doagentToolTraceRecord, len(items))
	hasCompletedObservation := false
	for _, record := range items {
		if record.CallID == "" || record.Tool == "" {
			return nil, "", fmt.Errorf("tool call id and name are required")
		}
		if _, exists := records[record.CallID]; exists {
			return nil, "", fmt.Errorf("tool call %q is duplicated", record.CallID)
		}
		if record.Status != "completed" && record.Status != "failed" {
			return nil, "", fmt.Errorf("tool call %q has invalid terminal status", record.CallID)
		}
		if !validAgentPlanDigest(record.Digest) {
			return nil, "", fmt.Errorf("tool call %q digest must be a sha256 digest", record.CallID)
		}
		if record.Status == "completed" && record.Observation {
			hasCompletedObservation = true
		}
		records[record.CallID] = record
	}
	if status == "converged" && !hasCompletedObservation {
		return nil, "", fmt.Errorf("converged reconciliation requires a completed observation tool call")
	}
	expected, err := doagentReconciliationTraceDigest(
		execution.PlanDigest,
		execution.SourceRevision,
		execution.WorkspaceCommit,
		turnID,
		items,
	)
	if err != nil {
		return nil, "", err
	}
	if expected != traceDigest {
		return nil, "", fmt.Errorf("traceDigest does not match the bridge tool trace")
	}
	return records, traceDigest, nil
}

func validateReconciliationEvidenceObjects(
	raw interface{},
	toolCalls map[string]doagentToolTraceRecord,
	traceDigest string,
) ([]string, error) {
	items, ok := raw.([]interface{})
	if !ok {
		return nil, fmt.Errorf("evidence must be an array")
	}
	kinds := make([]string, 0, len(items))
	for _, item := range items {
		object, _ := item.(map[string]interface{})
		if object == nil {
			return nil, fmt.Errorf("evidence items must be objects")
		}
		kind := stringField(object, "kind")
		if kind == "" {
			return nil, fmt.Errorf("kind is required")
		}
		if stringField(object, "subject") == "" {
			return nil, fmt.Errorf("subject is required")
		}
		if _, err := time.Parse(time.RFC3339, stringField(object, "observedAt")); err != nil {
			return nil, fmt.Errorf("observedAt must be RFC3339: %w", err)
		}
		if stringField(object, "value") == "" {
			return nil, fmt.Errorf("value is required")
		}
		callID := stringField(object, "toolCallId")
		if callID == "" {
			return nil, fmt.Errorf("toolCallId is required")
		}
		record, exists := toolCalls[callID]
		if !exists {
			return nil, fmt.Errorf("toolCallId %q is not present in execution evidence", callID)
		}
		if record.Status != "completed" {
			return nil, fmt.Errorf("toolCallId %q did not complete successfully", callID)
		}
		if !record.Observation {
			return nil, fmt.Errorf("toolCallId %q is not an observation call", callID)
		}
		if stringField(object, "toolDigest") != record.Digest {
			return nil, fmt.Errorf("toolDigest does not match toolCallId %q", callID)
		}
		if stringField(object, "traceDigest") != traceDigest {
			return nil, fmt.Errorf("traceDigest does not match the bridge tool trace")
		}
		kinds = append(kinds, kind)
	}
	return kinds, nil
}

func normalizeRequiredEvidence(values []string) []string {
	seen := make(map[string]bool, len(values))
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		normalized = append(normalized, value)
	}
	sort.Strings(normalized)
	return normalized
}

func stringField(object map[string]interface{}, key string) string {
	value, _ := object[key].(string)
	return strings.TrimSpace(value)
}

func boolField(object map[string]interface{}, key string) bool {
	value, _ := object[key].(bool)
	return value
}

func integerField(object map[string]interface{}, key string) (int, bool) {
	value, ok := object[key].(float64)
	if !ok || value != float64(int(value)) {
		return 0, false
	}
	return int(value), true
}
