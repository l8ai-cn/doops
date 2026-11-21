package server

// Helm release execution keeps the transport and result contract small:
// execute Helm, identify the revision created by this call, and read that
// release back to confirm its deployed status. Chart contents and workload
// health remain Helm or task-level concerns.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/user/doops/agent/internal/dispatcher"
)

const (
	// helmSettleReserve is kept free at the end of the MCP tool timeout for the
	// SIGTERM grace period and the settle step.
	helmSettleReserve = 90 * time.Second
	// maxHelmWaitTimeout bounds one Helm call so the wait still fits inside the
	// MCP tool deadline and leaves time to record the outcome.
	maxHelmWaitTimeout = 25 * time.Minute
	// helmSettleTimeout bounds reading history and unlocking a pending release.
	helmSettleTimeout = 45 * time.Second
)

var (
	// helmToolDeadline bounds one whole doops_helm call. It is tighter than
	// mcpToolCallDeadline so the SIGTERM grace and settle step still fit before
	// doagent's own timeout.
	helmToolDeadline = time.Duration(doopsMCPToolTimeoutSeconds)*time.Second - helmSettleReserve
	// helmTerminationGrace is how long Helm has after SIGTERM to record a
	// terminal status before it is killed.
	helmTerminationGrace = 30 * time.Second
)

// helmStepError carries a stable code plus the reason a step failed.
type helmStepError struct {
	code           string
	reason         string
	outcomeUnknown bool
}

func (e *helmStepError) Error() string {
	if e.reason == "" {
		return e.code
	}
	return e.code + ": " + e.reason
}

func helmStepFailure(code, reason string) error {
	return &helmStepError{code: code, reason: reason}
}

func (e *helmStepError) Is(target error) bool {
	return e.outcomeUnknown && target == errExecutionOutcomeUnknown
}

func helmStepUnknown(code, reason string) error {
	return &helmStepError{code: code, reason: reason, outcomeUnknown: true}
}

// helmStderrReason keeps the complete useful stderr from Helm. Chart-controlled
// stdout is never returned; stderr is redacted and bounded because a template
// error can quote a value. Keeping multiple lines matters because Helm often
// puts the actionable missing field after the summary line.
func helmStderrReason(stderr []byte) string {
	const maxReasonRunes = 4096
	reason := strings.TrimPrefix(strings.TrimSpace(redactHelmDiagnostic(string(stderr))), "Error: ")
	if reason == "" {
		return "helm exited without an error message"
	}
	if utf8.RuneCountInString(reason) <= maxReasonRunes {
		return reason
	}
	return string([]rune(reason)[:maxReasonRunes]) + "\n[stderr truncated]"
}

func redactHelmDiagnostic(text string) string {
	public, err := redactDoagentUIText(text)
	if err != nil {
		return "[Helm diagnostic withheld: incomplete private payload could not be redacted]"
	}
	return public
}

func runHelmCommand(ctx context.Context, command helmCommand, _ notificationSender) (string, error) {
	output, started, err := mutateHelmRelease(ctx, command)
	if !started {
		var denied *dispatcher.BlockedError
		if errors.As(err, &denied) {
			return "", &helmToolFailure{cause: err, phase: "policy"}
		}
		return "", &helmToolFailure{cause: err, phase: "preparation"}
	}
	if err != nil {
		if command.mutationID == "" {
			if (command.operation() == "rollback" || command.operation() == "uninstall") &&
				(ctx.Err() != nil || errors.Is(err, errExecutionOutcomeUnknown)) {
				return "", helmStepUnknown("helm_mutation_unverified", "Helm did not confirm the operation; inspect release status before retrying")
			}
			return "", err
		}
		// A failed upgrade may leave a pending revision that locks the release.
		// Settle it before returning the failure so the operator gets a truthful
		// next step instead of a hidden stale lock.
		note, settleErr := settleHelmRevision(ctx, command)
		if settleErr != nil {
			if ctx.Err() != nil {
				return "", helmStepUnknown("helm_interrupted", fmt.Sprintf(
					"the call ran out of time; %s; check helm status before any retry; helm reported: %s", settleErr, err))
			}
			return "", helmStepUnknown("helm_mutation_failed", fmt.Sprintf(
				"%s; check helm status before any retry; helm reported: %s", settleErr.Error(), err.Error()))
		}
		if note == "" {
			return "", err
		}
		if ctx.Err() != nil || errors.Is(err, errExecutionOutcomeUnknown) {
			return "", helmStepUnknown("helm_interrupted", fmt.Sprintf(
				"the call ran out of time; %s; helm reported: %s", note, err.Error()))
		}
		return "", helmStepFailure("helm_mutation_failed", fmt.Sprintf("%s; helm reported: %s", note, err.Error()))
	}
	if command.operation() != "upgrade" {
		return "", nil
	}

	revision, err := parseHelmReleaseOutput(output, command.mutationID)
	if err != nil {
		return "", helmStepUnknown("helm_mutation_invalid_result", err.Error())
	}
	command.revision = revision
	verified, err := verifyHelmReleaseState(ctx, command)
	if err != nil {
		if ctx.Err() != nil {
			return "", helmStepUnknown("helm_interrupted", fmt.Sprintf(
				"the call ran out of time after revision %d was reported; release status was not confirmed: %s", revision, err))
		}
		return "", err
	}
	return verified, nil
}

