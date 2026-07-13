package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

const reconciliationResultKind = "ReconciliationResult"

type ReconciliationStatus string

const (
	ReconciliationConverged ReconciliationStatus = "converged"
	ReconciliationBlocked   ReconciliationStatus = "blocked"
	ReconciliationFailed    ReconciliationStatus = "failed"
)

type ReconciliationResult struct {
	APIVersion         string                          `json:"apiVersion"`
	Kind               string                          `json:"kind"`
	PlanDigest         string                          `json:"planDigest"`
	Status             ReconciliationStatus            `json:"status"`
	Attempts           int                             `json:"attempts"`
	NoProgressAttempts int                             `json:"noProgressAttempts"`
	Evidence           []ReconciliationEvidence        `json:"evidence"`
	FailureEvidence    []ReconciliationEvidence        `json:"failureEvidence"`
	ExecutionEvidence  ReconciliationExecutionEvidence `json:"executionEvidence"`
}

type ReconciliationEvidence struct {
	Kind        string `json:"kind"`
	Subject     string `json:"subject"`
	ObservedAt  string `json:"observedAt"`
	Value       string `json:"value"`
	ToolCallID  string `json:"toolCallId"`
	ToolDigest  string `json:"toolDigest"`
	TraceDigest string `json:"traceDigest"`
}

type ReconciliationExecutionEvidence struct {
	TurnID          string                       `json:"turnId"`
	WorkspaceCommit string                       `json:"workspaceCommit"`
	TraceDigest     string                       `json:"traceDigest"`
	ToolCalls       []ReconciliationToolEvidence `json:"toolCalls"`
}

type ReconciliationToolEvidence struct {
	CallID      string `json:"callId"`
	Tool        string `json:"tool"`
	Status      string `json:"status"`
	Observation bool   `json:"observation"`
	Digest      string `json:"digest"`
}

func validateReconciliationResult(plan DeploymentPlan, workspaceCommit string, result ReconciliationResult) error {
	if result.APIVersion != deploymentAPIVersion || result.Kind != reconciliationResultKind {
		return fmt.Errorf("invalid reconciliation result type")
	}
	if strings.TrimSpace(plan.Digest) == "" || result.PlanDigest != plan.Digest {
		return fmt.Errorf("reconciliation result plan digest mismatch")
	}
	if result.Attempts <= 0 {
		return fmt.Errorf("reconciliation result attempts must be positive")
	}
	if result.NoProgressAttempts < 0 || result.NoProgressAttempts > result.Attempts {
		return fmt.Errorf("reconciliation result noProgressAttempts must be between 0 and attempts")
	}
	if err := validateReconciliationExecutionEvidence(plan.Digest, workspaceCommit, result.ExecutionEvidence, result.Status); err != nil {
		return fmt.Errorf("validate reconciliation execution evidence: %w", err)
	}
	if err := validateReconciliationEvidence(result.Evidence, result.ExecutionEvidence); err != nil {
		return fmt.Errorf("validate reconciliation evidence: %w", err)
	}
	if err := validateReconciliationEvidence(result.FailureEvidence, result.ExecutionEvidence); err != nil {
		return fmt.Errorf("validate reconciliation failure evidence: %w", err)
	}

	switch result.Status {
	case ReconciliationConverged:
		return requireReconciliationEvidence(plan.Spec.Acceptance.RequiredEvidence, result.Evidence)
	case ReconciliationBlocked, ReconciliationFailed:
		if len(result.FailureEvidence) == 0 {
			return fmt.Errorf("blocked or failed reconciliation requires observed failure evidence")
		}
		return nil
	default:
		return fmt.Errorf("unsupported reconciliation result status %q", result.Status)
	}
}

