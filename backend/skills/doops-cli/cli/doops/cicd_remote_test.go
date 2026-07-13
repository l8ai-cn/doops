package main

import (
	"context"
	"strings"
	"testing"
)

func TestAgenticDeploymentPushesBeforeStructuredAsk(t *testing.T) {
	calls := make([]string, 0, 2)
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:plan"
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "agentic-release",
		pushWorkspace: func(Server, string, string) error {
			calls = append(calls, "push")
			return nil
		},
		ask: func(string) (ReconciliationResult, error) {
			calls = append(calls, "ask")
			return ReconciliationResult{
				APIVersion: deploymentAPIVersion,
				Kind:       reconciliationResultKind,
				PlanDigest: plan.Digest,
				Status:     ReconciliationConverged,
				Attempts:   1,
				Evidence: []ReconciliationEvidence{
					reconciliationResultTestEvidence("source-identity"),
					reconciliationResultTestEvidence("runtime-state"),
				},
			}, nil
		},
	}
	run, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{})
	if err != nil {
		t.Fatalf("run agentic deployment: %v", err)
	}
	if len(calls) != 2 || calls[0] != "push" || calls[1] != "ask" {
		t.Fatalf("CI/CD must push before Ask, got %#v", calls)
	}
	if run.Result.Status != ReconciliationConverged {
		t.Fatalf("unexpected reconciliation result: %#v", run.Result)
	}
}

func TestAgenticDeploymentInstructionCarriesGoalAndAcceptance(t *testing.T) {
	instruction, err := buildAgenticDeploymentInstruction(DeploymentPlan{
		Digest: "sha256:semantic-plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{RequiredEvidence: []string{"source-identity", "runtime-state"}},
			Policy:     CICDDeploymentPolicy{MaxAttempts: 3, MaxNoProgress: 1},
		},
	}, CICDAgenticRunRequest{
		SessionID:   "agentic-release",
		AllowMutate: true,
	})
	if err != nil {
		t.Fatalf("build Ask instruction: %v", err)
	}
	for _, want := range []string{
		"/root/ws/agentic-release",
		"DeploymentPlan",
		"requiredEvidence",
		"ReconciliationResult",
		"exactly one JSON object",
		"DOOPS_GATEWAY_CLUSTER",
		"DOOPS_GATEWAY_INSTANCE",
		"Kubernetes node hostname is not target identity",
	} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("Ask instruction must contain %q:\n%s", want, instruction)
		}
	}
	for _, forbidden := range []string{"deploy.sh", "uses: shell", `"stages":`} {
		if strings.Contains(instruction, forbidden) {
			t.Fatalf("Ask instruction must not contain %q:\n%s", forbidden, instruction)
		}
	}
}

func TestAgenticDeploymentRejectsBlockedResult(t *testing.T) {
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:blocked-plan"
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "blocked-release",
		pushWorkspace:   func(Server, string, string) error { return nil },
		ask: func(string) (ReconciliationResult, error) {
			return ReconciliationResult{
				APIVersion: deploymentAPIVersion,
				Kind:       reconciliationResultKind,
				PlanDigest: plan.Digest,
				Status:     ReconciliationBlocked,
				Attempts:   1,
				FailureEvidence: []ReconciliationEvidence{
					reconciliationResultTestEvidence("rollback-state"),
				},
			}, nil
		},
	}

	run, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{})
	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("blocked result must fail command, got run=%#v err=%v", run, err)
	}
	if run.Result.Status != ReconciliationBlocked {
		t.Fatalf("blocked result must remain observable: %#v", run)
	}
}

func TestAgenticDeploymentRejectsResultWithoutRequiredEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:incomplete-plan"
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "incomplete-release",
		pushWorkspace:   func(Server, string, string) error { return nil },
		ask: func(string) (ReconciliationResult, error) {
			return ReconciliationResult{
				APIVersion: deploymentAPIVersion,
				Kind:       reconciliationResultKind,
				PlanDigest: plan.Digest,
				Status:     ReconciliationConverged,
				Attempts:   1,
				Evidence: []ReconciliationEvidence{
					reconciliationResultTestEvidence("source-identity"),
				},
			}, nil
		},
	}

	_, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{})
	if err == nil || !strings.Contains(err.Error(), "required reconciliation evidence") {
		t.Fatalf("missing evidence must fail command, got %v", err)
	}
}
