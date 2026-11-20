package server

import (
	"reflect"
	"testing"
	"time"
)

func TestAskToolFailuresReachGatewayAudit(t *testing.T) {
	type call struct{ id, name, status, code string }
	for _, tc := range []struct {
		name, terminal, reason, auditStatus, category string
		calls                                         []call
		wantIDs                                       []string
	}{
		{"normal", "completed", "end_turn", "success", "", []call{{"read", "mcp_doops_web_read", "completed", ""}}, nil},
		{"read-errors", "completed", "end_turn", "error", askToolFailed, []call{
			{"k8s", "mcp_doops_k8s_read", "failed", "mcp_tool_error"},
			{"web", "mcp_doops_web_read", "failed", ""},
			{"retry", "mcp_doops_web_read", "completed", ""},
		}, []string{"k8s", "web"}},
		{"timeout", "completed", "end_turn", "timeout", askToolTimeout, []call{
			{"bash", "Bash", "failed", "tool_timeout"}, {"retry", "Bash", "completed", ""},
		}, []string{"bash"}},
		{"nonzero", "completed", "end_turn", "error", askToolFailed, []call{
			{"nonzero", "Bash", "failed", "command_failed"},
		}, []string{"nonzero"}},
		{"tool-cancel", "completed", "end_turn", "canceled", askToolCancelled, []call{
			{"web", "mcp_doops_web_read", "failed", "tool_cancelled"},
		}, []string{"web"}},
		{"turn-cancel", "cancelled", "cancelled", "canceled", askToolCancelled, []call{
			{"bash", "Bash", "failed", "tool_timeout"},
		}, []string{"bash"}},
		{"turn-timeout", "failed", "llm_call_timeout", "timeout", askToolTimeout, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock := newCancellationDoagentMock(t, "", "")
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			startClusterAgent(t, f, "dev")
			client := f.client(t, f.admin)
			session := "audit-" + tc.name
			lifecycleSend(t, client, 2, "doops_agent_prompt", session, map[string]any{"instruction": "check workload"})
			awaitCancellationTestSignal(t, mock.prompted)
			send := func(sid string, update map[string]any) {
				mock.events <- map[string]any{"jsonrpc": "2.0", "method": "session/update",
					"params": map[string]any{"sessionId": sid, "update": update}}
			}
			const sid, turn = "cancel-agent-session", "cancel-turn"
			for _, stale := range []struct {
				sid, turn string
				replay    bool
			}{{"other-session", turn, false}, {sid, "other-turn", false}, {sid, "", false}, {sid, turn, true}} {
				send(stale.sid, map[string]any{"sessionUpdate": "tool_call_update", "turnId": stale.turn, "replay": stale.replay,
					"toolName": "Bash", "toolCallId": "stale", "status": "failed", "errorCode": "tool_cancelled"})
			}
			for _, call := range tc.calls {
				send(sid, map[string]any{"sessionUpdate": "tool_call", "turnId": turn, "toolCallId": call.id,
					"toolName": call.name, "status": "in_progress"})
				result := map[string]any{"sessionUpdate": "tool_call_update", "turnId": turn,
					"toolCallId": call.id, "status": call.status, "errorCode": call.code}
				if call.id == "nonzero" {
					result["exitCode"] = 37
				}
				for range 2 { // A repeated terminal frame still represents one call.
					send(sid, result)
				}
			}
			send(sid, map[string]any{"sessionUpdate": "agent_message", "turnId": turn,
				"content": map[string]any{"type": "text", "text": `{"errorCode":"tool_timeout","isError":true,"summary":"all checks succeeded"}`}})
			send(sid, map[string]any{"sessionUpdate": "turn_finished", "turnId": turn, "status": tc.terminal, "stopReason": tc.reason})
			result := requireK8SToolResult(t, readK8SFunctionalResponse(t, client, 2))
			if (result["isError"] == true) != (tc.category != "") || stringField(result, "errorCode") != tc.category {
				t.Fatalf("unexpected terminal result: %#v", result)
			}
			var ids []string
			failures, _ := result["toolFailures"].([]any)
			for _, item := range failures {
				failure := item.(map[string]any)
				ids = append(ids, stringField(failure, "toolCallId"))
				if stringField(failure, "toolName") == "" || stringField(failure, "errorCode") == "" {
					t.Fatalf("incomplete failed call: %#v", failure)
				}
				if failure["toolCallId"] == "nonzero" && (failure["exitCode"] != float64(37) || failure["errorCode"] != "command_failed") {
					t.Fatalf("lost native command receipt: %#v", failure)
				}
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("failed calls %v, want %v", ids, tc.wantIDs)
			}
			events, err := f.store.ListAuditFiltered(AuditFilter{Session: session, Action: ActionAsk})
			if err != nil || len(events) != 1 || events[0].Status != tc.auditStatus || events[0].Error != tc.category {
				t.Fatalf("terminal audit %#v, %v", events, err)
			}
		})
	}
}

func TestAskGatewayInterruptionAudit(t *testing.T) {
	for _, tc := range []struct {
		name, status, category, message string
		limit                           time.Duration
	}{
		{"deadline", "timeout", "operation_timeout", "operation timed out", 150 * time.Millisecond},
		{"cancel", "canceled", "operation_canceled", "operation canceled", 5 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{OperationTimeout: tc.limit})
			remote := f.agent(t, "ask-interruption")
			client := f.client(t, f.admin)
			session := "ask-" + tc.name
			lifecycleSend(t, client, 2, "doops_agent_prompt", session, map[string]any{"instruction": "check workload"})
			call := remote.call(t, "doops_agent_prompt", session)
			if tc.name == "cancel" {
				writeGatewayJSON(t, client, map[string]any{"jsonrpc": "2.0", "method": "tools/cancel", "id": 3,
					"params": map[string]any{"id": 2}})
				lifecycleResponse(t, client, 3, "")
			}
			select {
			case id := <-remote.cancels:
				if id != call.id {
					t.Fatalf("cancel referred to %d, want %d", id, call.id)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("original operation did not receive cancellation")
			}
			remote.complete(t, call, "operation canceled")
			lifecycleResponse(t, client, 2, tc.message)
			events, err := f.store.ListAuditFiltered(AuditFilter{Session: session, Action: ActionAsk})
			if err != nil || len(events) != 1 || events[0].Status != tc.status || events[0].Error != tc.category {
				t.Fatalf("interruption audit %#v, %v", events, err)
			}
		})
	}
}