func validateReconciliationExecutionEvidence(
	planDigest string,
	workspaceCommit string,
	execution ReconciliationExecutionEvidence,
	status ReconciliationStatus,
) error {
	if strings.TrimSpace(execution.TurnID) == "" {
		return fmt.Errorf("turnId is required")
	}
	if !validWorkspaceCommit(workspaceCommit) || execution.WorkspaceCommit != workspaceCommit {
		return fmt.Errorf("workspaceCommit does not match the pushed workspace")
	}
	if !ociDigestPattern.MatchString(execution.TraceDigest) {
		return fmt.Errorf("traceDigest must be a sha256 digest")
	}
	seen := make(map[string]bool, len(execution.ToolCalls))
	hasCompletedObservation := false
	for _, toolCall := range execution.ToolCalls {
		if strings.TrimSpace(toolCall.CallID) == "" || strings.TrimSpace(toolCall.Tool) == "" {
			return fmt.Errorf("tool call id and name are required")
		}
		if seen[toolCall.CallID] {
			return fmt.Errorf("tool call %q is duplicated", toolCall.CallID)
		}
		seen[toolCall.CallID] = true
		if toolCall.Status != "completed" && toolCall.Status != "failed" {
			return fmt.Errorf("tool call %q has invalid terminal status", toolCall.CallID)
		}
		if !ociDigestPattern.MatchString(toolCall.Digest) {
			return fmt.Errorf("tool call %q digest must be a sha256 digest", toolCall.CallID)
		}
		if toolCall.Status == "completed" && toolCall.Observation {
			hasCompletedObservation = true
		}
	}
	if status == ReconciliationConverged && !hasCompletedObservation {
		return fmt.Errorf("converged reconciliation requires a completed observation tool call")
	}
	if reconciliationTraceDigest(planDigest, execution) != execution.TraceDigest {
		return fmt.Errorf("traceDigest does not match the bridge tool trace")
	}
	return nil
}

func reconciliationTraceDigest(planDigest string, execution ReconciliationExecutionEvidence) string {
	toolCalls := append([]ReconciliationToolEvidence(nil), execution.ToolCalls...)
	sort.Slice(toolCalls, func(i, j int) bool {
		return toolCalls[i].CallID < toolCalls[j].CallID
	})
	payload := struct {
		PlanDigest      string                       `json:"planDigest"`
		WorkspaceCommit string                       `json:"workspaceCommit"`
		TurnID          string                       `json:"turnId"`
		ToolCalls       []ReconciliationToolEvidence `json:"toolCalls"`
	}{
		PlanDigest:      planDigest,
		WorkspaceCommit: execution.WorkspaceCommit,
		TurnID:          execution.TurnID,
		ToolCalls:       toolCalls,
	}
	encoded, _ := json.Marshal(payload)
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("sha256:%x", digest[:])
}

func validateReconciliationEvidence(
	evidence []ReconciliationEvidence,
	execution ReconciliationExecutionEvidence,
) error {
	toolCalls := make(map[string]ReconciliationToolEvidence, len(execution.ToolCalls))
	for _, toolCall := range execution.ToolCalls {
		toolCalls[toolCall.CallID] = toolCall
	}
	for _, item := range evidence {
		if strings.TrimSpace(item.Kind) == "" {
			return fmt.Errorf("kind is required")
		}
		if strings.TrimSpace(item.Subject) == "" {
			return fmt.Errorf("subject is required")
		}
		if _, err := time.Parse(time.RFC3339, item.ObservedAt); err != nil {
			return fmt.Errorf("observedAt must be RFC3339: %w", err)
		}
		if strings.TrimSpace(item.Value) == "" {
			return fmt.Errorf("value is required")
		}
		callID := strings.TrimSpace(item.ToolCallID)
		if callID == "" {
			return fmt.Errorf("toolCallId is required")
		}
		toolCall, ok := toolCalls[callID]
		if !ok {
			return fmt.Errorf("toolCallId %q is not present in execution evidence", callID)
		}
		if toolCall.Status != "completed" {
			return fmt.Errorf("toolCallId %q did not complete successfully", callID)
		}
		if !toolCall.Observation {
			return fmt.Errorf("toolCallId %q is not an observation call", callID)
		}
		if item.ToolDigest != toolCall.Digest {
			return fmt.Errorf("toolDigest does not match toolCallId %q", callID)
		}
		if item.TraceDigest != execution.TraceDigest {
			return fmt.Errorf("traceDigest does not match the bridge tool trace")
		}
	}
	return nil
}

func requireReconciliationEvidence(required []string, evidence []ReconciliationEvidence) error {
	present := make(map[string]bool, len(evidence))
	for _, item := range evidence {
		present[item.Kind] = true
	}
	for _, kind := range normalizeEvidenceKinds(required) {
		if !present[kind] {
			return fmt.Errorf("required reconciliation evidence %q is missing", kind)
		}
	}
	return nil
}
