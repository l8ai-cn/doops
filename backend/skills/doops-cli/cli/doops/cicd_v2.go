package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	deploymentConfiguration = "deploy/environments.yaml"

	cicdExecutorHelm     = "helm"
	cicdArtifactImageSet = "image-set"
	cicdArtifactManifest = "manifest"
)

var (
	immutableGitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	ociDigestPattern          = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

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
	Parameters          map[string]DeploymentParameter `json:"parameters,omitempty" yaml:"parameters,omitempty"`
	Application         string                         `json:"application" yaml:"application"`
	Release             CICDReleaseReference           `json:"release" yaml:"release"`
	Environment         string                         `json:"environment" yaml:"environment"`
	ConfigurationSource string                         `json:"configurationSource,omitempty" yaml:"configurationSource,omitempty"`
}

type DeploymentParameter struct {
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Default  string `json:"default,omitempty" yaml:"default,omitempty"`
}

type DeploymentPlan struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   DeploymentMetadata `json:"metadata"`
	Inputs     map[string]string  `json:"inputs"`
	Spec       DeploymentPlanSpec `json:"spec"`
	Digest     string             `json:"digest"`
}

type DeploymentPlanSpec struct {
	Release          CICDReleaseReference `json:"release" yaml:"release"`
	Target           CICDDeploymentTarget `json:"target" yaml:"target"`
	ArtifactContract CICDArtifactContract `json:"artifactContract" yaml:"artifactContract"`
	DesiredState     CICDDesiredState     `json:"desiredState" yaml:"desiredState"`
	Acceptance       CICDAcceptance       `json:"acceptance" yaml:"acceptance"`
}

type CICDReleaseReference struct {
	Source   *CICDSourceRelease   `json:"source,omitempty" yaml:"source,omitempty"`
	Manifest *CICDManifestRelease `json:"manifest,omitempty" yaml:"manifest,omitempty"`
	Version  string               `json:"version,omitempty" yaml:"version,omitempty"`
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
	ExecutionTarget string                  `json:"executionTarget" yaml:"executionTarget"`
	ProfileDigest   string                  `json:"profileDigest" yaml:"profileDigest"`
	Profile         *CICDEnvironmentProfile `json:"profile" yaml:"profile"`
}

type CICDDesiredState struct {
	Application string `json:"application" yaml:"application"`
}

type CICDAcceptance struct {
	VerificationProfile string   `json:"verificationProfile" yaml:"verificationProfile"`
	RequiredEvidence    []string `json:"requiredEvidence" yaml:"requiredEvidence"`
}

