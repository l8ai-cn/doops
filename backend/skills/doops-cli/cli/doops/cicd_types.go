package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	cicdAPIVersion = "doops.sh/v1"
	cicdKind       = "Workflow"
)

type CICDWorkflow struct {
	APIVersion string           `json:"apiVersion" yaml:"apiVersion"`
	Kind       string           `json:"kind" yaml:"kind"`
	Metadata   CICDMetadata     `json:"metadata" yaml:"metadata"`
	Spec       CICDWorkflowSpec `json:"spec" yaml:"spec"`
	path       string
}

type CICDMetadata struct {
	Name string `json:"name" yaml:"name"`
}

type CICDWorkflowSpec struct {
	Inputs       map[string]CICDInput `json:"inputs,omitempty" yaml:"inputs,omitempty"`
	Policy       CICDPolicy           `json:"policy,omitempty" yaml:"policy,omitempty"`
	Source       CICDSource           `json:"source" yaml:"source"`
	Environments []CICDEnvironment    `json:"environments,omitempty" yaml:"environments,omitempty"`
	Locks        []CICDLock           `json:"locks,omitempty" yaml:"locks,omitempty"`
	Stages       []CICDStage          `json:"stages" yaml:"stages"`
	// Shared, free-form context handed to the doagent with every agent-native
	// stage. This is where environment facts live (image mirrors, proxies,
	// source-transfer quirks, build matrix, etc.) so the agent — which owns the
	// HOW — has the knowledge it needs. Captured as raw YAML subtrees.
	Context  yaml.Node `json:"-" yaml:"context,omitempty"`
	BuildEnv yaml.Node `json:"-" yaml:"buildEnv,omitempty"`
}

type CICDInput struct {
	Required bool   `json:"required,omitempty" yaml:"required,omitempty"`
	Default  string `json:"default,omitempty" yaml:"default,omitempty"`
}

type CICDPolicy struct {
	AgentNative         bool     `json:"agentNative,omitempty" yaml:"agentNative,omitempty"`
	MaxToolScripts      int      `json:"maxToolScripts,omitempty" yaml:"maxToolScripts,omitempty"`
	ToolScriptDir       string   `json:"toolScriptDir,omitempty" yaml:"toolScriptDir,omitempty"`
	RequiredDocSections []string `json:"requiredDocSections,omitempty" yaml:"requiredDocSections,omitempty"`
}

type CICDSource struct {
	Path               string `json:"path,omitempty" yaml:"path,omitempty"`
	Repo               string `json:"repo,omitempty" yaml:"repo,omitempty"`
	Branch             string `json:"branch,omitempty" yaml:"branch,omitempty"`
	RequireCleanCommit bool   `json:"requireCleanCommit,omitempty" yaml:"requireCleanCommit,omitempty"`
}

type CICDEnvironment struct {
	Name          string            `json:"name" yaml:"name"`
	Target        string            `json:"target" yaml:"target"`
	Namespace     string            `json:"namespace" yaml:"namespace"`
	Release       string            `json:"release" yaml:"release"`
	DeploymentDoc string            `json:"deploymentDoc" yaml:"deploymentDoc"`
	Services      []string          `json:"services" yaml:"services"`
	Values        map[string]string `json:"values,omitempty" yaml:"values,omitempty"`
}

type CICDLock struct {
	Key           string `json:"key" yaml:"key"`
	Wait          bool   `json:"wait,omitempty" yaml:"wait,omitempty"`
	CancelWaiting bool   `json:"cancelWaiting,omitempty" yaml:"cancelWaiting,omitempty"`
	CancelRunning bool   `json:"cancelRunning,omitempty" yaml:"cancelRunning,omitempty"`
}

type CICDStage struct {
	ID      string            `json:"id" yaml:"id"`
	Name    string            `json:"name,omitempty" yaml:"name,omitempty"`
	Uses    string            `json:"uses" yaml:"uses"`
	Run     string            `json:"run,omitempty" yaml:"run,omitempty"`
	Workdir string            `json:"workdir,omitempty" yaml:"workdir,omitempty"`
	With    map[string]string `json:"with,omitempty" yaml:"with,omitempty"`
	Mutates bool              `json:"mutates,omitempty" yaml:"mutates,omitempty"`
	Confirm bool              `json:"confirm,omitempty" yaml:"confirm,omitempty"`
}

