package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Exercise the production stdio executor and Gateway SSE/WebSocket bridge.
// Only Helm's cluster process and DoAgent's model/attestation transport are fixtures.
func TestAgentApplyRuntimeReceiptBridge(t *testing.T) {
	for _, scenario := range []struct {
		name, first, format, want string
	}{
		{"corrected parameters", "validation", "json", "succeeded"},
		{"text apply still returns receipts", "validation", "", "succeeded"},
		{"missing target cannot be inferred", "empty", "json", "failed"},
		{"executed failure survives later success", "execution", "json", "failed"},
		{"rollback recovered with same revision and corrected timeout", "rollback-recovered", "json", "succeeded"},
		{"upgrade recovered with corrected chart values", "upgrade-recovered", "json", "succeeded"},
		{"initial install schema failure recovered after correction", "upgrade-not-found-recovered", "json", "succeeded"},
		{"unreadable upgrade history remains unknown", "upgrade-history", "json", "unknown"},
		{"invalid upgrade result remains unknown", "upgrade-result", "json", "unknown"},
		{"unavailable upgrade status remains unknown", "upgrade-status", "json", "unknown"},
		{"pending upgrade remains unknown", "upgrade-pending", "json", "unknown"},
		{"unexecuted preparation corrected", "preparation", "json", "succeeded"},
		{"interrupted execution stays unknown", "unknown", "json", "unknown"},
		{"missing raw output stays unknown", "missing-raw", "json", "unknown"},
		{"tampered receipt stays unknown", "tampered", "json", "unknown"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			const sessionID, agentSessionID, turnID = "receipt-bridge", "receipt-agent", "receipt-turn"
			workspace := t.TempDir()
			t.Setenv("DOOPS_WORKSPACE_ROOT", workspace)
			bin := t.TempDir()
			behavior := "exit 0"
			switch scenario.first {
			case "execution":
				behavior = "if [ \"$3\" = 2 ]; then printf 'Error: synthetic Helm failure\\n' >&2; exit 9; fi"
			case "unknown":
				behavior = "if [ \"$3\" = 2 ]; then exec sleep 30; fi"
			case "rollback-recovered":
				behavior = `for arg in "$@"; do
  if [ "$arg" = "1ms" ]; then printf 'Error: timed out waiting for the condition\n' >&2; exit 9; fi
done`
			}
			if strings.HasPrefix(scenario.first, "upgrade-") {
				chart := filepath.Join(workspace, sessionID, "chart")
				if err := os.MkdirAll(chart, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(chart, "Chart.yaml"), []byte("apiVersion: v2\nname: app\nversion: 1.0.0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("RECEIPT_HELM_STATE", t.TempDir())
				t.Setenv("RECEIPT_HELM_FAILURE", scenario.first)
				behavior = `case "$1" in
history)
  case "$RECEIPT_HELM_FAILURE" in
  upgrade-history) exit 1;;
  upgrade-not-found-recovered) printf 'Error: release: not found\n' >&2; exit 1;;
  esac
  cat "$RECEIPT_HELM_STATE/history.json"; exit $?;;
status) cat "$RECEIPT_HELM_STATE/status.json"; exit $?;;
esac
previous=""; description=""; value=""
for arg in "$@"; do
  [ "$previous" != "--description" ] || description="$arg"
  [ "$previous" != "--set-string" ] || value="$arg"
  previous="$arg"
done
if [ "$value" = "service.port=bad" ]; then
  printf '[{"revision":2,"status":"failed","description":"%s"}]' "$description" > "$RECEIPT_HELM_STATE/history.json"
  case "$RECEIPT_HELM_FAILURE" in
  upgrade-result) printf 'invalid release receipt'; exit 0;;
  upgrade-status) printf '{"version":2,"info":{"description":"%s"}}' "$description"; exit 0;;
  upgrade-pending)
    printf '{"version":2,"info":{"status":"pending-upgrade","description":"%s"}}' "$description" > "$RECEIPT_HELM_STATE/status.json"
    cat "$RECEIPT_HELM_STATE/status.json"; exit 0;;
  esac
  printf 'Error: chart values validation failed\n' >&2
  printf '%01800d\n' 0 >&2
  printf 'service.port must be numeric\n' >&2; exit 9
fi
printf '{"version":3,"info":{"status":"deployed","description":"%s"}}' "$description" > "$RECEIPT_HELM_STATE/status.json"
cat "$RECEIPT_HELM_STATE/status.json"
exit 0`
			}
			writeK8SMCPExecutable(t, bin, "helm", "#!/bin/sh\n"+behavior+"\nexit 0\n")
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			previousSecret := processMutationAuthoritySecret
			processMutationAuthoritySecret = strings.Repeat("b", 64)
			t.Cleanup(func() { processMutationAuthoritySecret = previousSecret })

			promptReady := make(chan map[string]interface{}, 1)
			receiptsReady := make(chan struct{})
			var wireReceipts []interface{}
			const summary = `{"verified":false,"outcome":"model-claimed","mutations":["fabricated"]}`
			doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/rpc":
					var request map[string]interface{}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						return
					}
					result := map[string]interface{}{}
					switch request["method"] {
					case "initialize":
						result = doagentAdmissionCapabilities()
					case "session/new":
						result["sessionId"] = agentSessionID
					case "session/setMode":
					case "session/prompt":
						params := request["params"].(map[string]interface{})
						if params["completionReview"] != true {
							t.Error("apply omitted doagent completion review")
						}
						if budget, ok := params["wallClockTimeoutSecs"].(float64); !ok || budget <= 0 || budget > 1800 {
							t.Errorf("prompt omitted the remaining execution deadline: %v", params["wallClockTimeoutSecs"])
						}
						promptReady <- params
						result["status"], result["turnId"] = "accepted", turnID
					default:
						t.Errorf("unexpected RPC: %v", request["method"])
					}
					_ = json.NewEncoder(w).Encode(map[string]interface{}{
						"jsonrpc": "2.0", "id": request["id"], "result": result,
					})
				case "/events":
					w.Header().Set("Content-Type", "text/event-stream")
					flusher := w.(http.Flusher)
					flusher.Flush()
					params := <-promptReady
					_, authorityDigest, err := signMutationAuthorization(
						processMutationAuthoritySecret, agentSessionID,
						params["mutationAuthorizationId"].(string),
						int64(params["mutationAuthorizationIssuedAt"].(float64)),
						doagentAllowedTools(true, ""), params["mutationContextDigest"].(string),
					)
					if err != nil {
						t.Error(err)
						return
					}
					send := func(update map[string]interface{}) {
						update["turnId"] = turnID
						encoded, _ := json.Marshal(map[string]interface{}{
							"jsonrpc": "2.0", "method": "session/update",
							"params": map[string]interface{}{"sessionId": agentSessionID, "update": update},
						})
						fmt.Fprintf(w, "data: %s\n\n", encoded)
						flusher.Flush()
					}
					for index, id := range []string{"z-first", "a-correction"} {
						input := map[string]interface{}{
							"operation": "rollback", "release": "app", "namespace": "apps", "revision": 2 + index,
						}
						if scenario.first == "rollback-recovered" {
							input["revision"] = 2
							input["timeout"] = []string{"1ms", "1m30s"}[index]
						}
						if strings.HasPrefix(scenario.first, "upgrade-") {
							delete(input, "revision")
							input["operation"], input["chart"], input["asset_source"] = "upgrade-install", "chart", "workspace"
							input["set_strings"] = map[string]interface{}{"service.port": []string{"bad", "8080"}[index]}
						}
						if index == 0 {
							switch scenario.first {
							case "validation":
								input["revision"] = 0
							case "empty":
								input = map[string]interface{}{}
							}
						}
						send(map[string]interface{}{
							"sessionUpdate": "tool_call", "toolCallId": id, "toolName": "mcp_doops_helm",
							"status": "in_progress", "input": input,
						})
						request, _ := json.Marshal(map[string]interface{}{
							"jsonrpc": "2.0", "id": index + 1, "method": "tools/call",
							"params": map[string]interface{}{"name": "helm", "arguments": input},
						})
						var output bytes.Buffer
						callContext := t.Context()
						cancel := func() {}
						if index == 0 && scenario.first == "unknown" {
							callContext, cancel = context.WithTimeout(callContext, 500*time.Millisecond)
						}
						err := runK8SMCPStdioContext(callContext, strings.NewReader(string(request)+"\n"), &output, func() (string, error) {
							if index == 0 && scenario.first == "preparation" {
								return "", errors.New("synthetic session preparation failure")
							}
							return sessionID, nil
						})
						cancel()
						if err != nil {
							t.Error(err)
							return
						}
						response := decodeK8SMCPResponse(t, output.Bytes())
						if response["error"] != nil {
							t.Errorf("MCP receipt was lost before attestation: %#v", response)
							return
						}
						result := response["result"].(map[string]interface{})
						wireReceipts = append(wireReceipts, result)
						trace := attestedMutationTraceForSession(t, "mcp_doops_helm", input, sessionID, authorityDigest)
						trace.ToolCallID = id
						trace.resultJSON, _ = json.Marshal(result)
						trace.AttestedResultDigest, _ = digestRuntimeAttestationJSON(result)
						if result["isError"] == true {
							trace.Status = "failed"
						}
						update := testRuntimeTraceUpdate(t, trace)
						if index == 0 {
							switch scenario.first {
							case "missing-raw":
								delete(update, "rawOutput")
								update["status"] = "failed"
								update["content"] = []interface{}{map[string]interface{}{
									"type": "content", "content": map[string]interface{}{
										"type": "text", "text": `{"passwd":"synthetic-raw-canary"}`,
									},
								}}
							case "tampered":
								update["resultDigest"] = "sha256:" + strings.Repeat("0", 64)
							}
						}
						send(update)
					}
					close(receiptsReady)
					send(map[string]interface{}{
						"sessionUpdate": "agent_message", "content": map[string]interface{}{"type": "text", "text": summary},
					})
					send(map[string]interface{}{
						"sessionUpdate": "turn_finished", "status": "completed", "stopReason": "end_turn",
					})
				default:
					http.NotFound(w, r)
				}
			}))
			defer doagent.Close()
			t.Setenv("DO_AGENT_URL", doagent.URL)
			gateway := newGatewayAuthorizedAgentServer(t, NewGateway())
			defer gateway.Close()
			conn := dialAgentTestWS(t, gateway.URL)
			defer conn.Close()
			initializeAgentTestWS(t, conn)
			sendCancellationTestTool(t, conn, "doops_agent_prompt", map[string]interface{}{
				"session_id": sessionID, "operation": "apply", "instruction": "rollback app using the authorized revision",
				"response_format": scenario.format,
			})
			var response map[string]interface{}
			var statuses []string
			for {
				var message map[string]interface{}
				if err := conn.ReadJSON(&message); err != nil {
					t.Fatal(err)
				}
				if message["method"] == "notifications/message" {
					params, _ := message["params"].(map[string]interface{})
					if params["kind"] == "tool" {
						statuses = append(statuses, fmt.Sprint(params["status"]))
					}
					if params["kind"] == "assistant_delta" {
						t.Fatalf("unverified model summary became the CLI answer: %#v", message)
					}
					continue
				}
				response = message
				break
			}
			<-receiptsReady
			if len(statuses) != 4 || statuses[0] != "in_progress" || statuses[2] != "in_progress" || statuses[3] != "completed" {
				t.Fatalf("WebSocket lost tool lifecycle: %v", statuses)
			}
			var report map[string]interface{}
			if scenario.want == "unknown" {
				rpcError, _ := response["error"].(map[string]interface{})
				if !strings.Contains(fmt.Sprint(rpcError["message"]), "execution_outcome_unknown") {
					t.Fatalf("unknown execution became known: %#v", response)
				}
				data, _ := rpcError["data"].(map[string]interface{})
				report, _ = data["structuredContent"].(map[string]interface{})
			} else {
				result, _ := response["result"].(map[string]interface{})
				if response["error"] != nil || result["isError"] != (scenario.want == "failed") {
					t.Fatalf("wrong terminal result: %#v", response)
				}
				report, _ = result["structuredContent"].(map[string]interface{})
				if scenario.want == "failed" && !strings.Contains(fmt.Sprint(result["content"]), "doagent_apply_tool_failed") {
					t.Fatalf("model failure replaced tool failure: %#v", response)
				}
			}
			if report["outcome"] != scenario.want || report["summary"] != summary {
				t.Fatalf("model replaced runtime facts: %#v", report)
			}
			history, _ := report["mutations"].([]interface{})
			if len(history) != 2 {
				t.Fatalf("incomplete history: %#v", report)
			}
			for index, id := range []string{"z-first", "a-correction"} {
				entry := history[index].(map[string]interface{})
				if entry["tool_call_id"] != id {
					t.Fatalf("history is not in call order: %#v", history)
				}
				if index == 0 && (scenario.first == "missing-raw" || scenario.first == "tampered") {
					if entry["verified"] != false || entry["result"] != nil {
						t.Fatalf("untrusted result was published: %#v", entry)
					}
					continue
				}
				original, _ := json.Marshal(wireReceipts[index])
				kept, _ := json.Marshal(entry["result"])
				if string(original) != string(kept) || entry["verified"] != true {
					t.Fatalf("complete MCP receipt was not preserved: original=%s got=%s", original, kept)
				}
			}
			first := history[0].(map[string]interface{})
			if scenario.first == "upgrade-recovered" || scenario.first == "upgrade-not-found-recovered" {
				encoded, _ := json.Marshal(first["result"])
				if !strings.Contains(string(encoded), "chart values validation failed") ||
					!strings.Contains(string(encoded), "service.port must be numeric") {
					t.Fatalf("multiline Helm cause did not reach the final receipt: %s", encoded)
				}
			}
			if strings.HasPrefix(scenario.first, "upgrade-") || scenario.first == "rollback-recovered" {
				receipt := first["result"].(map[string]interface{})
				facts := receipt["structuredContent"].(map[string]interface{})
				if receipt["isError"] != true || facts["executionStarted"] != true || facts["phase"] != "execution" {
					t.Fatalf("executed failure was relabeled as a parameter rejection: %#v", first)
				}
				if (facts["executionOutcome"] == "unknown") != (scenario.want == "unknown") {
					t.Fatalf("execution uncertainty was lost before attestation: %#v", first)
				}
			}
			if scenario.want == "succeeded" && first["resolved_by"] != "a-correction" {
				t.Fatalf("correction was not linked: %#v", first)
			}
			if scenario.want != "succeeded" && first["resolved_by"] != nil {
				t.Fatalf("unsafe failure was hidden: %#v", first)
			}
			if strings.Contains(fmt.Sprint(response), "synthetic-raw-canary") {
				t.Fatalf("untrusted raw failure content leaked: %#v", response)
			}
		})
	}
}

