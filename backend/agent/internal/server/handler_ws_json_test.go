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
	resultPath := filepath.Join(root, "json-prompt", ".doops", "structured-result.json")
	doagent := newJSONAgentPromptTestServer(t, "```json\n{\"ignored\":true}\n```", func(prompt string) {
		if !strings.Contains(prompt, resultPath) {
			t.Errorf("structured prompt must declare the result artifact path: %s", prompt)
		}
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
			t.Fatalf("create result directory: %v", err)
		}
		if err := os.WriteFile(resultPath, []byte(`{"status":"converged","attempts":1}`), 0o600); err != nil {
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
		"session_id":      "json-prompt",
		"instruction":     "return JSON",
		"response_format": "json",
	})
	if result["error"] != nil {
		t.Fatalf("json prompt returned error: %#v", result)
	}
	toolResult, _ := result["result"].(map[string]interface{})
	structured, _ := toolResult["structuredContent"].(map[string]interface{})
	if structured["status"] != "converged" || structured["attempts"] != float64(1) {
		t.Fatalf("unexpected structured content: %#v", toolResult)
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
	t.Setenv("DOOPS_AGENT_AUTO_APPROVE", "")
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
			<-prompted
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":{\"sessionUpdate\":\"agent_message\",\"content\":{\"type\":\"text\",\"text\":%q}}}}\n\n", finalMessage)
		default:
			http.NotFound(w, r)
		}
	}))
}
