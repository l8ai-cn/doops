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

// isCICDAgentDrivenStage reports whether a stage is a structured agent-native
// stage (agent.task / doops.k8s / doops.exec with no inline run script). These
// are driven entirely by the doagent and bypass the code-driven safety gates.
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
func runCICDAgentStage(executor cicdExecutor, plan CICDPlan, stage CICDPlanStage, mode string) error {
	if executor == nil {
		return fmt.Errorf("stage %s: nil executor", stage.ID)
	}
	if strings.TrimSpace(stage.Uses) == "" {
		return fmt.Errorf("stage %s: uses is required", stage.ID)
	}
	return executor.Call("doops_agent_prompt", map[string]interface{}{
		"instruction": cicdAgentInstruction(plan, stage, mode),
	})
}

// cicdAgentInstruction renders a stable, self-contained goal for the doagent.
// The same stage + mode always produces the same instruction (sorted keys), so
// runs stay reproducible even though execution is agent-driven.
func cicdAgentInstruction(plan CICDPlan, stage CICDPlanStage, mode string) string {
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
	if strings.TrimSpace(plan.Source.Path) != "" {
		b.WriteString("source.path: " + plan.Source.Path + "\n")
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

	b.WriteString("\nReport a concise structured result: what you did, evidence of success (or the blocker if it failed), and leave behind a repeatable script/record where useful.\n")
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
