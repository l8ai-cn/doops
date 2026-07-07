package main

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestCICDAgentInstructionCarriesIntent(t *testing.T) {
	plan := CICDPlan{
		Name:   "scu-deploy",
		Inputs: map[string]string{"target": "gw-scu", "reason": "release"},
		Source: CICDSource{Path: "/tmp/src"},
	}
	stage := CICDPlanStage{
		ID:      "deploy-zhiyong",
		Name:    "6.2 helm deploy",
		Uses:    "agent.task",
		Mutates: true,
		With: map[string]string{
			"task":      "helm-upgrade",
			"release":   "zhiyong",
			"namespace": "oilan-system",
		},
	}
	got := cicdAgentInstruction(plan, stage, "apply")
	for _, want := range []string{
		"scu-deploy",
		"stage.id: deploy-zhiyong",
		"stage.uses: agent.task",
		"mode: apply",
		"mutates: true",
		"task: helm-upgrade",
		"release: zhiyong",
		"namespace: oilan-system",
		"target: gw-scu",
		"source.path: /tmp/src",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instruction missing %q in:\n%s", want, got)
		}
	}
}

func TestCICDAgentInstructionDryRunDirective(t *testing.T) {
	plan := CICDPlan{Name: "wf"}
	stage := CICDPlanStage{ID: "s", Uses: "doops.k8s", With: map[string]string{"operation": "rollout"}}
	got := cicdAgentInstruction(plan, stage, "dry-run")
	if !strings.Contains(got, "mode: dry-run") {
		t.Fatalf("expected dry-run mode in instruction:\n%s", got)
	}
	if !strings.Contains(got, "dry-run") || !strings.Contains(strings.ToLower(got), "do not apply") {
		t.Fatalf("expected dry-run guidance in instruction:\n%s", got)
	}
}

func TestCICDAgentInstructionIsDeterministic(t *testing.T) {
	plan := CICDPlan{Name: "wf", Inputs: map[string]string{"b": "2", "a": "1"}}
	stage := CICDPlanStage{ID: "s", Uses: "agent.task", With: map[string]string{"task": "t", "z": "9", "m": "5"}}
	first := cicdAgentInstruction(plan, stage, "apply")
	second := cicdAgentInstruction(plan, stage, "apply")
	if first != second {
		t.Fatalf("instruction not deterministic:\n%s\n---\n%s", first, second)
	}
}

func TestIsCICDAgentDrivenStage(t *testing.T) {
	cases := []struct {
		uses string
		run  string
		want bool
	}{
		{"agent.task", "", true},
		{"doops.k8s", "", true},
		{"doops.exec", "", true},
		{"agent.task", "echo hi", false}, // inline script => shell lane
		{"shell", "", false},
		{"git.clone", "", false},
	}
	for _, c := range cases {
		got := isCICDAgentDrivenStage(CICDPlanStage{Uses: c.uses, Run: c.run})
		if got != c.want {
			t.Fatalf("uses=%q run=%q => %v, want %v", c.uses, c.run, got, c.want)
		}
	}
}

// With an executor wired, a structured agent-native stage is dispatched to the
// doagent via doops_agent_prompt (agent-driven), regardless of --allow-mutate.
func TestCICDRunnerDispatchesToAgent(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: dispatch-smoke
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: deploy
      uses: agent.task
      mutates: true
      confirm: true
      with:
        task: helm-upgrade
        release: zhiyong
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	caller := &fakeK8SCaller{}
	// Note: no AllowMutate -> agent-driven stages have no code gate.
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{Executor: caller})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "success" {
		t.Fatalf("expected success step, got %#v", result.Steps)
	}
	if len(caller.calls) != 1 || caller.calls[0].tool != "doops_agent_prompt" {
		t.Fatalf("expected one doops_agent_prompt dispatch, got %#v", caller.calls)
	}
	instruction, _ := caller.calls[0].args["instruction"].(string)
	if !strings.Contains(instruction, "helm-upgrade") || !strings.Contains(instruction, "mode: apply") {
		t.Fatalf("instruction missing intent: %q", instruction)
	}
}

// Dry-run is also agent-driven: dispatched with mode=dry-run (agent judges).
func TestCICDRunnerDispatchesDryRunToAgent(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: dry-dispatch
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: deploy
      uses: doops.k8s
      mutates: true
      confirm: true
      with:
        operation: deploy-image
        resource: deploy/app
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	caller := &fakeK8SCaller{}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{DryRun: true, Executor: caller})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "success" {
		t.Fatalf("expected success step, got %#v", result.Steps)
	}
	if len(caller.calls) != 1 {
		t.Fatalf("expected one dispatch on dry-run, got %#v", caller.calls)
	}
	instruction, _ := caller.calls[0].args["instruction"].(string)
	if !strings.Contains(instruction, "mode: dry-run") {
		t.Fatalf("expected dry-run mode dispatched, got %q", instruction)
	}
}

// Without an executor, agent-native stages are recorded as planned (no gate,
// no failure) — offline lint/plan behavior.
func TestCICDRunnerPlansAgentStageWithoutExecutor(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: no-exec
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: deploy
      uses: doops.k8s
      mutates: true
      confirm: true
      with:
        operation: deploy-image
        resource: deploy/app
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "planned" {
		t.Fatalf("expected planned step without executor, got %#v", result.Steps)
	}
}

// A failing gateway call surfaces as a failed stage (no masking).
func TestCICDRunnerAgentDispatchFailurePropagates(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: dispatch-fail
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: deploy
      uses: agent.task
      mutates: true
      confirm: true
      with:
        task: helm-upgrade
        release: app
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{Executor: errExecutor{}})
	if err == nil || !strings.Contains(err.Error(), "deploy") {
		t.Fatalf("expected failure to propagate, got %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "failed" {
		t.Fatalf("expected failed step, got %#v", result.Steps)
	}
}

// Workflow-level buildEnv context must reach the doagent so it can drive the
// stage (image mirrors, proxies, quirks) without hardcoded knowledge.
func TestCICDBuildEnvContextReachesAgent(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: ctx-flow
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  buildEnv:
    registries:
      dest: repo.scu.aiedulab.cn:30443/oilan-system
    baseImageMap:
      "golang:1.23-alpine": repo.aiedulab.cn:8443/library/golang:1.24-alpine
  stages:
    - id: build
      uses: agent.task
      mutates: true
      confirm: true
      with:
        task: buildkit-build
        service: zhiyong-lab-api
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	caller := &fakeK8SCaller{}
	if _, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{Executor: caller}); err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(caller.calls) != 1 {
		t.Fatalf("expected one dispatch, got %#v", caller.calls)
	}
	instruction, _ := caller.calls[0].args["instruction"].(string)
	for _, want := range []string{
		"workflow context",
		"repo.scu.aiedulab.cn:30443/oilan-system",
		"repo.aiedulab.cn:8443/library/golang:1.24-alpine",
	} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("instruction missing context %q in:\n%s", want, instruction)
		}
	}
}

type errExecutor struct{}

func (errExecutor) Call(tool string, args map[string]interface{}) error {
	return fmt.Errorf("gateway boom for %s", tool)
}
