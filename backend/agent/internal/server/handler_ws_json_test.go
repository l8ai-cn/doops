package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentPromptJSONReturnsFinalObjectInStructuredContent(t *testing.T) {
	doagent := newJSONAgentPromptTestServer(t, `{"status":"converged","attempts":1}`)
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

func TestAgentPromptJSONRejectsNonJSONObjectFinalMessage(t *testing.T) {
	doagent := newJSONAgentPromptTestServer(t, "deployment completed")
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
		t.Fatalf("non-JSON terminal message must fail: %#v", result)
	}
	if !strings.Contains(fmt.Sprint(toolResult["content"]), "JSON object") {
		t.Fatalf("expected JSON object validation error, got %#v", result)
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

func newJSONAgentPromptTestServer(t *testing.T, finalMessage string) *httptest.Server {
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
