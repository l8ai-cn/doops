package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDoagentModeForPromptUsesNativeModes(t *testing.T) {
	tests := []struct {
		name          string
		operation     string
		executionMode string
		want          string
		wantErr       bool
	}{
		{name: "general", want: "auto"},
		{name: "reconcile dry run", operation: "reconcile", executionMode: "dry-run", want: "plan"},
		{name: "normalized reconcile", operation: " ReConCiLe ", executionMode: " Dry-Run ", want: "plan"},
		{name: "reconcile apply", operation: "reconcile", executionMode: "apply", want: "build"},
		{name: "missing reconcile authorization", operation: "reconcile", wantErr: true},
		{name: "mode without operation", executionMode: "apply", wantErr: true},
		{name: "unknown operation", operation: "custom", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := doagentModeForPrompt(test.operation, test.executionMode)
			if test.wantErr {
				if err == nil {
					t.Fatalf("expected mode mapping error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("map native mode: %v", err)
			}
			if got != test.want {
				t.Fatalf("native mode mismatch: got %q want %q", got, test.want)
			}
		})
	}
}

func TestAgentPromptSetsNativeModeForEveryPrompt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	sessionID := "native-mode"
	commit := strings.Repeat("a", 40)
	if err := os.MkdirAll(filepath.Join(root, sessionID), 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, sessionID, ".doops-ready"), []byte(commit), 0o644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
	}

	var mu sync.Mutex
	var modes []string
	prompted := make(chan struct{}, 2)
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			var req map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode rpc: %v", err)
				return
			}
			switch req["method"] {
			case "session/new":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"sessionId": "doagent-native-mode"},
				})
			case "session/setMode":
				params, _ := req["params"].(map[string]interface{})
				mode, _ := params["modeId"].(string)
				mu.Lock()
				modes = append(modes, mode)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{},
				})
			case "session/prompt":
				prompted <- struct{}{}
				w.WriteHeader(http.StatusAccepted)
			default:
				t.Errorf("unexpected rpc method: %v", req["method"])
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-prompted
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message","content":{"type":"text","text":"done"}}}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","turnId":"turn-native-mode","status":"completed","stopReason":"end_turn"}}}`)
			fmt.Fprintln(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	first := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":  sessionID,
		"instruction": "inspect the workspace",
	})
	if first["error"] != nil {
		t.Fatalf("general prompt failed: %#v", first)
	}
	second := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":       sessionID,
		"instruction":      "reconcile the plan",
		"operation":        "reconcile",
		"plan_digest":      "sha256:" + strings.Repeat("b", 64),
		"execution_mode":   "dry-run",
		"workspace_commit": commit,
	})
	if second["error"] != nil {
		t.Fatalf("reconcile prompt failed: %#v", second)
	}

	mu.Lock()
	gotModes := append([]string(nil), modes...)
	mu.Unlock()
	if len(gotModes) != 2 || gotModes[0] != "auto" || gotModes[1] != "plan" {
		t.Fatalf("native mode must be reset for every prompt, got %#v", gotModes)
	}
}

func TestDoagentPermissionRequestFailsClosedWithoutReply(t *testing.T) {
	rpcCalled := make(chan struct{}, 1)
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"permission.updated","params":{"sessionId":"permission-session","permission":{"id":"permission-1","title":"run mutating tool","toolName":"shell"}}}`)
			fmt.Fprintln(w)
		case "/rpc":
			rpcCalled <- struct{}{}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"jsonrpc": "2.0",
				"result":  map[string]interface{}{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()

	err := subscribeDoagentSSEWithCollector(
		context.Background(),
		doagent.URL,
		"permission-session",
		func(notificationEvent) {},
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "permission required") {
		t.Fatalf("unexpected permission result: %v", err)
	}
	select {
	case <-rpcCalled:
		t.Fatal("DoOps bridge must never reply to doagent permission requests")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAgentPromptAttestsReconciliationWithToolTrace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	sessionID := "attested-reconcile"
	workspaceCommit := strings.Repeat("c", 40)
	planDigest := "sha256:" + strings.Repeat("d", 64)
	if err := os.MkdirAll(filepath.Join(root, sessionID), 0o755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, sessionID, ".doops-ready"), []byte(workspaceCommit), 0o644); err != nil {
		t.Fatalf("write workspace marker: %v", err)
	}

	prompted := make(chan struct{})
	finalMessage := fmt.Sprintf(
		`{"apiVersion":"doops.sh/v2","kind":"ReconciliationResult","planDigest":%q,"status":"converged","attempts":1,"noProgressAttempts":0,"evidence":[{"kind":"runtime-state","subject":"service","observedAt":"2026-07-13T00:00:00Z","value":"ready","toolRef":{"tool":"WebFetch","ordinal":1}}],"failureEvidence":[]}`,
		planDigest,
	)
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			var req map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode rpc: %v", err)
				return
			}
			switch req["method"] {
			case "session/new":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"sessionId": "doagent-attested"},
				})
			case "session/setMode":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{},
				})
			case "session/prompt":
				params, _ := req["params"].(map[string]interface{})
				prompt, _ := params["prompt"].(string)
				for _, required := range []string{
					"toolRef",
					"sourceRevision " + strings.Repeat("e", 40),
					"workspaceCommit " + workspaceCommit,
					".doops/source.json",
					"do not compare the workspace Git HEAD with sourceRevision",
				} {
					if !strings.Contains(prompt, required) {
						t.Errorf("reconciliation prompt is missing %q: %s", required, prompt)
					}
				}
				resultPath := filepath.Join(root, sessionID, ".doops", "structured-result.json")
				if err := os.WriteFile(resultPath, []byte(finalMessage), 0o600); err != nil {
					t.Errorf("write structured result: %v", err)
					return
				}
				close(prompted)
				w.WriteHeader(http.StatusAccepted)
			default:
				t.Errorf("unexpected rpc method: %v", req["method"])
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-prompted
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-delegate","toolName":"Agent","status":"completed","input":{"subagent_type":"verification","prompt":"inspect deployment evidence"},"content":[{"type":"content","content":{"type":"text","text":"verification delegated"}}]}}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"call-observe","toolName":"WebFetch","status":"completed","input":{"url":"https://service.example/health"},"content":[{"type":"content","content":{"type":"text","text":"runtime ready"}}]}}}`)
			fmt.Fprintln(w)
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message\",\"content\":{\"type\":\"text\",\"text\":%q}}}}\n\n", finalMessage)
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","turnId":"turn-attested","status":"completed","stopReason":"end_turn"}}}`)
			fmt.Fprintln(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":       sessionID,
		"instruction":      "reconcile",
		"response_format":  "json",
		"operation":        "reconcile",
		"plan_digest":      planDigest,
		"execution_mode":   "apply",
		"source_revision":  strings.Repeat("e", 40),
		"workspace_commit": workspaceCommit,
	})
	toolResult, _ := result["result"].(map[string]interface{})
	structured, _ := toolResult["structuredContent"].(map[string]interface{})
	execution, _ := structured["executionEvidence"].(map[string]interface{})
	if execution["turnId"] != "turn-attested" ||
		execution["sourceRevision"] != strings.Repeat("e", 40) ||
		execution["workspaceCommit"] != workspaceCommit {
		t.Fatalf("missing bridge execution attestation: %#v", structured)
	}
	toolCalls, _ := execution["toolCalls"].([]interface{})
	if len(toolCalls) != 2 {
		t.Fatalf("expected delegation and observation tool calls: %#v", execution)
	}
	var delegated map[string]interface{}
	var observed map[string]interface{}
	for _, raw := range toolCalls {
		toolCall, _ := raw.(map[string]interface{})
		switch toolCall["callId"] {
		case "call-delegate":
			delegated = toolCall
		case "call-observe":
			observed = toolCall
		}
	}
	if delegated["tool"] != "Agent" || delegated["observation"] != false {
		t.Fatalf("native multi-Agent delegation must be traced but not treated as evidence: %#v", execution)
	}
	if observed["tool"] != "WebFetch" || observed["observation"] != true {
		t.Fatalf("completed observation must remain evidence-capable: %#v", execution)
	}
	evidence, _ := structured["evidence"].([]interface{})
	item, _ := evidence[0].(map[string]interface{})
	if item["toolCallId"] != "call-observe" || item["toolDigest"] != observed["digest"] {
		t.Fatalf("evidence must bind to its completed observation tool call: %#v", structured)
	}
	if _, exists := item["toolRef"]; exists {
		t.Fatalf("bridge must replace the model-visible selector with runtime attestation: %#v", structured)
	}
	if item["traceDigest"] == "" || item["traceDigest"] != execution["traceDigest"] {
		t.Fatalf("evidence must bind to the bridge trace: %#v", structured)
	}
}

