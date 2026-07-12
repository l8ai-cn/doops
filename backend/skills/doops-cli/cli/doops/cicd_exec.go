package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// cicdExecutor dispatches a CICD stage to either the DoOps agent-native
// executor or the direct shell tool. It shares the k8sCaller shape so the
// existing *MCPClient satisfies it without any adapter.
//
// Most structured stages are agent-driven. Versioned build and manifest tasks
// are intentionally different: their repository-declared command is the
// release contract and must not be replaced by agent-selected behavior.
type cicdExecutor = k8sCaller

type cicdVerificationExecutor interface {
	CallAndCapture(toolName string, arguments map[string]interface{}) (string, error)
}

const (
	cicdAgentStatusPass = "DOOPS_STAGE_STATUS=PASS"
	cicdAgentStatusFail = "DOOPS_STAGE_STATUS=FAIL"
)

// isCICDAgentDrivenStage reports whether a stage is a structured agent-native
// stage (agent.task / doops.k8s / doops.exec with no inline run script). These
// are driven by the doagent for HOW, but mutating apply runs still require
// --allow-mutate at the CLI gate before dispatch.
func isCICDAgentDrivenStage(stage CICDPlanStage) bool {
	if strings.TrimSpace(stage.Run) != "" {
		return false
	}
	switch stage.Uses {
	case "agent.task", "doops.k8s", "doops.exec":
		return true
	default:
		return false
	}
}

func isCICDVersionedCommandTask(stage CICDPlanStage) bool {
	if strings.TrimSpace(stage.Uses) != "agent.task" || strings.TrimSpace(stage.Run) != "" {
		return false
	}
	switch strings.TrimSpace(stage.With["task"]) {
	case "run-versioned-build-tool", "run-versioned-deploy-command", "publish-release-manifest":
		return true
	default:
		return false
	}
}

func isCICDReleaseSourceVerificationTask(stage CICDPlanStage) bool {
	return strings.TrimSpace(stage.Uses) == "agent.task" &&
		strings.TrimSpace(stage.Run) == "" &&
		strings.TrimSpace(stage.With["task"]) == "verify-release-source"
}

func runCICDRemoteSourceReleaseVerification(executor cicdExecutor, plan CICDPlan, stage CICDPlanStage, session string) error {
	verifier, ok := executor.(cicdVerificationExecutor)
	if !ok {
		return fmt.Errorf("stage %s requires an executor with output capture", stage.ID)
	}
	workspace, err := cicdRemoteWorkspace(session)
	if err != nil {
		return fmt.Errorf("stage %s: %w", stage.ID, err)
	}
	output, err := verifier.CallAndCapture("doops_shell", map[string]interface{}{
		"command": "cat " + shellQuote(workspace+"/.doops-source-release.json"),
	})
	if err != nil {
		return err
	}
	attestation, err := parseCICDSourceReleaseAttestation(output)
	if err != nil {
		return err
	}
	if attestation.ReleaseID != strings.TrimSpace(stage.With["releaseId"]) {
		return fmt.Errorf("synced releaseId mismatch: want=%s got=%s", strings.TrimSpace(stage.With["releaseId"]), attestation.ReleaseID)
	}
	if attestation.Repository != strings.TrimSpace(plan.Source.Repo) {
		return fmt.Errorf("synced source repository mismatch: want=%s got=%s", strings.TrimSpace(plan.Source.Repo), attestation.Repository)
	}
	if attestation.Branch != strings.TrimSpace(stage.With["branch"]) {
		return fmt.Errorf("synced source branch mismatch: want=%s got=%s", strings.TrimSpace(stage.With["branch"]), attestation.Branch)
	}
	return nil
}

func parseCICDSourceReleaseAttestation(output string) (cicdSourceReleaseAttestation, error) {
	start := strings.Index(output, "{")
	if start < 0 {
		return cicdSourceReleaseAttestation{}, fmt.Errorf("parse synced source release attestation: JSON object not found")
	}
	var attestation cicdSourceReleaseAttestation
	if err := json.NewDecoder(strings.NewReader(output[start:])).Decode(&attestation); err != nil {
		return cicdSourceReleaseAttestation{}, fmt.Errorf("parse synced source release attestation: %w", err)
	}
	return attestation, nil
}