type CICDPlan struct {
	Name         string            `json:"name"`
	Inputs       map[string]string `json:"inputs"`
	Policy       CICDPolicy        `json:"policy,omitempty"`
	Source       CICDSource        `json:"source"`
	Environments []CICDEnvironment `json:"environments,omitempty"`
	Locks        []CICDLock        `json:"locks"`
	Stages       []CICDPlanStage   `json:"stages"`
	// Context is the rendered, shared workflow context (buildEnv + context)
	// injected into every agent-native stage instruction.
	Context string `json:"context,omitempty"`
}

type CICDPlanStage struct {
	ID      string            `json:"id"`
	Name    string            `json:"name,omitempty"`
	Uses    string            `json:"uses"`
	Run     string            `json:"run,omitempty"`
	Workdir string            `json:"workdir,omitempty"`
	With    map[string]string `json:"with,omitempty"`
	Mutates bool              `json:"mutates,omitempty"`
	Confirm bool              `json:"confirm,omitempty"`
}

func loadCICDWorkflow(path string) (CICDWorkflow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CICDWorkflow{}, fmt.Errorf("read workflow: %w", err)
	}
	var workflow CICDWorkflow
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		return CICDWorkflow{}, fmt.Errorf("parse workflow: %w", err)
	}
	workflow.path = path
	if err := validateCICDWorkflow(workflow); err != nil {
		return CICDWorkflow{}, err
	}
	return workflow, nil
}

func validateCICDWorkflow(workflow CICDWorkflow) error {
	if workflow.APIVersion != cicdAPIVersion {
		return fmt.Errorf("unsupported apiVersion %q", workflow.APIVersion)
	}
	if workflow.Kind != cicdKind {
		return fmt.Errorf("unsupported kind %q", workflow.Kind)
	}
	if strings.TrimSpace(workflow.Metadata.Name) == "" {
		return fmt.Errorf("metadata.name is required")
	}
	if strings.TrimSpace(workflow.Spec.Source.Path) == "" && strings.TrimSpace(workflow.Spec.Source.Repo) == "" {
		return fmt.Errorf("spec.source.path or spec.source.repo is required")
	}
	if len(workflow.Spec.Stages) == 0 {
		return fmt.Errorf("spec.stages is required")
	}
	if err := validateCICDEnvironments(workflow.Spec.Environments); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, stage := range workflow.Spec.Stages {
		id := strings.TrimSpace(stage.ID)
		if id == "" {
			return fmt.Errorf("stage id is required")
		}
		if seen[id] {
			return fmt.Errorf("duplicate stage id %q", id)
		}
		seen[id] = true
		if !isSupportedCICDStageUse(stage.Uses) {
			return fmt.Errorf("unsupported stage uses %q in stage %s", stage.Uses, id)
		}
		if stage.Uses == "shell" && strings.TrimSpace(stage.Run) == "" {
			return fmt.Errorf("shell stage %s requires run", id)
		}
		if stage.Mutates && !stage.Confirm {
			return fmt.Errorf("mutating stage %s requires confirm", id)
		}
	}
	if err := validateCICDAgentNativePolicy(workflow.Spec.Policy, workflow.Spec.Stages); err != nil {
		return err
	}
	if workflow.path != "" {
		if err := reviewCICDDeploymentDocs(workflow); err != nil {
			return err
		}
	}
	return nil
}

func buildCICDPlan(workflow CICDWorkflow, overrides map[string]string) (CICDPlan, error) {
	if err := validateCICDWorkflow(workflow); err != nil {
		return CICDPlan{}, err
	}
	inputs, err := resolveCICDInputs(workflow.Spec.Inputs, overrides)
	if err != nil {
		return CICDPlan{}, err
	}
	locks := make([]CICDLock, 0, len(workflow.Spec.Locks))
	for _, lock := range workflow.Spec.Locks {
		lock.Key = renderCICDTemplate(lock.Key, inputs)
		locks = append(locks, lock)
	}
	stages := make([]CICDPlanStage, 0, len(workflow.Spec.Stages))
	for _, stage := range workflow.Spec.Stages {
		stages = append(stages, CICDPlanStage{
			ID:      stage.ID,
			Name:    stage.Name,
			Uses:    stage.Uses,
			Run:     renderCICDTemplate(stage.Run, inputs),
			Workdir: renderCICDTemplate(stage.Workdir, inputs),
			With:    renderCICDMap(stage.With, inputs),
			Mutates: stage.Mutates,
			Confirm: stage.Confirm,
		})
	}
	source := workflow.Spec.Source
	source.Path = renderCICDTemplate(source.Path, inputs)
	source.Repo = renderCICDTemplate(source.Repo, inputs)
	source.Branch = renderCICDTemplate(source.Branch, inputs)
	if source.Path != "" {
		if abs, err := filepath.Abs(source.Path); err == nil {
			source.Path = abs
		}
	}
	return CICDPlan{
		Name:         workflow.Metadata.Name,
		Inputs:       inputs,
		Policy:       workflow.Spec.Policy,
		Source:       source,
		Environments: renderCICDEnvironments(workflow.Spec.Environments, inputs),
		Locks:        locks,
		Stages:       stages,
		Context:      renderCICDWorkflowContext(workflow.Spec, inputs),
	}, nil
}

