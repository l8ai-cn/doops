package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

type doagentToolTrace struct {
	ToolCallID           string                 `json:"toolCallId"`
	ToolName             string                 `json:"toolName"`
	Title                string                 `json:"title,omitempty"`
	Status               string                 `json:"status"`
	ResultDigest         string                 `json:"resultDigest"`
	AttestationSchema    string                 `json:"attestationSchemaVersion,omitempty"`
	ContextSchema        string                 `json:"contextSchemaVersion,omitempty"`
	OperationID          string                 `json:"operationId,omitempty"`
	ContextDigest        string                 `json:"contextDigest,omitempty"`
	PlanDigest           string                 `json:"planDigest,omitempty"`
	PlanBindingDigest    string                 `json:"planBindingDigest,omitempty"`
	ExecutionMode        string                 `json:"executionMode,omitempty"`
	MutationAuthorized   *bool                  `json:"mutationAuthorized,omitempty"`
	CapabilityKey        string                 `json:"capabilityKey,omitempty"`
	AttestedTool         string                 `json:"attestedTool,omitempty"`
	EvidenceKind         string                 `json:"evidenceKind,omitempty"`
	EvidenceSubject      string                 `json:"evidenceSubject,omitempty"`
	CanonicalScope       map[string]interface{} `json:"canonicalScope,omitempty"`
	ScopeDigest          string                 `json:"scopeDigest,omitempty"`
	InputDigest          string                 `json:"inputDigest,omitempty"`
	AttestedResultDigest string                 `json:"attestedResultDigest,omitempty"`
	ResultText           string                 `json:"-"`
}

type doagentToolTraceCollector struct {
	mu          sync.Mutex
	calls       map[string]doagentToolTrace
	catalogPath string
}

func newDoagentToolTraceCollector(catalogPath string) *doagentToolTraceCollector {
	return &doagentToolTraceCollector{
		calls:       make(map[string]doagentToolTrace),
		catalogPath: catalogPath,
	}
}

