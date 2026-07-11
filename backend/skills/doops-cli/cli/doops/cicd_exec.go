package main

import (
	"fmt"
	"sort"
	"strings"
)

// cicdExecutor dispatches a CICD stage to the DoOps agent-native executor
// (the built-in doagent) via doops_agent_prompt. It shares the k8sCaller shape
// so the existing *MCPClient satisfies it without any adapter.
//
// CICD is agent-driven, not code-driven: the CLI never translates a stage into
// concrete kubectl/helm/buildkit commands. It hands the stage's declarative
// intent to the doagent, which owns HOW — tool selection, environment
// adaptation (base-image mirrors, proxies, source quirks), retries, and
// verification — and reports back.
type cicdExecutor = k8sCaller

type cicdVerificationExecutor interface {
	CallAndCapture(toolName string, arguments map[string]interface{}) (string, error)
}

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

// runCICDAgentStage hands one stage's intent to the doagent and lets it drive.
func runCICDAgentStage(executor cicdExecutor, plan CICDPlan, stage CICDPlanStage, mode, session string) error {
	if executor == nil {
		return fmt.Errorf("stage %s: nil executor", stage.ID)
	}
	if strings.TrimSpace(stage.Uses) == "" {
		return fmt.Errorf("stage %s: uses is required", stage.ID)
	}
	instruction := cicdAgentInstruction(plan, stage, mode, session)
	const maxAttempts = 3
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		lastErr = executor.Call("doops_agent_prompt", map[string]interface{}{
			"instruction": instruction,
		})
		if lastErr == nil {
			break
		}
		if !isTransientCICDAgentError(lastErr) || attempt == maxAttempts {
			return lastErr
		}
		fmt.Printf("⚠️ stage %s: transient agent connection error (attempt %d/%d): %v; retrying...\n",
			stage.ID, attempt, maxAttempts, lastErr)
	}
	if lastErr != nil {
		return lastErr
	}

	verificationCommand := strings.TrimSpace(stage.With["verificationCommand"])
	if mode != "apply" || verificationCommand == "" {
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
	return b.String()
}

func cicdSortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
