package main

import (
	"context"
	"strings"
	"testing"
)

func TestAgenticDeploymentPushesBeforeAsk(t *testing.T) {
	calls := make([]string, 0, 2)
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "agentic-release",
		pushWorkspace: func(Server, string, string) error {
			calls = append(calls, "push")
			return nil
		},
		ask: func(string) (string, error) {
			calls = append(calls, "ask")
			return "Converged", nil
		},
	}
	run, err := runner.Run(context.Background(), DeploymentPlan{
		Digest: "sha256:plan",
		Spec: DeploymentPlanSpec{
			Target: CICDDeploymentTarget{ExecutionTarget: "gw-oilan-node"},
		},
	}, CICDAgenticRunRequest{})
	if err != nil {
		t.Fatalf("run agentic deployment: %v", err)
	}
	if len(calls) != 2 || calls[0] != "push" || calls[1] != "ask" {
		t.Fatalf("CI/CD must push before Ask, got %#v", calls)
	}
	if run.Outcome != "Converged" {
		t.Fatalf("Ask outcome = %q", run.Outcome)
	}
}

func TestAgenticDeploymentInstructionCarriesGoalAndAcceptance(t *testing.T) {
	instruction, err := buildAgenticDeploymentInstruction(DeploymentPlan{
		Digest: "sha256:semantic-plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{RequiredEvidence: []string{"source-identity", "runtime-state"}},
		},
	}, CICDAgenticRunRequest{
		SessionID:     "agentic-release",
		AllowMutate:   true,
		MaxIterations: 12,
		MaxNoProgress: 3,
	})
	if err != nil {
		t.Fatalf("build Ask instruction: %v", err)
	}
	for _, want := range []string{
		"/root/ws/agentic-release",
		"DeploymentPlan",
		"Validate every requiredEvidence",
		"restore the last known good revision",
	} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("Ask instruction must contain %q:\n%s", want, instruction)
		}
	}
	for _, forbidden := range []string{"deploy.sh", "uses: shell", "agent.task"} {
		if strings.Contains(instruction, forbidden) {
			t.Fatalf("Ask instruction must not contain %q:\n%s", forbidden, instruction)
		}
	}
}
