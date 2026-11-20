package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHelmCommandFilterReceipts(t *testing.T) {
	const upgrade = `"operation":"upgrade-install","asset_source":"workspace","chart":"deploy/helm/prod"`
	for _, tc := range []struct {
		name, args, deny, calls string
	}{
		{"upgrade refused", upgrade, "upgrade", ""},
		{"explicit rollback refused", `"operation":"rollback","revision":1`, "rollback", ""},
		{"uninstall refused", `"operation":"uninstall"`, "uninstall", ""},
		{"verification refused", upgrade, "status", "upgrade\n"},
		{"settle history refused", upgrade, "history", "upgrade\n"},
		{"compensating rollback refused", upgrade, "rollback", "upgrade\nhistory\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOOPS_WORKSPACE_ROOT", root)
			writeWorkspaceHelmChart(t, root)
			calls := filepath.Join(root, "calls")
			if err := os.WriteFile(calls, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("HELM_CALLS", calls)
			t.Setenv("PATH", root)
			t.Setenv("DOOPS_COMMAND_DENY_RULES", `[{"name":"local_hold","match":"^helm `+tc.deny+` app ","hint":"Contact the release owner"}]`)
			if tc.deny == "status" {
				t.Setenv("UPGRADE_RESULT", "deployed")
			} else {
				t.Setenv("UPGRADE_RESULT", "pending-upgrade")
			}
			writeK8SMCPExecutable(t, root, "helm", `#!/bin/sh
set -eu
printf '%s\n' "$1" >> "$HELM_CALLS"
case "$1" in
  upgrade)
    while [ "$1" != --description ]; do shift; done
    printf '%s\n' "$2" > revision-description
    [ "$UPGRADE_RESULT" = deployed ] || exit 1
    printf '{"version":2,"info":{"description":"%s"}}' "$2";;
  history)
    read -r description < revision-description
    printf '[{"revision":1,"status":"deployed"},{"revision":2,"status":"pending-upgrade","description":"%s"}]' "$description";;
  *) exit 9;;
esac
`)
			request := k8sMCPRequest{Params: json.RawMessage(`{"name":"helm","arguments":{"release":"app","namespace":"apps",` + tc.args + `}}`)}
			response := callK8SMCPTool(t.Context(), request, "release")
			result, _ := response.Result.(map[string]any)
			facts, _ := result["structuredContent"].(map[string]any)
			started := tc.calls != ""
			if result["isError"] != true || facts["executionStarted"] != started || (facts["executionOutcome"] == "unknown") != started {
				t.Fatalf("incorrect execution receipt: %+v", response)
			}
			if (!started && facts["phase"] != "policy") || (started && facts["phase"] != "execution") {
				t.Fatalf("incorrect phase: %v", facts)
			}
			content, err := json.Marshal(result["content"])
			if err != nil || !strings.Contains(string(content), "local_hold") || !strings.Contains(string(content), "Contact the release owner") {
				t.Fatalf("rule or hint missing: %s (%v)", content, err)
			}
			actual, err := os.ReadFile(calls)
			if err != nil || string(actual) != tc.calls {
				t.Fatalf("Helm commands started: %q, want %q: %v", actual, tc.calls, err)
			}
		})
	}
}

func TestHelmRollbackCorrectionUsesActualReceipt(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PATH", root)
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	t.Setenv("DOOPS_COMMAND_DENY_RULES", `[{"name":"revision_hold","match":"^helm rollback app 1 ","hint":"Contact the release owner"}]`)
	writeK8SMCPExecutable(t, root, "helm", "#!/bin/sh\n[ \"$3\" = 2 ]\n")
	for _, tc := range []struct {
		name, outcome string
		firstRevision int
	}{
		{"policy denial remains failed", "failed", 1},
		{"invalid revision can be corrected", "succeeded", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			for index, revision := range []int{tc.firstRevision, 2} {
				input := map[string]any{"operation": "rollback", "release": "app", "namespace": "apps", "revision": revision}
				params, err := json.Marshal(map[string]any{"name": "helm", "arguments": input})
				if err != nil {
					t.Fatal(err)
				}
				response := callK8SMCPTool(t.Context(), k8sMCPRequest{Params: params}, "apply-test")
				result, _ := response.Result.(map[string]any)
				if result["isError"] != (index == 0) {
					t.Fatalf("rollback revision %d: %+v", revision, response)
				}
				update := testRuntimeTraceUpdate(t, attestedMutationTrace(t, "mcp_doops_helm", input))
				update["toolCallId"], update["status"] = "later", "completed"
				if index == 0 {
					update["toolCallId"], update["status"] = "first", "failed"
				}
				update["rawOutput"] = result
				update["resultDigest"], err = digestRuntimeAttestationJSON(result)
				if err != nil {
					t.Fatal(err)
				}
				if err := collector.collect(update); err != nil {
					t.Fatal(err)
				}
			}
			_, report, err := collector.verifiedMutationResult()
			if err != nil || report["outcome"] != tc.outcome {
				t.Fatalf("expected %s, got %+v: %v", tc.outcome, report, err)
			}
			err = collector.requireCompletedToolEvidence()
			if tc.outcome == "failed" {
				if err == nil || !strings.Contains(err.Error(), "blocked_command(revision_hold): next: Contact the release owner") {
					t.Fatalf("missing policy denial in final error: %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid correction rejected: %v", err)
			}
		})
	}
}

func TestHelmCommandFilterAfterInterruptedUpgrade(t *testing.T) {
	root := installInterruptFakeHelm(t)
	t.Setenv("MINE_STATUS", "pending-upgrade")
	t.Setenv("DOOPS_COMMAND_DENY_RULES", `[{"name":"hold_rollback","match":"^helm rollback ","hint":"Inspect the pending release"}]`)
	command, err := buildHelmCommand(testHelmParams())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	_, err = runHelmCommand(ctx, command, nil)
	if !errors.Is(err, errExecutionOutcomeUnknown) || !strings.Contains(err.Error(), "hold_rollback") || !strings.Contains(err.Error(), "Inspect the pending release") {
		t.Fatalf("interrupted mutation lost its outcome or rollback denial: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "rollback-args")); !os.IsNotExist(err) {
		t.Fatalf("expected no rollback process: %v", err)
	}
}
