package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestBuildK8SRequestGetPods(t *testing.T) {
	req, err := buildK8SRequest([]string{"get", "pods", "--target", "prod", "--namespace", "oilan-system", "--output", "json"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if req.Target != "prod" {
		t.Fatalf("target mismatch: %q", req.Target)
	}
	want := map[string]interface{}{
		"operation": "get",
		"kind":      "pods",
		"namespace": "oilan-system",
		"output":    "json",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestLogs(t *testing.T) {
	req, err := buildK8SRequest([]string{"logs", "deploy/exam-api", "--target", "prod", "--namespace", "oilan-system", "--tail", "100"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "logs",
		"resource":  "deploy/exam-api",
		"namespace": "oilan-system",
		"tail":      100,
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestTopNodes(t *testing.T) {
	req, err := buildK8SRequest([]string{"top", "nodes", "--target", "prod"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "top",
		"kind":      "nodes",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestRolloutRestart(t *testing.T) {
	req, err := buildK8SRequest([]string{"rollout", "restart", "deploy/exam-api", "--target", "prod", "--namespace", "oilan-system"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "rollout-restart",
		"resource":  "deploy/exam-api",
		"namespace": "oilan-system",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestDeployImage(t *testing.T) {
	req, err := buildK8SRequest([]string{
		"deploy-image",
		"deploy/exam-api",
		"--container", "app",
		"--image", "registry.example.com/example/exam-api:v2",
		"--target", "prod",
		"--namespace", "oilan-system",
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	want := map[string]interface{}{
		"operation": "set-image",
		"resource":  "deploy/exam-api",
		"container": "app",
		"image":     "registry.example.com/example/exam-api:v2",
		"namespace": "oilan-system",
	}
	if !reflect.DeepEqual(req.Payload, want) {
		t.Fatalf("payload mismatch:\n got: %#v\nwant: %#v", req.Payload, want)
	}
}

func TestBuildK8SRequestPlanDeployImage(t *testing.T) {
	req, err := buildK8SRequest([]string{
		"plan",
		"deploy-image",
		"deploy/exam-api",
		"--container", "app",
		"--image", "registry.example.com/example/exam-api:v2",
		"--target", "prod",
		"--namespace", "oilan-system",
		"--out", "ops/k8s/changes/exam-api.yaml",
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if req.PlanOut != "ops/k8s/changes/exam-api.yaml" {
		t.Fatalf("plan output mismatch: %q", req.PlanOut)
	}
	if req.Payload["operation"] != "plan-set-image" {
		t.Fatalf("operation mismatch: %#v", req.Payload)
	}
}

func TestBuildK8SRequestRejectsMissingTarget(t *testing.T) {
	if _, err := buildK8SRequest([]string{"get", "pods", "--namespace", "oilan-system"}); err == nil {
		t.Fatal("expected missing target to fail")
	}
}

func TestNewK8SDeployImagePlanIncludesVersionedSteps(t *testing.T) {
	req, err := buildK8SRequest([]string{
		"plan",
		"deploy-image",
		"deploy/exam-api",
		"--container", "app",
		"--image", "registry.example.com/example/exam-api:v2",
		"--target", "prod",
		"--namespace", "oilan-system",
		"--out", "ops/k8s/changes/exam-api.yaml",
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	plan, err := newK8SDeployImagePlan(req, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("new plan: %v", err)
	}
	if plan.APIVersion != "doops.sh/v1" || plan.Kind != "K8SChangePlan" {
		t.Fatalf("plan version mismatch: %#v", plan)
	}
	if plan.Metadata.Target != "prod" || plan.Metadata.RunbookVersion != "v1" {
		t.Fatalf("metadata mismatch: %#v", plan.Metadata)
	}
	if len(plan.Spec.Steps) != 1 || plan.Spec.Steps[0].Operation != "set-image" {
		t.Fatalf("steps mismatch: %#v", plan.Spec.Steps)
	}
	if len(plan.Spec.Checks) != 1 || plan.Spec.Checks[0].Operation != "rollout-status" {
		t.Fatalf("checks mismatch: %#v", plan.Spec.Checks)
	}
}

func TestApplyK8SChangePlanRequiresConfirm(t *testing.T) {
	plan := K8SChangePlan{
		APIVersion: "doops.sh/v1",
		Kind:       "K8SChangePlan",
	}
	plan.Metadata.Target = "prod"
	plan.Metadata.RunbookVersion = "v1"
	plan.Spec.Namespace = "oilan-system"
	plan.Spec.Steps = []K8SPlanStep{{ID: "set-image", Operation: "set-image", Resource: "deploy/exam-api", Container: "app", Image: "image:v2"}}

	caller := &fakeK8SCaller{}
	if err := applyK8SChangePlan(caller, "prod", plan, false); err == nil {
		t.Fatal("expected apply without confirm to fail")
	}
	if len(caller.calls) != 0 {
		t.Fatalf("unexpected remote calls: %#v", caller.calls)
	}
}

func TestApplyK8SChangePlanRejectsTargetMismatch(t *testing.T) {
	plan := K8SChangePlan{
		APIVersion: "doops.sh/v1",
		Kind:       "K8SChangePlan",
	}
	plan.Metadata.Target = "prod"
	plan.Metadata.RunbookVersion = "v1"
	plan.Spec.Namespace = "oilan-system"
	plan.Spec.Steps = []K8SPlanStep{{ID: "set-image", Operation: "set-image", Resource: "deploy/exam-api", Container: "app", Image: "image:v2"}}

	if err := applyK8SChangePlan(&fakeK8SCaller{}, "staging", plan, true); err == nil {
		t.Fatal("expected target mismatch to fail")
	}
}

func TestApplyK8SChangePlanCallsStepsThenChecks(t *testing.T) {
	plan := K8SChangePlan{
		APIVersion: "doops.sh/v1",
		Kind:       "K8SChangePlan",
	}
	plan.Metadata.Target = "prod"
	plan.Metadata.RunbookVersion = "v1"
	plan.Spec.Namespace = "oilan-system"
	plan.Spec.Steps = []K8SPlanStep{{ID: "set-image", Operation: "set-image", Resource: "deploy/exam-api", Container: "app", Image: "image:v2"}}
	plan.Spec.Checks = []K8SPlanStep{{ID: "rollout-status", Operation: "rollout-status", Resource: "deploy/exam-api", Timeout: "5m"}}

	caller := &fakeK8SCaller{}
	if err := applyK8SChangePlan(caller, "prod", plan, true); err != nil {
		t.Fatalf("apply plan: %v", err)
	}
	if len(caller.calls) != 2 {
		t.Fatalf("call count mismatch: %#v", caller.calls)
	}
	if caller.calls[0].tool != "doops_k8s" || caller.calls[0].args["operation"] != "set-image" || caller.calls[0].args["confirm"] != true {
		t.Fatalf("step call mismatch: %#v", caller.calls[0])
	}
	if caller.calls[1].args["operation"] != "rollout-status" || caller.calls[1].args["confirm"] != nil {
		t.Fatalf("check call mismatch: %#v", caller.calls[1])
	}
}

func TestRunK8SRequestDirectCallsDoopsK8S(t *testing.T) {
	req, err := buildK8SRequest([]string{"get", "pods", "--target", "prod", "--namespace", "oilan-system"})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	caller := &fakeK8SCaller{}
	msg, err := runK8SRequest(caller, req, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("run request: %v", err)
	}
	if msg != "" {
		t.Fatalf("unexpected message: %q", msg)
	}
	if len(caller.calls) != 1 || caller.calls[0].tool != "doops_k8s" || caller.calls[0].args["operation"] != "get" {
		t.Fatalf("remote call mismatch: %#v", caller.calls)
	}
}

func TestRunK8SRequestWritesPlanFile(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "ops", "k8s", "changes", "exam-api.yaml")
	req, err := buildK8SRequest([]string{
		"plan",
		"deploy-image",
		"deploy/exam-api",
		"--container", "app",
		"--image", "image:v2",
		"--target", "prod",
		"--namespace", "oilan-system",
		"--out", planPath,
	})
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	msg, err := runK8SRequest(nil, req, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("run request: %v", err)
	}
	if msg != "wrote "+planPath {
		t.Fatalf("message mismatch: %q", msg)
	}
	data, err := os.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if !strings.Contains(string(data), "apiVersion: doops.sh/v1") || !strings.Contains(string(data), "runbookVersion: v1") {
		t.Fatalf("plan content missing version metadata:\n%s", string(data))
	}
}

type fakeK8SCaller struct {
	calls []fakeK8SCall
}

type fakeK8SCall struct {
	tool string
	args map[string]interface{}
}

func (f *fakeK8SCaller) Call(toolName string, arguments map[string]interface{}) error {
	copied := make(map[string]interface{}, len(arguments))
	for k, v := range arguments {
		copied[k] = v
	}
	f.calls = append(f.calls, fakeK8SCall{tool: toolName, args: copied})
	return nil
}

func (f *fakeK8SCaller) CallAndCapture(toolName string, arguments map[string]interface{}) (string, error) {
	if err := f.Call(toolName, arguments); err != nil {
		return "", err
	}
	if toolName == "doops_agent_prompt" {
		return cicdAgentStatusPass + "\n", nil
	}
	return "verified", nil
}
