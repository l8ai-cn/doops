package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCICDWorkflowLoadsAndBuildsPlan(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: sample
spec:
  inputs:
    target:
      required: true
    reason:
      default: smoke
  source:
    path: `+quoteYAML(dir)+`
    requireCleanCommit: false
  locks:
    - key: deploy-${inputs.target}
      wait: true
      cancelWaiting: true
      cancelRunning: false
  stages:
    - id: validate
      name: Validate source
      uses: shell
      run: test -d .
    - id: deploy
      name: Deploy
      uses: doops.k8s
      mutates: true
      confirm: true
`)

	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	plan, err := buildCICDPlan(workflow, map[string]string{"target": "test"})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if plan.Name != "sample" {
		t.Fatalf("plan name mismatch: %q", plan.Name)
	}
	if plan.Inputs["target"] != "test" || plan.Inputs["reason"] != "smoke" {
		t.Fatalf("inputs mismatch: %#v", plan.Inputs)
	}
	if len(plan.Stages) != 2 || !plan.Stages[1].Mutates {
		t.Fatalf("stages mismatch: %#v", plan.Stages)
	}
	if !plan.Stages[1].Confirm {
		t.Fatalf("mutating stage confirmation policy was not preserved: %#v", plan.Stages[1])
	}
	if len(plan.Locks) != 1 || plan.Locks[0].Key != "deploy-test" {
		t.Fatalf("locks mismatch: %#v", plan.Locks)
	}
}

func TestResolveCICDExecutionTargetUsesDeclaredRoutes(t *testing.T) {
	t.Run("single environment", func(t *testing.T) {
		target, err := resolveCICDExecutionTarget(
			CICDPlan{Environments: []CICDEnvironment{{Name: "test", Target: "gw-test"}}},
			map[string]CICDInput{},
		)
		if err != nil || target != "gw-test" {
			t.Fatalf("expected gw-test, got target=%q err=%v", target, err)
		}
	})

	t.Run("selected environment", func(t *testing.T) {
		target, err := resolveCICDExecutionTarget(
			CICDPlan{
				Inputs: map[string]string{"environment": "oilan"},
				Environments: []CICDEnvironment{
					{Name: "oilan", Target: "gw-oilan"},
					{Name: "scu", Target: "gw-scu"},
				},
			},
			map[string]CICDInput{"environment": {Required: true}},
		)
		if err != nil || target != "gw-oilan" {
			t.Fatalf("expected gw-oilan, got target=%q err=%v", target, err)
		}
	})

	t.Run("legacy target input", func(t *testing.T) {
		target, err := resolveCICDExecutionTarget(
			CICDPlan{Inputs: map[string]string{"target": "legacy-target"}},
			map[string]CICDInput{"target": {Required: true}},
		)
		if err != nil || target != "legacy-target" {
			t.Fatalf("expected legacy target, got target=%q err=%v", target, err)
		}
	})

	t.Run("ambiguous routes fail", func(t *testing.T) {
		_, err := resolveCICDExecutionTarget(
			CICDPlan{Environments: []CICDEnvironment{
				{Name: "oilan", Target: "gw-oilan"},
				{Name: "scu", Target: "gw-scu"},
			}},
			map[string]CICDInput{},
		)
		if err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("expected ambiguous target error, got %v", err)
		}
	})
}

func TestCICDRunDerivesTargetWithoutTargetInput(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: routed-run
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  environments:
    - name: test
      target: gw-test
      namespace: test
      release: app
      deploymentDoc: docs/deploy/test.md
      services: [api]
  stages:
    - id: verify
      uses: agent.task
      with:
        task: verify-rollout
`)
	writeDeploymentDoc(t, dir, "docs/deploy/test.md")

	resolvedTarget := ""
	err := runCICDCommandWithSync(
		context.Background(),
		[]string{"run", "-f", workflowPath, "--dry-run"},
		func(target string) (cicdExecutor, func(), error) {
			resolvedTarget = target
			return &fakeK8SCaller{}, nil, nil
		},
		nil,
		"routed-run-session",
	)
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if resolvedTarget != "gw-test" {
		t.Fatalf("expected derived target gw-test, got %q", resolvedTarget)
	}
}

