package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentPromptJSONReturnsResultArtifactInStructuredContent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	workspaceCommit := strings.Repeat("a", 40)
	workspace := filepath.Join(root, "json-prompt")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".doops-ready"), []byte(workspaceCommit+"\n"), 0o600); err != nil {
		t.Fatalf("write workspace commit: %v", err)
	}
	resultPath := filepath.Join(workspace, ".doops", "structured-result.json")
	runtimeCallsPath := filepath.Join(workspace, ".doops", "runtime-tool-calls.json")
	doagent := newJSONAgentPromptTestServer(t, "```json\n{\"ignored\":true}\n```", func(prompt string) {
		if !strings.Contains(prompt, resultPath) {
			t.Errorf("structured prompt must declare the result artifact path: %s", prompt)
		}
		if !strings.Contains(prompt, runtimeCallsPath) {
			t.Errorf("structured prompt must declare the runtime tool call catalog path: %s", prompt)
		}
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
			t.Fatalf("create result directory: %v", err)
		}
		if err := os.WriteFile(resultPath, []byte(`{
			"apiVersion":"doops.sh/v2",
			"kind":"DeploymentRun",
			"spec":{"mode":"dry-run"},
			"status":{
				"phase":"planned",
				"mutationCount":0,
					"evidence":[{
						"subject":"source",
						"module":"doops-source-observer",
						"toolCallId":"tool-source",
						"observedAt":"2026-07-15T00:00:00Z",
					"result":{"revision":"immutable"}
				}]
			}
		}`), 0o600); err != nil {
			t.Fatalf("write structured result: %v", err)
		}
	})
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":       "json-prompt",
		"instruction":      "return JSON",
		"response_format":  "json",
		"workspace_commit": workspaceCommit,
	})
	if result["error"] != nil {
		t.Fatalf("json prompt returned error: %#v", result)
	}
	toolResult, _ := result["result"].(map[string]interface{})
	structured, _ := toolResult["structuredContent"].(map[string]interface{})
	if structured["apiVersion"] != "doops.sh/v2" || structured["kind"] != "DeploymentRun" {
		t.Fatalf("unexpected structured content: %#v", toolResult)
	}
	status, _ := structured["status"].(map[string]interface{})
	if status["phase"] != "planned" || status["mutationCount"] != float64(0) {
		t.Fatalf("unexpected DeploymentRun status: %#v", structured)
	}
	if !strings.HasPrefix(fmt.Sprint(status["resultDigest"]), "sha256:") {
		t.Fatalf("Gateway must bind the result digest to runtime evidence: %#v", status)
	}
	attestation, _ := status["runtimeAttestation"].(map[string]interface{})
	toolCalls, _ := attestation["toolCalls"].([]interface{})
	if len(toolCalls) != 1 {
		t.Fatalf("Gateway must attach one completed tool call: %#v", status)
	}
}

func TestAgentPromptJSONRejectsEvidenceWithoutCompletedToolCall(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	workspaceCommit := strings.Repeat("a", 40)
	workspace := filepath.Join(root, "unbound-evidence")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".doops-ready"), []byte(workspaceCommit+"\n"), 0o600); err != nil {
		t.Fatalf("write workspace commit: %v", err)
	}
	resultPath := filepath.Join(workspace, ".doops", "structured-result.json")
	doagent := newJSONAgentPromptTestServer(t, "done", func(string) {
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
			t.Fatalf("create result directory: %v", err)
		}
		if err := os.WriteFile(resultPath, []byte(`{
			"apiVersion":"doops.sh/v2",
			"kind":"DeploymentRun",
			"spec":{"mode":"dry-run"},
			"status":{
				"phase":"planned",
				"mutationCount":0,
				"evidence":[{
					"subject":"source",
					"module":"doops-source-observer",
					"toolCallId":"invented-call",
					"observedAt":"2026-07-15T00:00:00Z",
					"result":{"revision":"immutable"}
				}]
			}
		}`), 0o600); err != nil {
			t.Fatalf("write structured result: %v", err)
		}
	})
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":       "unbound-evidence",
		"instruction":      `{"task":"execute-doops-cicd-workflow"}`,
		"response_format":  "json",
		"workspace_commit": workspaceCommit,
	})
	toolResult, _ := result["result"].(map[string]interface{})
	if toolResult["isError"] != true ||
		!strings.Contains(fmt.Sprint(toolResult["content"]), "completed runtime tool call") {
		t.Fatalf("unbound evidence must fail: %#v", result)
	}
}

