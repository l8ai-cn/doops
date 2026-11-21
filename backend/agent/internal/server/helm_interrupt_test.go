package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/user/doops/agent/api"
)

// interruptFakeHelm answers history from $ROOT/history.json and records
// rollback arguments. A real upgrade records revision 6 with this call's
// description and status $MINE_STATUS, then hangs until SIGTERM.
const interruptFakeHelm = `#!/bin/sh
case "$1" in
history)
  if [ -n "$HISTORY_FAIL" ]; then exit 1; fi
  cat "$ROOT/history.json"; exit 0;;
status)
  printf '%s\n' '{"version":7,"info":{"status":"deployed","description":"Rollback to 5"}}'; exit 0;;
rollback)
  printf '%s\n' "$@" > "$ROOT/rollback-args"
  if [ -n "$ROLLBACK_FAIL" ]; then printf 'Error: %s\n' "$ROLLBACK_FAIL" >&2; exit 1; fi
  exit 0;;
esac
prev=""; desc=""
for arg in "$@"; do
  if [ "$prev" = "--description" ]; then desc="$arg"; fi
  prev="$arg"
done
printf '[{"revision":5,"status":"deployed","description":"old"},{"revision":6,"status":"%s","description":"%s"}]' "$MINE_STATUS" "$desc" > "$ROOT/history.json"
trap 'echo term > "$ROOT/terminated"; exit 1' TERM
while :; do sleep 0.05; done
`

func installInterruptFakeHelm(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helm"), []byte(interruptFakeHelm), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ROOT", root)
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	t.Setenv("HISTORY_FAIL", "")
	t.Setenv("ROLLBACK_FAIL", "")
	return root
}

func TestInterruptedHelmObservationUsesReadOnlyStatus(t *testing.T) {
	root := installInterruptFakeHelm(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, operation := range []string{"rollback", "uninstall"} {
		t.Run(operation, func(t *testing.T) {
			command := helmCommand{args: []string{operation}, cwd: root, release: "app", namespace: "apps"}
			observation := interruptedHelmObservation(ctx, command)
			if !strings.Contains(observation, "did not confirm") || !strings.Contains(observation, "revision 7 in state deployed") {
				t.Fatalf("unexpected observation: %s", observation)
			}
			if _, err := os.Stat(filepath.Join(root, "rollback-args")); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("readback dispatched a rollback: %v", err)
			}
		})
	}
}