func (c *doagentToolTraceCollector) collect(update map[string]interface{}) (bool, bool, error) {
	updateType, _ := update["sessionUpdate"].(string)
	if c.catalogPath != "" && (updateType == "agent_message" || updateType == "agent_message_chunk") {
		return true, false, nil
	}
	if updateType != "tool_call" && updateType != "tool_call_update" {
		return false, false, nil
	}
	toolCallID, _ := update["toolCallId"].(string)
	if strings.TrimSpace(toolCallID) == "" {
		return false, false, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	trace := c.calls[toolCallID]
	trace.ToolCallID = toolCallID
	if toolName, _ := update["toolName"].(string); strings.TrimSpace(toolName) != "" {
		trace.ToolName = toolName
	}
	if title, _ := update["title"].(string); strings.TrimSpace(title) != "" {
		trace.Title = title
	}
	if trace.ToolName == "" {
		trace.ToolName = trace.Title
	}
	if status, _ := update["status"].(string); strings.TrimSpace(status) != "" {
		trace.Status = status
	}
	if trace.Status == "completed" {
		resultText := doagentToolResultText(update)
		if resultText != "" {
			trace.ResultText = resultText
			sum := sha256.Sum256([]byte(resultText))
			trace.ResultDigest = "sha256:" + hex.EncodeToString(sum[:])
		}
		trace.AttestationSchema, _ = update["attestationSchemaVersion"].(string)
		trace.ContextSchema, _ = update["contextSchemaVersion"].(string)
		trace.OperationID, _ = update["operationId"].(string)
		trace.ContextDigest, _ = update["contextDigest"].(string)
		trace.PlanDigest, _ = update["planDigest"].(string)
		trace.PlanBindingDigest, _ = update["planBindingDigest"].(string)
		trace.ExecutionMode, _ = update["executionMode"].(string)
		if value, ok := update["mutationAuthorized"].(bool); ok {
			trace.MutationAuthorized = &value
		}
		trace.CapabilityKey, _ = update["capabilityKey"].(string)
		trace.AttestedTool, _ = update["attestedTool"].(string)
		trace.EvidenceKind, _ = update["evidenceKind"].(string)
		trace.EvidenceSubject, _ = update["evidenceSubject"].(string)
		trace.CanonicalScope, _ = update["canonicalScope"].(map[string]interface{})
		trace.ScopeDigest, _ = update["scopeDigest"].(string)
		trace.InputDigest, _ = update["inputDigest"].(string)
		trace.AttestedResultDigest, _ = update["resultDigest"].(string)
	}
	c.calls[toolCallID] = trace
	return false, false, c.publishLocked()
}

func (c *doagentToolTraceCollector) completed() []doagentToolTrace {
	c.mu.Lock()
	defer c.mu.Unlock()
	traces := make([]doagentToolTrace, 0, len(c.calls))
	for _, trace := range c.calls {
		if trace.trustedEvidence() {
			traces = append(traces, trace)
		}
	}
	sort.Slice(traces, func(i, j int) bool {
		return traces[i].ToolCallID < traces[j].ToolCallID
	})
	return traces
}

func (c *doagentToolTraceCollector) publishLocked() error {
	if strings.TrimSpace(c.catalogPath) == "" {
		return nil
	}
	traces := make([]doagentToolTrace, 0, len(c.calls))
	for _, trace := range c.calls {
		if trace.trustedEvidence() {
			traces = append(traces, trace)
		}
	}
	sort.Slice(traces, func(i, j int) bool {
		return traces[i].ToolCallID < traces[j].ToolCallID
	})
	return writeDoagentToolTraceCatalog(c.catalogPath, traces)
}

func doagentToolResultText(update map[string]interface{}) string {
	content, _ := update["content"].([]interface{})
	if len(content) != 1 {
		return ""
	}
	item, _ := content[0].(map[string]interface{})
	inner, _ := item["content"].(map[string]interface{})
	if inner["type"] != "text" {
		return ""
	}
	text, _ := inner["text"].(string)
	return text
}

func (trace doagentToolTrace) trustedEvidence() bool {
	return trace.Status == "completed" &&
		trace.ResultDigest != "" &&
		trace.AttestationSchema == "doops.tool-attestation/v1" &&
		trace.ContextSchema == "doops.reconciliation-context/v1" &&
		strings.TrimSpace(trace.OperationID) != "" &&
		validSHA256Digest(trace.ContextDigest) &&
		validSHA256Digest(trace.PlanDigest) &&
		validSHA256Digest(trace.PlanBindingDigest) &&
		(trace.ExecutionMode == "dry-run" || trace.ExecutionMode == "apply") &&
		trace.MutationAuthorized != nil &&
		strings.TrimSpace(trace.CapabilityKey) != "" &&
		strings.TrimSpace(trace.AttestedTool) != "" &&
		strings.TrimSpace(trace.EvidenceKind) != "" &&
		strings.TrimSpace(trace.EvidenceSubject) != "" &&
		trace.CanonicalScope != nil &&
		validSHA256Digest(trace.ScopeDigest) &&
		validSHA256Digest(trace.InputDigest) &&
		validSHA256Digest(trace.AttestedResultDigest)
}

func validSHA256Digest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == sha256.Size
}