func runCICDVersionedCommandTask(executor cicdExecutor, stage CICDPlanStage, mode, session string) (bool, error) {
	if executor == nil {
		return false, fmt.Errorf("stage %s: nil executor", stage.ID)
	}
	workspace, err := cicdRemoteWorkspace(session)
	if err != nil {
		return false, fmt.Errorf("stage %s: %w", stage.ID, err)
	}

	var commands []string
	if mode == "dry-run" {
		if command := strings.TrimSpace(stage.With["dryRunVerificationCommand"]); command != "" {
			commands = append(commands, command)
		} else {
			return false, nil
		}
	} else {
		requiredCommand := strings.TrimSpace(stage.With["requiredCommand"])
		verificationCommand := strings.TrimSpace(stage.With["verificationCommand"])
		if requiredCommand == "" || verificationCommand == "" {
			return false, fmt.Errorf("stage %s requires requiredCommand and verificationCommand", stage.ID)
		}
		commands = append(commands, requiredCommand, verificationCommand)
	}

	command := strings.Join(append([]string{
		"set -e",
		"cd " + shellQuote(workspace),
	}, commands...), "\n")
	if err := executor.Call("doops_shell", map[string]interface{}{"command": command}); err != nil {
		return false, err
	}
	return true, nil
}

func cicdRemoteWorkspace(session string) (string, error) {
	session = strings.TrimSpace(session)
	if session == "" {
		return "", fmt.Errorf("session is required for deterministic remote command tasks")
	}
	if strings.Contains(session, "/") || strings.Contains(session, "\\") || strings.Contains(session, "..") {
		return "", fmt.Errorf("invalid session %q", session)
	}
	return "/root/ws/" + session, nil
}

// runCICDAgentStage hands one stage's intent to the doagent and lets it drive.
func runCICDAgentStage(executor cicdExecutor, plan CICDPlan, stage CICDPlanStage, mode, session string) error {
	if executor == nil {
		return fmt.Errorf("stage %s: nil executor", stage.ID)
	}
	if strings.TrimSpace(stage.Uses) == "" {
		return fmt.Errorf("stage %s: uses is required", stage.ID)
	}
	resultExecutor, ok := executor.(cicdVerificationExecutor)
	if !ok {
		return fmt.Errorf("stage %s requires an executor with output capture", stage.ID)
	}
	instruction := cicdAgentInstruction(plan, stage, mode, session)
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		output, err := resultExecutor.CallAndCapture("doops_agent_prompt", map[string]interface{}{
			"instruction": instruction,
		})
		if err == nil {
			if verdictErr := validateCICDAgentResult(output); verdictErr != nil {
				return fmt.Errorf("stage %s %w", stage.ID, verdictErr)
			}
			lastErr = nil
			break
		}
		lastErr = err
		if !isTransientCICDAgentError(lastErr) || attempt == maxAttempts {
			return lastErr
		}
		fmt.Printf("⚠️ stage %s: transient agent connection error (attempt %d/%d): %v; retrying...\n",
			stage.ID, attempt, maxAttempts, lastErr)
	}
	if lastErr != nil {
		return lastErr
	}

	verificationCommand := cicdVerificationCommand(stage, mode, session)
	if verificationCommand == "" {
		return nil
	}
	verifier, ok := executor.(cicdVerificationExecutor)
	if !ok {
		return fmt.Errorf("stage %s verification requires an executor with output capture", stage.ID)
	}
	// Verification runs on a separate doops_shell call after the agent turn.
	// Long buildkit/helm stages often leave the WS idle; treat connection loss
	// the same as agent-prompt dispatch and retry before failing the stage.
	var verifyErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		_, verifyErr = verifier.CallAndCapture("doops_shell", map[string]interface{}{
			"command": verificationCommand,
		})
		if verifyErr == nil {
			return nil
		}
		if !isTransientCICDAgentError(verifyErr) || attempt == maxAttempts {
			return fmt.Errorf("stage %s verification failed: %w", stage.ID, verifyErr)
		}
		fmt.Printf("⚠️ stage %s: transient verification connection error (attempt %d/%d): %v; retrying...\n",
			stage.ID, attempt, maxAttempts, verifyErr)
	}
	return fmt.Errorf("stage %s verification failed: %w", stage.ID, verifyErr)
}

func cicdVerificationCommand(stage CICDPlanStage, mode, session string) string {
	var command string
	switch mode {
	case "apply":
		command = stage.With["verificationCommand"]
	case "dry-run":
		command = stage.With["dryRunVerificationCommand"]
	default:
		return ""
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}
	session = strings.TrimSpace(session)
	if session == "" {
		return command
	}
	workspace := "/root/ws/" + session
	return "cd -- '" + strings.ReplaceAll(workspace, "'", "'\"'\"'") + "' && " + command
}

func isTransientCICDAgentError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection lost") ||
		strings.Contains(msg, "ws connection lost") ||
		strings.Contains(msg, "failed to connect to agent ws") ||
		strings.Contains(msg, "agent disconnected") ||
		strings.Contains(msg, "websocket: close") ||
		strings.Contains(msg, "unexpected eof") ||
		strings.HasSuffix(msg, ": eof") ||
		strings.Contains(msg, "eof")
}