func TestCICDWorkflowRejectsInvalidSchema(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "missing metadata name",
			body: `
apiVersion: doops.sh/v1
kind: Workflow
spec:
  source:
    path: ` + quoteYAML(dir) + `
  stages:
    - id: validate
      uses: shell
      run: echo ok
`,
			want: "metadata.name is required",
		},
		{
			name: "duplicate stage",
			body: `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: duplicate-stage
spec:
  source:
    path: ` + quoteYAML(dir) + `
  stages:
    - id: validate
      uses: shell
      run: echo ok
    - id: validate
      uses: shell
      run: echo again
`,
			want: "duplicate stage id",
		},
		{
			name: "unknown stage type",
			body: `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: unknown-stage
spec:
  source:
    path: ` + quoteYAML(dir) + `
  stages:
    - id: validate
      uses: terraform.apply
`,
			want: "unsupported stage uses",
		},
		{
			name: "mutating stage without confirm",
			body: `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: unsafe-mutation
spec:
  source:
    path: ` + quoteYAML(dir) + `
  stages:
    - id: deploy
      uses: doops.k8s
      mutates: true
`,
			want: "mutating stage deploy requires confirm",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workflowPath := writeCICDTestWorkflow(t, dir, tc.body)
			_, err := loadCICDWorkflow(workflowPath)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q error, got %v", tc.want, err)
			}
		})
	}
}

func TestCICDWorkflowReviewsDeploymentDocsAndEnvironmentConsistency(t *testing.T) {
	dir := t.TempDir()
	writeDeploymentDoc(t, dir, "docs/deploy/test.md")
	writeDeploymentDoc(t, dir, "docs/deploy/prod.md")
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: native-deploy
spec:
  policy:
    agentNative: true
    maxToolScripts: 1
    requiredDocSections:
      - Inputs
      - Deployment
      - Rollback
      - Verification
  source:
    repo: `+quoteYAML(dir)+`
    path: `+quoteYAML(filepath.Join(dir, "work"))+`
  environments:
    - name: test
      target: gw-test
      namespace: test
      release: app
      deploymentDoc: docs/deploy/test.md
      services: [api, worker]
    - name: prod
      target: gw-prod
      namespace: prod
      release: app
      deploymentDoc: docs/deploy/prod.md
      services: [worker, api]
  stages:
    - id: validate-docs
      uses: shell
      run: test -d docs
    - id: deploy
      uses: doops.k8s
      mutates: true
      confirm: true
      with:
        operation: deploy-image
        environment: ${inputs.target}
        workload: deployment/app
        container: app
`)

	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	plan, err := buildCICDPlan(workflow, map[string]string{"target": "test"})
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if got := plan.Stages[1].With["operation"]; got != "deploy-image" {
		t.Fatalf("structured native operation mismatch: %q", got)
	}
	if plan.Stages[1].Run != "" {
		t.Fatalf("agent-native stage should not carry script run: %#v", plan.Stages[1])
	}
}

func TestCICDWorkflowReadsDeploymentDocFromRepositoryRoot(t *testing.T) {
	dir := t.TempDir()
	runTestGitCommand(t, "", "init", dir)
	workflowDir := filepath.Join(dir, "ops", "cicd")
	workflowPath := filepath.Join(workflowDir, "zhiyong.deploy.yaml")
	writeDeploymentDoc(t, dir, "ops/cicd/docs/deploy/test.md")
	if err := os.MkdirAll(workflowDir, 0o755); err != nil {
		t.Fatalf("mkdir workflow dir: %v", err)
	}
	if err := os.WriteFile(workflowPath, []byte(`
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: repository-root-doc
spec:
  policy:
    requiredDocSections: [Inputs, Deployment, Rollback, Verification]
  source:
    repo: https://example.invalid/education.git
    path: /tmp/work
  environments:
    - name: test
      target: gw-test
      namespace: test
      release: app
      deploymentDoc: ops/cicd/docs/deploy/test.md
      services: [api]
  stages:
    - id: validate
      uses: shell
      run: echo ok
`), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	if _, err := loadCICDWorkflow(workflowPath); err != nil {
		t.Fatalf("load workflow with repository-root deploymentDoc: %v", err)
	}
}

func TestCICDWorkflowRejectsDeploymentDocMissingRequiredSection(t *testing.T) {
	dir := t.TempDir()
	docPath := filepath.Join(dir, "docs/deploy/test.md")
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	if err := os.WriteFile(docPath, []byte("# Test\n\n## Inputs\n\n## Deployment\n"), 0o644); err != nil {
		t.Fatalf("write doc: %v", err)
	}
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: missing-doc-section
spec:
  policy:
    requiredDocSections: [Inputs, Deployment, Rollback, Verification]
  source:
    repo: `+quoteYAML(dir)+`
    path: `+quoteYAML(filepath.Join(dir, "work"))+`
  environments:
    - name: test
      target: gw-test
      namespace: test
      release: app
      deploymentDoc: docs/deploy/test.md
      services: [api]
  stages:
    - id: validate
      uses: shell
      run: echo ok
`)

	_, err := loadCICDWorkflow(workflowPath)
	if err == nil || !strings.Contains(err.Error(), `deployment doc "docs/deploy/test.md" missing section "Rollback"`) {
		t.Fatalf("expected missing section error, got %v", err)
	}
}

