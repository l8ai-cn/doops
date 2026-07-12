package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCICDOperationGraphRequiresIndependentReleaseRoles(t *testing.T) {
	graph := CICDOperationGraph{
		Operations: []CICDOperation{
			{
				ID:     "source",
				Role:   CICDOperationRoleSourceVerifier,
				Target: CICDOperationTarget{Target: "source-verifier", Cluster: "ops", Instance: "source-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "source-identity", Immutable: true},
				},
			},
			{
				ID:        "build",
				Role:      CICDOperationRoleImageBuilder,
				DependsOn: []string{"source"},
				Target:    CICDOperationTarget{Target: "image-builder", Cluster: "ops", Instance: "builder-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "image-digest", Immutable: true},
				},
			},
			{
				ID:        "attest",
				Role:      CICDOperationRoleArtifactAttestor,
				DependsOn: []string{"build"},
				Target:    CICDOperationTarget{Target: "artifact-attestor", Cluster: "ops", Instance: "attestor-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "release-manifest", Immutable: true},
				},
			},
			{
				ID:        "deploy",
				Role:      CICDOperationRoleDeploymentExecutor,
				DependsOn: []string{"attest"},
				Target:    CICDOperationTarget{Target: "deployment-executor", Cluster: "doops-edu", Instance: "edu-coder"},
				Outputs: []CICDOperationOutput{
					{Kind: "runtime-state", Immutable: true},
				},
			},
			{
				ID:        "observe",
				Role:      CICDOperationRoleHealthObserver,
				DependsOn: []string{"deploy"},
				Target:    CICDOperationTarget{Target: "health-observer", Cluster: "ops", Instance: "observer-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "public-contract", Immutable: true},
					{Kind: "post-deploy-log-scan", Immutable: true},
				},
			},
			{
				ID:        "rollback",
				Role:      CICDOperationRoleRollbackController,
				DependsOn: []string{"deploy", "observe"},
				Target:    CICDOperationTarget{Target: "rollback-controller", Cluster: "doops-edu", Instance: "edu-coder"},
				Outputs: []CICDOperationOutput{
					{Kind: "rollback-state", Immutable: true},
				},
			},
		},
	}

	if err := validateCICDOperationGraph(graph); err != nil {
		t.Fatalf("validate complete multi-Ops graph: %v", err)
	}
}

func TestCICDOperationGraphRejectsMissingRollbackController(t *testing.T) {
	graph := CICDOperationGraph{
		Operations: []CICDOperation{
			{
				ID:     "source",
				Role:   CICDOperationRoleSourceVerifier,
				Target: CICDOperationTarget{Target: "source-verifier", Cluster: "ops", Instance: "source-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "source-identity", Immutable: true},
				},
			},
		},
	}

	err := validateCICDOperationGraph(graph)
	if err == nil || !strings.Contains(err.Error(), "rollback-controller") {
		t.Fatalf("expected missing rollback-controller rejection, got %v", err)
	}
}

func TestCICDOperationGraphRejectsBuilderWithoutImmutableDigestOutput(t *testing.T) {
	graph := CICDOperationGraph{
		Operations: []CICDOperation{
			{
				ID:     "source",
				Role:   CICDOperationRoleSourceVerifier,
				Target: CICDOperationTarget{Target: "source-verifier", Cluster: "ops", Instance: "source-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "source-identity", Immutable: true},
				},
			},
			{
				ID:        "build",
				Role:      CICDOperationRoleImageBuilder,
				DependsOn: []string{"source"},
				Target:    CICDOperationTarget{Target: "image-builder", Cluster: "ops", Instance: "builder-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "image-tag", Immutable: false},
				},
			},
			{
				ID:        "attest",
				Role:      CICDOperationRoleArtifactAttestor,
				DependsOn: []string{"build"},
				Target:    CICDOperationTarget{Target: "artifact-attestor", Cluster: "ops", Instance: "attestor-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "release-manifest", Immutable: true},
				},
			},
			{
				ID:        "deploy",
				Role:      CICDOperationRoleDeploymentExecutor,
				DependsOn: []string{"attest"},
				Target:    CICDOperationTarget{Target: "deployment-executor", Cluster: "doops-edu", Instance: "edu-coder"},
				Outputs: []CICDOperationOutput{
					{Kind: "runtime-state", Immutable: true},
				},
			},
			{
				ID:        "observe",
				Role:      CICDOperationRoleHealthObserver,
				DependsOn: []string{"deploy"},
				Target:    CICDOperationTarget{Target: "health-observer", Cluster: "ops", Instance: "observer-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "public-contract", Immutable: true},
					{Kind: "post-deploy-log-scan", Immutable: true},
				},
			},
			{
				ID:        "rollback",
				Role:      CICDOperationRoleRollbackController,
				DependsOn: []string{"deploy", "observe"},
				Target:    CICDOperationTarget{Target: "rollback-controller", Cluster: "ops", Instance: "rollback-1"},
				Outputs: []CICDOperationOutput{
					{Kind: "rollback-state", Immutable: true},
				},
			},
		},
	}

	err := validateCICDOperationGraph(graph)
	if err == nil || !strings.Contains(err.Error(), "image-digest") {
		t.Fatalf("expected image digest output rejection, got %v", err)
	}
}

func TestParseCICDOperationGraphRejectsCommandFields(t *testing.T) {
	raw, err := json.Marshal(map[string]interface{}{
		"operations": []map[string]interface{}{
			{
				"id":   "source",
				"role": "source-verifier",
				"target": map[string]string{
					"target":   "source-verifier",
					"cluster":  "ops",
					"instance": "source-1",
				},
				"run": "git clone",
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal graph: %v", err)
	}

	_, err = parseCICDOperationGraph(raw)
	if err == nil || !strings.Contains(err.Error(), "forbidden command-driven field") {
		t.Fatalf("expected command field rejection, got %v", err)
	}
}