func TestSettleInterruptedHelmRelease(t *testing.T) {
	const mine = "doops:release:1"
	entry := func(revision, status, description string) string {
		return `{"revision":` + revision + `,"status":"` + status + `","description":"` + description + `"}`
	}
	history := func(entries ...string) string { return "[" + strings.Join(entries, ",") + "]" }
	tests := []struct {
		name         string
		history      string
		historyFail  bool
		rollbackFail string
		want         string
		wantErr      bool
		wantRollback string
	}{
		{
			name: "history unreadable", historyFail: true,
			want: "release history could not be read", wantErr: true,
		},
		{
			name:    "no revision for this mutation",
			history: history(entry("5", "deployed", "old")),
		},
		{
			name:    "Helm recorded the revision failed",
			history: history(entry("5", "deployed", "old"), entry("6", "failed", mine)),
		},
		{
			name:    "deployed revision contradicts command failure",
			history: history(entry("5", "superseded", "old"), entry("6", "deployed", mine)),
			want:    "deployed despite the command failure", wantErr: true,
		},
		{
			name:         "pending revision is rolled back to the last deployed one",
			history:      history(entry("5", "deployed", "old"), entry("6", "pending-upgrade", mine)),
			want:         "revision 6 stayed pending-upgrade, so the release was rolled back to revision 5",
			wantRollback: "rollback\napp\n5\n--namespace\napps\n--wait\n",
		},
		{
			name:    "pending install with nothing to roll back to",
			history: history(entry("1", "pending-install", mine)),
			want:    "revision 1 is pending-install and there is no deployed revision", wantErr: true,
		},
		{
			name:         "rollback failure names Helm's reason",
			history:      history(entry("5", "deployed", "old"), entry("6", "pending-upgrade", mine)),
			rollbackFail: "release locked by synthetic test",
			want:         "the rollback to revision 5 failed: release locked by synthetic test",
			wantErr:      true,
			wantRollback: "rollback\napp\n5\n--namespace\napps\n--wait\n",
		},
		{
			name:    "other terminal state is left alone",
			history: history(entry("5", "deployed", "old"), entry("6", "superseded", mine)),
			want:    "did not record this revision as failed", wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := installInterruptFakeHelm(t)
			if err := os.WriteFile(filepath.Join(root, "history.json"), []byte(tt.history), 0o600); err != nil {
				t.Fatal(err)
			}
			if tt.historyFail {
				t.Setenv("HISTORY_FAIL", "1")
			}
			t.Setenv("ROLLBACK_FAIL", tt.rollbackFail)
			ctx, cancel := context.WithCancel(context.Background())
			cancel() // settle runs after the call's own context has ended
			note, err := settleHelmRevision(ctx, helmCommand{
				release: "app", namespace: "apps", mutationID: mine, cwd: root,
			})
			if tt.wantErr {
				if err == nil || !strings.HasPrefix(err.Error(), "helm_interrupted: ") || !strings.Contains(err.Error(), tt.want) {
					t.Fatalf("error = %v, want helm_interrupted containing %q", err, tt.want)
				}
				if !errors.Is(err, errExecutionOutcomeUnknown) {
					t.Fatalf("unconfirmed release state became a known failure: %v", err)
				}
			} else if err != nil {
				t.Fatalf("settle failed: %v", err)
			}
			if !tt.wantErr && !strings.Contains(note, tt.want) {
				t.Fatalf("note = %q, want it to contain %q", note, tt.want)
			}
			args, _ := os.ReadFile(filepath.Join(root, "rollback-args"))
			if string(args) != tt.wantRollback {
				t.Fatalf("rollback args = %q, want %q", args, tt.wantRollback)
			}
		})
	}
}

// Given an upgrade that outlives its call budget, When the budget ends, Then
// helm gets SIGTERM (not SIGKILL) and a revision left pending is rolled back,
// so the release is never stuck in pending-upgrade.
func TestRunHelmCommandInterruptedUpgradeNeverStaysPending(t *testing.T) {
	for _, tt := range []struct {
		name          string
		mineStatus    string
		want          string
		wantInterrupt bool
		wantRollback  bool
	}{
		{"helm left the revision pending", "pending-upgrade", "rolled back to revision 5", true, true},
		{"helm recorded the revision failed", "failed", "Helm recorded revision 6 failed", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := installInterruptFakeHelm(t)
			t.Setenv("MINE_STATUS", tt.mineStatus)
			previous := helmTerminationGrace
			helmTerminationGrace = 2 * time.Second
			t.Cleanup(func() { helmTerminationGrace = previous })

			command, err := buildHelmCommand(testHelmParams())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
			defer cancel()
			_, err = runHelmCommand(ctx, command, func(notificationEvent) {})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.want)
			}
			if !errors.Is(err, errExecutionOutcomeUnknown) {
				t.Fatalf("interrupted execution lost its unknown outcome: %v", err)
			}
			if tt.wantInterrupt && !strings.HasPrefix(err.Error(), "helm_interrupted: ") {
				t.Fatalf("error = %v, want helm_interrupted", err)
			}
			if marker, _ := os.ReadFile(filepath.Join(root, "terminated")); strings.TrimSpace(string(marker)) != "term" {
				t.Fatal("interrupted helm did not receive SIGTERM")
			}
			_, statErr := os.Stat(filepath.Join(root, "rollback-args"))
			if (statErr == nil) != tt.wantRollback {
				t.Fatalf("rollback ran = %v, want %v", statErr == nil, tt.wantRollback)
			}
		})
	}
}

