package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestCICDSubmitDoesNotReadLocalDeploymentState(t *testing.T) {
	var submitted ReleaseRequest
	err := runCICDSubmitCommand(context.Background(), []string{
		"submit",
		"--target", "release-control-plane",
		"--repository-id", "repo_zhiyong",
		"--revision", "0123456789abcdef0123456789abcdef01234567",
		"--workflow", "deploy/workflows/test.yaml",
		"--set", "reason=release",
		"--allow-mutate",
	}, func(request ReleaseRequest) (ReleaseResult, error) {
		submitted = request
		return ReleaseResult{
			ReleaseID: "release-20260712-0123456789ab",
			Status:    "Accepted",
		}, nil
	})
	if err != nil {
		t.Fatalf("submit release request: %v", err)
	}

	if submitted.RepositoryID != "repo_zhiyong" {
		t.Fatalf("repository id = %q, want repo_zhiyong", submitted.RepositoryID)
	}
	if submitted.Revision != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("revision = %q", submitted.Revision)
	}
	if submitted.WorkflowPath != "deploy/workflows/test.yaml" {
		t.Fatalf("workflow path = %q", submitted.WorkflowPath)
	}
	if submitted.Inputs["reason"] != "release" {
		t.Fatalf("inputs = %#v", submitted.Inputs)
	}
	if !submitted.AllowMutate {
		t.Fatal("expected an explicitly approved mutation request")
	}
	payload, err := json.Marshal(submitted)
	if err != nil {
		t.Fatalf("marshal submitted request: %v", err)
	}
	if strings.Contains(string(payload), "localTemplatePath") || strings.Contains(string(payload), "planSignature") {
		t.Fatalf("submit request must not carry local deployment authority: %s", payload)
	}
}

func TestCICDSubmitAcceptsBackendDeploymentWorkflow(t *testing.T) {
	command, err := buildCICDSubmitCommand([]string{
		"submit",
		"--target", "release-control-plane",
		"--repository-id", "repo_doops",
		"--revision", "0123456789abcdef0123456789abcdef01234567",
		"--workflow", "backend/deploy/workflows/oilan-agent-bootstrap.yaml",
		"--allow-mutate",
	})
	if err != nil {
		t.Fatalf("build release request for backend deployment workflow: %v", err)
	}
	if command.Request.WorkflowPath != "backend/deploy/workflows/oilan-agent-bootstrap.yaml" {
		t.Fatalf("workflow path = %q", command.Request.WorkflowPath)
	}
}

func TestCICDSubmitRejectsLocalWorkflowFile(t *testing.T) {
	err := runCICDSubmitCommand(context.Background(), []string{
		"submit",
		"--target", "release-control-plane",
		"--repository-id", "repo_zhiyong",
		"--revision", "0123456789abcdef0123456789abcdef01234567",
		"-f", "deploy/workflows/test.yaml",
	}, func(ReleaseRequest) (ReleaseResult, error) {
		t.Fatal("local workflow file must not be submitted")
		return ReleaseResult{}, nil
	})
	if err == nil {
		t.Fatal("expected local workflow file rejection")
	}
}

func TestCICDSubmitRejectsNonAcceptedRemoteStatus(t *testing.T) {
	err := runCICDSubmitCommand(context.Background(), []string{
		"submit",
		"--target", "release-control-plane",
		"--repository-id", "repo_zhiyong",
		"--revision", "0123456789abcdef0123456789abcdef01234567",
		"--workflow", "deploy/workflows/test.yaml",
		"--allow-mutate",
	}, func(ReleaseRequest) (ReleaseResult, error) {
		return ReleaseResult{
			ReleaseID: "release-20260712-0123456789ab",
			Status:    "Blocked",
		}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("expected non-accepted remote status rejection, got %v", err)
	}
}
