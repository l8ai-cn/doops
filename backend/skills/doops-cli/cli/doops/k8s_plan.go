package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	k8sPlanAPIVersion     = "doops.sh/v1"
	k8sPlanKind           = "K8SChangePlan"
	k8sRunbookVersionV1   = "v1"
	k8sDeployImageRunbook = "deploy-image"
)

type k8sCaller interface {
	Call(toolName string, arguments map[string]interface{}) error
}

type K8SChangePlan struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   K8SPlanMetadata `yaml:"metadata"`
	Spec       K8SPlanSpec     `yaml:"spec"`
}

type K8SPlanMetadata struct {
	Name           string `yaml:"name"`
	Target         string `yaml:"target"`
	CreatedAt      string `yaml:"createdAt"`
	Runbook        string `yaml:"runbook"`
	RunbookVersion string `yaml:"runbookVersion"`
	Revision       int    `yaml:"revision"`
}

type K8SPlanSpec struct {
	Namespace string        `yaml:"namespace"`
	Steps     []K8SPlanStep `yaml:"steps"`
	Checks    []K8SPlanStep `yaml:"checks,omitempty"`
}

type K8SPlanStep struct {
	ID        string `yaml:"id"`
	Operation string `yaml:"operation"`
	Resource  string `yaml:"resource,omitempty"`
	Kind      string `yaml:"kind,omitempty"`
	Name      string `yaml:"name,omitempty"`
	Container string `yaml:"container,omitempty"`
	Image     string `yaml:"image,omitempty"`
	Replicas  *int   `yaml:"replicas,omitempty"`
	Timeout   string `yaml:"timeout,omitempty"`
}

func newK8SDeployImagePlan(req K8SRequest, now time.Time) (K8SChangePlan, error) {
	if req.Payload["operation"] != "plan-set-image" {
		return K8SChangePlan{}, fmt.Errorf("plan deploy-image requires plan-set-image operation")
	}
	resource, _ := req.Payload["resource"].(string)
	container, _ := req.Payload["container"].(string)
	image, _ := req.Payload["image"].(string)
	namespace, _ := req.Payload["namespace"].(string)
	if strings.TrimSpace(resource) == "" || strings.TrimSpace(container) == "" || strings.TrimSpace(image) == "" || strings.TrimSpace(namespace) == "" {
		return K8SChangePlan{}, fmt.Errorf("plan deploy-image requires resource, --namespace, --container and --image")
	}

	return K8SChangePlan{
		APIVersion: k8sPlanAPIVersion,
		Kind:       k8sPlanKind,
		Metadata: K8SPlanMetadata{
			Name:           planNameForResource(resource),
			Target:         req.Target,
			CreatedAt:      now.UTC().Format(time.RFC3339),
			Runbook:        k8sDeployImageRunbook,
			RunbookVersion: k8sRunbookVersionV1,
			Revision:       1,
		},
		Spec: K8SPlanSpec{
			Namespace: namespace,
			Steps: []K8SPlanStep{{
				ID:        "set-image",
				Operation: "set-image",
				Resource:  resource,
				Container: container,
				Image:     image,
			}},
			Checks: []K8SPlanStep{{
				ID:        "rollout-status",
				Operation: "rollout-status",
				Resource:  resource,
				Timeout:   "5m",
			}},
		},
	}, nil
}

func writeK8SChangePlan(path string, plan K8SChangePlan) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("plan output path is required")
	}
	if err := validateK8SChangePlan(plan); err != nil {
		return err
	}
	data, err := yaml.Marshal(plan)
	if err != nil {
		return fmt.Errorf("marshal plan: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create plan directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}
	return nil
}

func readK8SChangePlan(path string) (K8SChangePlan, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return K8SChangePlan{}, fmt.Errorf("read plan: %w", err)
	}
	var plan K8SChangePlan
	if err := yaml.Unmarshal(data, &plan); err != nil {
		return K8SChangePlan{}, fmt.Errorf("parse plan: %w", err)
	}
	if err := validateK8SChangePlan(plan); err != nil {
		return K8SChangePlan{}, err
	}
	return plan, nil
}