func TestBuildHelmCommandBoundsTheWaitTimeout(t *testing.T) {
	for _, tt := range []struct {
		timeout string
		wantErr bool
	}{
		{"", false},
		{"180s", false},
		{"10m", false},
		{"600s", false},
		{"25m", false},
		{"1500s", false},
		{"1501s", true},
		{"26m", true},
		{"1h", true},
	} {
		t.Run("timeout="+tt.timeout, func(t *testing.T) {
			params := testHelmParams()
			params.Timeout = tt.timeout
			t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
			_, err := buildHelmCommand(params)
			if (err != nil) != tt.wantErr {
				t.Fatalf("timeout %q: err = %v, wantErr %v", tt.timeout, err, tt.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), "at most 25m") {
				t.Fatalf("rejection does not name the limit: %v", err)
			}
		})
	}
	// Rollback shares the bound.
	params := api.HelmParams{SessionID: "release", Operation: "rollback", Release: "app", Namespace: "apps", Revision: 2, Timeout: "26m"}
	if _, err := buildHelmCommand(params); err == nil {
		t.Fatal("rollback accepted a timeout beyond the call budget")
	}
}

func TestHelmToolDeadlineLeavesRoomBeforeDoagentTimeout(t *testing.T) {
	doagent := time.Duration(doopsMCPToolTimeoutSeconds) * time.Second
	if maxHelmWaitTimeout >= helmToolDeadline {
		t.Fatalf("max helm wait %s does not fit in the call deadline %s", maxHelmWaitTimeout, helmToolDeadline)
	}
	if helmToolDeadline+helmTerminationGrace+helmSettleTimeout > doagent {
		t.Fatalf("deadline %s + grace %s + settle %s exceeds doagent's %s",
			helmToolDeadline, helmTerminationGrace, helmSettleTimeout, doagent)
	}
}

// Helm's own --wait can time out: helm exits non-zero and leaves the revision
// pending-upgrade, which locks the release for every later operation. The
// Agent must unlock it and say which revision is in effect.
func TestHelmWaitTimeoutLeavesNoPendingRevision(t *testing.T) {
	const waitTimeoutHelm = `#!/bin/sh
case "$1" in
history)
  cat "$ROOT/history.json"; exit 0;;
status)
  printf '%s\n' '{"version":7,"info":{"status":"deployed","description":"Rollback to 5"}}'; exit 0;;
rollback)
  printf '%s\n' "$@" > "$ROOT/rollback-args"; exit 0;;
esac
prev=""; desc=""
for arg in "$@"; do
  if [ "$prev" = "--description" ]; then desc="$arg"; fi
  prev="$arg"
done
printf '[{"revision":5,"status":"deployed","description":"old"},{"revision":6,"status":"pending-upgrade","description":"%s"}]' "$desc" > "$ROOT/history.json"
printf 'Error: context deadline exceeded\n' >&2
exit 1
`
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "helm"), []byte(waitTimeoutHelm), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ROOT", root)
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	command, err := buildHelmCommand(testHelmParams())
	if err != nil {
		t.Fatal(err)
	}
	_, err = runHelmCommand(context.Background(), command, func(notificationEvent) {})
	if err == nil {
		t.Fatal("a failed upgrade was reported as success")
	}
	if !strings.Contains(err.Error(), "rolled back to revision 5") {
		t.Fatalf("error = %v, want it to report the rollback to revision 5", err)
	}
	args, readErr := os.ReadFile(filepath.Join(root, "rollback-args"))
	if readErr != nil {
		t.Fatalf("the pending revision was left locked: %v", err)
	}
	if !strings.Contains(string(args), "\n5\n") {
		t.Fatalf("rollback args = %q, want revision 5", args)
	}
}