func TestSubscribeDoagentSSEForwardsToolLifecycleAndHonestWaiting(t *testing.T) {
	previous := doagentWaitingNoticeInterval
	doagentWaitingNoticeInterval = 10 * time.Millisecond
	t.Cleanup(func() { doagentWaitingNoticeInterval = previous })
	notices := make(chan notificationEvent, 32)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		send := func(update string) {
			fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":%s}}\n\n", update)
			flusher.Flush()
		}
		send(`{"sessionUpdate":"tool_call","toolCallId":"helm","toolName":"mcp_doops_helm","status":"in_progress"}`)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		send(`{"sessionUpdate":"tool_call_update","toolCallId":"helm","status":"failed"}`)
		send(`{"sessionUpdate":"tool_call","toolCallId":"next","toolName":"mcp_doops_helm","status":"in_progress"}`)
		send(`{"sessionUpdate":"tool_call_update","toolCallId":"next","status":"completed"}`)
		send(`{"sessionUpdate":"turn_finished","status":"completed"}`)
	}))
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		done <- subscribeDoagentSSE(t.Context(), server.URL, "waiting", func(event notificationEvent) { notices <- event })
	}()
	var statuses []string
	waited := false
	timeout := time.After(time.Second)
	for {
		select {
		case event := <-notices:
			if event.Kind == "tool" {
				statuses = append(statuses, event.Status)
				if !strings.Contains(event.Data, event.Status) {
					t.Errorf("CLI cannot see status: %#v", event)
				}
			}
			if !waited && strings.Contains(event.Data, "[waiting]") {
				waited = true
				if !strings.Contains(event.Data, "last event: mcp_doops_helm in_progress") {
					t.Errorf("waiting notice invented progress: %#v", event)
				}
				close(release)
			}
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
			for len(notices) > 0 {
				event := <-notices
				if event.Kind == "tool" {
					statuses = append(statuses, event.Status)
				}
			}
			if !waited || strings.Join(statuses, ",") != "in_progress,failed,in_progress,completed" {
				t.Fatalf("missing waiting or lifecycle: waited=%t statuses=%v", waited, statuses)
			}
			return
		case <-timeout:
			if !waited {
				close(release)
			}
			t.Fatal("no truthful waiting notice or terminal result")
		}
	}
}

