package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgenticDeploymentPushesBeforeStructuredAsk(t *testing.T) {
	calls := make([]string, 0, 2)
	var askedArguments map[string]interface{}
	workspaceCommit := strings.Repeat("1", 40)
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:" + strings.Repeat("a", 64)
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "agentic-release",
		pushWorkspace: func(Server, string, string) (string, error) {
			calls = append(calls, "push")
			return workspaceCommit, nil
		},
		ask: func(arguments map[string]interface{}) (ReconciliationResult, error) {
			calls = append(calls, "ask")
			askedArguments = arguments
			result := ReconciliationResult{
				APIVersion: deploymentAPIVersion,
				Kind:       reconciliationResultKind,
				PlanDigest: plan.Digest,
				Status:     ReconciliationConverged,
				Attempts:   1,
				Evidence: []ReconciliationEvidence{
					reconciliationResultTestEvidence("source-identity"),
					reconciliationResultTestEvidence("runtime-state"),
				},
			}
			attestReconciliationResultForTest(plan, workspaceCommit, &result)
			return result, nil
		},
	}
	run, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("run agentic deployment: %v", err)
	}
	if len(calls) != 2 || calls[0] != "push" || calls[1] != "ask" {
		t.Fatalf("CI/CD must push before Ask, got %#v", calls)
	}
	if askedArguments["workspace_commit"] != workspaceCommit {
		t.Fatalf("reconciliation must bind the pushed workspace commit: %#v", askedArguments)
	}
	if run.Result.Status != ReconciliationConverged {
		t.Fatalf("unexpected reconciliation result: %#v", run.Result)
	}
}

func TestAgenticDeploymentInstructionIsMinimalSkillEnvelope(t *testing.T) {
	instruction, err := buildAgenticDeploymentInstruction(DeploymentPlan{
		Digest: "sha256:semantic-plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{RequiredEvidence: []string{"source-identity", "runtime-state"}},
		},
	}, CICDAgenticRunRequest{
		SessionID:   "agentic-release",
		AllowMutate: true,
	})
	if err != nil {
		t.Fatalf("build Ask instruction: %v", err)
	}
	var envelope struct {
		Task           string         `json:"task"`
		Skill          string         `json:"skill"`
		ExecutionMode  string         `json:"executionMode"`
		DeploymentPlan DeploymentPlan `json:"deploymentPlan"`
	}
	if err := json.Unmarshal([]byte(instruction), &envelope); err != nil {
		t.Fatalf("Ask instruction must be one JSON envelope: %v\n%s", err, instruction)
	}
	if envelope.Task != "reconcile-deployment-plan" {
		t.Fatalf("unexpected Agent task: %#v", envelope)
	}
	if envelope.Skill != "semantic-deployment" {
		t.Fatalf("CI/CD must select the semantic deployment Skill: %#v", envelope)
	}
	if envelope.ExecutionMode != "apply" {
		t.Fatalf("mutation authorization must be explicit: %#v", envelope)
	}
	if envelope.DeploymentPlan.Digest != "sha256:semantic-plan" ||
		len(envelope.DeploymentPlan.Spec.Acceptance.RequiredEvidence) != 2 {
		t.Fatalf("DeploymentPlan must remain intact: %#v", envelope.DeploymentPlan)
	}
	for _, forbidden := range []string{
		"Use your available tools",
		"until it converges",
		"last known good",
		"rollback-state",
		"stage list",
		"buildctl",
		"kubectl",
		"helm",
		"deploy.sh",
	} {
		if strings.Contains(instruction, forbidden) {
			t.Fatalf("Ask instruction must leave orchestration to doagent, found %q:\n%s", forbidden, instruction)
		}
	}
}

func TestAgenticDeploymentArgumentsCarryMachineReadableAuthorization(t *testing.T) {
	plan := DeploymentPlan{Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	arguments, err := buildAgenticDeploymentArguments(
		plan,
		CICDAgenticRunRequest{DryRun: true},
		"reconcile this plan",
	)
	if err != nil {
		t.Fatalf("build deployment arguments: %v", err)
	}
	if arguments["operation"] != "reconcile" {
		t.Fatalf("deployment operation must be machine-readable: %#v", arguments)
	}
	if arguments["plan_digest"] != plan.Digest {
		t.Fatalf("plan digest must be machine-readable: %#v", arguments)
	}
	if arguments["execution_mode"] != "dry-run" {
		t.Fatalf("dry-run mode must be machine-readable: %#v", arguments)
	}
	if arguments["response_format"] != "json" {
		t.Fatalf("reconciliation must require structured JSON: %#v", arguments)
	}
}

func TestAgenticDeploymentRejectsBlockedResult(t *testing.T) {
	workspaceCommit := strings.Repeat("2", 40)
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:" + strings.Repeat("b", 64)
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "blocked-release",
		pushWorkspace:   func(Server, string, string) (string, error) { return workspaceCommit, nil },
		ask: func(map[string]interface{}) (ReconciliationResult, error) {
			result := ReconciliationResult{
				APIVersion: deploymentAPIVersion,
				Kind:       reconciliationResultKind,
				PlanDigest: plan.Digest,
				Status:     ReconciliationBlocked,
				Attempts:   1,
				FailureEvidence: []ReconciliationEvidence{
					reconciliationResultTestEvidence("rollback-state"),
				},
			}
			attestReconciliationResultForTest(plan, workspaceCommit, &result)
			return result, nil
		},
	}

	run, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "did not converge") {
		t.Fatalf("blocked result must fail command, got run=%#v err=%v", run, err)
	}
	if run.Result.Status != ReconciliationBlocked {
		t.Fatalf("blocked result must remain observable: %#v", run)
	}
}

func TestAgenticDeploymentRejectsResultWithoutRequiredEvidence(t *testing.T) {
	workspaceCommit := strings.Repeat("3", 40)
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:" + strings.Repeat("c", 64)
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "incomplete-release",
		pushWorkspace:   func(Server, string, string) (string, error) { return workspaceCommit, nil },
		ask: func(map[string]interface{}) (ReconciliationResult, error) {
			result := ReconciliationResult{
				APIVersion: deploymentAPIVersion,
				Kind:       reconciliationResultKind,
				PlanDigest: plan.Digest,
				Status:     ReconciliationConverged,
				Attempts:   1,
				Evidence: []ReconciliationEvidence{
					reconciliationResultTestEvidence("source-identity"),
				},
			}
			attestReconciliationResultForTest(plan, workspaceCommit, &result)
			return result, nil
		},
	}

	_, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "required reconciliation evidence") {
		t.Fatalf("missing evidence must fail command, got %v", err)
	}
}

func TestFindCICDSourceDirectoryAcceptsGitWorktreeMarker(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /tmp/example.git/worktrees/test\n"), 0o644); err != nil {
		t.Fatalf("write worktree marker: %v", err)
	}
	templatePath := filepath.Join(root, "deploy", "workflow.yaml")
	if err := os.MkdirAll(filepath.Dir(templatePath), 0o755); err != nil {
		t.Fatalf("create workflow directory: %v", err)
	}
	if err := os.WriteFile(templatePath, []byte("template"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	directory, err := findCICDSourceDirectory(templatePath)
	if err != nil {
		t.Fatalf("Git worktree must be accepted: %v", err)
	}
	if directory != root {
		t.Fatalf("unexpected source directory %q, want %q", directory, root)
	}
}
