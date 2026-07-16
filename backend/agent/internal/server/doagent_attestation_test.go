package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolTraceCollectorPublishesCompletedRuntimeCallCatalog(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "runtime-tool-calls.json")
	collector := newDoagentToolTraceCollector(catalogPath)

	if _, _, err := collector.collect(attestedToolUpdate(
		"call-source",
		"mcp_doops_plan_ObserveWorkspaceSource",
		"source-identity",
		`{"subject":"source-identity","observedAt":"2026-07-16T00:00:00Z","data":{"revision":"immutable"}}`,
	)); err != nil {
		t.Fatalf("collect completed tool call: %v", err)
	}

	data, err := os.ReadFile(catalogPath)
	if err != nil {
		t.Fatalf("read runtime tool call catalog: %v", err)
	}
	var catalog struct {
		ToolCalls []doagentToolTrace `json:"toolCalls"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil {
		t.Fatalf("decode runtime tool call catalog: %v", err)
	}
	if len(catalog.ToolCalls) != 1 {
		t.Fatalf("expected one completed tool call, got %#v", catalog.ToolCalls)
	}
	call := catalog.ToolCalls[0]
	if call.ToolCallID != "call-source" ||
		call.ToolName != "mcp_doops_plan_ObserveWorkspaceSource" ||
		call.Status != "completed" || call.ResultDigest == "" ||
		call.AttestationSchema != "doops.tool-attestation/v1" {
		t.Fatalf("unexpected runtime tool call catalog entry: %#v", call)
	}
}

func TestToolTraceCollectorExcludesUnattestedBashOutput(t *testing.T) {
	catalogPath := filepath.Join(t.TempDir(), "runtime-tool-calls.json")
	collector := newDoagentToolTraceCollector(catalogPath)
	if _, _, err := collector.collect(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-bash",
		"toolName":      "Bash",
		"status":        "completed",
		"content": []interface{}{map[string]interface{}{
			"type": "content",
			"content": map[string]interface{}{
				"type": "text",
				"text": `{"subject":"source-identity","observedAt":"2026-07-16T00:00:00Z","data":{"revision":"forged"}}`,
			},
		}},
	}); err != nil {
		t.Fatalf("collect Bash output: %v", err)
	}
	if calls := collector.completed(); len(calls) != 0 {
		t.Fatalf("unattested Bash output entered evidence catalog: %#v", calls)
	}
}

func TestAttestDeploymentRunRejectsEvidenceResultThatDiffersFromToolOutput(t *testing.T) {
	result := map[string]interface{}{
		"kind": "DeploymentRun",
		"status": map[string]interface{}{
			"evidence": []interface{}{
				map[string]interface{}{
					"subject":    "source-identity",
					"module":     "mcp_doops_plan_ObserveWorkspaceSource",
					"toolCallId": "call-source",
					"observedAt": "2026-07-16T00:00:00Z",
					"result": map[string]interface{}{
						"subject":    "source-identity",
						"observedAt": "2026-07-16T00:00:00Z",
						"data": map[string]interface{}{
							"revision": "tampered",
						},
					},
				},
			},
		},
	}
	traces := []doagentToolTrace{{
		ToolCallID:           "call-source",
		ToolName:             "mcp_doops_plan_ObserveWorkspaceSource",
		Status:               "completed",
		ResultText:           `{"subject":"source-identity","observedAt":"2026-07-16T00:00:00Z","data":{"revision":"immutable"}}`,
		ResultDigest:         "sha256:" + strings.Repeat("1", 64),
		AttestationSchema:    "doops.tool-attestation/v1",
		ContextSchema:        "doops.reconciliation-context/v1",
		OperationID:          "op_0123456789abcdef0123456789abcdef",
		ContextDigest:        "sha256:" + strings.Repeat("2", 64),
		PlanDigest:           "sha256:" + strings.Repeat("3", 64),
		PlanBindingDigest:    "sha256:" + strings.Repeat("4", 64),
		ExecutionMode:        "apply",
		MutationAuthorized:   boolPointer(true),
		CapabilityKey:        "source-identity",
		AttestedTool:         "ObserveWorkspaceSource",
		EvidenceKind:         "source-identity",
		EvidenceSubject:      "source-identity",
		CanonicalScope:       map[string]interface{}{"repository": "example"},
		ScopeDigest:          "sha256:" + strings.Repeat("5", 64),
		InputDigest:          "sha256:" + strings.Repeat("6", 64),
		AttestedResultDigest: "sha256:" + strings.Repeat("7", 64),
	}}

	if _, _, err := attestDeploymentRun(result, traces); err == nil {
		t.Fatal("evidence result that differs from runtime output must be rejected")
	}
}

func TestAttestDeploymentRunRejectsSensitiveEvidenceFields(t *testing.T) {
	update := attestedToolUpdate(
		"call-authz",
		"mcp_doops_plan_ObserveAuthorizationState",
		"authorization-state",
		`{"subject":"authorization-state","observedAt":"2026-07-16T00:00:00Z","data":{"token":"forbidden"}}`,
	)
	collector := newDoagentToolTraceCollector("")
	if _, _, err := collector.collect(update); err != nil {
		t.Fatalf("collect attested tool: %v", err)
	}
	result := map[string]interface{}{
		"kind": "DeploymentRun",
		"status": map[string]interface{}{
			"evidence": []interface{}{map[string]interface{}{
				"subject":    "authorization-state",
				"module":     "mcp_doops_plan_ObserveAuthorizationState",
				"toolCallId": "call-authz",
				"observedAt": "2026-07-16T00:00:00Z",
				"result": map[string]interface{}{
					"subject":    "authorization-state",
					"observedAt": "2026-07-16T00:00:00Z",
					"data":       map[string]interface{}{"token": "forbidden"},
				},
			}},
		},
	}
	if _, _, err := attestDeploymentRun(result, collector.completed()); err == nil {
		t.Fatal("sensitive evidence field must be rejected")
	}
}

func attestedToolUpdate(callID, toolName, subject, output string) map[string]interface{} {
	authorized := true
	return map[string]interface{}{
		"sessionUpdate":            "tool_call_update",
		"toolCallId":               callID,
		"toolName":                 toolName,
		"title":                    "Trusted observer",
		"status":                   "completed",
		"content":                  []interface{}{map[string]interface{}{"type": "content", "content": map[string]interface{}{"type": "text", "text": output}}},
		"attestationSchemaVersion": "doops.tool-attestation/v1",
		"contextSchemaVersion":     "doops.reconciliation-context/v1",
		"operationId":              "op_0123456789abcdef0123456789abcdef",
		"contextDigest":            "sha256:" + strings.Repeat("2", 64),
		"planDigest":               "sha256:" + strings.Repeat("3", 64),
		"planBindingDigest":        "sha256:" + strings.Repeat("4", 64),
		"executionMode":            "apply",
		"mutationAuthorized":       authorized,
		"capabilityKey":            subject,
		"attestedTool":             strings.TrimPrefix(toolName, "mcp_doops_plan_"),
		"evidenceKind":             subject,
		"evidenceSubject":          subject,
		"canonicalScope":           map[string]interface{}{"subject": subject},
		"scopeDigest":              "sha256:" + strings.Repeat("5", 64),
		"inputDigest":              "sha256:" + strings.Repeat("6", 64),
		"resultDigest":             "sha256:" + strings.Repeat("7", 64),
	}
}

func boolPointer(value bool) *bool {
	return &value
}
