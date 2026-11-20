package server

import (
	"context"
	"testing"
	"time"
)

func TestExecutionShellStartResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		noShell bool
		failed  bool
	}{
		{"invalid_argument", "printf NEVER_STARTED\x00", false, true},
		{"unavailable_shell", "printf NEVER_STARTED", true, true},
		{"success", "printf 'Error starting command: ordinary output\\n'", false, false},
	} {
		for _, path := range []string{"rpc", "internal"} {
			t.Run(tc.name+"/"+path, func(t *testing.T) {
				t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
				if tc.noShell {
					t.Setenv("PATH", t.TempDir())
				}
				f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
				agent := &Gateway{}
				conn := dialFakeGatewayAgent(t, f.server.URL, "dev", "local")
				stopped := make(chan struct{})
				go func() {
					defer close(stopped)
					agent.ServeGatewayWebSocketConnWithReady(conn, "shell-start-regression", nil)
				}()
				t.Cleanup(func() {
					_ = conn.Close()
					select {
					case <-stopped:
					case <-time.After(3 * time.Second):
						t.Error("Agent did not stop")
					}
					requireEventually(t, time.Second, func() bool { return f.hub.getAgent("dev", "local") == nil })
				})
				waitForGatewayAgent(t, f.hub, "dev", "local")
				session := "shell-start-" + path
				args := map[string]interface{}{"session_id": session, "command": tc.command}
				if path == "rpc" {
					client := f.client(t, f.admin)
					lifecycleSend(t, client, 2, "doops_shell", session, args)
					response := lifecycleResponse(t, client, 2, "")
					result := response["result"].(map[string]interface{})
					if got := result["isError"] == true; got != tc.failed {
						t.Errorf("tool failure=%t, want %t: %#v", got, tc.failed, response)
					}
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					_, err := f.hub.RunInternalToolCall(ctx, "dev", "local", "doops_shell", args)
					if (err != nil) != tc.failed {
						t.Errorf("internal call error=%v, want failure=%t", err, tc.failed)
					}
				}
				events, err := f.store.ListAuditFiltered(AuditFilter{Session: session})
				if err != nil || len(events) != 1 {
					t.Fatalf("audits=%#v, err=%v", events, err)
				}
				want := "success"
				if tc.failed {
					want = "error"
				}
				if events[0].Status != want || events[0].EndedAt.IsZero() {
					t.Errorf("audit status=%s, want %s; ended_at=%v", events[0].Status, want, events[0].EndedAt)
				}
				if tc.failed && events[0].Error == "" {
					t.Error("startup failure lost its audit error")
				}
				t.Logf("path=%s failure=%t audit=%s", path, tc.failed, events[0].Status)
			})
		}
	}
}
