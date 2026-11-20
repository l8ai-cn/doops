package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLifecycleCLIThroughGatewayAndRealAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(root, "workspaces"))
	binary := filepath.Join(root, "doops")
	buildCtx, stopBuild := context.WithTimeout(context.Background(), time.Minute)
	defer stopBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "./cli/doops")
	build.Dir = "../../../cli"
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := &Gateway{
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	conn := dialFakeGatewayAgent(t, f.server.URL, "dev", "local")
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.ServeGatewayWebSocketConnWithReady(conn, "cli-functional", nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("real Agent did not stop")
		}
	})
	waitForGatewayAgent(t, f.hub, "dev", "local")

	for name, value := range map[string]interface{}{
		"config.json": map[string]interface{}{"servers": []map[string]string{{
			"name": "functional", "gateway": f.server.URL, "cluster": "dev", "instance": "local",
		}}},
		"auth.json": map[string]interface{}{"tokens": map[string]string{f.server.URL: f.admin.Plaintext}},
	} {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, background := range []bool{false, true} {
		for _, exitCode := range []int{0, 17} {
			session := fmt.Sprintf("functional-bg-%t-exit-%d", background, exitCode)
			t.Run(session, func(t *testing.T) {
				args := []string{"-session", session, "exec", "--target", "functional",
					"--cmd", fmt.Sprintf("printf 'LIFECYCLE_STDOUT\\n'; printf 'LIFECYCLE_STDERR\\n' >&2; exit %d", exitCode)}
				if background {
					args = append(args, "--bg")
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				command := exec.CommandContext(ctx, binary, args...)
				command.Env = append(os.Environ(), "DOOPS_CONFIG="+filepath.Join(root, "config.json"))
				output, err := command.CombinedOutput()
				if (err != nil) != (exitCode != 0) {
					t.Fatalf("CLI exit=%v, want remote exit %d: %s", err, exitCode, output)
				}
				for _, marker := range []string{"LIFECYCLE_STDOUT", "LIFECYCLE_STDERR"} {
					if !strings.Contains(string(output), marker) {
						t.Fatalf("CLI lost real command output %q: %s", marker, output)
					}
				}
				if background && !strings.Contains(string(output), fmt.Sprintf("ExitCode: %d", exitCode)) {
					t.Fatalf("background terminal exit code missing: %s", output)
				}
				if operations := f.hub.listActiveOperations(); len(operations) != 0 {
					t.Fatalf("terminal CLI left live operations: %#v", operations)
				}
				if unresolved, err := f.store.UnresolvedExecutions("dev", "local"); err != nil || len(unresolved) != 0 {
					t.Fatalf("ordinary exit created unknown-outcome blocker: %#v, %v", unresolved, err)
				}
				wantStatus := "success"
				if exitCode != 0 {
					wantStatus = "error"
				}
				events, err := f.store.ListAuditFiltered(AuditFilter{Session: session})
				if err != nil || len(events) != 1 || events[0].Status != wantStatus || events[0].EndedAt.IsZero() {
					t.Fatalf("terminal Gateway audit = %#v, %v; want %s", events, err, wantStatus)
				}
				t.Logf("actual CLI output: %s", output)
			})
		}
	}
}
