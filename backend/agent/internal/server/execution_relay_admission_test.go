package server

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/user/doops/agent/api"
)

func TestExecutionRelayPromptAdmissionInterruption(t *testing.T) {
	rpcError := func(code int, message string) map[string]interface{} {
		return map[string]interface{}{"error": map[string]interface{}{"code": code, "message": message}}
	}
	cases := []struct {
		name  string
		tool  string
		reply map[string]interface{}
		known bool
	}{
		{name: "queue_cancelled", reply: rpcError(-32603, doagentPromptQueued), known: true},
		{name: "no_prompt", reply: rpcError(-32602, doagentPromptNoPrompt), known: true},
		{name: "rejected", reply: rpcError(-32003, "doagent_prompt_rejected"), known: true},
		{name: "queue_timeout", reply: rpcError(-32603, "doagent_prompt_queue_timeout"), known: true},
		{name: "before_prompt", reply: rpcError(-32603, "doagent_sse_unavailable"), known: true},
		{name: "busy", reply: rpcError(-32603, "doagent_prompt_busy"), known: true},
		{name: "admission_unsupported", reply: rpcError(-32603, "doagent_prompt_admission_unsupported"), known: true},
		{name: "withdrawn_before_admission", reply: rpcError(-32603, doagentPromptAdmissionTimeout), known: true},
		{name: "cancelled_before_admission", reply: rpcError(-32007, doagentPromptCancelledBeforeAdmission), known: true},
		{name: "cleanup_unknown", reply: rpcError(-32603, doagentPromptCleanupUnknown)},
		{name: "execution_unknown", reply: rpcError(-32603, "execution_outcome_unknown: prompt_transport_failed")},
		{name: "wrapped_queued", reply: rpcError(-32603, "execution_outcome_unknown: remote error: doagent_prompt_queued")},
		{name: "completed_mutation", reply: map[string]interface{}{"result": map[string]interface{}{"content": []interface{}{}}}},
		{name: "failed_mutation", reply: map[string]interface{}{"result": map[string]interface{}{"isError": true}}},
		{name: "error_with_result", reply: map[string]interface{}{
			"error":  map[string]interface{}{"code": -32603, "message": doagentPromptQueued},
			"result": map[string]interface{}{"isError": true},
		}},
		{name: "error_with_malformed_result", reply: map[string]interface{}{
			"error":  map[string]interface{}{"code": -32603, "message": doagentPromptQueued},
			"result": "malformed",
		}},
		{name: "ordinary_mutation", tool: "doops_shell", reply: rpcError(-32603, doagentPromptQueued)},
	}
	for _, test := range cases {
		for _, interruption := range []string{"cancel", "timeout"} {
			for _, ordering := range []string{"terminal_before_interrupt", "terminal_after_cancel_ack"} {
				t.Run(test.name+"/"+interruption+"/"+ordering, func(t *testing.T) {
					timeout := 5 * time.Second
					if interruption == "timeout" && ordering == "terminal_after_cancel_ack" {
						timeout = 80 * time.Millisecond
					}
					f := newExecutionLifecycleFixture(t, GatewayHubOptions{OperationTimeout: timeout})
					remote := f.agent(t, "prompt-admission-interruption")
					auth, err := f.store.VerifyUserToken(f.admin.Plaintext)
					if err != nil {
						t.Fatal(err)
					}
					tool := test.tool
					if tool == "" {
						tool = "doops_agent_prompt"
					}
					args := map[string]interface{}{
						"session_id": "admission-interruption", "operation": "apply",
						"instruction": "synthetic Helm deployment", "command": "synthetic mutation",
					}
					params, err := json.Marshal(map[string]interface{}{"name": tool, "arguments": args})
					if err != nil {
						t.Fatal(err)
					}
					var ctx context.Context
					var cancel context.CancelFunc
					if interruption == "timeout" && ordering == "terminal_before_interrupt" {
						ctx, cancel = context.WithTimeout(context.Background(), time.Second)
					} else {
						ctx, cancel = context.WithCancel(context.Background())
					}
					entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
					var enterOnce, releaseOnce sync.Once
					var response map[string]interface{}
					var responses int
					var callErr error
					write := func(raw []byte) error {
						var message map[string]interface{}
						if err := json.Unmarshal(raw, &message); err != nil {
							return err
						}
						if message["method"] == "notifications/message" {
							enterOnce.Do(func() { close(entered) })
							<-release
						} else {
							response = message
							responses++
						}
						return nil
					}
					go func() {
						defer close(finished)
						callErr = f.hub.client.handleGatewayToolCall(ctx, auth, "dev", "local", api.JSONRPCRequest{
							JSONRPC: "2.0", ID: 2, Method: "tools/call", Params: params,
						}, write, func(value interface{}) error {
							raw, err := json.Marshal(value)
							if err != nil {
								return err
							}
							return write(raw)
						})
					}()
					t.Cleanup(func() {
						releaseOnce.Do(func() { close(release) })
						cancel()
						_ = remote.conn.Close()
						select {
						case <-finished:
						case <-time.After(3 * time.Second):
							t.Error("Gateway call did not stop")
						}
						requireEventually(t, time.Second, func() bool { return f.hub.getAgent("dev", "local") == nil })
					})
					call := remote.call(t, tool, "admission-interruption")
					op := f.operation(t, "admission-interruption", "running")
					agent := f.hub.getAgent("dev", "local")
					agent.pendingMu.Lock()
					queue := agent.pending[call.id]
					agent.pendingMu.Unlock()
					if queue == nil {
						t.Fatal("dispatched request has no pending queue")
					}
					sendTerminal := func() {
						reply := make(map[string]interface{}, len(test.reply)+2)
						for key, value := range test.reply {
							reply[key] = value
						}
						reply["jsonrpc"], reply["id"] = "2.0", call.id
						if err := remote.write(reply); err != nil {
							t.Fatal(err)
						}
						select {
						case <-queue.terminal:
						case <-time.After(time.Second):
							t.Fatal("original request terminal was not observed")
						}
					}

					if ordering == "terminal_before_interrupt" {
						if err := remote.write(map[string]interface{}{
							"jsonrpc": "2.0", "method": "notifications/message",
							"params": map[string]interface{}{"request_id": call.id, "data": "barrier"},
						}); err != nil {
							t.Fatal(err)
						}
						select {
						case <-entered:
						case <-time.After(time.Second):
							t.Fatal("output barrier was not reached")
						}
						sendTerminal()
						if interruption == "cancel" {
							cancel()
						} else {
							// Wait on the actual parent deadline while forwarding is
							// blocked, with the original terminal already observed.
							<-ctx.Done()
						}
						releaseOnce.Do(func() { close(release) })
					} else {
						if interruption == "cancel" {
							cancel()
						}
						select {
						case id := <-remote.cancels:
							if id != call.id {
								t.Fatalf("cancel request id = %d, want %d", id, call.id)
							}
						case <-time.After(time.Second):
							t.Fatal("Gateway did not request cancellation")
						}
						if err := remote.write(map[string]interface{}{
							"jsonrpc": "2.0", "method": "notifications/message",
							"params": map[string]interface{}{"request_id": call.id, "data": "after-cancel-ack"},
						}); err != nil {
							t.Fatal(err)
						}
						requireEventually(t, time.Second, func() bool {
							queue.mu.Lock()
							defer queue.mu.Unlock()
							return len(queue.items) != 0
						})
						// The fake Agent has sent only the cancel ACK. It cannot
						// replace the original request's terminal or release its lease.
						select {
						case <-queue.terminal:
							t.Fatal("cancel ACK was treated as terminal evidence")
						case <-finished:
							t.Fatal("cancel ACK finished the operation")
						default:
						}
						if event := executionAuditTestEvent(t, f.store, op.AuditID); event.Status != "executing" {
							t.Fatalf("cancel ACK retired the audit: %s", event.Status)
						}
						lease, err := agent.acquireActionLeaseForSession(context.Background(), ActionWrite,
							"workspace:admission-interruption", "admission-interruption", 0, time.Second)
						if err == nil {
							lease.Release()
							t.Fatal("cancel ACK released the workspace lease")
						}
						sendTerminal()
					}

					select {
					case <-finished:
					case <-time.After(3 * time.Second):
						t.Fatal("Gateway did not finish after the original terminal")
					}
					if callErr != nil {
						t.Fatal(callErr)
					}
					if responses != 1 {
						t.Fatalf("terminal response count = %d, want 1", responses)
					}
					wantStatus := "outcome_unknown"
					if test.known {
						wantStatus = "error"
						switch test.name {
						case "withdrawn_before_admission", "queue_timeout":
							wantStatus = "timeout"
						case "cancelled_before_admission":
							wantStatus = "canceled"
						}
						wantJSON, _ := json.Marshal(test.reply["error"])
						var wantError interface{}
						if err := json.Unmarshal(wantJSON, &wantError); err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(response["error"], wantError) || response["id"] != float64(2) {
							t.Errorf("confirmed pre-admission reply changed: got %#v, want error %#v with id 2", response, wantError)
						}
					} else if !executionResponseUnknown(response) {
						t.Errorf("unconfirmed or executed mutation lost uncertainty: %#v", response)
					}
					event := executionAuditTestEvent(t, f.store, op.AuditID)
					if event.Status != wantStatus {
						t.Errorf("audit status = %s, want %s", event.Status, wantStatus)
					}
					if test.known && event.Error != fmt.Sprint(test.reply["error"].(map[string]interface{})["message"]) {
						t.Errorf("audit lost the pre-admission category: %q", event.Error)
					}
					if len(remote.calls) != 0 || len(remote.cancels) != 0 {
						t.Fatal("operation was replayed or canceled more than once")
					}
					lease, err := agent.acquireActionLeaseForSession(context.Background(), ActionWrite,
						"workspace:admission-interruption", "admission-interruption", 0, time.Second)
					if err != nil {
						t.Fatalf("original terminal did not release the workspace lease: %v", err)
					}
					lease.Release()
				})
			}
		}
	}
}