func mutateHelmRelease(ctx context.Context, command helmCommand) ([]byte, bool, error) {
	args := append([]string(nil), command.args...)
	if command.operation() == "upgrade" {
		args = append(args, "--output=json")
	}
	cmd, err := helmProcess(ctx, command.cwd, args...)
	if err != nil {
		return nil, false, err
	}
	// stdout may contain chart-controlled notes and is only parsed for the
	// release revision; it is never returned to the caller.
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, false, helmStepFailure("helm_mutation_failed", err.Error())
	}
	if err := cmd.Wait(); err != nil {
		var exit *exec.ExitError
		if ctx.Err() != nil || (errors.As(err, &exit) && exit.ExitCode() < 0) {
			return nil, true, helmStepUnknown("helm_interrupted", helmStderrReason(stderr.Bytes()))
		}
		return nil, true, helmStepFailure("helm_mutation_failed", helmStderrReason(stderr.Bytes()))
	}
	return stdout.Bytes(), true, nil
}

// helmProcess stops Helm with SIGTERM when the context ends, so Helm can record
// the revision and release its lock before the grace period expires.
func helmProcess(ctx context.Context, cwd string, args ...string) (*exec.Cmd, error) {
	cmd, err := dispatcher.CommandContext(ctx, "helm", args...)
	if err != nil {
		return nil, err
	}
	cmd.Dir = cwd
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = helmTerminationGrace
	return cmd, nil
}

type helmHistoryEntry struct {
	Revision    int    `json:"revision"`
	Status      string `json:"status"`
	Description string `json:"description"`
}

