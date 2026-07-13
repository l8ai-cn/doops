package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeploymentTemplateCompilesImmutablePlanFromEnvironmentProfile(t *testing.T) {
	template := validDeploymentTemplate()
	registry := validEnvironmentRegistry()

	plan, err := compileDeploymentPlan(template, map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	}, registry)
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
	if plan.Spec.Target.Profile.Executor.Config.ReleaseManifestRepository != "registry.example.test/oilan-system/zhiyong-release-manifest-test" {
		t.Fatalf("plan must retain the environment-owned manifest repository: %#v", plan.Spec.Target.Profile)
	}
	if plan.Spec.Acceptance.VerificationProfile != "production" {
		t.Fatalf("verification profile was not resolved: %#v", plan.Spec.Acceptance)
	}
	if len(plan.Spec.Acceptance.RequiredEvidence) != 3 {
		t.Fatalf("verification evidence was not compiled: %#v", plan.Spec.Acceptance)
	}
	if plan.Digest == "" || !strings.HasPrefix(plan.Digest, "sha256:") {
		t.Fatalf("expected immutable plan digest, got %q", plan.Digest)
	}

	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatalf("encode plan: %v", err)
	}
	for _, removed := range []string{"policy", "requiredFailureEvidence", "delivery", "authorization", "configurationSource"} {
		if strings.Contains(string(encoded), `"`+removed+`"`) {
			t.Fatalf("compiled plan must not contain removed field %q: %s", removed, encoded)
		}
	}
}

