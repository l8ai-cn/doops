package server

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestParseCredentialPlanReturnsOnlyDeclaredReferenceMetadata(t *testing.T) {
	root := t.TempDir()
	writeCredentialPlanTestFile(t, root, "deploy/release.yaml", `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: oilan-agent-release
spec:
  application: oilan
  environment: production
  configurationSource: deploy/environments.yaml
  credentialRefs:
    - name: cnb-oci-pull
      use: imagePull
      namespace: kz-ops
      registryRepository: team/app
      registryReference: latest
      workload:
        kind: Deployment
        name: doops-agent-live
  credentialBundleRefs:
    - name: release-shared
`)
	writeCredentialPlanTestFile(t, root, "deploy/environments.yaml", `
environments:
  production:
    target:
      name: gw-edu-coder
      cluster: doops-edu
      instance: edu-coder
`)

	plan, err := parseCredentialPlan(root, "deploy/release.yaml")
	if err != nil {
		t.Fatalf("parse credential plan: %v", err)
	}
	if plan.Template != "oilan-agent-release" ||
		plan.Project != "oilan" ||
		plan.Environment != "production" ||
		plan.Target != "gw-edu-coder" ||
		plan.Cluster != "doops-edu" ||
		plan.Instance != "edu-coder" {
		t.Fatalf("unexpected deployment context: %#v", plan)
	}
	if len(plan.CredentialRefs) != 1 {
		t.Fatalf("credential refs = %#v", plan.CredentialRefs)
	}
	ref := plan.CredentialRefs[0]
	if ref.Name != "cnb-oci-pull" ||
		ref.Use != CredentialUseImagePull ||
		ref.Namespace != "kz-ops" ||
		ref.Workload.Kind != "Deployment" ||
		ref.Workload.Name != "doops-agent-live" {
		t.Fatalf("unexpected credential reference: %#v", ref)
	}
	if len(plan.BundleRefs) != 1 || plan.BundleRefs[0] != "release-shared" {
		t.Fatalf("bundle refs = %#v", plan.BundleRefs)
	}
}

func TestParseCredentialPlanRejectsInlineSecretLikeFieldsAtAnyDepth(t *testing.T) {
	for _, field := range []string{"token", "password", "auth", "data", "stringData"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			writeCredentialPlanTestFile(t, root, "release.yaml", `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: release
spec:
  application: app
  environment: production
  configurationSource: environments.yaml
  executor:
    nested:
      `+field+`: canary-secret
`)
			writeCredentialPlanTestFile(t, root, "environments.yaml", `
environments:
  production:
    target:
      name: target
      cluster: cluster
      instance: instance
`)
			if _, err := parseCredentialPlan(root, "release.yaml"); !errors.Is(err, ErrInlineCredentialMaterial) {
				t.Fatalf("inline field %s error = %v, want ErrInlineCredentialMaterial", field, err)
			}
		})
	}
}

func TestParseCredentialPlanRejectsDuplicateKeysAndPathEscape(t *testing.T) {
	root := t.TempDir()
	writeCredentialPlanTestFile(t, root, "duplicate.yaml", `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: one
  name: two
spec:
  application: app
  environment: production
  configurationSource: environments.yaml
`)
	if _, err := parseCredentialPlan(root, "duplicate.yaml"); !errors.Is(err, ErrCredentialDeclarationInvalid) {
		t.Fatalf("duplicate YAML error = %v, want ErrCredentialDeclarationInvalid", err)
	}
	if _, err := parseCredentialPlan(root, "../outside.yaml"); !errors.Is(err, ErrCredentialDeclarationInvalid) {
		t.Fatalf("path escape error = %v, want ErrCredentialDeclarationInvalid", err)
	}
}

func TestParseCredentialPlanRejectsUnknownReferenceFields(t *testing.T) {
	root := t.TempDir()
	writeCredentialPlanTestFile(t, root, "release.yaml", `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: release
spec:
  application: app
  environment: production
  configurationSource: environments.yaml
  credentialRefs:
    - name: registry
      use: imagePull
      namespace: app
      secretValue: canary-secret
`)
	writeCredentialPlanTestFile(t, root, "environments.yaml", `
environments:
  production:
    target:
      name: target
      cluster: cluster
      instance: instance
`)
	if _, err := parseCredentialPlan(root, "release.yaml"); !errors.Is(err, ErrCredentialDeclarationInvalid) {
		t.Fatalf("unknown credentialRef field error = %v, want ErrCredentialDeclarationInvalid", err)
	}
}

func TestParseCredentialPlanRejectsUnknownDeploymentTemplateFields(t *testing.T) {
	root := t.TempDir()
	writeCredentialPlanTestFile(t, root, "release.yaml", `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: release
spec:
  application: app
  environment: production
  configurationSource: environments.yaml
  unrecognized: value
`)
	writeCredentialPlanTestFile(t, root, "environments.yaml", `
environments:
  production:
    target:
      name: target
      cluster: cluster
      instance: instance
`)
	if _, err := parseCredentialPlan(root, "release.yaml"); !errors.Is(err, ErrCredentialDeclarationInvalid) {
		t.Fatalf("unknown DeploymentTemplate field error = %v, want ErrCredentialDeclarationInvalid", err)
	}
}

func TestParseCredentialPlanRejectsInlineSecretLikeFieldsInConfigurationSource(t *testing.T) {
	root := t.TempDir()
	writeCredentialPlanTestFile(t, root, "release.yaml", `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: release
spec:
  application: app
  environment: production
  configurationSource: environments.yaml
`)
	writeCredentialPlanTestFile(t, root, "environments.yaml", `
environments:
  production:
    target:
      name: target
      cluster: cluster
      instance: instance
    nested:
      StringData: canary-secret
`)
	if _, err := parseCredentialPlan(root, "release.yaml"); !errors.Is(err, ErrInlineCredentialMaterial) {
		t.Fatalf("configuration secret-like field error = %v, want ErrInlineCredentialMaterial", err)
	}
}

func TestParseCredentialPlanRejectsDuplicateKeysInConfigurationSource(t *testing.T) {
	root := t.TempDir()
	writeCredentialPlanTestFile(t, root, "release.yaml", `
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: release
spec:
  application: app
  environment: production
  configurationSource: environments.yaml
`)
	writeCredentialPlanTestFile(t, root, "environments.yaml", `
environments:
  production:
    target:
      name: first
      name: second
      cluster: cluster
      instance: instance
`)
	if _, err := parseCredentialPlan(root, "release.yaml"); !errors.Is(err, ErrCredentialDeclarationInvalid) {
		t.Fatalf("configuration duplicate YAML error = %v, want ErrCredentialDeclarationInvalid", err)
	}
}

func writeCredentialPlanTestFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("create test directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