// renderCICDWorkflowContext serializes the workflow's shared context blocks
// (spec.buildEnv and spec.context) back to YAML text and renders ${inputs.*}
// templates over it. This text is handed to the doagent so it has the
// environment facts required to drive each stage.
func renderCICDWorkflowContext(spec CICDWorkflowSpec, inputs map[string]string) string {
	var sb strings.Builder
	appendBlock := func(name string, node yaml.Node) {
		if node.Kind == 0 {
			return
		}
		out, err := yaml.Marshal(&node)
		if err != nil {
			return
		}
		body := strings.TrimRight(string(out), "\n")
		if strings.TrimSpace(body) == "" {
			return
		}
		sb.WriteString(name + ":\n")
		for _, line := range strings.Split(body, "\n") {
			sb.WriteString("  " + line + "\n")
		}
	}
	appendBlock("buildEnv", spec.BuildEnv)
	appendBlock("context", spec.Context)
	return renderCICDTemplate(sb.String(), inputs)
}

func validateCICDEnvironments(environments []CICDEnvironment) error {
	if len(environments) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var baselineName string
	var baselineServices []string
	for _, env := range environments {
		name := strings.TrimSpace(env.Name)
		if name == "" {
			return fmt.Errorf("environment name is required")
		}
		if seen[name] {
			return fmt.Errorf("duplicate environment %q", name)
		}
		seen[name] = true
		if strings.TrimSpace(env.Target) == "" {
			return fmt.Errorf("environment %s target is required", name)
		}
		if strings.TrimSpace(env.Namespace) == "" {
			return fmt.Errorf("environment %s namespace is required", name)
		}
		if strings.TrimSpace(env.Release) == "" {
			return fmt.Errorf("environment %s release is required", name)
		}
		if strings.TrimSpace(env.DeploymentDoc) == "" {
			return fmt.Errorf("environment %s deploymentDoc is required", name)
		}
		services := normalizeCICDServices(env.Services)
		if len(services) == 0 {
			return fmt.Errorf("environment %s services is required", name)
		}
		if baselineServices == nil {
			baselineName = name
			baselineServices = services
			continue
		}
		if !equalStringSlices(services, baselineServices) {
			return fmt.Errorf("environment %s services %v differ from %s services %v", name, services, baselineName, baselineServices)
		}
	}
	return nil
}

func validateCICDAgentNativePolicy(policy CICDPolicy, stages []CICDStage) error {
	if !policy.AgentNative {
		return nil
	}
	toolScriptCount := 0
	for _, stage := range stages {
		id := strings.TrimSpace(stage.ID)
		if strings.TrimSpace(stage.Run) != "" {
			if stage.Uses != "shell" {
				return fmt.Errorf("agent-native stage %s must use structured with, not run", id)
			}
			toolScriptCount++
		}
		if stage.Mutates && stage.Uses == "shell" {
			return fmt.Errorf("agent-native mutating stage %s cannot use shell", id)
		}
		switch stage.Uses {
		case "agent.task", "doops.k8s", "doops.exec", "http.check":
			if len(stage.With) == 0 {
				return fmt.Errorf("agent-native stage %s requires structured with", id)
			}
			if strings.TrimSpace(stage.With["script"]) != "" {
				return fmt.Errorf("agent-native stage %s must not use with.script", id)
			}
		}
	}
	if toolScriptCount > policy.MaxToolScripts {
		return fmt.Errorf("agent-native workflow allows at most %d tool scripts, found %d", policy.MaxToolScripts, toolScriptCount)
	}
	return nil
}