func TestBuildDeploymentPlanUsesDefaultRepositoryConfiguration(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git marker: %v", err)
	}
	deployDir := filepath.Join(root, "deploy")
	if err := os.MkdirAll(deployDir, 0o755); err != nil {
		t.Fatalf("create deploy directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(deployDir, "environments.yaml"), []byte(validRegistryYAML()), 0o644); err != nil {
		t.Fatalf("write registry: %v", err)
	}
	templatePath := filepath.Join(deployDir, "deployment.yaml")
	if err := os.WriteFile(templatePath, []byte(`
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: test
spec:
  parameters:
    releaseId:
      required: true
  application: zhiyong
  release:
    source:
      repository: https://example.test/zhiyong.git
      revision: ${inputs.releaseId}
      branch: main
  environment: test
`), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	template, err := loadDeploymentTemplate(templatePath)
	if err != nil {
		t.Fatalf("load template: %v", err)
	}
	plan, err := buildDeploymentPlan(template, map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("build deployment plan: %v", err)
	}
	if plan.Spec.Target.ExecutionTarget != "gw-oilan-node" {
		t.Fatalf("environment target mismatch: %#v", plan.Spec.Target)
	}
}

func TestImageSetVersionReleaseAllowsRepeatedDailyDeployment(t *testing.T) {
	templatePath := writeDeploymentFixture(t, `
artifactContract:
  type: image-set
  sourceRegistry: docker.example.test/demo
  services: [api]
  imageTagPattern: "^release-[0-9]{8}$"
  imageTagTimeZone: Asia/Shanghai
verificationProfiles:
  production:
    requiredEvidence: [runtime-state, public-http]
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
        imageBindings:
          api: api
    verificationProfile: production
`, `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: demo-test
spec:
  parameters:
    version:
      required: true
    reason:
      required: true
  application: demo
  release:
    version: ${inputs.version}
  environment: test
  configurationSource: deploy/environments.yaml
`)

	template, err := loadDeploymentTemplate(templatePath)
	if err != nil {
		t.Fatalf("load version deployment template: %v", err)
	}
	first, err := buildDeploymentPlan(template, map[string]string{
		"version": "release-20260713",
		"reason":  "initial deployment",
	})
	if err != nil {
		t.Fatalf("compile first version deployment: %v", err)
	}
	second, err := buildDeploymentPlan(template, map[string]string{
		"version": "release-20260713",
		"reason":  "repeat deployment",
	})
	if err != nil {
		t.Fatalf("compile repeated version deployment: %v", err)
	}
	for _, plan := range []DeploymentPlan{first, second} {
		encoded, err := json.Marshal(plan.Spec.Release)
		if err != nil {
			t.Fatalf("encode version release: %v", err)
		}
		if !strings.Contains(string(encoded), `"version":"release-20260713"`) {
			t.Fatalf("plan must preserve the requested release version: %s", encoded)
		}
	}
	_, err = buildDeploymentPlan(template, map[string]string{
		"version": "release-not-a-date",
		"reason":  "invalid version",
	})
	if err == nil || !strings.Contains(err.Error(), "image tag pattern") {
		t.Fatalf("version outside image tag pattern must be rejected, got %v", err)
	}
}

func TestEnvironmentRegistryPreservesOperationalConfiguration(t *testing.T) {
	registryPath := filepath.Join(t.TempDir(), "environments.yaml")
	if err := os.WriteFile(registryPath, []byte(`
apiVersion: doops.sh/v2
kind: EnvironmentRegistry
metadata:
  name: demo-environments
  updatedAt: "2026-07-13"
identityRules:
  - environment names do not identify physical targets
artifactContract:
  type: image-set
  sourceRegistry: docker.example.test/demo
  services: [api]
  imageTagPattern: "^release-[0-9]{8}$"
  imageTagTimeZone: Asia/Shanghai
verificationProfiles:
  production:
    requiredEvidence: [runtime-state]
environments:
  oilan:
    displayName: Oilan
    target:
      name: gw-oilan
      cluster: doops-edu
      instance: edu-coder
    executor:
      type: helm
      config:
        namespace: oilan
        release: demo
        chart: deploy/chart
        values: deploy/environments/oilan/values.yaml
        registry: registry.example.test/demo
        imageBindings:
          api: api
        deploymentMode: application
        registryCredential:
          namespace: oilan
          secretName: regcred
          key: .dockerconfigjson
        workloadNamespaces:
          api: oilan
        artifactSync:
          sourceCredentialRef:
            kind: KubernetesSecret
            name: cnb-regcred
        retiredResources:
          - apiVersion: networking.k8s.io/v1
            kind: Ingress
            name: legacy-demo
            deletionPolicy: require-unmanaged-exact-match
            expected:
              manager: kubectl-client-side-apply
              ingressClassName: oilan-edge
              hosts: [demo.oilan.ai]
    verificationProfile: production
  ali:
    displayName: Ali consumer
    target:
      name: gw-ali
      cluster: doops-ali
      instance: master-1
    deploymentMode: external-control-plane
    controlPlaneEnvironment: oilan
    publicHosts: [ecp.example.test]
`), 0o644); err != nil {
		t.Fatalf("write environment registry: %v", err)
	}

	registry, err := loadCICDEnvironmentRegistry(registryPath)
	if err != nil {
		t.Fatalf("load operational environment registry: %v", err)
	}
	if registry.Metadata.Name != "demo-environments" || len(registry.IdentityRules) != 1 {
		t.Fatalf("registry metadata must be retained: %#v", registry)
	}
	if registry.ArtifactContract.ImageTagTimeZone != "Asia/Shanghai" {
		t.Fatalf("registry version time zone was lost: %#v", registry.ArtifactContract)
	}
	oilan := registry.Environments["oilan"]
	if oilan.Executor.Config.DeploymentMode != "application" {
		t.Fatalf("executor deployment mode was lost: %#v", oilan.Executor.Config)
	}
	if oilan.Executor.Config.RegistryCredential.SecretName != "regcred" {
		t.Fatalf("registry credential was lost: %#v", oilan.Executor.Config.RegistryCredential)
	}
	if len(oilan.Executor.Config.RetiredResources) != 1 {
		t.Fatalf("retired resource cleanup declaration was lost: %#v", oilan.Executor.Config.RetiredResources)
	}
	ali := registry.Environments["ali"]
	if ali.DeploymentMode != "external-control-plane" || ali.ControlPlaneEnvironment != "oilan" {
		t.Fatalf("external control-plane consumer declaration was lost: %#v", ali)
	}
}

func TestDeploymentPlanRejectsMutableAndUnresolvedReleaseReferences(t *testing.T) {
	if err := validateCICDSourceRelease(CICDSourceRelease{
		Repository: "https://example.test/repo.git",
		Revision:   "main",
	}, false); err == nil {
		t.Fatal("compiled deployment plan must reject a mutable source revision")
	}
	if err := validateCICDSourceRelease(CICDSourceRelease{
		Repository: "${inputs.repo}",
		Revision:   "0123456789abcdef0123456789abcdef01234567",
	}, false); err == nil {
		t.Fatal("compiled deployment plan must reject unresolved repository inputs")
	}
	if err := validateCICDManifestRelease(CICDManifestRelease{
		Repository: "registry.example.test/releases",
		Reference:  "release",
	}, false); err == nil {
		t.Fatal("compiled deployment plan must reject a manifest without an OCI digest")
	}
	if err := validateDeploymentIntent(DeploymentTemplateSpec{
		Application: "${inputs.application}",
		Environment: "test",
		Release: CICDReleaseReference{Manifest: &CICDManifestRelease{
			Repository: "registry.example.test/releases",
			Reference:  "demo",
			Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		}},
	}, false); err == nil {
		t.Fatal("compiled deployment intent must reject unresolved application inputs")
	}
}

func TestDeploymentPlanRequiresTargetGatewayBinding(t *testing.T) {
	profile := validEnvironmentProfile()
	profile.Target.Cluster = ""

	err := validateCICDEnvironmentProfile("test", profile)
	if err == nil || !strings.Contains(err.Error(), "target.cluster") {
		t.Fatalf("environment profile without cluster binding must be rejected, got %v", err)
	}
}

func TestHelmExecutorRequiresOnlyCoreHelmCoordinates(t *testing.T) {
	profile := CICDEnvironmentProfile{
		Target: CICDEnvironmentTarget{
			Name:     "gw-test",
			Cluster:  "cluster-test",
			Instance: "instance-test",
		},
		Executor: CICDEnvironmentExecutor{
			Type: cicdExecutorHelm,
			Config: CICDHelmExecutorConfig{
				Namespace: "test",
				Release:   "demo",
				Chart:     "deploy/chart",
				Values:    "deploy/values.yaml",
			},
		},
		VerificationProfile: "production",
	}

	if err := validateCICDEnvironmentProfile("test", profile); err != nil {
		t.Fatalf("Helm profile must not require workload, registry, public checks, or application-specific settings: %v", err)
	}
}

func TestImageSetArtifactRequiresExecutorOwnedBindings(t *testing.T) {
	release := CICDReleaseReference{Source: &CICDSourceRelease{
		Repository: "https://example.test/zhiyong.git",
		Revision:   "0123456789abcdef0123456789abcdef01234567",
		Branch:     "main",
	}}
	artifact := validEnvironmentRegistry().ArtifactContract
	executor := validEnvironmentProfile().Executor
	delete(executor.Config.ImageBindings, "zhiyong-exam-api")

	err := validateCICDArtifactContract(release, artifact, executor)
	if err == nil || !strings.Contains(err.Error(), "image binding") {
		t.Fatalf("image-set Helm executor must bind every declared service, got %v", err)
	}
}

func TestManifestArtifactDoesNotRequireImageBuildFields(t *testing.T) {
	release := CICDReleaseReference{Manifest: &CICDManifestRelease{
		Repository: "registry.example.test/releases",
		Reference:  "demo",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	artifact := CICDArtifactContract{Type: cicdArtifactManifest}
	executor := CICDEnvironmentExecutor{
		Type: cicdExecutorHelm,
		Config: CICDHelmExecutorConfig{
			Namespace: "test",
			Release:   "demo",
			Chart:     "deploy/chart",
			Values:    "deploy/values.yaml",
		},
	}

	if err := validateCICDArtifactContract(release, artifact, executor); err != nil {
		t.Fatalf("manifest release must not require image build or Helm image binding fields: %v", err)
	}
}

func TestDeploymentConfigurationCannotEscapeRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git marker: %v", err)
	}
	template := validDeploymentTemplate()
	template.path = filepath.Join(root, "deploy", "deployment.yaml")

	_, err := resolveDeploymentConfigurationPath(template, "../outside.yaml")
	if err == nil || !strings.Contains(err.Error(), "inside repository root") {
		t.Fatalf("configuration source traversal must be rejected, got %v", err)
	}
}

func TestDeploymentConfigurationSymlinkCannotEscapeRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create git marker: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "environments.yaml")
	if err := os.WriteFile(outside, []byte("environments: {}"), 0o644); err != nil {
		t.Fatalf("write outside registry: %v", err)
	}
	link := filepath.Join(root, "deploy", "environments.yaml")
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatalf("create deploy directory: %v", err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	template := validDeploymentTemplate()
	template.path = filepath.Join(root, "deploy", "deployment.yaml")

	_, err := resolveDeploymentConfigurationPath(template, "deploy/environments.yaml")
	if err == nil || !strings.Contains(err.Error(), "inside repository root") {
		t.Fatalf("configuration symlink escape must be rejected, got %v", err)
	}
}

