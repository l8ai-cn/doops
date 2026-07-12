package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	deploymentAPIVersion    = "doops.sh/v2"
	deploymentTemplateKind  = "DeploymentTemplate"
	deploymentPlanKind      = "DeploymentPlan"
	deploymentMutationGate  = "require-explicit-approval"
	deploymentConvergence   = "until-verified"
	deploymentFailureMode   = "restore-last-known-good"
	deploymentConfiguration = "deploy/environments.yaml"
	deploymentPlanIssuer    = "doops-cicd-compiler"
)

var (
	immutableGitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ociDigestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

var forbiddenCICDDeclarationFields = map[string]struct{}{
	"stages":                    {},
	"uses":                      {},
	"task":                      {},
	"run":                       {},
	"requiredCommand":           {},
	"verificationCommand":       {},
	"dryRunVerificationCommand": {},
	"script":                    {},
	"context":                   {},
	"route":                     {},
	"namespace":                 {},
	"services":                  {},
	"executionTarget":           {},
	"profileDigest":             {},
	"profile":                   {},
	"artifactContract":          {},
	"registry":                  {},
	"chart":                     {},
	"values":                    {},
	"runtimeFiles":              {},
	"deploymentMode":            {},
	"publicHosts":               {},
	"healthChecks":              {},
	"authz":                     {},
}

type DeploymentTemplate struct {
	APIVersion string                 `json:"apiVersion" yaml:"apiVersion"`
	Kind       string                 `json:"kind" yaml:"kind"`
	Metadata   DeploymentMetadata     `json:"metadata" yaml:"metadata"`
	Spec       DeploymentTemplateSpec `json:"spec" yaml:"spec"`
	path       string
}

type DeploymentMetadata struct {
	Name        string `json:"name" yaml:"name"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
}

type DeploymentTemplateSpec struct {
	Parameters map[string]DeploymentParameter `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	Plan       DeploymentPlanSpec             `json:"plan" yaml:"plan"`
}

type DeploymentParameter struct {
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Default  string `json:"default,omitempty" yaml:"default,omitempty"`
}

type DeploymentPlan struct {
	APIVersion  string               `json:"apiVersion"`
	Kind        string               `json:"kind"`
	Metadata    DeploymentMetadata   `json:"metadata"`
	Inputs      map[string]string    `json:"inputs"`
	Spec        DeploymentPlanSpec   `json:"spec"`
	Digest      string               `json:"digest"`
	Attestation *CICDPlanAttestation `json:"attestation,omitempty"`
}

type CICDPlanAttestation struct {
	Algorithm  string `json:"algorithm"`
	Issuer     string `json:"issuer"`
	PlanDigest string `json:"planDigest"`
	Signature  string `json:"signature"`
}

type DeploymentPlanSpec struct {
	Release          CICDReleaseReference `json:"release" yaml:"release"`
	Target           CICDDeploymentTarget `json:"target" yaml:"target"`
	ArtifactContract CICDArtifactContract `json:"artifactContract,omitempty" yaml:"artifactContract,omitempty"`
	DesiredState     CICDDesiredState     `json:"desiredState" yaml:"desiredState"`
	Acceptance       CICDAcceptance       `json:"acceptance" yaml:"acceptance"`
	Policy           CICDReconcilePolicy  `json:"policy" yaml:"policy"`
}

type CICDReleaseReference struct {
	Source   *CICDSourceRelease   `json:"source,omitempty" yaml:"source,omitempty"`
	Manifest *CICDManifestRelease `json:"manifest,omitempty" yaml:"manifest,omitempty"`
}

type CICDSourceRelease struct {
	Repository string `json:"repository" yaml:"repository"`
	Revision   string `json:"revision" yaml:"revision"`
	Branch     string `json:"branch,omitempty" yaml:"branch,omitempty"`
	TreeDigest string `json:"treeDigest,omitempty" yaml:"treeDigest,omitempty"`
}

type CICDManifestRelease struct {
	Repository string `json:"repository" yaml:"repository"`
	Reference  string `json:"reference" yaml:"reference"`
	Digest     string `json:"digest" yaml:"digest"`
}

type CICDDeploymentTarget struct {
	Environment     string                  `json:"environment" yaml:"environment"`
	ExecutionTarget string                  `json:"executionTarget,omitempty" yaml:"executionTarget,omitempty"`
	ProfileDigest   string                  `json:"profileDigest,omitempty" yaml:"profileDigest,omitempty"`
	Profile         *CICDEnvironmentProfile `json:"profile,omitempty" yaml:"profile,omitempty"`
}

type CICDDesiredState struct {
	Application         string `json:"application" yaml:"application"`
	Delivery            string `json:"delivery" yaml:"delivery"`
	ConfigurationSource string `json:"configurationSource" yaml:"configurationSource"`
	Authorization       string `json:"authorization" yaml:"authorization"`
}

type CICDAcceptance struct {
	RequiredEvidence        []string `json:"requiredEvidence" yaml:"requiredEvidence"`
	RequiredFailureEvidence []string `json:"requiredFailureEvidence" yaml:"requiredFailureEvidence"`
}

type CICDReconcilePolicy struct {
	Mutation    string `json:"mutation" yaml:"mutation"`
	Convergence string `json:"convergence" yaml:"convergence"`
	FailureMode string `json:"failureMode" yaml:"failureMode"`
}

type CICDEnvironmentRegistry struct {
	ArtifactContract CICDArtifactContract              `json:"artifactContract" yaml:"artifactContract"`
	Environments     map[string]CICDEnvironmentProfile `json:"environments" yaml:"environments"`
}

type CICDEnvironmentProfile struct {
	DisplayName    string            `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Target         string            `json:"target" yaml:"target"`
	Cluster        string            `json:"cluster" yaml:"cluster"`
	Instance       string            `json:"instance" yaml:"instance"`
	Namespace      string            `json:"namespace" yaml:"namespace"`
	Release        string            `json:"release" yaml:"release"`
	Registry       string            `json:"registry" yaml:"registry"`
	Chart          string            `json:"chart" yaml:"chart"`
	Values         string            `json:"values" yaml:"values"`
	RuntimeFiles   string            `json:"runtimeFiles,omitempty" yaml:"runtimeFiles,omitempty"`
	DeploymentMode string            `json:"deploymentMode" yaml:"deploymentMode"`
	PublicHosts    []string          `json:"publicHosts,omitempty" yaml:"publicHosts,omitempty"`
	HealthChecks   CICDHealthChecks  `json:"healthChecks" yaml:"healthChecks"`
	Authz          map[string]string `json:"authz,omitempty" yaml:"authz,omitempty"`
}

type CICDArtifactContract struct {
	SourceRepository     string                 `json:"sourceRepository,omitempty" yaml:"sourceRepository,omitempty"`
	SourceBranch         string                 `json:"sourceBranch,omitempty" yaml:"sourceBranch,omitempty"`
	Services             []string               `json:"services" yaml:"services"`
	ImageTagPattern      string                 `json:"imageTagPattern,omitempty" yaml:"imageTagPattern,omitempty"`
	ImageReferenceFormat string                 `json:"imageReferenceFormat,omitempty" yaml:"imageReferenceFormat,omitempty"`
	HelmImageBindings    map[string]string      `json:"helmImageBindings,omitempty" yaml:"helmImageBindings,omitempty"`
	ManifestRepository   string                 `json:"manifestRepository,omitempty" yaml:"manifestRepository,omitempty"`
	Authz                map[string]interface{} `json:"authz,omitempty" yaml:"authz,omitempty"`
}

type CICDHealthChecks struct {
	Public    []CICDPublicHealthCheck   `json:"public" yaml:"public"`
	Workloads []CICDWorkloadHealthCheck `json:"workloads" yaml:"workloads"`
}

type CICDPublicHealthCheck struct {
	ID             string                 `json:"id" yaml:"id"`
	URL            string                 `json:"url" yaml:"url"`
	ExpectedStatus int                    `json:"expectedStatus" yaml:"expectedStatus"`
	BodyContains   string                 `json:"bodyContains,omitempty" yaml:"bodyContains,omitempty"`
	ExpectedJSON   map[string]interface{} `json:"expectedJson,omitempty" yaml:"expectedJson,omitempty"`
}

type CICDWorkloadHealthCheck struct {
	Service          string `json:"service" yaml:"service"`
	MinReadyReplicas int    `json:"minReadyReplicas" yaml:"minReadyReplicas"`
	RequireEndpoints bool   `json:"requireEndpoints" yaml:"requireEndpoints"`
}

type CICDReconcileStatus string

const (
	CICDReconcilePending     CICDReconcileStatus = "Pending"
	CICDReconcileReconciling CICDReconcileStatus = "Reconciling"
	CICDReconcileConverged   CICDReconcileStatus = "Converged"
	CICDReconcileBlocked     CICDReconcileStatus = "Blocked"
	CICDReconcileFailed      CICDReconcileStatus = "Failed"
)

type CICDEvidence struct {
	Kind      string `json:"kind"`
	Reference string `json:"reference"`
}

type CICDViolation struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type CICDReconcileResult struct {
	PlanDigest string              `json:"planDigest"`
	Status     CICDReconcileStatus `json:"status"`
	Evidence   []CICDEvidence      `json:"evidence,omitempty"`
	Violations []CICDViolation     `json:"violations,omitempty"`
}

func loadDeploymentTemplate(path string) (DeploymentTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DeploymentTemplate{}, fmt.Errorf("read deployment template: %w", err)
	}

	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return DeploymentTemplate{}, fmt.Errorf("parse deployment template: %w", err)
	}

	var template DeploymentTemplate
	if err := yaml.Unmarshal(data, &template); err != nil {
		return DeploymentTemplate{}, fmt.Errorf("parse deployment template: %w", err)
	}
	template.path = path
	if template.APIVersion != deploymentAPIVersion {
		return DeploymentTemplate{}, fmt.Errorf("unsupported apiVersion %q", template.APIVersion)
	}
	if template.Kind != deploymentTemplateKind {
		return DeploymentTemplate{}, fmt.Errorf("unsupported kind %q", template.Kind)
	}
	if field, found := findForbiddenCICDDeclarationField(&raw); found {
		return DeploymentTemplate{}, fmt.Errorf("forbidden command-driven field %q", field)
	}
	if err := validateDeploymentTemplate(template); err != nil {
		return DeploymentTemplate{}, err
	}
	return template, nil
}

func validateDeploymentTemplate(template DeploymentTemplate) error {
	if template.APIVersion != deploymentAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q", template.APIVersion)
	}
	if template.Kind != deploymentTemplateKind {
		return fmt.Errorf("unsupported kind %q", template.Kind)
	}
	if strings.TrimSpace(template.Metadata.Name) == "" {
		return fmt.Errorf("metadata.name is required")
	}
	return validateDeploymentPlanSpec(template.Spec.Plan, true)
}

func compileDeploymentPlan(template DeploymentTemplate, overrides map[string]string, registry CICDEnvironmentRegistry) (DeploymentPlan, error) {
	if err := validateDeploymentTemplate(template); err != nil {
		return DeploymentPlan{}, err
	}
	inputs, err := resolveDeploymentParameters(template.Spec.Parameters, overrides)
	if err != nil {
		return DeploymentPlan{}, err
	}
	spec := renderDeploymentPlanSpec(template.Spec.Plan, inputs)
	if err := validateDeploymentPlanSpec(spec, false); err != nil {
		return DeploymentPlan{}, err
	}
	profile, ok := registry.Environments[spec.Target.Environment]
	if !ok {
		return DeploymentPlan{}, fmt.Errorf("environment %q is not declared in environment profile", spec.Target.Environment)
	}
	if err := validateCICDEnvironmentProfile(spec.Target.Environment, profile); err != nil {
		return DeploymentPlan{}, err
	}
	if err := validateCICDArtifactContract(registry.ArtifactContract); err != nil {
		return DeploymentPlan{}, err
	}
	spec.Target.ExecutionTarget = profile.Target
	spec.Target.Profile = &profile
	profileDigest, err := digestDeploymentValue(profile)
	if err != nil {
		return DeploymentPlan{}, err
	}
	spec.Target.ProfileDigest = profileDigest
	spec.ArtifactContract = registry.ArtifactContract

	plan := DeploymentPlan{
		APIVersion: deploymentAPIVersion,
		Kind:       deploymentPlanKind,
		Metadata:   template.Metadata,
		Inputs:     inputs,
		Spec:       spec,
	}
	digest, err := digestDeploymentPlan(plan)
	if err != nil {
		return DeploymentPlan{}, err
	}
	plan.Digest = digest
	if err := validateDeploymentPlan(plan); err != nil {
		return DeploymentPlan{}, err
	}
	return plan, nil
}

func buildDeploymentPlan(template DeploymentTemplate, overrides map[string]string) (DeploymentPlan, error) {
	inputs, err := resolveDeploymentParameters(template.Spec.Parameters, overrides)
	if err != nil {
		return DeploymentPlan{}, err
	}
	configurationSource := renderCICDTemplate(template.Spec.Plan.DesiredState.ConfigurationSource, inputs)
	registryPath, err := resolveDeploymentConfigurationPath(template, configurationSource)
	if err != nil {
		return DeploymentPlan{}, err
	}
	registry, err := loadCICDEnvironmentRegistry(registryPath)
	if err != nil {
		return DeploymentPlan{}, err
	}
	return compileDeploymentPlan(template, inputs, registry)
}

func loadCICDEnvironmentRegistry(path string) (CICDEnvironmentRegistry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CICDEnvironmentRegistry{}, fmt.Errorf("read environment registry: %w", err)
	}
	var registry CICDEnvironmentRegistry
	if err := yaml.Unmarshal(data, &registry); err != nil {
		return CICDEnvironmentRegistry{}, fmt.Errorf("parse environment registry: %w", err)
	}
	if len(registry.Environments) == 0 {
		return CICDEnvironmentRegistry{}, fmt.Errorf("environment registry has no environments")
	}
	return registry, nil
}

func resolveDeploymentConfigurationPath(template DeploymentTemplate, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("deployment configuration source is required")
	}
	if filepath.IsAbs(source) {
		return source, nil
	}
	if strings.TrimSpace(template.path) == "" {
		return "", fmt.Errorf("deployment template path is required to resolve configuration source")
	}
	templatePath, err := filepath.Abs(template.path)
	if err != nil {
		return "", fmt.Errorf("resolve deployment template path: %w", err)
	}
	root := filepath.Dir(templatePath)
	for dir := root; ; {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			root = dir
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return filepath.Join(root, source), nil
}

func validateDeploymentPlanSpec(spec DeploymentPlanSpec, allowTemplates bool) error {
	sourceSet := spec.Release.Source != nil
	manifestSet := spec.Release.Manifest != nil
	if sourceSet == manifestSet {
		return fmt.Errorf("spec.plan.release requires exactly one of source or manifest")
	}
	if sourceSet {
		if err := validateCICDSourceRelease(*spec.Release.Source, allowTemplates); err != nil {
			return err
		}
	}
	if manifestSet {
		if err := validateCICDManifestRelease(*spec.Release.Manifest, allowTemplates); err != nil {
			return err
		}
	}
	if strings.TrimSpace(spec.Target.Environment) == "" {
		return fmt.Errorf("spec.plan.target.environment is required")
	}
	if strings.TrimSpace(spec.DesiredState.Application) == "" {
		return fmt.Errorf("spec.plan.desiredState.application is required")
	}
	if strings.TrimSpace(spec.DesiredState.Delivery) == "" {
		return fmt.Errorf("spec.plan.desiredState.delivery is required")
	}
	if strings.TrimSpace(spec.DesiredState.ConfigurationSource) == "" {
		return fmt.Errorf("spec.plan.desiredState.configurationSource is required")
	}
	if strings.TrimSpace(spec.DesiredState.Authorization) == "" {
		return fmt.Errorf("spec.plan.desiredState.authorization is required")
	}
	if len(normalizeEvidenceKinds(spec.Acceptance.RequiredEvidence)) == 0 {
		return fmt.Errorf("spec.plan.acceptance.requiredEvidence is required")
	}
	if len(normalizeEvidenceKinds(spec.Acceptance.RequiredFailureEvidence)) == 0 {
		return fmt.Errorf("spec.plan.acceptance.requiredFailureEvidence is required")
	}
	if spec.Policy.Mutation != deploymentMutationGate {
		return fmt.Errorf("spec.plan.policy.mutation must be %q", deploymentMutationGate)
	}
	if spec.Policy.Convergence != deploymentConvergence {
		return fmt.Errorf("spec.plan.policy.convergence must be %q", deploymentConvergence)
	}
	if spec.Policy.FailureMode != deploymentFailureMode {
		return fmt.Errorf("spec.plan.policy.failureMode must be %q", deploymentFailureMode)
	}
	return nil
}

func validateCICDSourceRelease(source CICDSourceRelease, allowTemplates bool) error {
	if strings.TrimSpace(source.Repository) == "" {
		return fmt.Errorf("spec.plan.release.source.repository is required")
	}
	if strings.TrimSpace(source.Revision) == "" {
		return fmt.Errorf("spec.plan.release.source.revision is required")
	}
	if !allowTemplates && strings.Contains(source.Revision, "${") {
		return fmt.Errorf("spec.plan.release.source.revision was not rendered")
	}
	if !allowTemplates && !immutableGitCommitPattern.MatchString(source.Revision) {
		return fmt.Errorf("spec.plan.release.source.revision must be a 40-character immutable Git commit")
	}
	return nil
}

func validateCICDManifestRelease(manifest CICDManifestRelease, allowTemplates bool) error {
	if strings.TrimSpace(manifest.Repository) == "" {
		return fmt.Errorf("spec.plan.release.manifest.repository is required")
	}
	if strings.TrimSpace(manifest.Reference) == "" {
		return fmt.Errorf("spec.plan.release.manifest.reference is required")
	}
	if !allowTemplates && strings.Contains(manifest.Reference, "${") {
		return fmt.Errorf("spec.plan.release.manifest.reference was not rendered")
	}
	if strings.TrimSpace(manifest.Digest) == "" {
		return fmt.Errorf("spec.plan.release.manifest.digest is required")
	}
	if !allowTemplates && strings.Contains(manifest.Digest, "${") {
		return fmt.Errorf("spec.plan.release.manifest.digest was not rendered")
	}
	if !allowTemplates && !ociDigestPattern.MatchString(manifest.Digest) {
		return fmt.Errorf("spec.plan.release.manifest.digest must be an OCI sha256 digest")
	}
	return nil
}

func resolveDeploymentParameters(spec map[string]DeploymentParameter, overrides map[string]string) (map[string]string, error) {
	inputs := make(map[string]string, len(spec)+len(overrides))
	for name, parameter := range spec {
		if parameter.Default != "" {
			inputs[name] = parameter.Default
		}
	}
	for name, value := range overrides {
		if _, declared := spec[name]; !declared {
			return nil, fmt.Errorf("undeclared deployment parameter %q", name)
		}
		inputs[name] = value
	}
	for name, parameter := range spec {
		if parameter.Required && strings.TrimSpace(inputs[name]) == "" {
			return nil, fmt.Errorf("parameter %s is required", name)
		}
	}
	return inputs, nil
}

func renderDeploymentPlanSpec(spec DeploymentPlanSpec, inputs map[string]string) DeploymentPlanSpec {
	spec.Release = renderCICDReleaseReference(spec.Release, inputs)
	spec.Target.Environment = renderCICDTemplate(spec.Target.Environment, inputs)
	spec.DesiredState.Application = renderCICDTemplate(spec.DesiredState.Application, inputs)
	spec.DesiredState.Delivery = renderCICDTemplate(spec.DesiredState.Delivery, inputs)
	spec.DesiredState.ConfigurationSource = renderCICDTemplate(spec.DesiredState.ConfigurationSource, inputs)
	spec.DesiredState.Authorization = renderCICDTemplate(spec.DesiredState.Authorization, inputs)
	for i := range spec.Acceptance.RequiredEvidence {
		spec.Acceptance.RequiredEvidence[i] = renderCICDTemplate(spec.Acceptance.RequiredEvidence[i], inputs)
	}
	for i := range spec.Acceptance.RequiredFailureEvidence {
		spec.Acceptance.RequiredFailureEvidence[i] = renderCICDTemplate(spec.Acceptance.RequiredFailureEvidence[i], inputs)
	}
	spec.Policy.Mutation = renderCICDTemplate(spec.Policy.Mutation, inputs)
	spec.Policy.Convergence = renderCICDTemplate(spec.Policy.Convergence, inputs)
	spec.Policy.FailureMode = renderCICDTemplate(spec.Policy.FailureMode, inputs)
	return spec
}

func renderCICDReleaseReference(release CICDReleaseReference, inputs map[string]string) CICDReleaseReference {
	if release.Source != nil {
		source := *release.Source
		source.Repository = renderCICDTemplate(source.Repository, inputs)
		source.Revision = renderCICDTemplate(source.Revision, inputs)
		source.Branch = renderCICDTemplate(source.Branch, inputs)
		source.TreeDigest = renderCICDTemplate(source.TreeDigest, inputs)
		release.Source = &source
	}
	if release.Manifest != nil {
		manifest := *release.Manifest
		manifest.Repository = renderCICDTemplate(manifest.Repository, inputs)
		manifest.Reference = renderCICDTemplate(manifest.Reference, inputs)
		manifest.Digest = renderCICDTemplate(manifest.Digest, inputs)
		release.Manifest = &manifest
	}
	return release
}

func evaluateDeploymentReconcile(plan DeploymentPlan, result CICDReconcileResult) (CICDReconcileStatus, error) {
	if strings.TrimSpace(plan.Digest) == "" {
		return "", fmt.Errorf("deployment plan digest is required")
	}
	if result.PlanDigest != plan.Digest {
		return "", fmt.Errorf("reconcile result plan digest mismatch: want=%s got=%s", plan.Digest, result.PlanDigest)
	}
	switch result.Status {
	case CICDReconcileBlocked, CICDReconcileFailed:
		if len(result.Violations) == 0 {
			return "", fmt.Errorf("%s reconciliation result requires at least one violation", strings.ToLower(string(result.Status)))
		}
		requiredFailureEvidence := normalizeEvidenceKinds(plan.Spec.Acceptance.RequiredFailureEvidence)
		actualFailureEvidence := map[string]bool{}
		for _, evidence := range result.Evidence {
			if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Reference) == "" {
				return "", fmt.Errorf("reconcile evidence requires kind and reference")
			}
			actualFailureEvidence[evidence.Kind] = true
		}
		for _, kind := range requiredFailureEvidence {
			if !actualFailureEvidence[kind] {
				return "", fmt.Errorf("%s reconciliation result is missing required failure evidence %q", strings.ToLower(string(result.Status)), kind)
			}
		}
		return result.Status, nil
	case CICDReconcilePending, CICDReconcileReconciling, CICDReconcileConverged:
	default:
		return "", fmt.Errorf("unsupported reconcile status %q", result.Status)
	}

	required := normalizeEvidenceKinds(plan.Spec.Acceptance.RequiredEvidence)
	actual := map[string]bool{}
	for _, evidence := range result.Evidence {
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Reference) == "" {
			return "", fmt.Errorf("reconcile evidence requires kind and reference")
		}
		actual[evidence.Kind] = true
	}
	for _, kind := range required {
		if !actual[kind] {
			if result.Status == CICDReconcileConverged {
				return "", fmt.Errorf("converged result is missing required evidence %q", kind)
			}
			return CICDReconcileReconciling, nil
		}
	}
	return CICDReconcileConverged, nil
}

func validateCICDArtifactContract(artifact CICDArtifactContract) error {
	if strings.TrimSpace(artifact.SourceRepository) == "" {
		return fmt.Errorf("artifact contract source repository is required")
	}
	if strings.TrimSpace(artifact.SourceBranch) == "" {
		return fmt.Errorf("artifact contract source branch is required")
	}
	if len(artifact.Services) == 0 {
		return fmt.Errorf("artifact contract services are required")
	}
	if strings.TrimSpace(artifact.ImageTagPattern) == "" {
		return fmt.Errorf("artifact contract image tag pattern is required")
	}
	if strings.TrimSpace(artifact.ImageReferenceFormat) == "" {
		return fmt.Errorf("artifact contract image reference format is required")
	}
	if strings.TrimSpace(artifact.ManifestRepository) == "" {
		return fmt.Errorf("artifact contract manifest repository is required")
	}
	if len(artifact.HelmImageBindings) != len(artifact.Services) {
		return fmt.Errorf("artifact contract must bind every service to Helm")
	}
	for _, service := range artifact.Services {
		if strings.TrimSpace(artifact.HelmImageBindings[service]) == "" {
			return fmt.Errorf("artifact contract service %q is missing a Helm binding", service)
		}
	}
	return nil
}

func validateCICDEnvironmentProfile(name string, profile CICDEnvironmentProfile) error {
	required := map[string]string{
		"target":         profile.Target,
		"cluster":        profile.Cluster,
		"instance":       profile.Instance,
		"namespace":      profile.Namespace,
		"release":        profile.Release,
		"registry":       profile.Registry,
		"chart":          profile.Chart,
		"values":         profile.Values,
		"deploymentMode": profile.DeploymentMode,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("environment %q %s is required", name, field)
		}
	}
	if len(profile.HealthChecks.Public) == 0 {
		return fmt.Errorf("environment %q public health checks are required", name)
	}
	for _, check := range profile.HealthChecks.Public {
		if strings.TrimSpace(check.ID) == "" || strings.TrimSpace(check.URL) == "" || check.ExpectedStatus <= 0 {
			return fmt.Errorf("environment %q public health check requires id, url, and expected status", name)
		}
	}
	if profile.DeploymentMode == "application" {
		if strings.TrimSpace(profile.RuntimeFiles) == "" {
			return fmt.Errorf("application environment %q runtime files are required", name)
		}
		if len(profile.HealthChecks.Workloads) == 0 {
			return fmt.Errorf("application environment %q workload health checks are required", name)
		}
		for _, check := range profile.HealthChecks.Workloads {
			if strings.TrimSpace(check.Service) == "" || check.MinReadyReplicas < 1 || !check.RequireEndpoints {
				return fmt.Errorf("application environment %q workload health checks must require service readiness and endpoints", name)
			}
		}
	}
	return nil
}

func normalizeEvidenceKinds(kinds []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" || seen[kind] {
			continue
		}
		seen[kind] = true
		out = append(out, kind)
	}
	sort.Strings(out)
	return out
}

func digestDeploymentValue(value interface{}) (string, error) {
	data, err := canonicalDeploymentJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func digestDeploymentPlan(plan DeploymentPlan) (string, error) {
	plan.Digest = ""
	plan.Attestation = nil
	return digestDeploymentValue(plan)
}

func canonicalDeploymentJSON(value interface{}) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode deployment value: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	var canonical interface{}
	if err := decoder.Decode(&canonical); err != nil {
		return nil, fmt.Errorf("decode deployment value: %w", err)
	}
	data, err = json.Marshal(canonical)
	if err != nil {
		return nil, fmt.Errorf("canonicalize deployment value: %w", err)
	}
	return data, nil
}

func validateDeploymentPlan(plan DeploymentPlan) error {
	if plan.APIVersion != deploymentAPIVersion || plan.Kind != deploymentPlanKind {
		return fmt.Errorf("invalid deployment plan type")
	}
	if strings.TrimSpace(plan.Digest) == "" {
		return fmt.Errorf("deployment plan digest is required")
	}
	if err := validateDeploymentPlanSpec(plan.Spec, false); err != nil {
		return err
	}
	if plan.Spec.Target.Profile == nil {
		return fmt.Errorf("deployment plan resolved environment profile is required")
	}
	if plan.Spec.Target.ExecutionTarget != plan.Spec.Target.Profile.Target {
		return fmt.Errorf("deployment plan execution target does not match environment profile")
	}
	if err := validateCICDEnvironmentProfile(plan.Spec.Target.Environment, *plan.Spec.Target.Profile); err != nil {
		return err
	}
	profileDigest, err := digestDeploymentValue(*plan.Spec.Target.Profile)
	if err != nil {
		return err
	}
	if plan.Spec.Target.ProfileDigest != profileDigest {
		return fmt.Errorf("deployment plan environment profile digest mismatch")
	}
	if err := validateCICDArtifactContract(plan.Spec.ArtifactContract); err != nil {
		return err
	}
	expectedDigest, err := digestDeploymentPlan(plan)
	if err != nil {
		return err
	}
	if plan.Digest != expectedDigest {
		return fmt.Errorf("deployment plan digest mismatch")
	}
	return nil
}

func validateCICDServerBinding(server Server, plan DeploymentPlan) error {
	if err := validateDeploymentPlan(plan); err != nil {
		return err
	}
	profile := plan.Spec.Target.Profile
	if server.Name != plan.Spec.Target.ExecutionTarget {
		return fmt.Errorf("configured target %q does not match deployment plan target %q", server.Name, plan.Spec.Target.ExecutionTarget)
	}
	cluster := strings.TrimSpace(server.Cluster)
	if cluster == "" {
		cluster = "default"
	}
	instance := strings.TrimSpace(server.Instance)
	if instance == "" {
		instance = server.Name
	}
	if cluster != profile.Cluster || instance != profile.Instance {
		return fmt.Errorf("configured target %q resolves to %s/%s, but deployment plan requires %s/%s", server.Name, cluster, instance, profile.Cluster, profile.Instance)
	}
	return nil
}

func attestDeploymentPlanFromEnvironment(plan *DeploymentPlan) error {
	raw := strings.TrimSpace(os.Getenv("DOOPS_CICD_PLAN_SIGNING_KEY"))
	if raw == "" {
		return fmt.Errorf("DOOPS_CICD_PLAN_SIGNING_KEY is required for cicd run")
	}
	privateKey, err := decodeCICDEd25519PrivateKey(raw)
	if err != nil {
		return err
	}
	return attestDeploymentPlan(plan, privateKey)
}

func attestDeploymentPlan(plan *DeploymentPlan, privateKey ed25519.PrivateKey) error {
	if plan == nil {
		return fmt.Errorf("deployment plan is required")
	}
	if err := validateDeploymentPlan(*plan); err != nil {
		return err
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("CI/CD plan signing key must be an Ed25519 private key")
	}
	plan.Attestation = &CICDPlanAttestation{
		Algorithm:  "ed25519",
		Issuer:     deploymentPlanIssuer,
		PlanDigest: plan.Digest,
		Signature:  base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, []byte(plan.Digest))),
	}
	return nil
}

func decodeCICDEd25519PrivateKey(raw string) (ed25519.PrivateKey, error) {
	value, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode DOOPS_CICD_PLAN_SIGNING_KEY: %w", err)
	}
	if len(value) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("DOOPS_CICD_PLAN_SIGNING_KEY must decode to an Ed25519 private key")
	}
	return ed25519.PrivateKey(value), nil
}

func findForbiddenCICDDeclarationField(node *yaml.Node) (string, bool) {
	if node == nil {
		return "", false
	}
	if node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index]
			if _, forbidden := forbiddenCICDDeclarationFields[key.Value]; forbidden {
				return key.Value, true
			}
			if field, found := findForbiddenCICDDeclarationField(node.Content[index+1]); found {
				return field, true
			}
		}
		return "", false
	}
	for _, child := range node.Content {
		if field, found := findForbiddenCICDDeclarationField(child); found {
			return field, true
		}
	}
	return "", false
}

func renderCICDTemplate(value string, inputs map[string]string) string {
	for name, input := range inputs {
		value = strings.ReplaceAll(value, "${inputs."+name+"}", input)
	}
	return value
}
