package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/user/doops/agent/api"
)

func TestDoSQLCancelOriginalTerminal(t *testing.T) {
	for _, tc := range []struct {
		name, operation   string
		proof, disconnect bool
		want              error
	}{
		{"read confirmed", "ask", true, false, errDoSQLReadCanceled},
		{"read generic cancel", "ask", false, false, errExecutionOutcomeUnknown},
		{"read ACK then disconnect", "ask", false, true, errExecutionOutcomeUnknown},
		{"apply cannot use read proof", "apply", true, false, errExecutionOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			remote := f.agent(t, "dosql-cancel")
			agent := f.hub.getAgent("dev", "local")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			args, err := json.Marshal(api.AgentPromptParams{SessionID: "sql-stop", Skill: "dosql", Operation: tc.operation, SQL: "SELECT 1"})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				done <- agent.relayToolCall(ctx, api.ToolCallParams{Name: "doops_agent_prompt", Arguments: args}, time.Second*5, func(gatewayWSMessage) error { return nil })
			}()
			call := remote.call(t, "doops_agent_prompt", "sql-stop")
			cancel()
			select {
			case id := <-remote.cancels:
				if id != call.id {
					t.Fatalf("cancel targeted %d, want %d", id, call.id)
				}
			case <-time.After(time.Second):
				t.Fatal("cancel was not acknowledged")
			}
			if tc.disconnect {
				remote.conn.Close()
			} else {
				reply := buildErrorResponse(call.id, -32007, errDoSQLReadCanceled.Error())
				if tc.proof {
					reply.Error.Data = map[string]string{"category": "dosql_read_canceled", "sqlstate": "57014"}
				}
				if err := remote.conn.WriteJSON(reply); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-done:
				if !errors.Is(err, tc.want) {
					t.Fatalf("got %v, want %v", err, tc.want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("missing terminal")
			}
		})
	}
}