func TestAgentPromptRejectsWorkspaceCommitMismatchBeforeDoagent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	workspace := filepath.Join(root, "workspace-mismatch")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".doops-ready"), []byte(strings.Repeat("b", 40)+"\n"), 0o600); err != nil {
		t.Fatalf("write workspace commit: %v", err)
	}

	doagentRequests := 0
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		doagentRequests++
		t.Fatalf("workspace mismatch must fail before contacting doagent: %s", r.URL.Path)
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
		"session_id":       "workspace-mismatch",
		"instruction":      "return JSON",
		"response_format":  "json",
		"workspace_commit": strings.Repeat("a", 40),
	})
	errObj, _ := result["error"].(map[string]interface{})
	if errObj == nil {
		t.Fatalf("workspace commit mismatch must fail: %#v", result)
	}
	if !strings.Contains(fmt.Sprint(errObj["message"]), "workspace commit mismatch") {
		t.Fatalf("unexpected mismatch error: %#v", errObj)
	}
	if doagentRequests != 0 {
		t.Fatalf("workspace mismatch contacted doagent %d times", doagentRequests)
	}
}

func TestAgentPromptJSONRejectsMissingResultArtifact(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	resultPath := filepath.Join(root, "invalid-json-prompt", ".doops", "structured-result.json")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
		t.Fatalf("create stale result directory: %v", err)
	}
	if err := os.WriteFile(resultPath, []byte(`{"status":"stale"}`), 0o600); err != nil {
		t.Fatalf("write stale structured result: %v", err)
	}
	doagent := newJSONAgentPromptTestServer(t, `{"status":"converged"}`, nil)
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":      "invalid-json-prompt",
		"instruction":     "return JSON",
		"response_format": "json",
	})
	toolResult, _ := result["result"].(map[string]interface{})
	if toolResult["isError"] != true {
		t.Fatalf("missing structured result artifact must fail: %#v", result)
	}
	if !strings.Contains(fmt.Sprint(toolResult["content"]), "structured result artifact") {
		t.Fatalf("expected structured result artifact error, got %#v", result)
	}
	if _, err := os.Stat(resultPath); !os.IsNotExist(err) {
		t.Fatalf("stale structured result must be removed before prompting, stat error: %v", err)
	}
}

func TestReadDoagentStructuredResultRejectsMarkdown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "structured-result.json")
	if err := os.WriteFile(path, []byte("```json\n{\"status\":\"converged\"}\n```"), 0o600); err != nil {
		t.Fatalf("write structured result: %v", err)
	}
	if _, _, err := readDoagentStructuredResult(path); err == nil || !strings.Contains(err.Error(), "one JSON object") {
		t.Fatalf("Markdown-wrapped result artifact must be rejected, got %v", err)
	}
}

func TestAgentPromptJSONRejectsUnsupportedResponseFormat(t *testing.T) {
	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":      "unsupported-response-format",
		"instruction":     "hello",
		"response_format": "text",
	})
	errObj, _ := result["error"].(map[string]interface{})
	if !strings.Contains(fmt.Sprint(errObj["message"]), "response_format") {
		t.Fatalf("unsupported response format must be rejected: %#v", result)
	}
}

func newJSONAgentPromptTestServer(t *testing.T, finalMessage string, onPrompt func(string)) *httptest.Server {
	t.Helper()
	prompted := make(chan struct{})
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			var req map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode rpc: %v", err)
			}
			switch req["method"] {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{},
				})
			case "session/new":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"sessionId": "doagent-json"},
				})
			case "session/setMode":
				params, _ := req["params"].(map[string]interface{})
				if params["modeId"] != "auto" {
					t.Fatalf("generic prompt must use native auto mode: %#v", req)
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{},
				})
			case "session/prompt":
				params, _ := req["params"].(map[string]interface{})
				prompt, _ := params["prompt"].(string)
				if onPrompt != nil {
					onPrompt(prompt)
				}
				close(prompted)
				w.WriteHeader(http.StatusAccepted)
			default:
				t.Fatalf("unexpected rpc method: %v", req["method"])
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-prompted
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"tool-source","toolName":"doops-source-observer","title":"Observe source","status":"in_progress"}}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"tool-source","toolName":"doops-source-observer","title":"Observe source","status":"completed","resultText":"{\"revision\":\"immutable\"}"}}}`)
			fmt.Fprintln(w)
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message\",\"content\":{\"type\":\"text\",\"text\":%q}}}}\n\n", finalMessage)
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","turnId":"turn-json","status":"completed","stopReason":"end_turn"}}}`)
			fmt.Fprintln(w)
		default:
			http.NotFound(w, r)
		}
	}))
}