func reviewCICDDeploymentDocs(workflow CICDWorkflow) error {
	required := workflow.Spec.Policy.RequiredDocSections
	if len(required) == 0 {
		return nil
	}
	for _, env := range workflow.Spec.Environments {
		docPath, err := resolveCICDDeploymentDocPath(workflow, env)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(docPath)
		if err != nil {
			return fmt.Errorf("read deployment doc %q: %w", env.DeploymentDoc, err)
		}
		body := string(data)
		for _, section := range required {
			section = strings.TrimSpace(section)
			if section == "" {
				continue
			}
			if !cicdDocHasSection(body, section) {
				return fmt.Errorf("deployment doc %q missing section %q", env.DeploymentDoc, section)
			}
		}
	}
	return nil
}

func resolveCICDDeploymentDocPath(workflow CICDWorkflow, env CICDEnvironment) (string, error) {
	doc := strings.TrimSpace(env.DeploymentDoc)
	if doc == "" {
		return "", fmt.Errorf("environment %s deploymentDoc is required", env.Name)
	}
	if filepath.IsAbs(doc) {
		return doc, nil
	}
	workflowDir := filepath.Dir(workflow.path)
	workflowDoc := filepath.Join(workflowDir, doc)
	if _, err := os.Stat(workflowDoc); err == nil {
		return workflowDoc, nil
	}
	base := strings.TrimSpace(workflow.Spec.Source.Repo)
	if base == "" || strings.Contains(base, "${inputs.") || strings.Contains(base, "://") {
		base = workflowDir
	} else if !filepath.IsAbs(base) {
		base = filepath.Join(workflowDir, base)
	}
	return filepath.Join(base, doc), nil
}

func cicdDocHasSection(body string, section string) bool {
	want := strings.ToLower(strings.TrimSpace(section))
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "#") {
			continue
		}
		title := strings.TrimSpace(strings.TrimLeft(line, "#"))
		if strings.ToLower(title) == want {
			return true
		}
	}
	return false
}

func normalizeCICDServices(services []string) []string {
	out := make([]string, 0, len(services))
	seen := map[string]bool{}
	for _, service := range services {
		service = strings.TrimSpace(service)
		if service == "" || seen[service] {
			continue
		}
		seen[service] = true
		out = append(out, service)
	}
	sort.Strings(out)
	return out
}

func equalStringSlices(a []string, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func resolveCICDInputs(spec map[string]CICDInput, overrides map[string]string) (map[string]string, error) {
	resolved := map[string]string{}
	for key, input := range spec {
		if input.Default != "" {
			resolved[key] = input.Default
		}
	}
	for key, value := range overrides {
		resolved[key] = value
	}
	for key, input := range spec {
		if input.Required && strings.TrimSpace(resolved[key]) == "" {
			return nil, fmt.Errorf("input %s is required", key)
		}
	}
	return resolved, nil
}

func isSupportedCICDStageUse(uses string) bool {
	switch strings.TrimSpace(uses) {
	case "shell", "git.clone", "git.update", "doops.exec", "doops.k8s", "agent.task", "approval", "http.check":
		return true
	default:
		return false
	}
}

func renderCICDTemplate(value string, inputs map[string]string) string {
	out := value
	for key, input := range inputs {
		out = strings.ReplaceAll(out, "${inputs."+key+"}", input)
	}
	return out
}

func renderCICDMap(values map[string]string, inputs map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = renderCICDTemplate(value, inputs)
	}
	return out
}

func renderCICDEnvironments(environments []CICDEnvironment, inputs map[string]string) []CICDEnvironment {
	if len(environments) == 0 {
		return nil
	}
	out := make([]CICDEnvironment, 0, len(environments))
	for _, env := range environments {
		env.Name = renderCICDTemplate(env.Name, inputs)
		env.Target = renderCICDTemplate(env.Target, inputs)
		env.Namespace = renderCICDTemplate(env.Namespace, inputs)
		env.Release = renderCICDTemplate(env.Release, inputs)
		env.DeploymentDoc = renderCICDTemplate(env.DeploymentDoc, inputs)
		env.Values = renderCICDMap(env.Values, inputs)
		services := make([]string, 0, len(env.Services))
		for _, service := range env.Services {
			services = append(services, renderCICDTemplate(service, inputs))
		}
		env.Services = services
		out = append(out, env)
	}
	return out
}