func TestAttestReconciliationResultRejectsInvalidEvidenceToolReferences(t *testing.T) {
	execution := agentPromptExecutionContext{
		PlanDigest:      "sha256:" + strings.Repeat("d", 64),
		SourceRevision:  strings.Repeat("e", 40),
		WorkspaceCommit: strings.Repeat("c", 40),
	}
	tests := []struct {
		name    string
		item    map[string]interface{}
		updates []map[string]interface{}
		want    string
	}{
		{
			name: "missing toolRef",
			item: map[string]interface{}{},
			want: "toolRef is required",
		},
		{
			name: "model supplied toolCallId",
			item: map[string]interface{}{
				"toolCallId": "call-observe",
				"toolRef": map[string]interface{}{
					"tool":    "WebFetch",
					"ordinal": float64(1),
				},
			},
			updates: []map[string]interface{}{
				completedObservationUpdate("call-observe"),
			},
			want: "toolCallId is bridge-managed",
		},
		{
			name: "out of range ordinal",
			item: map[string]interface{}{
				"toolRef": map[string]interface{}{
					"tool":    "WebFetch",
					"ordinal": float64(2),
				},
			},
			updates: []map[string]interface{}{
				completedObservationUpdate("call-observe"),
			},
			want: `toolRef WebFetch#2 was not observed`,
		},
		{
			name: "exact tool name is required",
			item: map[string]interface{}{
				"toolRef": map[string]interface{}{
					"tool":    "webfetch",
					"ordinal": float64(1),
				},
			},
			updates: []map[string]interface{}{
				completedObservationUpdate("call-observe"),
			},
			want: `toolRef webfetch#1 was not observed`,
		},
		{
			name: "toolRef whitespace is rejected",
			item: map[string]interface{}{
				"toolRef": map[string]interface{}{
					"tool":    " WebFetch ",
					"ordinal": float64(1),
				},
			},
			updates: []map[string]interface{}{
				completedObservationUpdate("call-observe"),
			},
			want: "toolRef.tool must match the exact runtime tool name",
		},
		{
			name: "duplicate terminal call ID",
			item: map[string]interface{}{
				"toolRef": map[string]interface{}{
					"tool":    "WebFetch",
					"ordinal": float64(1),
				},
			},
			updates: []map[string]interface{}{
				completedObservationUpdate("call-duplicate"),
				completedObservationUpdate("call-duplicate"),
			},
			want: `terminal toolCallId "call-duplicate" was observed more than once`,
		},
		{
			name: "failed referenced call",
			item: map[string]interface{}{
				"toolRef": map[string]interface{}{
					"tool":    "WebFetch",
					"ordinal": float64(1),
				},
			},
			updates: []map[string]interface{}{
				{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call-failed",
					"toolName":      "WebFetch",
					"status":        "failed",
					"input":         map[string]interface{}{"url": "https://service.example/health"},
					"content":       []interface{}{"connection refused"},
				},
			},
			want: `toolRef WebFetch#1 did not complete successfully`,
		},
		{
			name: "non observation referenced call",
			item: map[string]interface{}{
				"toolRef": map[string]interface{}{
					"tool":    "ExecuteCode",
					"ordinal": float64(1),
				},
			},
			updates: []map[string]interface{}{
				{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call-echo",
					"toolName":      "ExecuteCode",
					"status":        "completed",
					"input":         map[string]interface{}{"command": "echo ready"},
					"content":       []interface{}{"ready"},
				},
			},
			want: `toolRef ExecuteCode#1 is not an observation call`,
		},
		{
			name: "unknown tools fail closed",
			item: map[string]interface{}{
				"toolRef": map[string]interface{}{
					"tool":    "UnknownMutator",
					"ordinal": float64(1),
				},
			},
			updates: []map[string]interface{}{
				{
					"sessionUpdate": "tool_call_update",
					"toolCallId":    "call-unknown",
					"toolName":      "UnknownMutator",
					"status":        "completed",
					"input":         map[string]interface{}{"mutate": true},
					"content":       []interface{}{"done"},
				},
			},
			want: `toolRef UnknownMutator#1 is not an observation call`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			records := make([]doagentToolTraceRecord, 0, len(test.updates))
			for _, update := range test.updates {
				record, ok, err := doagentToolTraceRecordFromUpdate(update)
				if err != nil {
					t.Fatalf("parse tool update: %v", err)
				}
				if !ok {
					t.Fatalf("expected terminal tool update: %#v", update)
				}
				records = append(records, record)
			}
			item := map[string]interface{}{
				"kind":       "runtime-state",
				"subject":    "service",
				"observedAt": "2026-07-13T00:00:00Z",
				"value":      "ready",
			}
			for key, value := range test.item {
				item[key] = value
			}
			result := map[string]interface{}{
				"status": "converged",
				"evidence": []interface{}{
					item,
				},
				"failureEvidence": []interface{}{},
			}

			err := attestReconciliationResult(result, execution, "turn-test", records)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("unexpected attestation error: %v", err)
			}
		})
	}
}