func TestCICDWorkflowRejectsEnvironmentServiceDrift(t *testing.T) {
	dir := t.TempDir()
	writeDeploymentDoc(t, dir, "docs/deploy/test.md")
	writeDeploymentDoc(t, dir, "docs/deploy/prod.md")
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: service-drift
spec:
  source:
    repo: `+quoteYAML(dir)+`
    path: `+quoteYAML(filepath.Join(dir, "work"))+`
  environments:
    - name: test
      target: gw-test
      namespace: test
      release: app
      deploymentDoc: docs/deploy/test.md
      services: [api, worker]
    - name: prod
      target: gw-prod
      namespace: prod
      release: app
      deploymentDoc: docs/deploy/prod.md
      services: [api]
  stages:
    - id: validate
      uses: shell
      run: echo ok
`)

	_, err := loadCICDWorkflow(workflowPath)
	if err == nil || !strings.Contains(err.Error(), `environment prod services [api] differ from test services [api worker]`) {
		t.Fatalf("expected service drift error, got %v", err)
	}
}

func TestCICDWorkflowRejectsScriptedAgentNativeStage(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: scripted-native
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
      run: bash ops/cicd/scripts/prod-deploy.sh
      with:
        operation: deploy-image
`)

	_, err := loadCICDWorkflow(workflowPath)
	if err == nil || !strings.Contains(err.Error(), "agent-native stage deploy must use structured with, not run") {
		t.Fatalf("expected scripted native stage error, got %v", err)
	}
}

