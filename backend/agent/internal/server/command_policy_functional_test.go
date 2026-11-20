package server

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAskNativeCommandTerminal(t *testing.T) {
	const denial = "blocked_command(hold): next: contact operator"
	t.Setenv("DOOPS_COMMAND_DENY_RULES", `[{"name":"hold","match":"held-command","hint":"contact operator"}]`)
	for _, status := range []string{"failed", "completed"} {
		t.Run(status, func(t *testing.T) {
			mock := newCancellationDoagentMock(t, "", "")
			fixture := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			startClusterAgent(t, fixture, "dev")
			client := fixture.client(t, fixture.admin)
			lifecycleSend(t, client, 2, "doops_agent_prompt", "policy-result", map[string]any{"instruction": "check status"})
			awaitCancellationTestSignal(t, mock.prompted)
			send := func(update map[string]any) {
				update["turnId"] = "cancel-turn"
				mock.events <- map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{
					"sessionId": "cancel-agent-session", "update": update,
				}}
			}
			send(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "first", "toolName": "Bash", "status": status,
				"content": []any{map[string]any{"type": "content", "content": map[string]any{"type": "text", "text": "synthetic-private-diagnostic\n" + denial}}}})
			send(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "second", "toolName": "Bash", "status": "completed"})
			send(map[string]any{"sessionUpdate": "agent_message", "content": map[string]any{"type": "text", "text": "complete"}})
			send(map[string]any{"sessionUpdate": "turn_finished", "status": "completed", "stopReason": "end_turn"})
			result := requireK8SToolResult(t, readK8SFunctionalResponse(t, client, 2))
			content := fmt.Sprint(result["content"])
			if (result["isError"] == true) != (status == "failed") || strings.Contains(content, "synthetic-private-diagnostic") ||
				strings.Contains(content, denial) != (status == "failed") {
				t.Fatalf("incorrect ask terminal: %v", result)
			}
		})
	}
}

func TestCommandPolicyFunctional(t *testing.T) {
	const denial = "blocked_command(workload_hold): next: Ask the workload owner"
	const session = "policy-test"
	workspace := t.TempDir()
	sessionDir := filepath.Join(workspace, session)
	if err := os.Mkdir(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOOPS_WORKSPACE_ROOT", workspace)
	t.Setenv("DOOPS_COMMAND_DENY_RULES", `[{"name":"workload_hold","match":"policy-held","hint":"Ask the workload owner"}]`)
	bin := t.TempDir()
	calls := filepath.Join(bin, "starts")
	t.Setenv("COMMAND_POLICY_STARTS", calls)
	t.Setenv("PATH", bin)
	for _, name := range []string{"bash", "kubectl", "helm"} {
		writeK8SMCPExecutable(t, bin, name, `#!/bin/sh
set -eu
printf '%s %s\n' "${0##*/}" "$*" >> "$COMMAND_POLICY_STARTS"
printf '{}\n'
`)
	}
	requireNoStart := func(t *testing.T) {
		t.Helper()
		if data, err := os.ReadFile(calls); !os.IsNotExist(err) {
			t.Fatalf("expected no process start; recorded %q, err=%v", data, err)
		}
	}
	requireToolDenial := func(t *testing.T, response map[string]any) {
		t.Helper()
		result := requireK8SToolResult(t, response)
		content, _ := result["content"].([]any)
		if result["isError"] != true || len(content) != 1 {
			t.Fatalf("expected a terminal policy denial: %#v", response)
		}
		entry, _ := content[0].(map[string]any)
		if entry["text"] != denial {
			t.Fatalf("expected %q, got %#v", denial, entry)
		}
	}

	t.Run("gateway", func(t *testing.T) {
		fixture := newExecutionLifecycleFixture(t, GatewayHubOptions{})
		startClusterAgent(t, fixture, "dev")
		client := fixture.client(t, fixture.admin)
		for index, tool := range []string{"doops_shell", "doops_bg"} {
			t.Run(tool, func(t *testing.T) {
				id := index + 2
				lifecycleSend(t, client, id, tool, session, map[string]any{"command": "echo policy-held"})
				response := readK8SFunctionalResponse(t, client, id)
				requireNoStart(t)
				rpcError, _ := response["error"].(map[string]any)
				if rpcError["code"] != float64(-32602) || rpcError["message"] != denial || response["result"] != nil {
					t.Fatalf("expected known pre-start rejection %q: %#v", denial, response)
				}
			})
		}
		t.Run("k8s_mutation", func(t *testing.T) {
			lifecycleSend(t, client, 4, "doops_k8s", session, map[string]any{
				"operation": "scale", "resource": "deployment/policy-held", "replicas": 1,
			})
			response := readK8SFunctionalResponse(t, client, 4)
			requireNoStart(t)
			requireToolDenial(t, response)
		})
		t.Run("node_info", func(t *testing.T) {
			t.Setenv("DOOPS_COMMAND_DENY_RULES", `[{"name":"workload_hold","match":"uptime","hint":"Ask the workload owner"}]`)
			lifecycleSend(t, client, 5, "doops_node_info", session, map[string]any{})
			requireToolDenial(t, readK8SFunctionalResponse(t, client, 5))
			requireNoStart(t)
		})
	})

	t.Run("mcp_stdio", func(t *testing.T) {
		t.Chdir(sessionDir)
		call := func(t *testing.T, arguments string) map[string]any {
			t.Helper()
			input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_read","arguments":` + arguments + "}}\n")
			var output bytes.Buffer
			if err := RunK8SMCPStdioContext(t.Context(), input, &output); err != nil {
				t.Fatal(err)
			}
			return decodeK8SMCPResponse(t, output.Bytes())
		}
		for _, tc := range []struct{ name, arguments string }{
			{"kubectl", `{"operation":"get","kind":"pods","namespace":"policy-held"}`},
			{"configmap_keys", `{"operation":"configmap-keys","namespace":"policy-held"}`},
			{"helm_read", `{"operation":"helm-status","name":"app","namespace":"policy-held"}`},
		} {
			t.Run(tc.name, func(t *testing.T) {
				response := call(t, tc.arguments)
				requireNoStart(t)
				requireToolDenial(t, response)
			})
		}
		t.Run("unmatched_allowed", func(t *testing.T) {
			response := call(t, `{"operation":"get","kind":"pods","namespace":"open"}`)
			if result := requireK8SToolResult(t, response); result["isError"] != false {
				t.Fatalf("unmatched read failed: %#v", result)
			}
			data, err := os.ReadFile(calls)
			if err != nil || string(data) != "kubectl get pods --namespace open\n" {
				t.Fatalf("expected one allowed kubectl start, got %q, err=%v", data, err)
			}
		})
	})
}
