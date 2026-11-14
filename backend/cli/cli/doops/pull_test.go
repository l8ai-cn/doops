package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func TestBuildGitWorkspaceCommitCommand(t *testing.T) {
	got := buildGitWorkspaceCommitCommand("release-1")
	for _, want := range []string{
		"cd '/root/ws/release-1'",
		"GIT_INDEX_FILE=\"$snapshot/index\"",
		"find . -name .git -prune",
		"update-index --add -z --stdin",
		"cmp \"$snapshot/files\" \"$snapshot/tree-files\"",
		"commit-tree \"$tree\" -m DoopsPullSnapshot",
		"update-ref refs/heads/doops-pull \"$commit\"",
	} {
		if !contains(got, want) {
			t.Fatalf("command missing %q\n%s", want, got)
		}
	}
}

func TestPullGitWorkspaceClonesFromBundle(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "course.tgz"), []byte("course data"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runTestGit(t, src, "init", "-b", "master")
	runTestGit(t, src, "config", "user.name", "doops")
	runTestGit(t, src, "config", "user.email", "doops@localhost")
	runTestGit(t, src, "add", "-A")
	runTestGit(t, src, "commit", "-m", "snapshot")
	bundle := filepath.Join(root, "workspace.bundle")
	runTestGit(t, src, "bundle", "create", bundle, "master")
	commit, err := gitCommandOutput(src, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(root, "dest")
	if err := pullGitWorkspace(bundle, "", dest, strings.TrimSpace(commit)); err != nil {
		t.Fatalf("pull bundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "course.tgz"))
	if err != nil {
		t.Fatalf("read pulled file: %v", err)
	}
	if string(got) != "course data" {
		t.Fatalf("pulled content mismatch: %q", got)
	}
}

func TestPullSnapshotCommitRequiresOneValidMarker(t *testing.T) {
	commit := strings.Repeat("a", 40)
	heartbeat := "\r\x1b[K[agent] \u23f3 \u547d\u4ee4\u540e\u53f0\u6267\u884c\u4e2d... (\u8017\u65f6 5s)"
	for _, tc := range []struct {
		name, output string
		valid        bool
	}{
		{"valid", "progress\npull-commit:" + commit + "\nOperation complete.", true},
		{"contaminated marker", heartbeat + "pull-commit:" + commit + "\nOperation complete.", false},
		{"missing", "Operation complete.", false},
		{"heartbeat only", heartbeat + "Operation complete.", false},
		{"invalid", "pull-commit:not-a-commit\n", false},
		{"invalid after heartbeat", heartbeat + "pull-commit:not-a-commit\n", false},
		{"trailing output", heartbeat + "pull-commit:" + commit + "unexpected\n", false},
		{"duplicate", "pull-commit:" + commit + "\npull-commit:" + commit, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := pullSnapshotCommit(tc.output)
			if (err == nil) != tc.valid || (tc.valid && got != commit) {
				t.Fatalf("snapshot marker = %q, %v", got, err)
			}
		})
	}
}

func TestPullSnapshotReceiptCapturesHeartbeatAndTerminal(t *testing.T) {
	commit := strings.Repeat("a", 40)
	chunks := []string{
		"\r\x1b[K[agent] \u23f3 \u547d\u4ee4\u540e\u53f0\u6267\u884c\u4e2d... (\u8017\u65f6 5s)",
		"pull-com",
		"\r\x1b[K[agent] \u23f3 \u547d\u4ee4\u540e\u53f0\u6267\u884c\u4e2d... (\u8017\u65f6 10s)",
		"mit:" + commit + "\n",
	}
	for _, tc := range []struct{ name, tool, kind string }{
		{"success", "doops_shell", "raw"},
		{"tool error", "doops_shell", "raw"},
		{"unknown", "doops_shell", "raw"},
		{"duplicate", "doops_shell", "raw"},
		{"other tool", "doops_node_info", "raw"},
		{"other kind", "doops_shell", "assistant_delta"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("DOOPS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
			upgrader := websocket.Upgrader{}
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.Close()
				for {
					var req map[string]interface{}
					if err := conn.ReadJSON(&req); err != nil {
						return
					}
					if req["method"] == "initialize" {
						_ = conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": req["id"], "result": agentInitializeResult()})
						continue
					}
					stream := append([]string(nil), chunks...)
					if tc.name == "duplicate" {
						stream = append(stream, chunks...)
					}
					for _, chunk := range stream {
						_ = conn.WriteJSON(map[string]interface{}{
							"jsonrpc": "2.0", "method": "notifications/message",
							"params": map[string]interface{}{"request_id": req["id"], "kind": tc.kind, "data": chunk},
						})
					}
					response := map[string]interface{}{"jsonrpc": "2.0", "id": req["id"]}
					if tc.name == "unknown" {
						response["error"] = map[string]interface{}{"code": -32008, "message": "execution_outcome_unknown"}
					} else {
						response["result"] = map[string]interface{}{
							"content": []map[string]interface{}{{"type": "text", "text": "Operation complete."}},
							"isError": tc.name == "tool error",
						}
					}
					_ = conn.WriteJSON(response)
				}
			}))
			defer gateway.Close()
			client := newGatewayTestClient(t, gateway.URL, "pull-receipt")
			defer client.Close()
			output, err := client.CallAndCapture(tc.tool, map[string]interface{}{
				"command": buildGitWorkspaceCommitCommand("pull-receipt"),
			})
			if tc.name == "tool error" || tc.name == "unknown" {
				if err == nil {
					t.Fatal("snapshot marker hid a failed or unknown terminal result")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := "pull-commit:" + commit + "\nOperation complete."
			if tc.tool != "doops_shell" || tc.kind != "raw" {
				want = strings.Join(chunks, "") + "Operation complete."
			} else if tc.name == "duplicate" {
				want = "pull-commit:" + commit + "\n" + want
			}
			if output != want {
				t.Fatalf("captured command output = %q; want %q", output, want)
			}
			if tc.tool != "doops_shell" || tc.kind != "raw" {
				return
			}
			got, err := pullSnapshotCommit(output)
			if tc.name == "duplicate" {
				if err == nil {
					t.Fatal("duplicate snapshot markers accepted")
				}
				return
			}
			if err != nil || got != commit {
				t.Fatalf("snapshot receipt = %q, %v", got, err)
			}
		})
	}
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
