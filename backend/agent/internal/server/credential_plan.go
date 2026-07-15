package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	ErrCredentialDeclarationInvalid = errors.New("credential declaration is invalid")
	ErrInlineCredentialMaterial     = errors.New("inline credential material is forbidden")
)

type CredentialPlan struct {
	Template       string
	Project        string
	Environment    string
	Target         string
	Cluster        string
	Instance       string
	CredentialRefs []CredentialPlanReference
	BundleRefs     []string
}

type CredentialPlanReference struct {
	CredentialID       string
	Name               string
	Use                CredentialUse
	Namespace          string
	Workload           CredentialPlanWorkload
	RegistryRepository string
	RegistryReference  string
	RequiredKeys       []string
}

type CredentialPlanWorkload struct {
	Kind string
	Name string
}

type credentialDeploymentTemplate struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name        string `yaml:"name"`
		Description string `yaml:"description,omitempty"`
	} `yaml:"metadata"`
	Spec struct {
		Parameters           map[string]credentialTemplateParameter `yaml:"parameters,omitempty"`
		Application          string                                 `yaml:"application"`
		Release              credentialTemplateRelease              `yaml:"release,omitempty"`
		Environment          string                                 `yaml:"environment"`
		ConfigurationSource  string                                 `yaml:"configurationSource"`
		CredentialRefs       []credentialTemplateCredentialRef      `yaml:"credentialRefs,omitempty"`
		CredentialBundleRefs []credentialTemplateBundleRef          `yaml:"credentialBundleRefs,omitempty"`
	} `yaml:"spec"`
}

type credentialTemplateParameter struct {
	Required *bool `yaml:"required,omitempty"`
	Default  any   `yaml:"default,omitempty"`
}

type credentialTemplateRelease struct {
	Source   credentialTemplateReleaseSource   `yaml:"source,omitempty"`
	Manifest credentialTemplateReleaseManifest `yaml:"manifest,omitempty"`
}

type credentialTemplateReleaseSource struct {
	Repository string `yaml:"repository,omitempty"`
	Revision   string `yaml:"revision,omitempty"`
	Branch     string `yaml:"branch,omitempty"`
}

type credentialTemplateReleaseManifest struct {
	Repository string `yaml:"repository,omitempty"`
	Digest     string `yaml:"digest,omitempty"`
}

type credentialTemplateCredentialRef struct {
	Name               string                     `yaml:"name"`
	Use                CredentialUse              `yaml:"use"`
	Namespace          string                     `yaml:"namespace"`
	Workload           credentialTemplateWorkload `yaml:"workload"`
	RegistryRepository string                     `yaml:"registryRepository,omitempty"`
	RegistryReference  string                     `yaml:"registryReference,omitempty"`
}

type credentialTemplateWorkload struct {
	Kind string `yaml:"kind"`
	Name string `yaml:"name"`
}

type credentialTemplateBundleRef struct {
	Name string `yaml:"name"`
}

type credentialConfigurationSource struct {
	Environments map[string]credentialConfigurationEnvironment `yaml:"environments"`
}

type credentialConfigurationEnvironment struct {
	Target credentialConfigurationTarget `yaml:"target"`
}

type credentialConfigurationTarget struct {
	Name     string `yaml:"name"`
	Cluster  string `yaml:"cluster"`
	Instance string `yaml:"instance"`
}