// settleHelmRevision reads what Helm recorded for this mutation and makes sure
// a pending revision does not leave the release locked.
func settleHelmRevision(ctx context.Context, command helmCommand) (string, error) {
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), helmSettleTimeout)
	defer cancel()
	historyCmd, err := dispatcher.CommandContext(settleCtx, "helm", "history", command.release,
		"--namespace", command.namespace, "--max", "20", "--output", "json")
	if err != nil {
		return "", helmStepUnknown("helm_interrupted", fmt.Sprintf("read release history: %v", err))
	}
	historyCmd.Dir = command.cwd
	raw, err := historyCmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() > 0 && settleCtx.Err() == nil &&
		len(bytes.TrimSpace(raw)) == 0 &&
		strings.TrimPrefix(strings.TrimSpace(string(exit.Stderr)), "Error: ") == "release: not found" {
		// A first install can fail template/schema validation before recording
		// a release. Helm's exact not-found result confirms that absence.
		return "", nil
	}
	var history []helmHistoryEntry
	if err == nil {
		err = json.Unmarshal(raw, &history)
	}
	if err != nil || history == nil {
		return "", helmStepUnknown("helm_interrupted",
			"the release history could not be read; check helm status before any retry")
	}
	var mine *helmHistoryEntry
	deployed := 0
	for i := range history {
		entry := &history[i]
		if entry.Description == command.mutationID {
			mine = entry
		}
		if entry.Status == "deployed" && entry.Revision > deployed {
			deployed = entry.Revision
		}
	}
	switch {
	case mine == nil:
		return "", nil
	case mine.Status == "failed":
		return fmt.Sprintf("Helm recorded revision %d failed; deployed revision %d is unchanged", mine.Revision, deployed), nil
	case mine.Status == "deployed":
		return "", helmStepUnknown("helm_interrupted", "Helm recorded the revision deployed despite the command failure")
	case !strings.HasPrefix(mine.Status, "pending-"):
		return "", helmStepUnknown("helm_interrupted", "Helm did not record this revision as failed")
	case deployed == 0:
		return "", helmStepUnknown("helm_interrupted", fmt.Sprintf(
			"revision %d is %s and there is no deployed revision to roll back to; inspect the release before retrying", mine.Revision, mine.Status))
	}
	rollbackCmd, err := dispatcher.CommandContext(settleCtx, "helm", "rollback", command.release, fmt.Sprintf("%d", deployed),
		"--namespace", command.namespace, "--wait")
	if err != nil {
		return "", helmStepUnknown("helm_interrupted", fmt.Sprintf("revision %d stayed %s; rollback to revision %d did not start: %v", mine.Revision, mine.Status, deployed, err))
	}
	rollbackCmd.Dir = command.cwd
	var stderr bytes.Buffer
	rollbackCmd.Stderr = &stderr
	if err := rollbackCmd.Run(); err != nil {
		return "", helmStepUnknown("helm_interrupted", fmt.Sprintf(
			"revision %d stayed %s and the rollback to revision %d failed: %s",
			mine.Revision, mine.Status, deployed, helmStderrReason(stderr.Bytes())))
	}
	statusCmd, err := dispatcher.CommandContext(settleCtx, "helm", "status", command.release,
		"--namespace", command.namespace, "--output", "json")
	if err != nil {
		return "", helmStepUnknown("helm_interrupted", "could not read release status after rollback")
	}
	statusCmd.Dir = command.cwd
	raw, err = statusCmd.Output()
	var status helmReleaseJSON
	if err != nil || json.Unmarshal(raw, &status) != nil || status.Info.Status != "deployed" || status.Version <= mine.Revision ||
		status.Info.Description != fmt.Sprintf("Rollback to %d", deployed) {
		return "", helmStepUnknown("helm_interrupted", "Helm reported a rollback but the restored release could not be verified")
	}
	return fmt.Sprintf("revision %d stayed %s, so the release was rolled back to revision %d to unlock it; check the release before retrying",
		mine.Revision, mine.Status, deployed), nil
}

func (c helmCommand) operation() string {
	if len(c.args) == 0 {
		return ""
	}
	return c.args[0]
}

type helmReleaseJSON struct {
	Version int `json:"version"`
	Info    struct {
		Status      string `json:"status"`
		Description string `json:"description"`
	} `json:"info"`
}

func parseHelmReleaseOutput(output []byte, expectedDescription string) (int, error) {
	var release helmReleaseJSON
	if err := json.Unmarshal(output, &release); err != nil {
		return 0, fmt.Errorf("helm release output is not JSON")
	}
	if release.Version <= 0 {
		return 0, fmt.Errorf("helm release output has no positive revision")
	}
	if release.Info.Description != expectedDescription {
		return 0, fmt.Errorf("helm release output description does not match mutation")
	}
	return release.Version, nil
}

func verifyHelmReleaseState(ctx context.Context, command helmCommand) (string, error) {
	statusCmd, err := dispatcher.CommandContext(ctx, "helm", "status", command.release,
		"--namespace", command.namespace, "--output", "json")
	if err != nil {
		return "", helmStepUnknown("helm_verification_failed", fmt.Sprintf("read current release status: %v", err))
	}
	statusCmd.Dir = command.cwd
	statusBytes, err := statusCmd.Output()
	if err != nil {
		return "", helmStepUnknown("helm_verification_failed", redactSecretText(fmt.Sprintf("read current release status: %v", err)))
	}
	var status helmReleaseJSON
	if err := json.Unmarshal(statusBytes, &status); err != nil {
		return "", helmStepUnknown("helm_verification_failed", "helm status output is not JSON")
	}
	if status.Version != command.revision {
		return "", helmStepUnknown("helm_verification_failed", fmt.Sprintf("release revision is %d, want %d", status.Version, command.revision))
	}
	if status.Info.Description != command.mutationID {
		return "", helmStepUnknown("helm_verification_failed", "release description does not match mutation")
	}
	if status.Info.Status != "deployed" {
		reason := redactSecretText(fmt.Sprintf("release status is %q, want deployed", status.Info.Status))
		if status.Info.Status == "failed" {
			return "", helmStepFailure("helm_verification_failed", reason)
		}
		return "", helmStepUnknown("helm_verification_failed", reason)
	}
	return fmt.Sprintf("release deployed: revision %d", status.Version), nil
}

func sameStringList(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