type CICDEnvironmentRegistry struct {
	APIVersion           string                             `json:"apiVersion,omitempty" yaml:"apiVersion,omitempty"`
	Kind                 string                             `json:"kind,omitempty" yaml:"kind,omitempty"`
	Metadata             CICDRegistryMetadata               `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	IdentityRules        []string                           `json:"identityRules,omitempty" yaml:"identityRules,omitempty"`
	ArtifactContract     CICDArtifactContract               `json:"artifactContract" yaml:"artifactContract"`
	VerificationProfiles map[string]CICDVerificationProfile `json:"verificationProfiles" yaml:"verificationProfiles"`
	Environments         map[string]CICDEnvironmentProfile  `json:"environments" yaml:"environments"`
}

type CICDRegistryMetadata struct {
	Name      string `json:"name,omitempty" yaml:"name,omitempty"`
	UpdatedAt string `json:"updatedAt,omitempty" yaml:"updatedAt,omitempty"`
}

type CICDVerificationProfile struct {
	RequiredEvidence []string `json:"requiredEvidence" yaml:"requiredEvidence"`
}

type CICDEnvironmentProfile struct {
	DisplayName             string                  `json:"displayName,omitempty" yaml:"displayName,omitempty"`
	Target                  CICDEnvironmentTarget   `json:"target" yaml:"target"`
	Executor                CICDEnvironmentExecutor `json:"executor" yaml:"executor"`
	VerificationProfile     string                  `json:"verificationProfile" yaml:"verificationProfile"`
	DeploymentMode          string                  `json:"deploymentMode,omitempty" yaml:"deploymentMode,omitempty"`
	ControlPlaneEnvironment string                  `json:"controlPlaneEnvironment,omitempty" yaml:"controlPlaneEnvironment,omitempty"`
	PublicHosts             []string                `json:"publicHosts,omitempty" yaml:"publicHosts,omitempty"`
}

type CICDEnvironmentTarget struct {
	Name     string `json:"name" yaml:"name"`
	Cluster  string `json:"cluster" yaml:"cluster"`
	Instance string `json:"instance" yaml:"instance"`
}

type CICDEnvironmentExecutor struct {
	Type   string                 `json:"type" yaml:"type"`
	Config CICDHelmExecutorConfig `json:"config" yaml:"config"`
}

type CICDHelmExecutorConfig struct {
	Namespace                 string                 `json:"namespace" yaml:"namespace"`
	Release                   string                 `json:"release" yaml:"release"`
	Chart                     string                 `json:"chart" yaml:"chart"`
	Values                    string                 `json:"values" yaml:"values"`
	Workload                  string                 `json:"workload,omitempty" yaml:"workload,omitempty"`
	Container                 string                 `json:"container,omitempty" yaml:"container,omitempty"`
	Registry                  string                 `json:"registry,omitempty" yaml:"registry,omitempty"`
	ReleaseManifestRepository string                 `json:"releaseManifestRepository,omitempty" yaml:"releaseManifestRepository,omitempty"`
	ImageBindings             map[string]string      `json:"imageBindings,omitempty" yaml:"imageBindings,omitempty"`
	RuntimeFiles              string                 `json:"runtimeFiles,omitempty" yaml:"runtimeFiles,omitempty"`
	PublicHosts               []string               `json:"publicHosts,omitempty" yaml:"publicHosts,omitempty"`
	HealthChecks              CICDHealthChecks       `json:"healthChecks,omitempty" yaml:"healthChecks,omitempty"`
	DeploymentMode            string                 `json:"deploymentMode,omitempty" yaml:"deploymentMode,omitempty"`
	ArtifactSync              CICDArtifactSync       `json:"artifactSync,omitempty" yaml:"artifactSync,omitempty"`
	RegistryCredential        CICDRegistryCredential `json:"registryCredential,omitempty" yaml:"registryCredential,omitempty"`
	WorkloadNamespaces        map[string]string      `json:"workloadNamespaces,omitempty" yaml:"workloadNamespaces,omitempty"`
	RetiredResources          []CICDRetiredResource  `json:"retiredResources,omitempty" yaml:"retiredResources,omitempty"`
	Authz                     map[string]string      `json:"authz,omitempty" yaml:"authz,omitempty"`
}

type CICDArtifactSync struct {
	SourceCredentialRef   *CICDKubernetesSecretReference `json:"sourceCredentialRef,omitempty" yaml:"sourceCredentialRef,omitempty"`
	RegistryCredentialRef *CICDKubernetesSecretReference `json:"registryCredentialRef,omitempty" yaml:"registryCredentialRef,omitempty"`
}

type CICDKubernetesSecretReference struct {
	Kind string `json:"kind" yaml:"kind"`
	Name string `json:"name" yaml:"name"`
}

type CICDRegistryCredential struct {
	Namespace  string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	SecretName string `json:"secretName,omitempty" yaml:"secretName,omitempty"`
	Key        string `json:"key,omitempty" yaml:"key,omitempty"`
}

type CICDRetiredResource struct {
	APIVersion     string                      `json:"apiVersion" yaml:"apiVersion"`
	Kind           string                      `json:"kind" yaml:"kind"`
	Name           string                      `json:"name" yaml:"name"`
	DeletionPolicy string                      `json:"deletionPolicy" yaml:"deletionPolicy"`
	Expected       CICDRetiredResourceExpected `json:"expected" yaml:"expected"`
}

type CICDRetiredResourceExpected struct {
	Manager          string   `json:"manager" yaml:"manager"`
	IngressClassName string   `json:"ingressClassName" yaml:"ingressClassName"`
	Hosts            []string `json:"hosts" yaml:"hosts"`
}

type CICDArtifactContract struct {
	Type                 string   `json:"type" yaml:"type"`
	SourceRegistry       string   `json:"sourceRegistry,omitempty" yaml:"sourceRegistry,omitempty"`
	SourceRepository     string   `json:"sourceRepository,omitempty" yaml:"sourceRepository,omitempty"`
	SourceBranch         string   `json:"sourceBranch,omitempty" yaml:"sourceBranch,omitempty"`
	Services             []string `json:"services,omitempty" yaml:"services,omitempty"`
	ImageTagPattern      string   `json:"imageTagPattern,omitempty" yaml:"imageTagPattern,omitempty"`
	ImageTagTimeZone     string   `json:"imageTagTimeZone,omitempty" yaml:"imageTagTimeZone,omitempty"`
	ImageReferenceFormat string   `json:"imageReferenceFormat,omitempty" yaml:"imageReferenceFormat,omitempty"`
}

type CICDHealthChecks struct {
	Public    []CICDPublicHealthCheck   `json:"public,omitempty" yaml:"public,omitempty"`
	Workloads []CICDWorkloadHealthCheck `json:"workloads,omitempty" yaml:"workloads,omitempty"`
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

func loadDeploymentTemplate(path string) (DeploymentTemplate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return DeploymentTemplate{}, fmt.Errorf("read deployment template: %w", err)
	}

	var template DeploymentTemplate
	if err := decodeStrictCICDYAML(data, &template); err != nil {
		return DeploymentTemplate{}, fmt.Errorf("parse deployment template: %w", err)
	}
	template.path = path
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
	return validateDeploymentIntent(template.Spec, true)
}

func compileDeploymentPlan(template DeploymentTemplate, overrides map[string]string, registry CICDEnvironmentRegistry) (DeploymentPlan, error) {
	if err := validateDeploymentTemplate(template); err != nil {
		return DeploymentPlan{}, err
	}
	inputs, err := resolveDeploymentParameters(template.Spec.Parameters, overrides)
	if err != nil {
		return DeploymentPlan{}, err
	}
	intent := renderDeploymentIntent(template.Spec, inputs)
	if err := validateDeploymentIntent(intent, false); err != nil {
		return DeploymentPlan{}, err
	}

	profile, ok := registry.Environments[intent.Environment]
	if !ok {
		return DeploymentPlan{}, fmt.Errorf("environment %q is not declared in environment registry", intent.Environment)
	}
	if err := validateCICDEnvironmentProfile(intent.Environment, profile); err != nil {
		return DeploymentPlan{}, err
	}
	verification, ok := registry.VerificationProfiles[profile.VerificationProfile]
	if !ok {
		return DeploymentPlan{}, fmt.Errorf("environment %q verification profile %q is not declared", intent.Environment, profile.VerificationProfile)
	}
	if err := validateCICDVerificationProfile(profile.VerificationProfile, verification); err != nil {
		return DeploymentPlan{}, err
	}
	if err := validateCICDArtifactContract(intent.Release, registry.ArtifactContract, profile.Executor); err != nil {
		return DeploymentPlan{}, err
	}

	profileDigest, err := digestDeploymentValue(profile)
	if err != nil {
		return DeploymentPlan{}, err
	}
	spec := DeploymentPlanSpec{
		Release: intent.Release,
		Target: CICDDeploymentTarget{
			Environment:     intent.Environment,
			ExecutionTarget: profile.Target.Name,
			ProfileDigest:   profileDigest,
			Profile:         &profile,
		},
		ArtifactContract: registry.ArtifactContract,
		DesiredState:     CICDDesiredState{Application: intent.Application},
		Acceptance: CICDAcceptance{
			VerificationProfile: profile.VerificationProfile,
			RequiredEvidence:    normalizeEvidenceKinds(verification.RequiredEvidence),
		},
	}
	plan := DeploymentPlan{
		APIVersion: deploymentAPIVersion,
		Kind:       deploymentPlanKind,
		Metadata:   template.Metadata,
		Inputs:     inputs,
		Spec:       spec,
	}
	plan.Digest, err = digestDeploymentPlan(plan)
	if err != nil {
		return DeploymentPlan{}, err
	}
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
	configurationSource := renderCICDTemplate(template.Spec.ConfigurationSource, inputs)
	if strings.TrimSpace(configurationSource) == "" {
		configurationSource = deploymentConfiguration
	}
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
	if err := decodeStrictCICDYAML(data, &registry); err != nil {
		return CICDEnvironmentRegistry{}, fmt.Errorf("parse environment registry: %w", err)
	}
	if len(registry.Environments) == 0 {
		return CICDEnvironmentRegistry{}, fmt.Errorf("environment registry has no environments")
	}
	if len(registry.VerificationProfiles) == 0 {
		return CICDEnvironmentRegistry{}, fmt.Errorf("environment registry has no verification profiles")
	}
	return registry, nil
}

func decodeStrictCICDYAML(data []byte, target interface{}) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("multiple YAML documents are not allowed")
	} else if err != io.EOF {
		return fmt.Errorf("parse trailing YAML document: %w", err)
	}
	return nil
}

func resolveDeploymentConfigurationPath(template DeploymentTemplate, source string) (string, error) {
	source = strings.TrimSpace(source)
	if source == "" {
		return "", fmt.Errorf("deployment configuration source is required")
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
			return "", fmt.Errorf("deployment template %q is not inside a Git repository", template.path)
		}
		dir = parent
	}

	resolved := source
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(root, resolved)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve deployment configuration source: %w", err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("validate deployment configuration source: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("deployment configuration source must stay inside repository root")
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return "", fmt.Errorf("resolve deployment configuration symlinks: %w", err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve repository root symlinks: %w", err)
	}
	relative, err = filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("validate deployment configuration source: %w", err)
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("deployment configuration source must stay inside repository root")
	}
	return resolved, nil
}

func validateDeploymentIntent(intent DeploymentTemplateSpec, allowTemplates bool) error {
	if strings.TrimSpace(intent.Application) == "" {
		return fmt.Errorf("spec.application is required")
	}
	if strings.TrimSpace(intent.Environment) == "" {
		return fmt.Errorf("spec.environment is required")
	}
	if !allowTemplates && hasUnresolvedCICDTemplate(intent.Application, intent.Environment, intent.ConfigurationSource) {
		return fmt.Errorf("deployment intent contains an unresolved template input")
	}
	return validateCICDReleaseReference(intent.Release, allowTemplates)
}

func validateDeploymentPlanSpec(spec DeploymentPlanSpec) error {
	if err := validateCICDReleaseReference(spec.Release, false); err != nil {
		return err
	}
	if strings.TrimSpace(spec.Target.Environment) == "" {
		return fmt.Errorf("spec.target.environment is required")
	}
	if strings.TrimSpace(spec.Target.ExecutionTarget) == "" {
		return fmt.Errorf("spec.target.executionTarget is required")
	}
	if strings.TrimSpace(spec.DesiredState.Application) == "" {
		return fmt.Errorf("spec.desiredState.application is required")
	}
	if strings.TrimSpace(spec.Acceptance.VerificationProfile) == "" {
		return fmt.Errorf("spec.acceptance.verificationProfile is required")
	}
	if len(normalizeEvidenceKinds(spec.Acceptance.RequiredEvidence)) == 0 {
		return fmt.Errorf("spec.acceptance.requiredEvidence is required")
	}
	return nil
}

func validateCICDReleaseReference(release CICDReleaseReference, allowTemplates bool) error {
	sourceSet := release.Source != nil
	manifestSet := release.Manifest != nil
	versionSet := strings.TrimSpace(release.Version) != ""
	if boolCount(sourceSet, manifestSet, versionSet) != 1 {
		return fmt.Errorf("release requires exactly one of source, manifest, or version")
	}
	if sourceSet {
		return validateCICDSourceRelease(*release.Source, allowTemplates)
	}
	if manifestSet {
		return validateCICDManifestRelease(*release.Manifest, allowTemplates)
	}
	if !allowTemplates && hasUnresolvedCICDTemplate(release.Version) {
		return fmt.Errorf("release.version contains an unresolved template input")
	}
	return nil
}

func validateCICDSourceRelease(source CICDSourceRelease, allowTemplates bool) error {
	if strings.TrimSpace(source.Repository) == "" {
		return fmt.Errorf("release.source.repository is required")
	}
	if strings.TrimSpace(source.Revision) == "" {
		return fmt.Errorf("release.source.revision is required")
	}
	if !allowTemplates {
		if hasUnresolvedCICDTemplate(source.Repository, source.Revision, source.Branch, source.TreeDigest) {
			return fmt.Errorf("release.source contains an unresolved template input")
		}
		if !immutableGitCommitPattern.MatchString(source.Revision) {
			return fmt.Errorf("release.source.revision must be a 40-character immutable Git commit")
		}
	}
	return nil
}

func validateCICDManifestRelease(manifest CICDManifestRelease, allowTemplates bool) error {
	if strings.TrimSpace(manifest.Repository) == "" {
		return fmt.Errorf("release.manifest.repository is required")
	}
	if strings.TrimSpace(manifest.Reference) == "" {
		return fmt.Errorf("release.manifest.reference is required")
	}
	if strings.TrimSpace(manifest.Digest) == "" {
		return fmt.Errorf("release.manifest.digest is required")
	}
	if !allowTemplates {
		if hasUnresolvedCICDTemplate(manifest.Repository, manifest.Reference, manifest.Digest) {
			return fmt.Errorf("release.manifest contains an unresolved template input")
		}
		if !ociDigestPattern.MatchString(manifest.Digest) {
			return fmt.Errorf("release.manifest.digest must be an OCI sha256 digest")
		}
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

func renderDeploymentIntent(intent DeploymentTemplateSpec, inputs map[string]string) DeploymentTemplateSpec {
	intent.Application = renderCICDTemplate(intent.Application, inputs)
	intent.Environment = renderCICDTemplate(intent.Environment, inputs)
	intent.ConfigurationSource = renderCICDTemplate(intent.ConfigurationSource, inputs)
	intent.Release = renderCICDReleaseReference(intent.Release, inputs)
	return intent
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
	release.Version = renderCICDTemplate(release.Version, inputs)
	return release
}

func validateCICDArtifactContract(release CICDReleaseReference, artifact CICDArtifactContract, executor CICDEnvironmentExecutor) error {
	switch artifact.Type {
	case cicdArtifactImageSet:
		if len(artifact.Services) == 0 {
			return fmt.Errorf("image-set artifact contract services are required")
		}
		if strings.TrimSpace(artifact.ImageTagPattern) == "" {
			return fmt.Errorf("image-set artifact contract image tag pattern is required")
		}
		tagPattern, err := regexp.Compile(artifact.ImageTagPattern)
		if err != nil {
			return fmt.Errorf("image-set artifact contract image tag pattern is invalid: %w", err)
		}
		if release.Version != "" {
			if strings.TrimSpace(artifact.SourceRegistry) == "" {
				return fmt.Errorf("version image-set artifact contract source registry is required")
			}
			if !tagPattern.MatchString(release.Version) {
				return fmt.Errorf("release.version %q does not match image tag pattern", release.Version)
			}
		} else if release.Source != nil {
			if strings.TrimSpace(artifact.SourceRepository) == "" {
				return fmt.Errorf("image-set artifact contract source repository is required")
			}
			if strings.TrimSpace(artifact.SourceBranch) == "" {
				return fmt.Errorf("image-set artifact contract source branch is required")
			}
			if artifact.SourceRepository != release.Source.Repository {
				return fmt.Errorf("artifact source repository does not match release source repository")
			}
			if artifact.SourceBranch != release.Source.Branch {
				return fmt.Errorf("artifact source branch does not match release source branch")
			}
		} else {
			return fmt.Errorf("image-set artifact contract requires a source or version release")
		}
		seenServices := make(map[string]bool, len(artifact.Services))
		for _, service := range artifact.Services {
			service = strings.TrimSpace(service)
			if service == "" {
				return fmt.Errorf("image-set artifact contract services must not contain empty names")
			}
			if seenServices[service] {
				return fmt.Errorf("image-set artifact contract service %q is duplicated", service)
			}
			seenServices[service] = true
		}
		if executor.Type == cicdExecutorHelm {
			if strings.TrimSpace(executor.Config.Registry) == "" {
				return fmt.Errorf("Helm image-set executor registry is required")
			}
			for _, service := range artifact.Services {
				if strings.TrimSpace(executor.Config.ImageBindings[service]) == "" {
					return fmt.Errorf("Helm image-set executor service %q is missing an image binding", service)
				}
			}
		}
		return nil
	case cicdArtifactManifest:
		if release.Manifest == nil {
			return fmt.Errorf("manifest artifact contract requires a manifest release")
		}
		if artifact.SourceRegistry != "" ||
			artifact.SourceRepository != "" ||
			artifact.SourceBranch != "" ||
			len(artifact.Services) > 0 ||
			artifact.ImageTagPattern != "" ||
			artifact.ImageTagTimeZone != "" ||
			artifact.ImageReferenceFormat != "" {
			return fmt.Errorf("manifest artifact contract must not contain image-set fields")
		}
		return nil
	default:
		return fmt.Errorf("unsupported artifact contract type %q", artifact.Type)
	}
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func validateCICDVerificationProfile(name string, profile CICDVerificationProfile) error {
	if len(normalizeEvidenceKinds(profile.RequiredEvidence)) == 0 {
		return fmt.Errorf("verification profile %q requiredEvidence is required", name)
	}
	return nil
}

func validateCICDEnvironmentProfile(name string, profile CICDEnvironmentProfile) error {
	requiredTarget := map[string]string{
		"target.name":     profile.Target.Name,
		"target.cluster":  profile.Target.Cluster,
		"target.instance": profile.Target.Instance,
	}
	for field, value := range requiredTarget {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("environment %q %s is required", name, field)
		}
	}
	if strings.TrimSpace(profile.VerificationProfile) == "" {
		return fmt.Errorf("environment %q verificationProfile is required", name)
	}
	switch profile.Executor.Type {
	case cicdExecutorHelm:
		required := map[string]string{
			"executor.config.namespace": profile.Executor.Config.Namespace,
			"executor.config.release":   profile.Executor.Config.Release,
			"executor.config.chart":     profile.Executor.Config.Chart,
			"executor.config.values":    profile.Executor.Config.Values,
		}
		for field, value := range required {
			if strings.TrimSpace(value) == "" {
				return fmt.Errorf("environment %q %s is required", name, field)
			}
		}
	default:
		return fmt.Errorf("environment %q has unsupported executor type %q", name, profile.Executor.Type)
	}
	for _, check := range profile.Executor.Config.HealthChecks.Public {
		if strings.TrimSpace(check.ID) == "" || strings.TrimSpace(check.URL) == "" || check.ExpectedStatus <= 0 {
			return fmt.Errorf("environment %q public health check requires id, url, and expected status", name)
		}
	}
	for _, check := range profile.Executor.Config.HealthChecks.Workloads {
		if strings.TrimSpace(check.Service) == "" || check.MinReadyReplicas < 1 {
			return fmt.Errorf("environment %q workload health check requires service and a positive ready replica count", name)
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
	if err := validateDeploymentPlanSpec(plan.Spec); err != nil {
		return err
	}
	if plan.Spec.Target.Profile == nil {
		return fmt.Errorf("deployment plan resolved environment profile is required")
	}
	profile := *plan.Spec.Target.Profile
	if plan.Spec.Target.ExecutionTarget != profile.Target.Name {
		return fmt.Errorf("deployment plan execution target does not match environment profile")
	}
	if plan.Spec.Acceptance.VerificationProfile != profile.VerificationProfile {
		return fmt.Errorf("deployment plan verification profile does not match environment profile")
	}
	if err := validateCICDEnvironmentProfile(plan.Spec.Target.Environment, profile); err != nil {
		return err
	}
	profileDigest, err := digestDeploymentValue(profile)
	if err != nil {
		return err
	}
	if plan.Spec.Target.ProfileDigest != profileDigest {
		return fmt.Errorf("deployment plan environment profile digest mismatch")
	}
	if err := validateCICDArtifactContract(plan.Spec.Release, plan.Spec.ArtifactContract, profile.Executor); err != nil {
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
	instance := strings.TrimSpace(server.Instance)
	if cluster == "" || instance == "" {
		return fmt.Errorf("configured target %q requires explicit cluster and instance bindings", server.Name)
	}
	if cluster != profile.Target.Cluster || instance != profile.Target.Instance {
		return fmt.Errorf(
			"configured target %q resolves to %s/%s, but deployment plan requires %s/%s",
			server.Name,
			cluster,
			instance,
			profile.Target.Cluster,
			profile.Target.Instance,
		)
	}
	return nil
}

func hasUnresolvedCICDTemplate(values ...string) bool {
	for _, value := range values {
		if strings.Contains(value, "${") {
			return true
		}
	}
	return false
}

func renderCICDTemplate(value string, inputs map[string]string) string {
	for name, input := range inputs {
		value = strings.ReplaceAll(value, "${inputs."+name+"}", input)
	}
	return value
}
