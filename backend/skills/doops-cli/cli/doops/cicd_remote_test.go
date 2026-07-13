package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgenticDeploymentPushesBeforeTicketRegistration(t *testing.T) {
	calls := make([]string, 0, 2)
	var registered CICDReleaseCreateRequest
	workspaceCommit := strings.Repeat("1", 40)
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:" + strings.Repeat("a", 64)
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	plan.Spec.Target.Environment = "production"
	plan.Spec.Target.Profile = &CICDEnvironmentProfile{
		Executor: CICDEnvironmentExecutor{Config: CICDHelmExecutorConfig{
			Namespace: "oilan",
			Release:   "zhiyong",
		}},
	}
	plan.Spec.DesiredState.Application = "zhiyong"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node", Gateway: "https://gateway.example", Cluster: "doops-oilan", Instance: "oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "agentic-release",
		pushWorkspace: func(_ Server, _, _ string, source *CICDSourceRelease) (string, error) {
			calls = append(calls, "push")
			if source == nil || source.Revision != plan.Spec.Release.Source.Revision {
				t.Fatalf("push must receive the immutable source identity: %#v", source)
			}
			return workspaceCommit, nil
		},
		enqueue: func(request CICDReleaseCreateRequest) (CICDReleaseTicket, error) {
			calls = append(calls, "enqueue")
			registered = request
			return CICDReleaseTicket{Number: 41, Status: "queued"}, nil
		},
	}
	run, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{DryRun: true})
	if err != nil {
		t.Fatalf("run agentic deployment: %v", err)
	}
	if len(calls) != 2 || calls[0] != "push" || calls[1] != "enqueue" {
		t.Fatalf("CI/CD must push before ticket registration, got %#v", calls)
	}
	if registered.WorkspaceCommit != workspaceCommit {
		t.Fatalf("release ticket must bind the pushed workspace commit: %#v", registered)
	}
	if registered.SourceRevision != plan.Spec.Release.Source.Revision {
		t.Fatalf("release ticket must carry source revision separately from the workspace commit: %#v", registered)
	}
	if registered.SessionID != "agentic-release" || registered.Cluster != "doops-oilan" ||
		registered.Instance != "oilan-node" || registered.Application != "zhiyong" {
		t.Fatalf("release ticket lost session or deployment identity: %#v", registered)
	}
	if run.Ticket.Number != 41 || run.Ticket.Status != "queued" {
		t.Fatalf("run must return the registered ticket: %#v", run)
	}
}

