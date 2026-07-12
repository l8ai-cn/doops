package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentTemplateCompilesImmutablePlanFromEnvironmentProfile(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "template.yaml")
	if err := os.WriteFile(templatePath, []byte(`
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: zhiyong-test
spec:
  parameters:
    releaseId:
      required: true
    repo:
      default: https://example.test/zhiyong.git
  plan:
    release:
      source:
        repository: ${inputs.repo}
        revision: ${inputs.releaseId}
        branch: main
    target:
      environment: test
    desiredState:
      application: zhiyong
      delivery: build-immutable-release
      configurationSource: deploy/environments.yaml
      authorization: reconcile
    acceptance:
      requiredEvidence:
        - source-identity
        - image-set
        - runtime-state
      requiredFailureEvidence:
        - rollout-status
        - rollback-state
    policy:
      mutation: require-explicit-approval
      convergence: until-verified
      failureMode: restore-last-known-good
`), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	template, err := loadDeploymentTemplate(templatePath)
	if err != nil {
		t.Fatalf("load template: %v", err)
	}
	plan, err := compileDeploymentPlan(template, map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	}, CICDEnvironmentRegistry{
		ArtifactContract: CICDArtifactContract{
			SourceRepository:     "https://example.test/zhiyong.git",
			SourceBranch:         "main",
			Services:             []string{"zhiyong-exam-api"},
			ImageTagPattern:      "^release-[0-9]{8}-[0-9a-f]{12}$",
			ImageReferenceFormat: "repository@digest",
			HelmImageBindings:    map[string]string{"zhiyong-exam-api": "examApi"},
			ManifestRepository:   "registry.example.test/releases",
		},
		Environments: map[string]CICDEnvironmentProfile{
			"test": {
				Target:         "gw-oilan-node",
				Cluster:        "doops-oilan",
				Instance:       "oilan-node",
				Namespace:      "test",
				Release:        "zhiyong",
				Registry:       "registry.example.test/oilan-system",
				Chart:          "deploy/environments/test/chart",
				Values:         "deploy/environments/test/chart/values.yaml",
				RuntimeFiles:   "deploy/environments/test/chart/files",
				DeploymentMode: "application",
				HealthChecks: CICDHealthChecks{
					Public: []CICDPublicHealthCheck{{
						ID:             "frontend-health",
						URL:            "https://study.example.test/healthz",
						ExpectedStatus: 200,
					}},
					Workloads: []CICDWorkloadHealthCheck{{
						Service:          "zhiyong-exam-api",
						MinReadyReplicas: 1,
						RequireEndpoints: true,
					}},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}

	if plan.Spec.Release.Source == nil {
		t.Fatalf("expected source release, got %#v", plan.Spec.Release)
	}
	if plan.Spec.Release.Source.Revision != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("release revision mismatch: %#v", plan.Spec.Release.Source)
	}
	if plan.Spec.Target.Environment != "test" || plan.Spec.Target.ExecutionTarget != "gw-oilan-node" {
		t.Fatalf("target was not resolved from environment profile: %#v", plan.Spec.Target)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("encode plan: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode plan: %v", err)
	}
	spec, _ := payload["spec"].(map[string]interface{})
	target, _ := spec["target"].(map[string]interface{})
	profile, _ := target["profile"].(map[string]interface{})
	if profile["target"] != "gw-oilan-node" {
		t.Fatalf("plan must carry the resolved environment profile, got %#v", target)
	}
	if plan.Digest == "" || !strings.HasPrefix(plan.Digest, "sha256:") {
		t.Fatalf("expected immutable plan digest, got %q", plan.Digest)
	}
}

func TestBuildDeploymentPlanReadsEnvironmentRegistryFromRepositoryRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git marker: %v", err)
	}
	templateDir := filepath.Join(root, "deploy", "workflows")
	if err := os.MkdirAll(templateDir, 0o755); err != nil {
		t.Fatalf("create template directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "deploy", "environments.yaml"), []byte(`
artifactContract:
  sourceRepository: https://example.test/zhiyong.git
  sourceBranch: main
  services: [zhiyong-exam-api]
  imageTagPattern: ^release-[0-9]{8}-[0-9a-f]{12}$
  imageReferenceFormat: repository@digest
  helmImageBindings:
    zhiyong-exam-api: examApi
  manifestRepository: registry.example.test/releases
environments:
  oilan:
    target: gw-edu-coder
    cluster: doops-edu
    instance: edu-coder
    namespace: oilan
    release: zhiyong
    registry: registry.example.test/oilan-system
    chart: deploy/environments/oilan/chart
    values: deploy/environments/oilan/chart/values.yaml
    runtimeFiles: deploy/environments/oilan/chart/files
    deploymentMode: application
    healthChecks:
      public:
        - id: frontend-health
          url: https://zy.example.test/healthz
          expectedStatus: 200
      workloads:
        - service: zhiyong-exam-api
          minReadyReplicas: 1
          requireEndpoints: true
`), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	templatePath := filepath.Join(templateDir, "promote.yaml")
	if err := os.WriteFile(templatePath, []byte(`
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: promote
spec:
  parameters:
    releaseId:
      required: true
    manifestDigest:
      required: true
  plan:
    release:
      manifest:
        repository: registry.example.test/releases
        reference: ${inputs.releaseId}
        digest: ${inputs.manifestDigest}
    target:
      environment: oilan
    desiredState:
      application: zhiyong
      delivery: promote-immutable-release
      configurationSource: deploy/environments.yaml
      authorization: reconcile
    acceptance:
      requiredEvidence: [release-manifest, runtime-state]
      requiredFailureEvidence: [rollout-status, rollback-state]
    policy:
      mutation: require-explicit-approval
      convergence: until-verified
      failureMode: restore-last-known-good
`), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	template, err := loadDeploymentTemplate(templatePath)
	if err != nil {
		t.Fatalf("load template: %v", err)
	}
	plan, err := buildDeploymentPlan(template, map[string]string{
		"releaseId":      "release-20260712-0123456789ab",
		"manifestDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatalf("build deployment plan: %v", err)
	}
	if plan.Spec.Target.ExecutionTarget != "gw-edu-coder" {
		t.Fatalf("environment target mismatch: %#v", plan.Spec.Target)
	}
}

func TestDeploymentTemplateRejectsLegacyStagesAndCommands(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "legacy workflow",
			body: `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: old
spec:
  stages: []
`,
			want: "unsupported apiVersion",
		},
		{
			name: "stage field",
			body: `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: invalid
spec:
  stages: []
  plan:
    release:
      source:
        repository: https://example.test/repo.git
        revision: abc
    target:
      environment: test
    desiredState:
      application: demo
      delivery: immutable
      configurationSource: deploy/environments.yaml
      authorization: reconcile
    acceptance:
      requiredEvidence: [source-identity]
    policy:
      mutation: require-explicit-approval
      convergence: until-verified
`,
			want: "forbidden command-driven field",
		},
		{
			name: "command field",
			body: `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: invalid
spec:
  plan:
    release:
      source:
        repository: https://example.test/repo.git
        revision: abc
    target:
      environment: test
    desiredState:
      application: demo
      delivery: immutable
      configurationSource: deploy/environments.yaml
      authorization: reconcile
    acceptance:
      requiredEvidence: [source-identity]
    policy:
      mutation: require-explicit-approval
      convergence: until-verified
    run: helm upgrade
`,
			want: "forbidden command-driven field",
		},
		{
			name: "physical route field",
			body: `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: invalid
spec:
  plan:
    release:
      source:
        repository: https://example.test/repo.git
        revision: abc
    target:
      environment: test
      namespace: test
    desiredState:
      application: demo
      delivery: immutable
      configurationSource: deploy/environments.yaml
      authorization: reconcile
    acceptance:
      requiredEvidence: [source-identity]
    policy:
      mutation: require-explicit-approval
      convergence: until-verified
`,
			want: "forbidden command-driven field",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".yaml")
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("write template: %v", err)
			}
			_, err := loadDeploymentTemplate(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestDeploymentPlanRejectsMutableReleaseReferences(t *testing.T) {
	if err := validateCICDSourceRelease(CICDSourceRelease{
		Repository: "https://example.test/repo.git",
		Revision:   "main",
	}, false); err == nil {
		t.Fatal("compiled deployment plan must reject a mutable source revision")
	}
	if err := validateCICDManifestRelease(CICDManifestRelease{
		Repository: "registry.example.test/releases",
		Reference:  "release-20260712-0123456789ab",
	}, false); err == nil {
		t.Fatal("compiled deployment plan must reject a manifest without an OCI digest")
	}
}

func TestDeploymentPlanRequiresTargetGatewayBinding(t *testing.T) {
	err := validateCICDEnvironmentProfile("test", CICDEnvironmentProfile{
		Target:         "gw-oilan-node",
		Namespace:      "test",
		Release:        "zhiyong",
		Registry:       "registry.example.test/oilan-system",
		Chart:          "deploy/environments/test/chart",
		Values:         "deploy/environments/test/chart/values.yaml",
		RuntimeFiles:   "deploy/environments/test/chart/files",
		DeploymentMode: "application",
		HealthChecks: CICDHealthChecks{
			Public: []CICDPublicHealthCheck{{
				ID:             "frontend-health",
				URL:            "https://study.example.test/healthz",
				ExpectedStatus: 200,
			}},
			Workloads: []CICDWorkloadHealthCheck{{
				Service:          "zhiyong-exam-api",
				MinReadyReplicas: 1,
				RequireEndpoints: true,
			}},
		},
	})
	if err == nil {
		t.Fatal("environment profile without cluster and instance binding must be rejected")
	}
}

func TestDeploymentPlanRejectsMismatchedConfiguredGatewayTarget(t *testing.T) {
	plan := DeploymentPlan{
		APIVersion: deploymentAPIVersion,
		Kind:       deploymentPlanKind,
		Spec: DeploymentPlanSpec{
			Release: CICDReleaseReference{Source: &CICDSourceRelease{
				Repository: "https://example.test/repo.git",
				Revision:   "0123456789abcdef0123456789abcdef01234567",
			}},
			Target: CICDDeploymentTarget{
				Environment:     "test",
				ExecutionTarget: "gw-oilan-node",
				Profile: &CICDEnvironmentProfile{
					Target:         "gw-oilan-node",
					Cluster:        "doops-oilan",
					Instance:       "oilan-node",
					Namespace:      "test",
					Release:        "zhiyong",
					Registry:       "registry.example.test/oilan-system",
					Chart:          "deploy/environments/test/chart",
					Values:         "deploy/environments/test/chart/values.yaml",
					RuntimeFiles:   "deploy/environments/test/chart/files",
					DeploymentMode: "application",
					HealthChecks: CICDHealthChecks{
						Public: []CICDPublicHealthCheck{{
							ID:             "frontend-health",
							URL:            "https://study.example.test/healthz",
							ExpectedStatus: 200,
						}},
						Workloads: []CICDWorkloadHealthCheck{{
							Service:          "zhiyong-exam-api",
							MinReadyReplicas: 1,
							RequireEndpoints: true,
						}},
					},
				},
			},
			ArtifactContract: CICDArtifactContract{
				SourceRepository:     "https://example.test/repo.git",
				SourceBranch:         "main",
				Services:             []string{"zhiyong-exam-api"},
				ImageTagPattern:      "^release-[0-9]{8}-[0-9a-f]{12}$",
				ImageReferenceFormat: "repository@digest",
				HelmImageBindings:    map[string]string{"zhiyong-exam-api": "examApi"},
				ManifestRepository:   "registry.example.test/releases",
			},
			DesiredState: CICDDesiredState{
				Application:         "zhiyong",
				Delivery:            "build-immutable-release",
				ConfigurationSource: deploymentConfiguration,
				Authorization:       "reconcile",
			},
			Acceptance: CICDAcceptance{
				RequiredEvidence:        []string{"source-identity"},
				RequiredFailureEvidence: []string{"rollback-state"},
			},
			Policy: CICDReconcilePolicy{
				Mutation:    deploymentMutationGate,
				Convergence: deploymentConvergence,
				FailureMode: deploymentFailureMode,
			},
		},
	}
	profileDigest, err := digestDeploymentValue(*plan.Spec.Target.Profile)
	if err != nil {
		t.Fatalf("digest profile: %v", err)
	}
	plan.Spec.Target.ProfileDigest = profileDigest
	plan.Digest, err = digestDeploymentPlan(plan)
	if err != nil {
		t.Fatalf("digest plan: %v", err)
	}
	err = validateCICDServerBinding(Server{
		Name:     "gw-oilan-node",
		Gateway:  "https://gateway.example.test",
		Cluster:  "doops-other",
		Instance: "oilan-node",
	}, plan)
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("expected target binding rejection, got %v", err)
	}
}

func TestReconcileEvaluationRequiresStructuredEvidence(t *testing.T) {
	plan := DeploymentPlan{
		Digest: "sha256:plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{
				RequiredEvidence: []string{"source-identity", "runtime-state"},
			},
		},
	}

	state, err := evaluateDeploymentReconcile(plan, CICDReconcileResult{
		PlanDigest: "sha256:plan",
		Status:     CICDReconcileReconciling,
		Evidence: []CICDEvidence{
			{Kind: "source-identity", Reference: "sha256:tree"},
		},
	})
	if err != nil {
		t.Fatalf("evaluate partial result: %v", err)
	}
	if state != CICDReconcileReconciling {
		t.Fatalf("expected reconciling while evidence is incomplete, got %q", state)
	}

	state, err = evaluateDeploymentReconcile(plan, CICDReconcileResult{
		PlanDigest: "sha256:plan",
		Status:     CICDReconcileReconciling,
		Evidence: []CICDEvidence{
			{Kind: "source-identity", Reference: "sha256:tree"},
			{Kind: "runtime-state", Reference: "helm:42"},
		},
	})
	if err != nil {
		t.Fatalf("evaluate complete result: %v", err)
	}
	if state != CICDReconcileConverged {
		t.Fatalf("expected converged after all evidence is present, got %q", state)
	}
}

func TestDeploymentReconcilerContinuesUntilEvidenceConverges(t *testing.T) {
	plan := DeploymentPlan{
		Digest: "sha256:plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{
				RequiredEvidence: []string{"source-identity", "runtime-state"},
			},
		},
	}
	executor := &sequenceDeploymentReconciler{results: []CICDReconcileResult{
		{
			PlanDigest: "sha256:plan",
			Status:     CICDReconcileReconciling,
			Evidence:   []CICDEvidence{{Kind: "source-identity", Reference: "sha256:tree"}},
		},
		{
			PlanDigest: "sha256:plan",
			Status:     CICDReconcileReconciling,
			Evidence: []CICDEvidence{
				{Kind: "source-identity", Reference: "sha256:tree"},
				{Kind: "runtime-state", Reference: "helm:42"},
			},
		},
	}}

	run, err := reconcileDeploymentPlan(t.Context(), plan, executor, DeploymentReconcileOptions{
		MaxIterations:     3,
		MaxNoProgressRuns: 2,
	})
	if err != nil {
		t.Fatalf("reconcile plan: %v", err)
	}
	if run.State != CICDReconcileConverged || run.Iterations != 2 {
		t.Fatalf("expected convergence on second iteration, got %#v", run)
	}
}

func TestDeploymentReconcilerBlocksAfterNoProgress(t *testing.T) {
	plan := DeploymentPlan{
		Digest: "sha256:plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{RequiredEvidence: []string{"runtime-state"}},
		},
	}
	result := CICDReconcileResult{
		PlanDigest: "sha256:plan",
		Status:     CICDReconcileReconciling,
		Evidence:   []CICDEvidence{{Kind: "source-identity", Reference: "sha256:tree"}},
	}
	executor := &sequenceDeploymentReconciler{results: []CICDReconcileResult{result, result, result}}

	run, err := reconcileDeploymentPlan(t.Context(), plan, executor, DeploymentReconcileOptions{
		MaxIterations:     5,
		MaxNoProgressRuns: 2,
	})
	if err == nil || !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("expected no-progress error, got run=%#v err=%v", run, err)
	}
	if run.State != CICDReconcileBlocked {
		t.Fatalf("expected blocked state, got %#v", run)
	}
}

type sequenceDeploymentReconciler struct {
	results []CICDReconcileResult
	calls   int
}

func (r *sequenceDeploymentReconciler) Reconcile(_ DeploymentPlan, _ DeploymentReconcileRequest) (CICDReconcileResult, error) {
	if r.calls >= len(r.results) {
		return CICDReconcileResult{}, os.ErrNotExist
	}
	result := r.results[r.calls]
	r.calls++
	return result, nil
}