// cicdAgentInstruction renders a stable, self-contained goal for the doagent.
// The same stage + mode always produces the same instruction (sorted keys), so
// runs stay reproducible even though execution is agent-driven.
func cicdAgentInstruction(plan CICDPlan, stage CICDPlanStage, mode, session string) string {
	var b strings.Builder
	b.WriteString("You are the DoOps agent-native executor. Drive this CI/CD stage to completion on the target node.\n")
	b.WriteString("You own the HOW end to end: choose the right tools (buildkit / kubectl / helm / shell), adapt to the environment (internal image mirrors, proxies, source-transfer quirks), self-heal and retry on transient failures, and verify the outcome before reporting success.\n")
	b.WriteString("Judge safety yourself: honor the declared mode and mutation intent below; if mode is dry-run, plan and validate only — do not apply changes.\n\n")

	b.WriteString("workflow: " + plan.Name + "\n")
	b.WriteString("stage.id: " + stage.ID + "\n")
	if strings.TrimSpace(stage.Name) != "" {
		b.WriteString("stage.name: " + stage.Name + "\n")
	}
	b.WriteString("stage.uses: " + stage.Uses + "\n")
	b.WriteString(fmt.Sprintf("mode: %s\n", mode))
	b.WriteString(fmt.Sprintf("mutates: %t\n", stage.Mutates))
	if target := strings.TrimSpace(plan.ExecutionTarget); target != "" {
		b.WriteString("execution.target: " + target + "\n")
	}
	if strings.TrimSpace(session) != "" {
		b.WriteString("session: " + strings.TrimSpace(session) + "\n")
		b.WriteString("remote.workspace: /root/ws/" + strings.TrimSpace(session) + "\n")
		b.WriteString("source.location: remote.workspace (operator already synced the release tree here; do NOT expect macOS /tmp paths to exist on this node)\n")
	}
	if strings.TrimSpace(plan.Source.Path) != "" {
		b.WriteString("source.path.local: " + plan.Source.Path + "\n")
	}
	if strings.TrimSpace(plan.Source.Repo) != "" {
		b.WriteString("source.repo: " + plan.Source.Repo + "\n")
	}

	if len(stage.With) > 0 {
		b.WriteString("\nintent:\n")
		for _, key := range cicdSortedKeys(stage.With) {
			b.WriteString("  " + key + ": " + stage.With[key] + "\n")
		}
	}
	if requiredCommand := strings.TrimSpace(stage.With["requiredCommand"]); requiredCommand != "" {
		b.WriteString("\nmandatory execution:\n")
		b.WriteString("You MUST execute requiredCommand exactly as declared. Do not replace it with a draft, approximation, or manual alternative. If it cannot complete, report the command failure and fail the stage.\n")
	}
	if len(plan.Inputs) > 0 {
		b.WriteString("\ninputs:\n")
		for _, key := range cicdSortedKeys(plan.Inputs) {
			b.WriteString("  " + key + ": " + plan.Inputs[key] + "\n")
		}
	}
	if strings.TrimSpace(plan.Context) != "" {
		b.WriteString("\nworkflow context (environment facts you must honor):\n")
		for _, line := range strings.Split(strings.TrimRight(plan.Context, "\n"), "\n") {
			b.WriteString("  " + line + "\n")
		}
	}

	b.WriteString("\nThis stage is self-contained. Do the work idempotently, verify the outcome, then STOP.\n")
	b.WriteString("Use remote.workspace as the repository root for charts, tests, and files. Ignore source.path.local if it is not present on this node.\n")
	b.WriteString("Do NOT build up cross-stage artifacts: do not read, append to, or rewrite a cumulative deploy script, audit log, or per-stage result file carried over from previous stages. Those unbounded artifacts grow past tool limits and stall the run. If you must write a file, keep it small and stage-local.\n")
	b.WriteString("Keep the streaming connection warm: emit progress/heartbeats during long tools so the SSE idle timer does not fire.\n")
	b.WriteString("Report a concise structured result: what you did, evidence of success, or the specific blocker if it failed. Keep the report short.\n")
	b.WriteString("End your final report with exactly one standalone status line: " + cicdAgentStatusPass + " only after verified success, or " + cicdAgentStatusFail + " for any blocker or failed verification. Never report PASS after a failure.\n")
	return b.String()
}

func validateCICDAgentResult(output string) error {
	status := ""
	for _, line := range strings.Split(output, "\n") {
		switch strings.TrimSpace(line) {
		case cicdAgentStatusPass:
			status = cicdAgentStatusPass
		case cicdAgentStatusFail:
			status = cicdAgentStatusFail
		}
	}
	switch status {
	case cicdAgentStatusPass:
		return nil
	case cicdAgentStatusFail:
		return fmt.Errorf("reported failure")
	default:
		return fmt.Errorf("did not report %s or %s", cicdAgentStatusPass, cicdAgentStatusFail)
	}
}

func cicdSortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
