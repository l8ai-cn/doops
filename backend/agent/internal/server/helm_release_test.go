package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/user/doops/agent/api"
)

// writeFakeHelm installs a minimal Helm replacement for command construction
// tests. Callers that execute it provide the output and status through env.
func writeFakeHelm(t *testing.T, mutationJSON, _, mutationStderr string) (string, string) {
	t.Helper()
	root := t.TempDir()
	marker := filepath.Join(root, "mutation-ran")
	script := `#!/bin/sh
set -eu
if [ "$1" = "status" ]; then
  cat "$STATUS_FILE"
  exit 0
fi
touch "$MUTATION_MARKER"
if [ -n "$MUTATION_STDERR" ]; then
  printf 'chart NOTES token=leaked-stdout-canary\n'
  printf '%s\n' "$MUTATION_STDERR" >&2
  exit 1
fi
printf '%s' "$MUTATION_JSON"
`
	if err := os.WriteFile(filepath.Join(root, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MUTATION_JSON", mutationJSON)
	t.Setenv("MUTATION_STDERR", mutationStderr)
	t.Setenv("MUTATION_MARKER", marker)
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	return root, marker
}

func TestParseHelmReleaseOutputBindsRevisionAndDescription(t *testing.T) {
	const description = "doops:session-a:123"
	revision, err := parseHelmReleaseOutput(
		[]byte(`{"version":7,"info":{"description":"doops:session-a:123","notes":"chart-output-canary"}}`),
		description,
	)
	if err != nil || revision != 7 {
		t.Fatalf("parse release output = %d, %v; want revision 7", revision, err)
	}
	for _, output := range []string{
		`{"version":0,"info":{"description":"doops:session-a:123"}}`,
		`{"version":7,"info":{"description":"doops:session-b:123"}}`,
		`not-json`,
	} {
		if _, err := parseHelmReleaseOutput([]byte(output), description); err == nil {
			t.Fatalf("invalid release output accepted: %s", output)
		}
	}
}

func TestVerifyHelmReleaseStateRequiresThisDeployedRevision(t *testing.T) {
	tests := []struct {
		name       string
		status     string
		wantErr    string
		wantResult string
		unknown    bool
	}{
		{
			name:       "deployed current revision",
			status:     `{"version":7,"info":{"status":"deployed","description":"doops:release:7"}}`,
			wantResult: "release deployed: revision 7",
		},
		{
			name:    "failed release",
			status:  `{"version":7,"info":{"status":"failed","description":"doops:release:7"}}`,
			wantErr: `release status is "failed", want deployed`,
		},
		{
			name:    "superseded revision",
			status:  `{"version":8,"info":{"status":"deployed","description":"doops:other:8"}}`,
			wantErr: "release revision is 8, want 7",
			unknown: true,
		},
		{
			name:    "different mutation",
			status:  `{"version":7,"info":{"status":"failed","description":"doops:other:7"}}`,
			wantErr: "description does not match mutation",
			unknown: true,
		},
		{
			name:    "pending is not a failed terminal",
			status:  `{"version":7,"info":{"status":"pending-upgrade","description":"doops:release:7"}}`,
			wantErr: `release status is "pending-upgrade", want deployed`,
			unknown: true,
		},
		{
			name:    "malformed status",
			status:  `not-json`,
			wantErr: "helm status output is not JSON",
			unknown: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			statusFile := filepath.Join(root, "status.json")
			if err := os.WriteFile(statusFile, []byte(tc.status), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "helm"), []byte("#!/bin/sh\ncat \"$STATUS_FILE\"\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("STATUS_FILE", statusFile)
			command := helmCommand{cwd: root, release: "app", namespace: "apps", mutationID: "doops:release:7", revision: 7}
			result, err := verifyHelmReleaseState(t.Context(), command)
			if tc.wantErr == "" {
				if err != nil || result != tc.wantResult {
					t.Fatalf("result=%q err=%v, want %q", result, err, tc.wantResult)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error=%v, want %q", err, tc.wantErr)
			}
			if errors.Is(err, errExecutionOutcomeUnknown) != tc.unknown {
				t.Fatalf("unknown=%t, want %t: %v", errors.Is(err, errExecutionOutcomeUnknown), tc.unknown, err)
			}
		})
	}
}

func TestRunHelmCommandReportsMutationFailureWithoutChartOutput(t *testing.T) {
	root, marker := writeFakeHelm(t, "", "", "Error: UPGRADE FAILED: synthetic hook failure")
	statusFile := filepath.Join(root, "status.json")
	if err := os.WriteFile(statusFile, []byte(`{"version":1,"info":{"status":"failed","description":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("STATUS_FILE", statusFile)
	command := helmCommand{
		args:       []string{"upgrade", "app", "chart", "--install"},
		cwd:        root,
		release:    "app",
		namespace:  "apps",
		mutationID: "doops:release:1",
	}
	_, err := runHelmCommand(t.Context(), command, nil)
	if err == nil || !strings.Contains(err.Error(), "synthetic hook failure") {
		t.Fatalf("error=%v, want Helm failure reason", err)
	}
	if strings.Contains(err.Error(), "leaked-stdout-canary") {
		t.Fatalf("chart stdout leaked into failure: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("mutation marker missing: %v", statErr)
	}
}

func TestHelmStderrReasonKeepsActionableMultilineDiagnostics(t *testing.T) {
	reason := helmStderrReason([]byte("Error: UPGRADE FAILED: values are invalid\n" +
		"missing required key: loki.storage.bucketNames.chunks\n" +
		"private_token=synthetic-helm-canary\n"))
	if !strings.Contains(reason, "UPGRADE FAILED: values are invalid") ||
		!strings.Contains(reason, "missing required key: loki.storage.bucketNames.chunks") {
		t.Fatalf("multiline Helm diagnostics were truncated: %q", reason)
	}
	if strings.Contains(reason, "synthetic-helm-canary") {
		t.Fatalf("Helm diagnostics leaked credential material: %q", reason)
	}
	key := "-----BEGIN PRIVATE KEY-----\nsynthetic-key-body\n-----END PRIVATE KEY-----"
	if strings.Contains(helmStderrReason([]byte(key)), "synthetic-key-body") {
		t.Fatal("multiline private key leaked")
	}
	for _, payload := range []string{
		`{"password":"synthetic-private-canary"}`,
		"password: synthetic-private-canary",
		"stringData: {\"password\":\"synthetic-private-canary\"}",
		"data: {\"password\":\"synthetic-private-canary\"",
	} {
		message := "UPGRADE FAILED: invalid values\n" + payload
		if got := helmStderrReason([]byte(message)); strings.Contains(got, "synthetic-private-canary") {
			t.Fatalf("structured Helm diagnostic leaked: %q", got)
		}
		response := helmMCPFailureResponse("private-diagnostic", &helmToolFailure{
			cause: errors.New(message), phase: "execution", executionStarted: true,
		})
		encoded, err := json.Marshal(response)
		if err != nil || strings.Contains(string(encoded), "synthetic-private-canary") ||
			!strings.Contains(string(encoded), `"isError":true`) ||
			!strings.Contains(string(encoded), `"executionStarted":true`) {
			t.Fatalf("Helm failure receipt lost failure or disclosed payload: %s err=%v", encoded, err)
		}
	}
	long := helmStderrReason([]byte(strings.Repeat("x", 70000)))
	if !strings.HasSuffix(long, "[stderr truncated]") || len(long) > 4200 {
		t.Fatalf("oversized stderr is not explicitly bounded: %d bytes", len(long))
	}
}

func TestRunHelmCommandClassifiesInitialInstallHistory(t *testing.T) {
	for _, test := range []struct {
		name, stderr, stdout string
		exit, unknown        bool
	}{
		{name: "release not found", stderr: "Error: release: not found", exit: true},
		{name: "connection failure", stderr: "Error: Kubernetes cluster unreachable", exit: true, unknown: true},
		{name: "authorization failure", stderr: "Error: forbidden", exit: true, unknown: true},
		{name: "not found inside another error", stderr: "Error: forbidden while checking release: not found", exit: true, unknown: true},
		{name: "generic not found", stderr: "Error: resource not found", exit: true, unknown: true},
		{name: "invalid history", stdout: "not-json", unknown: true},
		{name: "null history", stdout: "null", unknown: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeK8SMCPExecutable(t, root, "helm", `#!/bin/sh
if [ "$1" = "history" ]; then
  printf '%s\n' "$HISTORY_STDERR" >&2
  printf '%s' "$HISTORY_STDOUT"
  [ "$HISTORY_EXIT" != "true" ]
  exit $?
fi
printf 'Error: template schema rejected the chart\n' >&2
exit 1
`)
			t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("HISTORY_STDERR", test.stderr)
			t.Setenv("HISTORY_STDOUT", test.stdout)
			t.Setenv("HISTORY_EXIT", fmt.Sprint(test.exit))
			command := helmCommand{
				args: []string{"upgrade", "app", "chart", "--install"}, cwd: root,
				release: "app", namespace: "apps", mutationID: "doops:first-install:1",
			}
			_, err := runHelmCommand(t.Context(), command, nil)
			if err == nil || !strings.Contains(err.Error(), "template schema rejected the chart") {
				t.Fatalf("original Helm failure was lost: %v", err)
			}
			if errors.Is(err, errExecutionOutcomeUnknown) != test.unknown {
				t.Fatalf("unknown=%t, want %t: %v", errors.Is(err, errExecutionOutcomeUnknown), test.unknown, err)
			}
			response := helmMCPFailureResponse(1, err).Result.(map[string]interface{})
			facts := response["structuredContent"].(map[string]interface{})
			if (facts["executionOutcome"] == "unknown") != test.unknown {
				t.Fatalf("structured outcome differs from execution: %#v", response)
			}
		})
	}
}

func TestRunHelmCommandAcceptsNetworkPolicyOnlyRelease(t *testing.T) {
	root := t.TempDir()
	statusFile := filepath.Join(root, "status.json")
	marker := filepath.Join(root, "mutation-ran")
	mutationID := "doops:network-policy-release:1"
	script := `#!/bin/sh
set -eu
for arg in "$@"; do
  if [ "$arg" = "--dry-run=server" ]; then
    echo "unexpected DoOps preflight" >&2
    exit 9
  fi
done
if [ "$1" = "status" ]; then
  cat "$STATUS_FILE"
  exit 0
fi
touch "$MUTATION_MARKER"
printf '{"version":1,"info":{"status":"deployed","description":"%s"}}' "$MUTATION_ID"
`
	if err := os.WriteFile(filepath.Join(root, "helm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statusFile, []byte(fmt.Sprintf(
		`{"version":1,"info":{"status":"deployed","description":"%s"}}`, mutationID,
	)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("STATUS_FILE", statusFile)
	t.Setenv("MUTATION_ID", mutationID)
	t.Setenv("MUTATION_MARKER", marker)

	chart := filepath.Join(root, "network-policy-only.tgz")
	command := helmCommand{
		args:       []string{"upgrade", "network-policy", chart, "--install", "--description", mutationID},
		cwd:        root,
		release:    "network-policy",
		namespace:  "test",
		mutationID: mutationID,
	}
	result, err := runHelmCommand(t.Context(), command, nil)
	if err != nil {
		t.Fatalf("NetworkPolicy-only Helm release was rejected: %v", err)
	}
	if result != "release deployed: revision 1" {
		t.Fatalf("result = %q, want deployed revision receipt", result)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("Helm mutation did not run: %v", err)
	}
}

func TestHelmMCPRejectsRemovedPublicationFields(t *testing.T) {
	for _, field := range []string{"confirm", "expected_image"} {
		var params api.HelmParams
		decoder := json.NewDecoder(strings.NewReader(`{"session_id":"release","operation":"upgrade-install","release":"app","chart":"https://charts.example.test/app.tgz","namespace":"apps","` + field + `":true}`))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&params); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("removed field %q was accepted: %v", field, err)
		}
	}
}