func TestApplyDiagnosisProgressDoesNotBecomeFinalEvidence(t *testing.T) {
	for _, structured := range []bool{false, true} {
		t.Run(fmt.Sprint(structured), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, update := range []string{
					`{"sessionUpdate":"tool_call_update","toolCallId":"first","toolName":"mcp_doops_helm","status":"failed"}`,
					`{"sessionUpdate":"agent_progress","content":{"type":"text","text":"Found the missing port; correcting it. password=synthetic-progress-canary"}}`,
					`{"sessionUpdate":"agent_progress","content":{"type":"text","text":"Checking data: {pending synthetic-progress-canary"}}`,
					`{"sessionUpdate":"tool_call_update","toolCallId":"repair","toolName":"mcp_doops_helm","status":"completed"}`,
					`{"sessionUpdate":"turn_finished","status":"completed"}`,
				} {
					fmt.Fprintf(w, "data: {\"jsonrpc\":\"2.0\",\"method\":\"session/update\",\"params\":{\"update\":%s}}\n\n", update)
				}
			}))
			defer server.Close()
			var events []notificationEvent
			var finals int
			err := subscribeDoagentSSEWithCollectorReady(t.Context(), server.URL, "repair",
				func(event notificationEvent) { events = append(events, event) },
				func(update map[string]interface{}) error {
					if update["sessionUpdate"] == "agent_progress" {
						t.Fatal("diagnostic progress must not enter the receipt collector")
					}
					if update["sessionUpdate"] == "turn_finished" {
						finals++
					}
					return nil
				}, nil, false, false, nil, structured)
			if err != nil || finals != 1 {
				t.Fatalf("terminal=%d err=%v", finals, err)
			}
			found := false
			withheld := false
			for index, event := range events {
				if strings.Contains(event.Data, "synthetic-progress-canary") {
					t.Fatal("progress leaked a credential")
				}
				if strings.Contains(event.Data, "correcting it") {
					found = true
					if index != 1 || event.Kind == "assistant_delta" ||
						(structured && !strings.Contains(event.Data, `"sessionUpdate":"doops_notice"`)) {
						t.Fatalf("diagnosis was not ordered nonterminal progress: %#v", events)
					}
				}
				if strings.Contains(event.Data, "progress text withheld") {
					withheld = true
					if index != 2 || !strings.Contains(event.Data, "still waiting for the execution result") {
						t.Fatalf("unsafe progress did not retain its nonterminal status: %#v", event)
					}
				}
			}
			if !found || !withheld {
				t.Fatalf("diagnosis was discarded: %#v", events)
			}
		})
	}
}