func writeDoagentToolTraceCatalog(path string, traces []doagentToolTrace) error {
	data, err := json.Marshal(map[string]interface{}{"toolCalls": traces})
	if err != nil {
		return fmt.Errorf("encode runtime tool call catalog: %w", err)
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".runtime-tool-calls-*.tmp")
	if err != nil {
		return fmt.Errorf("create runtime tool call catalog: %w", err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("secure runtime tool call catalog: %w", err)
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return fmt.Errorf("write runtime tool call catalog: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close runtime tool call catalog: %w", err)
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish runtime tool call catalog: %w", err)
	}
	return nil
}

func attestDeploymentRun(
	result map[string]interface{}, traces []doagentToolTrace,
) (string, map[string]interface{}, error) {
	if result["kind"] != "DeploymentRun" {
		data, err := json.Marshal(result)
		return string(data), result, err
	}
	status, ok := result["status"].(map[string]interface{})
	if !ok {
		return "", nil, fmt.Errorf("DeploymentRun.status must be an object")
	}
	evidence, ok := status["evidence"].([]interface{})
	if !ok || len(evidence) == 0 {
		return "", nil, fmt.Errorf("DeploymentRun evidence must be a non-empty array")
	}
	completed := make(map[string]doagentToolTrace, len(traces))
	for _, trace := range traces {
		completed[trace.ToolCallID] = trace
	}
	for index, item := range evidence {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return "", nil, fmt.Errorf("DeploymentRun evidence[%d] must be an object", index)
		}
		toolCallID, _ := entry["toolCallId"].(string)
		trace, ok := completed[toolCallID]
		if !ok {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] must reference a completed runtime tool call",
				index,
			)
		}
		if !trace.trustedEvidence() {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] must reference an attested reconciliation tool call",
				index,
			)
		}
		module, _ := entry["module"].(string)
		if trace.ToolName != "" && module != trace.ToolName {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] module does not match runtime tool call",
				index,
			)
		}
		runtimeResult, err := decodeDoagentJSONObject(trace.ResultText)
		if err != nil {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] runtime tool output must be one JSON object",
				index,
			)
		}
		if err := validateRuntimeEvidenceEnvelope(runtimeResult); err != nil {
			return "", nil, fmt.Errorf("DeploymentRun evidence[%d]: %w", index, err)
		}
		evidenceResult, ok := entry["result"].(map[string]interface{})
		if !ok {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] result must be an object",
				index,
			)
		}
		runtimePayload, err := json.Marshal(runtimeResult)
		if err != nil {
			return "", nil, fmt.Errorf("encode runtime evidence[%d]: %w", index, err)
		}
		evidencePayload, err := json.Marshal(evidenceResult)
		if err != nil {
			return "", nil, fmt.Errorf("encode DeploymentRun evidence[%d]: %w", index, err)
		}
		if !bytes.Equal(runtimePayload, evidencePayload) {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] result does not match runtime tool output",
				index,
			)
		}
		subject, _ := entry["subject"].(string)
		runtimeSubject, _ := runtimeResult["subject"].(string)
		observedAt, _ := entry["observedAt"].(string)
		runtimeObservedAt, _ := runtimeResult["observedAt"].(string)
		if strings.TrimSpace(runtimeSubject) == "" || subject != runtimeSubject {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] subject does not match runtime tool output",
				index,
			)
		}
		if runtimeSubject != trace.EvidenceSubject {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] subject does not match tool attestation",
				index,
			)
		}
		if strings.TrimSpace(runtimeObservedAt) == "" || observedAt != runtimeObservedAt {
			return "", nil, fmt.Errorf(
				"DeploymentRun evidence[%d] observedAt does not match runtime tool output",
				index,
			)
		}
	}
	status["runtimeAttestation"] = map[string]interface{}{"toolCalls": traces}
	delete(status, "resultDigest")
	payload, err := json.Marshal(result)
	if err != nil {
		return "", nil, fmt.Errorf("encode DeploymentRun for digest: %w", err)
	}
	sum := sha256.Sum256(payload)
	status["resultDigest"] = "sha256:" + hex.EncodeToString(sum[:])
	payload, err = json.Marshal(result)
	if err != nil {
		return "", nil, fmt.Errorf("encode attested DeploymentRun: %w", err)
	}
	return string(payload), result, nil
}

func validateRuntimeEvidenceEnvelope(result map[string]interface{}) error {
	if len(result) != 3 {
		return fmt.Errorf("runtime tool output must contain only subject, observedAt and data")
	}
	subject, _ := result["subject"].(string)
	observedAt, _ := result["observedAt"].(string)
	data, ok := result["data"].(map[string]interface{})
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(observedAt) == "" || !ok {
		return fmt.Errorf("runtime tool output must contain subject, observedAt and object data")
	}
	return rejectSensitiveEvidenceFields(data)
}

func rejectSensitiveEvidenceFields(value interface{}) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, item := range typed {
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "token", "password", "passwd", "secret", "cookie", "authorization",
				"privatekey", "private_key", "clientsecret", "client_secret",
				"stringdata", "dockerconfigjson":
				return fmt.Errorf("runtime evidence contains forbidden sensitive field %q", key)
			}
			if err := rejectSensitiveEvidenceFields(item); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, item := range typed {
			if err := rejectSensitiveEvidenceFields(item); err != nil {
				return err
			}
		}
	}
	return nil
}
