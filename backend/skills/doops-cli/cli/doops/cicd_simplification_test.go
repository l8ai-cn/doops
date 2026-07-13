package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMinimalTemplateBuildsResolvedPlan(t *testing.T) {
	templatePath := writeDeploymentFixture(t, `
artifactContract:
  type: image-set
  sourceRepository: https://example.test/demo.git
  sourceBranch: main
  services: [api]
  imageTagPattern: "^[0-9a-f]{40}$"
  imageReferenceFormat: repository@digest
verificationProfiles:
  production:
    requiredEvidence: [source-identity, runtime-state]
environments:
  test:
    target:
      name: gw-test
      cluster: cluster-test
      instance: instance-test
    executor:
      type: helm
      config:
        namespace: test
        release: demo
        chart: deploy/chart
        values: deploy/values.yaml
        registry: registry.example.test/demo
        releaseManifestRepository: registry.example.test/demo/releases
        imageBindings:
          api: api
    verificationProfile: production
`, `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: demo
spec:
  parameters:
    releaseId:
      required: true
  application: demo
  release:
    source:
      repository: https://example.test/demo.git
      revision: ${inputs.releaseId}
      branch: main
  environment: test
  configurationSource: deploy/environments.yaml
`)

	template, err := loadDeploymentTemplate(templatePath)
	if err != nil {
		t.Fatalf("load template: %v", err)
	}
	plan, err := buildDeploymentPlan(template, map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("encode plan: %v", err)
	}
	text := string(encoded)
	for _, required := range []string{
		`"apiVersion":"doops.sh/v2"`,
		`"executionTarget":"gw-test"`,
		`"verificationProfile":"production"`,
		`"requiredEvidence":["runtime-state","source-identity"]`,
		`"type":"helm"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("compiled plan must contain %s: %s", required, text)
		}
	}
	for _, removed := range []string{
		`"policy"`,
		`"requiredFailureEvidence"`,
		`"delivery"`,
		`"authorization"`,
		`"configurationSource"`,
	} {
		if strings.Contains(text, removed) {
			t.Fatalf("compiled plan must not contain removed field %s: %s", removed, text)
		}
	}
	if plan.Digest == "" || !strings.HasPrefix(plan.Digest, "sha256:") {
		t.Fatalf("compiled plan must have a canonical digest: %#v", plan)
	}
}

func TestTemplateUsesStrictSchemaInsteadOfRecursiveBlacklist(t *testing.T) {
	templatePath := writeDeploymentFixture(t, `
artifactContract:
  type: manifest
verificationProfiles:
  production:
    requiredEvidence: [runtime-state]
environments:
  test:
    target:
      name: gw-test
      cluster: cluster-test
      instance: instance-test
    executor:
      type: helm
      config:
        namespace: test
        release: demo
        chart: deploy/chart
        values: deploy/values.yaml
    verificationProfile: production
`, `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: invalid
spec:
  application: demo
  release:
    manifest:
      repository: registry.example.test/releases
      reference: demo
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  environment: test
  policy:
    mutation: require-explicit-approval
`)

	_, err := loadDeploymentTemplate(templatePath)
	if err == nil || !strings.Contains(err.Error(), "field policy") {
		t.Fatalf("strict schema must report the unknown field path, got %v", err)
	}
}

func TestTemplateRejectsMultipleYAMLDocuments(t *testing.T) {
	templatePath := writeDeploymentFixture(t, `
artifactContract:
  type: manifest
verificationProfiles:
  production:
    requiredEvidence: [runtime-state]
environments:
  test:
    target:
      name: gw-test
      cluster: cluster-test
      instance: instance-test
    executor:
      type: helm
      config:
        namespace: test
        release: demo
        chart: deploy/chart
        values: deploy/values.yaml
    verificationProfile: production
`, `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: demo
spec:
  application: demo
  release:
    manifest:
      repository: registry.example.test/releases
      reference: demo
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  environment: test
---
unexpected: second-document
`)

	_, err := loadDeploymentTemplate(templatePath)
	if err == nil || !strings.Contains(err.Error(), "multiple YAML documents") {
		t.Fatalf("template must reject additional YAML documents, got %v", err)
	}
}

func TestGenericCompilerDoesNotSpecialCaseDoopsAgent(t *testing.T) {
	templatePath := writeDeploymentFixture(t, `
artifactContract:
  type: manifest
verificationProfiles:
  production:
    requiredEvidence: [runtime-state]
environments:
  test:
    target:
      name: gw-test
      cluster: cluster-test
      instance: instance-test
    executor:
      type: helm
      config:
        namespace: test
        release: doops-agent
        chart: deploy/chart
        values: deploy/values.yaml
    verificationProfile: production
`, `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: doops-agent
spec:
  application: doops-agent
  release:
    manifest:
      repository: registry.example.test/releases
      reference: doops-agent
      digest: sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
  environment: test
  configurationSource: deploy/environments.yaml
`)

	template, err := loadDeploymentTemplate(templatePath)
	if err != nil {
		t.Fatalf("load template: %v", err)
	}
	if _, err := buildDeploymentPlan(template, nil); err != nil {
		t.Fatalf("generic compiler must not require doops-agent model settings: %v", err)
	}
}

func TestRejectsArtifactSourceMismatch(t *testing.T) {
	templatePath := writeDeploymentFixture(t, `
artifactContract:
  type: image-set
  sourceRepository: https://example.test/expected.git
  sourceBranch: main
  services: [api]
  imageTagPattern: "^[0-9a-f]{40}$"
  imageReferenceFormat: repository@digest
verificationProfiles:
  production:
    requiredEvidence: [source-identity]
environments:
  test:
    target:
      name: gw-test
      cluster: cluster-test
      instance: instance-test
    executor:
      type: helm
      config:
        namespace: test
        release: demo
        chart: deploy/chart
        values: deploy/values.yaml
        registry: registry.example.test/demo
        releaseManifestRepository: registry.example.test/demo/releases
        imageBindings:
          api: api
    verificationProfile: production
`, `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: demo
spec:
  application: demo
  release:
    source:
      repository: https://example.test/other.git
      revision: 0123456789abcdef0123456789abcdef01234567
      branch: main
  environment: test
  configurationSource: deploy/environments.yaml
`)

	template, err := loadDeploymentTemplate(templatePath)
	if err != nil {
		t.Fatalf("load template: %v", err)
	}
	_, err = buildDeploymentPlan(template, nil)
	if err == nil || !strings.Contains(err.Error(), "source repository") {
		t.Fatalf("artifact/source mismatch must be rejected, got %v", err)
	}
}

func TestBlockedResultRequiresObservedFailureButNotRollback(t *testing.T) {
	workspaceCommit := strings.Repeat("a", 40)
	plan := DeploymentPlan{
		Digest: "sha256:expected-plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{
				RequiredEvidence: []string{"runtime-state"},
			},
		},
	}
	result := ReconciliationResult{
		APIVersion: deploymentAPIVersion,
		Kind:       reconciliationResultKind,
		PlanDigest: plan.Digest,
		Status:     ReconciliationBlocked,
		Attempts:   1,
		FailureEvidence: []ReconciliationEvidence{{
			Kind:       "access-denied",
			Subject:    "cluster",
			ObservedAt: "2026-07-13T00:00:00Z",
			Value:      "deployment credentials are unavailable",
		}},
	}
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err != nil {
		t.Fatalf("pre-mutation blocked result must not fabricate rollback evidence: %v", err)
	}
}

func TestResultDoesNotTreatAgentReportedAttemptBoundsAsHostPolicy(t *testing.T) {
	workspaceCommit := strings.Repeat("a", 40)
	plan := DeploymentPlan{
		Digest: "sha256:expected-plan",
		Spec: DeploymentPlanSpec{
			Acceptance: CICDAcceptance{
				RequiredEvidence: []string{"runtime-state"},
			},
		},
	}
	result := ReconciliationResult{
		APIVersion:         deploymentAPIVersion,
		Kind:               reconciliationResultKind,
		PlanDigest:         plan.Digest,
		Status:             ReconciliationConverged,
		Attempts:           12,
		NoProgressAttempts: 4,
		Evidence: []ReconciliationEvidence{{
			Kind:       "runtime-state",
			Subject:    "demo",
			ObservedAt: "2026-07-13T00:00:00Z",
			Value:      "verified",
		}},
	}
	attestReconciliationResultForTest(plan, workspaceCommit, &result)

	if err := validateReconciliationResult(plan, workspaceCommit, result); err != nil {
		t.Fatalf("result validation must not pretend agent-reported bounds are host enforced: %v", err)
	}
}

func TestCICDSourceWorkspaceRequiresDeclaredCleanCommit(t *testing.T) {
	root := t.TempDir()
	runGitForTest(t, root, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("release\n"), 0o644); err != nil {
		t.Fatalf("write release file: %v", err)
	}
	runGitForTest(t, root, "add", "app.txt")
	runGitForTest(t, root, "-c", "user.name=test", "-c", "user.email=test@example.test", "commit", "-m", "release")
	revision := strings.TrimSpace(runGitForTest(t, root, "rev-parse", "HEAD"))
	plan := DeploymentPlan{Spec: DeploymentPlanSpec{
		Release: CICDReleaseReference{Source: &CICDSourceRelease{Revision: revision}},
	}}

	if err := validateCICDSourceWorkspace(plan, root); err != nil {
		t.Fatalf("clean declared source commit must be accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatalf("modify release file: %v", err)
	}
	if err := validateCICDSourceWorkspace(plan, root); err == nil || !strings.Contains(err.Error(), "uncommitted") {
		t.Fatalf("dirty workspace must be rejected, got %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "app.txt"), []byte("release\n"), 0o644); err != nil {
		t.Fatalf("restore release file: %v", err)
	}
	plan.Spec.Release.Source.Revision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := validateCICDSourceWorkspace(plan, root); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched source commit must be rejected, got %v", err)
	}
}

func writeDeploymentFixture(t *testing.T, registry, template string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git marker: %v", err)
	}
	deployDir := filepath.Join(root, "deploy")
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		t.Fatalf("create deploy directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deployDir, "environments.yaml"), []byte(registry), 0o644); err != nil {
		t.Fatalf("write environment registry: %v", err)
	}
	templatePath := filepath.Join(deployDir, "deployment.yaml")
	if err := os.WriteFile(templatePath, []byte(template), 0o644); err != nil {
		t.Fatalf("write deployment template: %v", err)
	}
	return templatePath
}

func runGitForTest(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}