func TestCICDWorkflowRejectsScriptReferenceHiddenInStructuredStage(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: hidden-script
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
        task: deploy
        script: ops/cicd/scripts/prod-deploy.sh
`)

	_, err := loadCICDWorkflow(workflowPath)
	if err == nil || !strings.Contains(err.Error(), "agent-native stage deploy must not use with.script") {
		t.Fatalf("expected hidden script reference error, got %v", err)
	}
}

func TestCICDRunnerExecutesNonMutatingShellAndSkipsMutatingDryRun(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "validated.txt")
	mutated := filepath.Join(dir, "mutated.txt")
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: runner-smoke
spec:
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: validate
      uses: shell
      run: printf ok > `+quoteShell(marker)+`
    - id: deploy
      uses: shell
      mutates: true
      confirm: true
      run: printf bad > `+quoteShell(mutated)+`
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{DryRun: true})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 2 {
		t.Fatalf("step count mismatch: %#v", result.Steps)
	}
	if result.Steps[0].Status != "success" || result.Steps[1].Status != "skipped" {
		t.Fatalf("step statuses mismatch: %#v", result.Steps)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "ok" {
		t.Fatalf("validate marker mismatch data=%q err=%v", data, err)
	}
	if _, err := os.Stat(mutated); !os.IsNotExist(err) {
		t.Fatalf("mutating step should be skipped in dry-run, stat err=%v", err)
	}
}

func TestCICDRunnerRejectsMutatingRunWithoutAllowMutate(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: unsafe-run
spec:
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: deploy
      uses: shell
      mutates: true
      confirm: true
      run: echo no
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	_, err = runCICDWorkflow(context.Background(), workflow, CICDRunOptions{})
	if err == nil || !strings.Contains(err.Error(), "requires --allow-mutate") {
		t.Fatalf("expected allow-mutate error, got %v", err)
	}
}

func TestCICDRunnerExecutesAgentTaskWithInputsWhenMutationAllowed(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "agent-task.txt")
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: agent-task-run
spec:
  inputs:
    service:
      required: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: build
      uses: agent.task
      mutates: true
      confirm: true
      run: printf '%s' "$DOOPS_CICD_INPUT_SERVICE" > `+quoteShell(marker)+`
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Inputs:      map[string]string{"service": "zhiyong-frontend"},
		AllowMutate: true,
	})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "success" {
		t.Fatalf("step statuses mismatch: %#v", result.Steps)
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "zhiyong-frontend" {
		t.Fatalf("agent task marker mismatch data=%q err=%v", data, err)
	}
}

func TestCICDRunnerPlansStructuredAgentNativeStageWithoutScriptInDryRun(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: native-plan
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: verify
      uses: doops.exec
      with:
        operation: rollout-health
        target: test
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{DryRun: true})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "planned" {
		t.Fatalf("expected planned native step, got %#v", result.Steps)
	}
}

// Agent-driven contract: a structured agent-native mutating stage without a
// wired executor is recorded as planned (not a hard failure). There is no
// Without an executor, mutating agent-native stages stay planned for offline
// lint/plan. The --allow-mutate gate applies only when an executor is wired.
func TestCICDRunnerPlansAgentNativeMutationWithoutExecutor(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: native-mutation
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
        target: test
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
		t.Fatalf("expected planned native step, got %#v", result.Steps)
	}
}

func TestCICDRunnerClonesSourceBeforeShellStages(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin")
	clone := filepath.Join(dir, "clone")
	runTestGitCommand(t, "", "init", origin)
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	runTestGitCommand(t, origin, "add", "README.md")
	runTestGitCommand(t, origin, "-c", "user.name=doops", "-c", "user.email=doops@localhost", "commit", "-m", "init")
	runTestGitCommand(t, origin, "branch", "-M", "main")

	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: clone-smoke
spec:
  source:
    repo: `+quoteYAML(origin)+`
    branch: main
    path: `+quoteYAML(clone)+`
  stages:
    - id: clone
      uses: git.clone
    - id: validate
      uses: shell
      run: test -s README.md
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{DryRun: true})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 2 || result.Steps[0].Status != "success" || result.Steps[1].Status != "success" {
		t.Fatalf("step statuses mismatch: %#v", result.Steps)
	}
	if _, err := os.Stat(filepath.Join(clone, "README.md")); err != nil {
		t.Fatalf("expected cloned README: %v", err)
	}
}

func TestCICDRunnerRejectsSymlinkWorkdirEscape(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	outside := filepath.Join(dir, "outside")
	if err := os.Mkdir(source, 0o755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	if err := os.Mkdir(outside, 0o755); err != nil {
		t.Fatalf("mkdir outside: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(source, "escape")); err != nil {
		t.Fatalf("symlink escape: %v", err)
	}

	_, _, err := runCICDShellStage(context.Background(), source, CICDPlanStage{
		ID:      "escape",
		Uses:    "shell",
		Workdir: "escape",
		Run:     "pwd",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "outside source.path") {
		t.Fatalf("expected workdir escape error, got %v", err)
	}
}

func TestBuildCICDCommandParsesPlanArgs(t *testing.T) {
	req, err := buildCICDCommand([]string{
		"plan",
		"-f", "ops/cicd/zhiyong.deploy.yaml",
		"--set", "target=test",
		"--set", "reason=smoke",
	})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	if req.Command != "plan" || req.File != "ops/cicd/zhiyong.deploy.yaml" {
		t.Fatalf("request mismatch: %#v", req)
	}
	if req.Inputs["target"] != "test" || req.Inputs["reason"] != "smoke" {
		t.Fatalf("inputs mismatch: %#v", req.Inputs)
	}
}

func TestBuildCICDCommandParsesRunSafetyFlags(t *testing.T) {
	req, err := buildCICDCommand([]string{
		"run",
		"-f", "workflow.yaml",
		"--dry-run",
		"--allow-mutate",
	})
	if err != nil {
		t.Fatalf("build command: %v", err)
	}
	if !req.DryRun || !req.AllowMutate {
		t.Fatalf("safety flags mismatch: %#v", req)
	}
}

func TestBuildCICDCommandRejectsBadSetValue(t *testing.T) {
	_, err := buildCICDCommand([]string{"plan", "-f", "workflow.yaml", "--set", "target"})
	if err == nil || !strings.Contains(err.Error(), "--set must be key=value") {
		t.Fatalf("expected bad set value error, got %v", err)
	}
}

func writeCICDTestWorkflow(t *testing.T, dir string, body string) string {
	t.Helper()
	path := filepath.Join(dir, "workflow.yaml")
	if err := os.WriteFile(path, []byte(strings.TrimSpace(body)+"\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	return path
}

func writeDeploymentDoc(t *testing.T, root string, relPath string) {
	t.Helper()
	path := filepath.Join(root, relPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir deployment doc dir: %v", err)
	}
	body := "# Deployment\n\n## Inputs\n\n## Deployment\n\n## Rollback\n\n## Verification\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write deployment doc: %v", err)
	}
}

func quoteYAML(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `\"`) + `"`
}

func quoteShell(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `'\''`) + `'`
}

func runTestGitCommand(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
	}
}