func validateK8SChangePlan(plan K8SChangePlan) error {
	if plan.APIVersion != k8sPlanAPIVersion {
		return fmt.Errorf("unsupported plan apiVersion %q", plan.APIVersion)
	}
	if plan.Kind != k8sPlanKind {
		return fmt.Errorf("unsupported plan kind %q", plan.Kind)
	}
	if plan.Metadata.RunbookVersion != k8sRunbookVersionV1 {
		return fmt.Errorf("unsupported runbookVersion %q", plan.Metadata.RunbookVersion)
	}
	if strings.TrimSpace(plan.Metadata.Target) == "" {
		return fmt.Errorf("plan metadata.target is required")
	}
	if strings.TrimSpace(plan.Spec.Namespace) == "" {
		return fmt.Errorf("plan spec.namespace is required")
	}
	if len(plan.Spec.Steps) == 0 {
		return fmt.Errorf("plan spec.steps is required")
	}
	for _, step := range plan.Spec.Steps {
		if err := validateK8SPlanStep(step); err != nil {
			return err
		}
	}
	for _, check := range plan.Spec.Checks {
		if err := validateK8SPlanCheck(check); err != nil {
			return err
		}
	}
	return nil
}

func validateK8SPlanStep(step K8SPlanStep) error {
	switch step.Operation {
	case "set-image":
		if strings.TrimSpace(step.Resource) == "" || strings.TrimSpace(step.Container) == "" || strings.TrimSpace(step.Image) == "" {
			return fmt.Errorf("set-image step requires resource, container and image")
		}
	case "rollout-restart":
		if strings.TrimSpace(step.Resource) == "" {
			return fmt.Errorf("rollout-restart step requires resource")
		}
	case "scale":
		if strings.TrimSpace(step.Resource) == "" || step.Replicas == nil || *step.Replicas < 0 {
			return fmt.Errorf("scale step requires resource and non-negative replicas")
		}
	default:
		return fmt.Errorf("unsupported plan step operation %q", step.Operation)
	}
	return nil
}

func validateK8SPlanCheck(step K8SPlanStep) error {
	switch step.Operation {
	case "rollout-status":
		if strings.TrimSpace(step.Resource) == "" {
			return fmt.Errorf("rollout-status check requires resource")
		}
	case "get":
		if strings.TrimSpace(step.Kind) == "" {
			return fmt.Errorf("get check requires kind")
		}
	default:
		return fmt.Errorf("unsupported plan check operation %q", step.Operation)
	}
	return nil
}

func applyK8SChangePlan(caller k8sCaller, target string, plan K8SChangePlan, confirm bool) error {
	if caller == nil {
		return fmt.Errorf("k8s caller is required")
	}
	if err := validateK8SChangePlan(plan); err != nil {
		return err
	}
	if !confirm {
		return fmt.Errorf("apply-plan requires --confirm")
	}
	if plan.Metadata.Target != target {
		return fmt.Errorf("plan target %q does not match requested target %q", plan.Metadata.Target, target)
	}
	for _, step := range plan.Spec.Steps {
		args := k8sPlanStepArguments(plan.Spec.Namespace, step)
		args["confirm"] = true
		if err := caller.Call("doops_k8s", args); err != nil {
			return fmt.Errorf("apply step %s: %w", step.ID, err)
		}
	}
	for _, check := range plan.Spec.Checks {
		if err := caller.Call("doops_k8s", k8sPlanStepArguments(plan.Spec.Namespace, check)); err != nil {
			return fmt.Errorf("run check %s: %w", check.ID, err)
		}
	}
	return nil
}

func k8sPlanStepArguments(namespace string, step K8SPlanStep) map[string]interface{} {
	args := map[string]interface{}{
		"operation": step.Operation,
		"namespace": namespace,
	}
	if step.Resource != "" {
		args["resource"] = step.Resource
	}
	if step.Kind != "" {
		args["kind"] = step.Kind
	}
	if step.Name != "" {
		args["name"] = step.Name
	}
	if step.Container != "" {
		args["container"] = step.Container
	}
	if step.Image != "" {
		args["image"] = step.Image
	}
	if step.Replicas != nil {
		args["replicas"] = *step.Replicas
	}
	if step.Timeout != "" {
		args["timeout"] = step.Timeout
	}
	return args
}

func planNameForResource(resource string) string {
	name := strings.TrimSpace(resource)
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "_", "-")
	if name == "" {
		return "k8s-change"
	}
	return name
}