func TestDoagentToolTraceRejectsNonExactRuntimeToolName(t *testing.T) {
	_, _, err := doagentToolTraceRecordFromUpdate(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-observe",
		"toolName":      " WebFetch ",
		"status":        "completed",
	})
	if err == nil || !strings.Contains(err.Error(), "toolName must be exact") {
		t.Fatalf("runtime tool names with surrounding whitespace must be rejected: %v", err)
	}
}

func TestDoagentObservationToolUsesFailClosedAllowlist(t *testing.T) {
	for _, tool := range []string{"Read", "list_files", "glob", "grep", "search", "WebFetch", "WebSearch"} {
		if !doagentObservationTool(tool) {
			t.Fatalf("known read-only runtime tool must be observation-capable: %s", tool)
		}
	}
	for _, tool := range []string{"ExecuteCode", "Bash", "Write", "UnknownMutator", "Read "} {
		if doagentObservationTool(tool) {
			t.Fatalf("unknown, mutating, or non-exact tool must fail closed: %s", tool)
		}
	}
}

func TestAttestReconciliationResultResolvesSameToolOrdinalFromSSEOrder(t *testing.T) {
	execution := agentPromptExecutionContext{
		PlanDigest:      "sha256:" + strings.Repeat("d", 64),
		SourceRevision:  strings.Repeat("e", 40),
		WorkspaceCommit: strings.Repeat("c", 40),
	}
	records := make([]doagentToolTraceRecord, 0, 3)
	for _, update := range []map[string]interface{}{
		completedObservationUpdate("call-z"),
		{
			"sessionUpdate": "tool_call_update",
			"toolCallId":    "call-a",
			"toolName":      "Read",
			"status":        "completed",
			"input":         map[string]interface{}{"path": "/tmp/status"},
			"content":       []interface{}{"ready"},
		},
		completedObservationUpdate("call-m"),
	} {
		record, ok, err := doagentToolTraceRecordFromUpdate(update)
		if err != nil || !ok {
			t.Fatalf("parse terminal update: ok=%v err=%v", ok, err)
		}
		records = append(records, record)
	}
	result := map[string]interface{}{
		"evidence": []interface{}{
			map[string]interface{}{
				"kind":       "runtime-state",
				"subject":    "service",
				"observedAt": "2026-07-13T00:00:00Z",
				"value":      "ready",
				"toolRef": map[string]interface{}{
					"tool":    "WebFetch",
					"ordinal": float64(2),
				},
			},
		},
		"failureEvidence": []interface{}{},
	}

	if err := attestReconciliationResult(result, execution, "turn-test", records); err != nil {
		t.Fatalf("attest result: %v", err)
	}
	evidence := result["evidence"].([]interface{})[0].(map[string]interface{})
	if evidence["toolCallId"] != "call-m" {
		t.Fatalf("ordinal must resolve against original same-tool terminal SSE order: %#v", evidence)
	}
	executionEvidence := result["executionEvidence"].(map[string]interface{})
	toolCalls := executionEvidence["toolCalls"].([]doagentToolTraceRecord)
	if len(toolCalls) != 3 ||
		toolCalls[0].CallID != "call-z" ||
		toolCalls[1].CallID != "call-a" ||
		toolCalls[2].CallID != "call-m" {
		t.Fatalf("execution evidence must preserve original terminal SSE order: %#v", toolCalls)
	}
}

func TestAttestReconciliationResultRejectsAgentAuthoredExecutionEvidence(t *testing.T) {
	result := map[string]interface{}{
		"evidence":        []interface{}{},
		"failureEvidence": []interface{}{},
		"executionEvidence": map[string]interface{}{
			"turnId": "forged-turn",
		},
	}
	err := attestReconciliationResult(
		result,
		agentPromptExecutionContext{
			PlanDigest:      "sha256:" + strings.Repeat("d", 64),
			SourceRevision:  strings.Repeat("e", 40),
			WorkspaceCommit: strings.Repeat("c", 40),
		},
		"turn-test",
		nil,
	)
	if err == nil || !strings.Contains(err.Error(), "executionEvidence is bridge-managed") {
		t.Fatalf("Agent-authored execution evidence must be rejected: %v", err)
	}
}

func completedObservationUpdate(callID string) map[string]interface{} {
	return map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    callID,
		"toolName":      "WebFetch",
		"status":        "completed",
		"input":         map[string]interface{}{"url": "https://service.example/health"},
		"content":       []interface{}{"ready"},
	}
}
