package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeContainerRuntime writes an executable stand-in for a container runtime so
// the docker tool path can be exercised without a real engine. The path is
// generated with os.MkdirTemp so it never needs shell quoting.
func fakeContainerRuntime(t *testing.T, script string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "doops-docker-runtime-")
	if err != nil {
		t.Fatalf("create fake container runtime dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "fake-runtime")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatalf("write fake container runtime: %v", err)
	}
	return path
}

// dockerTestGateway builds the minimal Gateway the docker tool path needs and
// pins the container runtime the command is prefixed with.
func dockerTestGateway(containerRuntime string) *Gateway {
	gw := &Gateway{
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	gw.containerRuntime = containerRuntime
	return gw
}

// TestDockerToolCommandOutcomes pins every terminal reply the docker tool can
// produce. A command that fails before it starts must be a structured tool
// error: reporting it as success is how a failed docker invocation reached the
// client as "Operation complete" and the audit as success (taskbook A9/R18).
func TestDockerToolCommandOutcomes(t *testing.T) {
	const (
		wantSuccess = iota
		wantToolError
		wantRPCError
	)
	cases := []struct {
		name string
		// runtime returns the container runtime binary and, when the command
		// must be observed running before cancellation, its start marker.
		runtime  func(t *testing.T) (string, string)
		command  string
		cancel   bool
		want     int
		wantCode int
		wantText []string
	}{
		{
			name:    "command start failure",
			runtime: func(*testing.T) (string, string) { return "docker", "" },
			// A NUL byte cannot be an argv byte, so exec.Start fails before the
			// child process exists.
			command:  "ps\x00",
			want:     wantToolError,
			wantText: []string{"Error starting command:"},
		},
		{
			name:     "container runtime unavailable",
			runtime:  func(*testing.T) (string, string) { return "doops-missing-container-runtime", "" },
			command:  "ps",
			want:     wantToolError,
			wantText: []string{"Docker command failed:", "exit status 127"},
		},
		{
			name: "success",
			runtime: func(t *testing.T) (string, string) {
				return fakeContainerRuntime(t, "printf 'CONTAINER ID   IMAGE\\n'"), ""
			},
			command:  "ps",
			want:     wantSuccess,
			wantText: []string{"Docker command finished."},
		},
		{
			name: "non-zero exit",
			runtime: func(t *testing.T) (string, string) {
				return fakeContainerRuntime(t, "printf 'synthetic engine failure\\n' >&2\nexit 3"), ""
			},
			command:  "logs app",
			want:     wantToolError,
			wantText: []string{"Docker command failed:", "exit status 3"},
		},
		{
			name: "cancellation",
			runtime: func(t *testing.T) (string, string) {
				started := filepath.Join(t.TempDir(), "started")
				script := "printf started > '" + started + "'\nsleep 60"
				return fakeContainerRuntime(t, script), started
			},
			command:  "logs app",
			cancel:   true,
			want:     wantRPCError,
			wantCode: -32007,
			wantText: []string{"operation canceled"},
		},
		{
			name: "outcome unknown",
			runtime: func(t *testing.T) (string, string) {
				// The detached descendant keeps the output pipe open after the
				// leader exits, so the bounded drain cannot prove the outcome.
				return fakeContainerRuntime(t, "sleep 60 &\nexit 0"), ""
			},
			command:  "ps",
			want:     wantRPCError,
			wantCode: -32603,
			wantText: []string{errExecutionOutcomeUnknown.Error()},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runtimePath, startedPath := tc.runtime(t)
			server := newGatewayAuthorizedAgentServer(t, dockerTestGateway(runtimePath))
			t.Cleanup(server.Close)
			conn := dialAgentTestWS(t, server.URL)
			t.Cleanup(func() { _ = conn.Close() })
			initializeAgentTestWS(t, conn)

			rawArgs, err := json.Marshal(map[string]interface{}{
				"session_id": "docker-outcome",
				"command":    tc.command,
			})
			if err != nil {
				t.Fatalf("marshal docker args: %v", err)
			}
			if err := conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": float64(42), "method": "tools/call",
				"params": map[string]interface{}{"name": "doops_docker", "arguments": json.RawMessage(rawArgs)},
			}); err != nil {
				t.Fatalf("send docker call: %v", err)
			}
			if startedPath != "" {
				requireEventually(t, 5*time.Second, func() bool {
					_, statErr := os.Stat(startedPath)
					return statErr == nil
				})
			}
			if tc.cancel {
				if err := conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "id": "cancel-42", "method": "tools/cancel",
					"params": map[string]interface{}{"id": float64(42)},
				}); err != nil {
					t.Fatalf("send docker cancel: %v", err)
				}
			}
			if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
				t.Fatalf("set read deadline: %v", err)
			}
			defer conn.SetReadDeadline(time.Time{})

			message := readUntilID(t, conn, 42)
			rpcError, failed := message["error"].(map[string]interface{})
			if failed {
				if tc.want != wantRPCError {
					t.Fatalf("docker %s returned RPC error instead of a terminal result: %#v", tc.name, message)
				}
				code, _ := numericID(rpcError["code"])
				if int(code) != tc.wantCode {
					t.Fatalf("docker %s error code = %d, want %d: %#v", tc.name, code, tc.wantCode, message)
				}
				text := fmt.Sprint(rpcError["message"])
				for _, want := range tc.wantText {
					if !strings.Contains(text, want) {
						t.Fatalf("docker %s error %q does not contain %q", tc.name, text, want)
					}
				}
				return
			}
			result, ok := message["result"].(map[string]interface{})
			if !ok {
				t.Fatalf("docker %s returned neither result nor error: %#v", tc.name, message)
			}
			text := resultContentText(result)
			for _, want := range tc.wantText {
				if !strings.Contains(text, want) {
					t.Fatalf("docker %s text %q does not contain %q", tc.name, text, want)
				}
			}
			switch tc.want {
			case wantSuccess:
				if result["isError"] == true {
					t.Fatalf("docker %s reported success as a tool error: %#v", tc.name, result)
				}
			case wantToolError:
				if result["isError"] != true {
					t.Fatalf("docker %s failure was reported as success: %#v", tc.name, message)
				}
				if strings.Contains(text, "Docker command finished.") {
					t.Fatalf("docker %s failure still announced success: %q", tc.name, text)
				}
			default:
				t.Fatalf("docker %s expected a JSON-RPC error, got a tool result: %#v", tc.name, message)
			}
		})
	}
}