func TestDeploymentPlanRejectsMismatchedConfiguredGatewayTarget(t *testing.T) {
	plan, err := compileDeploymentPlan(validDeploymentTemplate(), map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	}, validEnvironmentRegistry())
	if err != nil {
		t.Fatalf("compile plan: %v", err)
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

func TestDeploymentPlanRejectsImplicitServerBindingDefaults(t *testing.T) {
	registry := validEnvironmentRegistry()
	profile := registry.Environments["test"]
	profile.Target.Cluster = "default"
	profile.Target.Instance = profile.Target.Name
	registry.Environments["test"] = profile
	plan, err := compileDeploymentPlan(validDeploymentTemplate(), map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	}, registry)
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}

	err = validateCICDServerBinding(Server{
		Name:    profile.Target.Name,
		Gateway: "https://gateway.example.test",
	}, plan)
	if err == nil || !strings.Contains(err.Error(), "explicit cluster and instance") {
		t.Fatalf("missing configured cluster/instance must be rejected, got %v", err)
	}
}

func TestImageSetArtifactRejectsInvalidTagPattern(t *testing.T) {
	registry := validEnvironmentRegistry()
	registry.ArtifactContract.ImageTagPattern = "["
	_, err := compileDeploymentPlan(validDeploymentTemplate(), map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	}, registry)
	if err == nil || !strings.Contains(err.Error(), "image tag pattern") {
		t.Fatalf("invalid image tag pattern must be rejected, got %v", err)
	}
}