func TestAgenticDeploymentSeparatesWorkspaceSnapshotFromSourceRevision(t *testing.T) {
	workspaceCommit := strings.Repeat("1", 40)
	sourceRevision := strings.Repeat("2", 40)
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:" + strings.Repeat("a", 64)
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	plan.Spec.Target.Environment = "production"
	plan.Spec.DesiredState.Application = "zhiyong"
	plan.Spec.Release.Source = &CICDSourceRelease{
		Repository: "https://example.test/zhiyong.git",
		Revision:   sourceRevision,
	}
	var pushedSource *CICDSourceRelease
	var registered CICDReleaseCreateRequest
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node", Gateway: "https://gateway.example", Cluster: "doops-oilan", Instance: "oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "identity-release",
		pushWorkspace: func(_ Server, _, _ string, source *CICDSourceRelease) (string, error) {
			if source != nil {
				copied := *source
				pushedSource = &copied
			}
			return workspaceCommit, nil
		},
		enqueue: func(request CICDReleaseCreateRequest) (CICDReleaseTicket, error) {
			registered = request
			return CICDReleaseTicket{Number: 42, Status: "queued"}, nil
		},
	}

	if _, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{DryRun: true}); err != nil {
		t.Fatalf("run deployment with distinct identities: %v", err)
	}
	if pushedSource == nil || pushedSource.Revision != sourceRevision {
		t.Fatalf("snapshot source manifest did not preserve immutable source revision: %#v", pushedSource)
	}
	if registered.WorkspaceCommit != workspaceCommit {
		t.Fatalf("transport commit missing from request: %#v", registered)
	}
	if registered.SourceRevision != sourceRevision {
		t.Fatalf("source revision missing from request: %#v", registered)
	}
	if registered.WorkspaceCommit == registered.SourceRevision {
		t.Fatalf("transport and source identities must remain separate: %#v", registered)
	}
	instruction := registered.Instruction
	if !strings.Contains(instruction, `"revision":"`+sourceRevision+`"`) {
		t.Fatalf("Agent envelope must preserve the immutable source revision: %s", instruction)
	}
	if strings.Contains(instruction, workspaceCommit) {
		t.Fatalf("pre-push Agent envelope cannot contain the later snapshot commit: %s", instruction)
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

func TestBuildCICDReleaseCreateRequestCarriesMachineReadableAuthorization(t *testing.T) {
	plan := DeploymentPlan{
		Digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Spec: DeploymentPlanSpec{
			Release: CICDReleaseReference{
				Source: &CICDSourceRelease{
					Repository: "https://cnb.cool/l8ai/example.git",
					Revision:   strings.Repeat("1", 40),
				},
			},
			Target: CICDDeploymentTarget{
				Environment: "production",
				Profile: &CICDEnvironmentProfile{
					Executor: CICDEnvironmentExecutor{Config: CICDHelmExecutorConfig{
						Namespace: "oilan",
						Release:   "zhiyong",
					}},
				},
			},
			DesiredState: CICDDesiredState{Application: "zhiyong"},
			Acceptance:   CICDAcceptance{RequiredEvidence: []string{"source-identity", "runtime-state"}},
		},
	}
	request, err := buildCICDReleaseCreateRequest(
		plan,
		Server{Cluster: "doops-oilan", Instance: "oilan-node"},
		CICDAgenticRunRequest{DryRun: true},
		"reconcile this plan",
		strings.Repeat("2", 40),
	)
	if err != nil {
		t.Fatalf("build release request: %v", err)
	}
	if request.PlanDigest != plan.Digest {
		t.Fatalf("plan digest must be machine-readable: %#v", request)
	}
	if request.ExecutionMode != "dry-run" {
		t.Fatalf("dry-run mode must be machine-readable: %#v", request)
	}
	if request.SourceRevision != plan.Spec.Release.Source.Revision {
		t.Fatalf("source revision must be a separate machine-readable identity: %#v", request)
	}
	if len(request.RequiredEvidence) != 2 {
		t.Fatalf("required evidence contract missing: %#v", request)
	}
}

func TestAgenticDeploymentReturnsTicketRegistrationFailure(t *testing.T) {
	workspaceCommit := strings.Repeat("2", 40)
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:" + strings.Repeat("b", 64)
	plan.Spec.Target.ExecutionTarget = "gw-oilan-node"
	plan.Spec.Target.Environment = "production"
	plan.Spec.DesiredState.Application = "zhiyong"
	runner := agenticDeploymentRunner{
		server:          Server{Name: "gw-oilan-node", Gateway: "https://gateway.example", Cluster: "doops-oilan", Instance: "oilan-node"},
		sourceDirectory: t.TempDir(),
		sessionID:       "blocked-release",
		pushWorkspace:   func(Server, string, string, *CICDSourceRelease) (string, error) { return workspaceCommit, nil },
		enqueue: func(CICDReleaseCreateRequest) (CICDReleaseTicket, error) {
			return CICDReleaseTicket{}, errors.New("gateway unavailable")
		},
	}

	_, err := runner.Run(context.Background(), plan, CICDAgenticRunRequest{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "register release ticket") {
		t.Fatalf("registration failure must fail command, got %v", err)
	}
}

func TestBuildCICDReleaseCreateRequestRejectsMissingRequiredEvidence(t *testing.T) {
	plan := reconciliationResultTestPlan()
	plan.Digest = "sha256:" + strings.Repeat("c", 64)
	plan.Spec.Target.Environment = "production"
	plan.Spec.DesiredState.Application = "zhiyong"
	plan.Spec.Acceptance.RequiredEvidence = nil
	_, err := buildCICDReleaseCreateRequest(
		plan,
		Server{Cluster: "doops-oilan", Instance: "oilan-node"},
		CICDAgenticRunRequest{DryRun: true},
		"reconcile this plan",
		strings.Repeat("3", 40),
	)
	if err == nil || !strings.Contains(err.Error(), "required evidence") {
		t.Fatalf("missing evidence contract must fail ticket creation, got %v", err)
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