func parseCredentialPlan(root, workflowPath string) (CredentialPlan, error) {
	workflowData, err := readCredentialDeclarationFile(root, workflowPath)
	if err != nil {
		return CredentialPlan{}, err
	}
	if _, err := credentialYAMLDocument(workflowData); err != nil {
		return CredentialPlan{}, err
	}

	var template credentialDeploymentTemplate
	if err := strictCredentialYAMLDecode(workflowData, &template); err != nil {
		return CredentialPlan{}, invalidCredentialDeclaration(err)
	}
	if template.APIVersion != "doops.sh/v2" || template.Kind != "DeploymentTemplate" ||
		strings.TrimSpace(template.Metadata.Name) == "" ||
		strings.TrimSpace(template.Spec.Application) == "" ||
		strings.TrimSpace(template.Spec.Environment) == "" ||
		strings.TrimSpace(template.Spec.ConfigurationSource) == "" {
		return CredentialPlan{}, invalidCredentialDeclaration(errors.New("invalid DeploymentTemplate identity or deployment context"))
	}

	configurationData, err := readCredentialDeclarationFile(root, template.Spec.ConfigurationSource)
	if err != nil {
		return CredentialPlan{}, err
	}
	if _, err := credentialYAMLDocument(configurationData); err != nil {
		return CredentialPlan{}, err
	}

	var configuration credentialConfigurationSource
	if err := yaml.Unmarshal(configurationData, &configuration); err != nil {
		return CredentialPlan{}, invalidCredentialDeclaration(err)
	}
	environment, ok := configuration.Environments[template.Spec.Environment]
	if !ok {
		return CredentialPlan{}, invalidCredentialDeclaration(fmt.Errorf("environment %q is not declared", template.Spec.Environment))
	}
	target := environment.Target
	if strings.TrimSpace(target.Name) == "" || strings.TrimSpace(target.Cluster) == "" || strings.TrimSpace(target.Instance) == "" {
		return CredentialPlan{}, invalidCredentialDeclaration(fmt.Errorf("environment %q has an incomplete target", template.Spec.Environment))
	}

	refs := make([]CredentialPlanReference, 0, len(template.Spec.CredentialRefs))
	for _, ref := range template.Spec.CredentialRefs {
		if strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Namespace) == "" || !validCredentialUse(ref.Use) {
			return CredentialPlan{}, invalidCredentialDeclaration(errors.New("credential reference is incomplete or invalid"))
		}
		if ref.Use == CredentialUseImagePull &&
			(strings.TrimSpace(ref.RegistryRepository) == "" || strings.TrimSpace(ref.RegistryReference) == "") {
			return CredentialPlan{}, invalidCredentialDeclaration(errors.New("registry credential reference requires repository and reference"))
		}
		refs = append(refs, CredentialPlanReference{
			Name:               ref.Name,
			Use:                ref.Use,
			Namespace:          ref.Namespace,
			RegistryRepository: strings.TrimSpace(ref.RegistryRepository),
			RegistryReference:  strings.TrimSpace(ref.RegistryReference),
			Workload: CredentialPlanWorkload{
				Kind: ref.Workload.Kind,
				Name: ref.Workload.Name,
			},
		})
	}

	bundles := make([]string, 0, len(template.Spec.CredentialBundleRefs))
	for _, ref := range template.Spec.CredentialBundleRefs {
		if strings.TrimSpace(ref.Name) == "" {
			return CredentialPlan{}, invalidCredentialDeclaration(errors.New("credential bundle reference name is required"))
		}
		bundles = append(bundles, ref.Name)
	}

	return CredentialPlan{
		Template:       template.Metadata.Name,
		Project:        template.Spec.Application,
		Environment:    template.Spec.Environment,
		Target:         target.Name,
		Cluster:        target.Cluster,
		Instance:       target.Instance,
		CredentialRefs: refs,
		BundleRefs:     bundles,
	}, nil
}

func parseCredentialPlanDocuments(workflowPath string, workflowData, configurationData []byte) (CredentialPlan, error) {
	if _, err := credentialYAMLDocument(workflowData); err != nil {
		return CredentialPlan{}, err
	}
	var template credentialDeploymentTemplate
	if err := strictCredentialYAMLDecode(workflowData, &template); err != nil {
		return CredentialPlan{}, invalidCredentialDeclaration(err)
	}
	workflowRelative, err := credentialDeclarationRelativePath(workflowPath)
	if err != nil {
		return CredentialPlan{}, err
	}
	configurationRelative, err := credentialDeclarationRelativePath(template.Spec.ConfigurationSource)
	if err != nil {
		return CredentialPlan{}, err
	}
	root, err := os.MkdirTemp("", "doops-credential-plan-*")
	if err != nil {
		return CredentialPlan{}, invalidCredentialDeclaration(err)
	}
	defer os.RemoveAll(root)
	for path, data := range map[string][]byte{
		workflowRelative:      workflowData,
		configurationRelative: configurationData,
	} {
		absolute := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(absolute), 0700); err != nil {
			return CredentialPlan{}, invalidCredentialDeclaration(err)
		}
		if err := os.WriteFile(absolute, data, 0600); err != nil {
			return CredentialPlan{}, invalidCredentialDeclaration(err)
		}
	}
	return parseCredentialPlan(root, filepath.ToSlash(workflowRelative))
}

func credentialDeclarationRelativePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", invalidCredentialDeclaration(errors.New("declaration path is required"))
	}
	relativePath := filepath.FromSlash(path)
	if filepath.IsAbs(relativePath) {
		return "", invalidCredentialDeclaration(fmt.Errorf("declaration path %q is absolute", path))
	}
	cleanPath := filepath.Clean(relativePath)
	if cleanPath == "." || cleanPath == ".." || strings.HasPrefix(cleanPath, ".."+string(filepath.Separator)) {
		return "", invalidCredentialDeclaration(fmt.Errorf("declaration path %q escapes the repository", path))
	}
	return cleanPath, nil
}

func readCredentialDeclarationFile(root, path string) ([]byte, error) {
	cleanPath, err := credentialDeclarationRelativePath(path)
	if err != nil {
		return nil, err
	}

	absoluteRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, invalidCredentialDeclaration(fmt.Errorf("resolve repository root: %w", err))
	}
	resolvedRoot, err := filepath.EvalSymlinks(absoluteRoot)
	if err != nil {
		return nil, invalidCredentialDeclaration(fmt.Errorf("resolve repository root: %w", err))
	}
	resolvedPath, err := filepath.EvalSymlinks(filepath.Join(resolvedRoot, cleanPath))
	if err != nil {
		return nil, invalidCredentialDeclaration(fmt.Errorf("resolve declaration path %q: %w", path, err))
	}
	relativeResolvedPath, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil || relativeResolvedPath == ".." || strings.HasPrefix(relativeResolvedPath, ".."+string(filepath.Separator)) {
		return nil, invalidCredentialDeclaration(fmt.Errorf("declaration path %q escapes the repository", path))
	}

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return nil, invalidCredentialDeclaration(fmt.Errorf("read declaration path %q: %w", path, err))
	}
	return data, nil
}

func credentialYAMLDocument(data []byte) (*yaml.Node, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, invalidCredentialDeclaration(err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, invalidCredentialDeclaration(errors.New("multiple YAML documents are not allowed"))
		}
		return nil, invalidCredentialDeclaration(err)
	}
	if err := auditCredentialYAMLNode(&document, make(map[*yaml.Node]struct{})); err != nil {
		return nil, err
	}
	return &document, nil
}

func auditCredentialYAMLNode(node *yaml.Node, visited map[*yaml.Node]struct{}) error {
	if node == nil {
		return nil
	}
	if _, ok := visited[node]; ok {
		return nil
	}
	visited[node] = struct{}{}

	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := auditCredentialYAMLNode(child, visited); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		if len(node.Content)%2 != 0 {
			return invalidCredentialDeclaration(errors.New("invalid YAML mapping"))
		}
		keys := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key, value := node.Content[index], node.Content[index+1]
			if key.Kind != yaml.ScalarNode {
				return invalidCredentialDeclaration(errors.New("YAML mapping keys must be scalars"))
			}
			if _, duplicate := keys[key.Value]; duplicate {
				return invalidCredentialDeclaration(fmt.Errorf("duplicate YAML mapping key %q", key.Value))
			}
			keys[key.Value] = struct{}{}
			if key.Value == "<<" {
				return invalidCredentialDeclaration(errors.New("YAML merge keys are not allowed"))
			}
			if credentialSecretLikeKeys[strings.ToLower(strings.TrimSpace(key.Value))] {
				return fmt.Errorf("%w: YAML key %q is not allowed", ErrInlineCredentialMaterial, key.Value)
			}
			if err := auditCredentialYAMLNode(key, visited); err != nil {
				return err
			}
			if err := auditCredentialYAMLNode(value, visited); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return auditCredentialYAMLNode(node.Alias, visited)
	}
	return nil
}

func strictCredentialYAMLDecode(data []byte, target any) error {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple YAML documents are not allowed")
		}
		return err
	}
	return nil
}

func invalidCredentialDeclaration(err error) error {
	return fmt.Errorf("%w: %v", ErrCredentialDeclarationInvalid, err)
}

var credentialSecretLikeKeys = map[string]bool{
	"token":      true,
	"password":   true,
	"auth":       true,
	"data":       true,
	"stringdata": true,
}