func TestArtifactContractDoesNotGateImageSetByDigestFormat(t *testing.T) {
	registry := validEnvironmentRegistry()
	registry.ArtifactContract.ImageReferenceFormat = "repository:tag"
	if _, err := compileDeploymentPlan(validDeploymentTemplate(), map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	}, registry); err != nil {
		t.Fatalf("image reference format must not block deployment planning: %v", err)
	}

	manifest := CICDArtifactContract{
		Type:             cicdArtifactManifest,
		Services:         []string{"api"},
		ImageTagPattern:  ".*",
		SourceRepository: "https://example.test/repo.git",
	}
	release := CICDReleaseReference{Manifest: &CICDManifestRelease{
		Repository: "registry.example.test/releases",
		Reference:  "demo",
		Digest:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	if err := validateCICDArtifactContract(release, manifest, validEnvironmentProfile().Executor); err == nil || !strings.Contains(err.Error(), "must not contain image-set fields") {
		t.Fatalf("manifest artifact contract must reject image-set fields, got %v", err)
	}
}

func TestDeploymentPlanDigestRejectsProfileTampering(t *testing.T) {
	plan, err := compileDeploymentPlan(validDeploymentTemplate(), map[string]string{
		"releaseId": "0123456789abcdef0123456789abcdef01234567",
	}, validEnvironmentRegistry())
	if err != nil {
		t.Fatalf("compile plan: %v", err)
	}
	plan.Spec.Target.Profile.Executor.Config.Namespace = "other"

	if err := validateDeploymentPlan(plan); err == nil {
		t.Fatal("plan validation must reject resolved profile tampering")
	}
}

func validDeploymentTemplate() DeploymentTemplate {
	return DeploymentTemplate{
		APIVersion: deploymentAPIVersion,
		Kind:       deploymentTemplateKind,
		Metadata:   DeploymentMetadata{Name: "zhiyong-test"},
		Spec: DeploymentTemplateSpec{
			Parameters: map[string]DeploymentParameter{
				"releaseId": {Required: true},
			},
			Application: "zhiyong",
			Release: CICDReleaseReference{Source: &CICDSourceRelease{
				Repository: "https://example.test/zhiyong.git",
				Revision:   "${inputs.releaseId}",
				Branch:     "main",
			}},
			Environment: "test",
		},
	}
}

func validEnvironmentRegistry() CICDEnvironmentRegistry {
	return CICDEnvironmentRegistry{
		ArtifactContract: CICDArtifactContract{
			Type:                 cicdArtifactImageSet,
			SourceRepository:     "https://example.test/zhiyong.git",
			SourceBranch:         "main",
			Services:             []string{"zhiyong-exam-api"},
			ImageTagPattern:      "^release-[0-9]{8}-[0-9a-f]{12}$",
			ImageReferenceFormat: "repository@digest",
		},
		VerificationProfiles: map[string]CICDVerificationProfile{
			"production": {
				RequiredEvidence: []string{"source-identity", "image-set", "runtime-state"},
			},
		},
		Environments: map[string]CICDEnvironmentProfile{
			"test": validEnvironmentProfile(),
		},
	}
}

func validEnvironmentProfile() CICDEnvironmentProfile {
	return CICDEnvironmentProfile{
		Target: CICDEnvironmentTarget{
			Name:     "gw-oilan-node",
			Cluster:  "doops-oilan",
			Instance: "oilan-node",
		},
		Executor: CICDEnvironmentExecutor{
			Type: cicdExecutorHelm,
			Config: CICDHelmExecutorConfig{
				Namespace:                 "test",
				Release:                   "zhiyong",
				Workload:                  "deployment/zhiyong",
				Container:                 "zhiyong",
				Registry:                  "registry.example.test/oilan-system",
				ReleaseManifestRepository: "registry.example.test/oilan-system/zhiyong-release-manifest-test",
				Chart:                     "deploy/environments/test/chart",
				Values:                    "deploy/environments/test/chart/values.yaml",
				RuntimeFiles:              "deploy/environments/test/chart/files",
				ImageBindings:             map[string]string{"zhiyong-exam-api": "examApi"},
				HealthChecks: CICDHealthChecks{
					Public: []CICDPublicHealthCheck{{
						ID:             "frontend-health",
						URL:            "https://study.example.test/healthz",
						ExpectedStatus: 200,
					}},
				},
			},
		},
		VerificationProfile: "production",
	}
}

func validRegistryYAML() string {
	return `
artifactContract:
  type: image-set
  sourceRepository: https://example.test/zhiyong.git
  sourceBranch: main
  services: [zhiyong-exam-api]
  imageTagPattern: ^release-[0-9]{8}-[0-9a-f]{12}$
  imageReferenceFormat: repository@digest
verificationProfiles:
  production:
    requiredEvidence: [source-identity, image-set, runtime-state]
environments:
  test:
    target:
      name: gw-oilan-node
      cluster: doops-oilan
      instance: oilan-node
    executor:
      type: helm
      config:
        namespace: test
        release: zhiyong
        chart: deploy/environments/test/chart
        values: deploy/environments/test/chart/values.yaml
        registry: registry.example.test/oilan-system
        releaseManifestRepository: registry.example.test/oilan-system/zhiyong-release-manifest-test
        imageBindings:
          zhiyong-exam-api: examApi
    verificationProfile: production
`
}
