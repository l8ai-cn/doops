package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCICDAgentInstructionCarriesIntent(t *testing.T) {
	plan := CICDPlan{
		Name:            "scu-deploy",
		Inputs:          map[string]string{"reason": "release"},
		ExecutionTarget: "gw-scu",
		Source:          CICDSource{Path: "/tmp/src"},
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
	got := cicdAgentInstruction(plan, stage, "apply", "sess-1")
	for _, want := range []string{
		"scu-deploy",
		"stage.id: deploy-zhiyong",
		"stage.uses: agent.task",
		"mode: apply",
		"mutates: true",
		"task: helm-upgrade",
		"release: zhiyong",
		"namespace: oilan-system",
		"execution.target: gw-scu",
		"source.path.local: /tmp/src",
		"remote.workspace: /root/ws/sess-1",
		"session: sess-1",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("instruction missing %q in:\n%s", want, got)
		}
	}
}

func TestCICDAgentInstructionDryRunDirective(t *testing.T) {
	plan := CICDPlan{Name: "wf"}
	stage := CICDPlanStage{ID: "s", Uses: "doops.k8s", With: map[string]string{"operation": "rollout"}}
	got := cicdAgentInstruction(plan, stage, "dry-run", "")
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
	first := cicdAgentInstruction(plan, stage, "apply", "sess")
	second := cicdAgentInstruction(plan, stage, "apply", "sess")
	if first != second {
		t.Fatalf("instruction not deterministic:\n%s\n---\n%s", first, second)
	}
}

func TestCICDAgentInstructionRequiresExactRequiredCommand(t *testing.T) {
	plan := CICDPlan{Name: "wf"}
	stage := CICDPlanStage{
		ID:   "publish",
		Uses: "agent.task",
		With: map[string]string{
			"task":            "publish-release-manifest",
			"requiredCommand": "python3 ops/cicd/tools/release_manifest.py publish",
		},
	}

	instruction := cicdAgentInstruction(plan, stage, "apply", "session-1")
	for _, want := range []string{
		"requiredCommand: python3 ops/cicd/tools/release_manifest.py publish",
		"mandatory execution:",
		"MUST execute requiredCommand exactly as declared",
		"Do not replace it with a draft, approximation, or manual alternative.",
	} {
		if !strings.Contains(instruction, want) {
			t.Fatalf("required command instruction missing %q:\n%s", want, instruction)
		}
	}
}

func TestCICDRunnerSyncsSourceBeforeAgentStage(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: sync-smoke
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
	synced := ""
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor:    caller,
		AllowMutate: true,
		Session:     "sess-sync",
		SourceSync: func(src string) error {
			synced = src
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if synced == "" {
		t.Fatalf("expected source sync before agent stage, got empty src; steps=%#v", result.Steps)
	}
	if len(caller.calls) != 1 {
		t.Fatalf("expected one agent dispatch after sync, got %#v", caller.calls)
	}
	instruction, _ := caller.calls[0].args["instruction"].(string)
	if !strings.Contains(instruction, "remote.workspace: /root/ws/sess-sync") {
		t.Fatalf("instruction missing remote workspace:\n%s", instruction)
	}
}

func TestCICDRunnerSyncsAttestedExactRelease(t *testing.T) {
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin")
	clone := filepath.Join(dir, "clone")
	runTestGitCommand(t, "", "init", origin)
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("release-one\n"), 0o644); err != nil {
		t.Fatalf("write release one: %v", err)
	}
	runTestGitCommand(t, origin, "add", "README.md")
	runTestGitCommand(t, origin, "-c", "user.name=doops", "-c", "user.email=doops@localhost", "commit", "-m", "release one")
	runTestGitCommand(t, origin, "branch", "-M", "main")
	releaseID, _, err := runCICDCommandOutput(context.Background(), origin, nil, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("resolve release commit: %v", err)
	}
	releaseID = strings.TrimSpace(releaseID)
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("release-two\n"), 0o644); err != nil {
		t.Fatalf("write release two: %v", err)
	}
	runTestGitCommand(t, origin, "add", "README.md")
	runTestGitCommand(t, origin, "-c", "user.name=doops", "-c", "user.email=doops@localhost", "commit", "-m", "release two")

	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: attested-release
spec:
  policy:
    agentNative: true
  source:
    repo: `+quoteYAML(origin)+`
    branch: main
    path: `+quoteYAML(clone)+`
    requireCleanCommit: true
  stages:
    - id: clone
      uses: git.clone
    - id: checkout
      uses: agent.task
      with:
        task: verify-release-source
        releaseId: ${inputs.releaseId}
        branch: main
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}

	synced := false
	verifier := &sourceReleaseVerifier{}
	_, err = runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Inputs:   map[string]string{"releaseId": releaseID},
		Executor: verifier,
		Session:  "attested-release",
		SourceSync: func(src string) error {
			data, err := os.ReadFile(filepath.Join(src, ".doops-source-release.json"))
			if err != nil {
				return err
			}
			var attestation struct {
				ReleaseID string `json:"releaseId"`
			}
			if err := json.Unmarshal(data, &attestation); err != nil {
				return err
			}
			if attestation.ReleaseID != releaseID {
				return fmt.Errorf("attested release=%s want=%s", attestation.ReleaseID, releaseID)
			}
			head, _, err := runCICDCommandOutput(context.Background(), src, nil, "git", "rev-parse", "HEAD")
			if err != nil {
				return err
			}
			if strings.TrimSpace(head) != releaseID {
				return fmt.Errorf("checked out release=%s want=%s", strings.TrimSpace(head), releaseID)
			}
			verifier.attestation = string(data)
			synced = true
			return nil
		},
	})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if !synced {
		t.Fatal("expected attested source sync")
	}
}

type countingCaller struct {
	failTimes int
	calls     int
}

type failedAgentResultCaller struct{}

func (failedAgentResultCaller) Call(tool string, args map[string]interface{}) error {
	return nil
}

func (failedAgentResultCaller) CallAndCapture(tool string, args map[string]interface{}) (string, error) {
	if tool != "doops_agent_prompt" {
		return "", fmt.Errorf("unexpected tool %s", tool)
	}
	return "verification failed\nDOOPS_STAGE_STATUS=FAIL\n", nil
}

func TestCICDRunnerRejectsExplicitFailedAgentResult(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: agent-verdict
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: validate
      uses: agent.task
      with:
        task: verify-rollout
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor: failedAgentResultCaller{},
		Session:  "agent-verdict",
	})
	if err == nil || !strings.Contains(err.Error(), "reported failure") {
		t.Fatalf("expected agent failure to stop workflow, got result=%#v err=%v", result, err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "failed" {
		t.Fatalf("expected failed step, got %#v", result.Steps)
	}
}

type noStatusAgentResultCaller struct{}

func (noStatusAgentResultCaller) Call(tool string, args map[string]interface{}) error {
	return nil
}

func (noStatusAgentResultCaller) CallAndCapture(tool string, args map[string]interface{}) (string, error) {
	if tool != "doops_agent_prompt" {
		return "", fmt.Errorf("unexpected tool %s", tool)
	}
	return "work completed without a verdict\n", nil
}

func TestCICDRunnerRejectsAgentResultWithoutStatus(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: agent-status-required
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: validate
      uses: agent.task
      with:
        task: verify-rollout
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	_, err = runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor: noStatusAgentResultCaller{},
		Session:  "agent-status-required",
	})
	if err == nil || !strings.Contains(err.Error(), "did not report") {
		t.Fatalf("expected missing agent status failure, got %v", err)
	}
}

type sourceReleaseVerifier struct {
	calls       []string
	attestation string
}

func (v *sourceReleaseVerifier) Call(tool string, args map[string]interface{}) error {
	v.calls = append(v.calls, tool)
	return nil
}

func (v *sourceReleaseVerifier) CallAndCapture(tool string, args map[string]interface{}) (string, error) {
	v.calls = append(v.calls, tool)
	if tool != "doops_shell" {
		return "", fmt.Errorf("verify-release-source must not dispatch %s", tool)
	}
	return v.attestation, nil
}

func TestCICDRunnerVerifiesSyncedReleaseWithoutAgent(t *testing.T) {
	dir := t.TempDir()
	releaseID := "0123456789abcdef0123456789abcdef01234567"
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: remote-release-attestation
spec:
  policy:
    agentNative: true
  source:
    repo: https://example.test/repo.git
    branch: main
    path: `+quoteYAML(dir)+`
  stages:
    - id: checkout
      uses: agent.task
      with:
        task: verify-release-source
        releaseId: `+releaseID+`
        branch: main
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	verifier := &sourceReleaseVerifier{
		attestation: `{"releaseId":"` + releaseID + `","repository":"https://example.test/repo.git","branch":"main"}`,
	}
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor: verifier,
		Session:  "remote-release-attestation",
	})
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "success" {
		t.Fatalf("expected successful source verification, got %#v", result.Steps)
	}
	if len(verifier.calls) != 1 || verifier.calls[0] != "doops_shell" {
		t.Fatalf("expected only deterministic remote attestation read, got %#v", verifier.calls)
	}
}

func (c *countingCaller) Call(tool string, args map[string]interface{}) error {
	c.calls++
	if c.calls <= c.failTimes {
		return fmt.Errorf("connection lost: WS connection lost")
	}
	return nil
}

func (c *countingCaller) CallAndCapture(tool string, args map[string]interface{}) (string, error) {
	if err := c.Call(tool, args); err != nil {
		return "", err
	}
	return cicdAgentStatusPass + "\n", nil
}

func TestRunCICDAgentStageRetriesTransientWSLoss(t *testing.T) {
	caller := &countingCaller{failTimes: 2}
	plan := CICDPlan{Name: "wf", Source: CICDSource{Path: "/tmp/src"}}
	stage := CICDPlanStage{ID: "render", Uses: "agent.task", Name: "Render"}
	if err := runCICDAgentStage(caller, plan, stage, "apply", "sess"); err != nil {
		t.Fatalf("expected retry success, got %v", err)
	}
	if caller.calls != 3 {
		t.Fatalf("expected 3 attempts, got %d", caller.calls)
	}
}

type verifyingCaller struct {
	countingCaller
	verifyCommand string
	verifyErr     error
}

func (c *verifyingCaller) CallAndCapture(tool string, args map[string]interface{}) (string, error) {
	if tool == "doops_agent_prompt" {
		if err := c.countingCaller.Call(tool, args); err != nil {
			return "", err
		}
		return cicdAgentStatusPass + "\n", nil
	}
	if tool != "doops_shell" {
		return "", fmt.Errorf("unexpected verification tool %s", tool)
	}
	c.verifyCommand, _ = args["command"].(string)
	return "verified", c.verifyErr
}

func TestRunCICDAgentStageEnforcesApplyVerificationCommand(t *testing.T) {
	caller := &verifyingCaller{verifyErr: fmt.Errorf("release marker missing")}
	plan := CICDPlan{Name: "wf", Source: CICDSource{Path: "/tmp/src"}}
	stage := CICDPlanStage{
		ID:   "build",
		Uses: "agent.task",
		With: map[string]string{"verificationCommand": "test -s output/release.json"},
	}
	err := runCICDAgentStage(caller, plan, stage, "apply", "sess")
	if err == nil || !strings.Contains(err.Error(), "release marker missing") {
		t.Fatalf("expected verification failure, got %v", err)
	}
	if caller.verifyCommand != "cd -- '/root/ws/sess' && test -s output/release.json" {
		t.Fatalf("unexpected verification command %q", caller.verifyCommand)
	}
}

type flakyVerifyingCaller struct {
	countingCaller
	verifyCommand string
	failuresLeft  int
	verifyCalls   int
}

func (c *flakyVerifyingCaller) CallAndCapture(tool string, args map[string]interface{}) (string, error) {
	if tool == "doops_agent_prompt" {
		if err := c.countingCaller.Call(tool, args); err != nil {
			return "", err
		}
		return cicdAgentStatusPass + "\n", nil
	}
	if tool != "doops_shell" {
		return "", fmt.Errorf("unexpected verification tool %s", tool)
	}
	c.verifyCalls++
	c.verifyCommand, _ = args["command"].(string)
	if c.failuresLeft > 0 {
		c.failuresLeft--
		return "", fmt.Errorf("connection lost: WS connection lost")
	}
	return "verified", nil
}

func TestRunCICDAgentStageRetriesTransientVerificationErrors(t *testing.T) {
	caller := &flakyVerifyingCaller{failuresLeft: 2}
	plan := CICDPlan{Name: "wf", Source: CICDSource{Path: "/tmp/src"}}
	stage := CICDPlanStage{
		ID:   "build",
		Uses: "agent.task",
		With: map[string]string{"verificationCommand": "test -s output/release.json"},
	}
	if err := runCICDAgentStage(caller, plan, stage, "apply", "sess"); err != nil {
		t.Fatalf("expected verification to succeed after transient retries, got %v", err)
	}
	if caller.verifyCalls != 3 {
		t.Fatalf("expected 3 verification attempts, got %d", caller.verifyCalls)
	}
	if caller.verifyCommand != "cd -- '/root/ws/sess' && test -s output/release.json" {
		t.Fatalf("unexpected verification command %q", caller.verifyCommand)
	}
}

func TestRunCICDAgentStageSkipsApplyVerificationOnDryRun(t *testing.T) {
	caller := &verifyingCaller{verifyErr: fmt.Errorf("must not run")}
	plan := CICDPlan{Name: "wf", Source: CICDSource{Path: "/tmp/src"}}
	stage := CICDPlanStage{
		ID:   "build",
		Uses: "agent.task",
		With: map[string]string{"verificationCommand": "test -s output/release.json"},
	}
	if err := runCICDAgentStage(caller, plan, stage, "dry-run", "sess"); err != nil {
		t.Fatalf("expected dry-run success, got %v", err)
	}
	if caller.verifyCommand != "" {
		t.Fatalf("dry-run must not execute apply verification, got %q", caller.verifyCommand)
	}
}

func TestRunCICDAgentStageRunsDryRunVerificationCommand(t *testing.T) {
	caller := &verifyingCaller{}
	plan := CICDPlan{Name: "wf", Source: CICDSource{Path: "/tmp/src"}}
	stage := CICDPlanStage{
		ID:   "contracts",
		Uses: "agent.task",
		With: map[string]string{
			"dryRunVerificationCommand": "python3 -m unittest deploy.tests.test_new_deploy_contract",
		},
	}
	if err := runCICDAgentStage(caller, plan, stage, "dry-run", "sess"); err != nil {
		t.Fatalf("expected dry-run verification success, got %v", err)
	}
	want := "cd -- '/root/ws/sess' && python3 -m unittest deploy.tests.test_new_deploy_contract"
	if caller.verifyCommand != want {
		t.Fatalf("unexpected dry-run verification command %q, want %q", caller.verifyCommand, want)
	}
}

func TestIsTransientCICDAgentError(t *testing.T) {
	if !isTransientCICDAgentError(fmt.Errorf("connection lost: WS connection lost")) {
		t.Fatal("expected WS loss to be transient")
	}
	if !isTransientCICDAgentError(fmt.Errorf("failed to connect to agent WS: EOF")) {
		t.Fatal("expected connect EOF to be transient")
	}
	if !isTransientCICDAgentError(fmt.Errorf("remote error: agent disconnected")) {
		t.Fatal("expected remote agent disconnection to be transient")
	}
	if isTransientCICDAgentError(fmt.Errorf("helm render failed: missing chart")) {
		t.Fatal("expected non-connection error to be permanent")
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

// Mutating agent-native apply runs are rejected without --allow-mutate even
// when an executor is wired.
func TestCICDRunnerRejectsAgentMutationWithoutAllowMutate(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: mutate-gate
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
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{Executor: caller})
	if err == nil {
		t.Fatalf("expected --allow-mutate rejection, got success: %#v", result.Steps)
	}
	if !strings.Contains(err.Error(), "requires --allow-mutate") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("executor must not be called without --allow-mutate, got %#v", caller.calls)
	}
	if len(result.Steps) != 1 || result.Steps[0].Status != "failed" {
		t.Fatalf("expected failed step, got %#v", result.Steps)
	}
}

// With an executor and --allow-mutate, a structured agent-native stage is
// dispatched to the doagent via doops_agent_prompt.
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
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor:    caller,
		AllowMutate: true,
	})
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

func TestCICDRunnerExecutesVersionedCommandTaskDirectly(t *testing.T) {
	dir := t.TempDir()
	workflowPath := writeCICDTestWorkflow(t, dir, `
apiVersion: doops.sh/v1
kind: Workflow
metadata:
  name: deterministic-release
spec:
  policy:
    agentNative: true
  source:
    path: `+quoteYAML(dir)+`
  stages:
    - id: build
      uses: agent.task
      mutates: true
      confirm: true
      with:
        task: run-versioned-build-tool
        requiredCommand: python3 ops/cicd/tools/build_release_images.py build --release-id deadbeef
        verificationCommand: python3 ops/cicd/tools/build_release_images.py verify --release-id deadbeef
`)
	workflow, err := loadCICDWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	caller := &fakeK8SCaller{}
	if _, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor:    caller,
		AllowMutate: true,
		Session:     "release-session",
		SourceSync:  func(string) error { return nil },
	}); err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if len(caller.calls) != 1 || caller.calls[0].tool != "doops_shell" {
		t.Fatalf("expected one direct doops_shell call, got %#v", caller.calls)
	}
	command, _ := caller.calls[0].args["command"].(string)
	for _, want := range []string{
		"cd '/root/ws/release-session'",
		"python3 ops/cicd/tools/build_release_images.py build --release-id deadbeef",
		"python3 ops/cicd/tools/build_release_images.py verify --release-id deadbeef",
	} {
		if !strings.Contains(command, want) {
			t.Fatalf("direct command missing %q:\n%s", want, command)
		}
	}
}

func TestCICDRunnerSkipsMutatingAgentStageOnDryRun(t *testing.T) {
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
	if len(result.Steps) != 1 || result.Steps[0].Status != "skipped" {
		t.Fatalf("expected skipped step, got %#v", result.Steps)
	}
	if result.Steps[0].Message != "dry-run skipped mutating stage" {
		t.Fatalf("unexpected skip message: %#v", result.Steps)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("dry-run must not dispatch mutating stage, got %#v", caller.calls)
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
	result, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor:    errExecutor{},
		AllowMutate: true,
	})
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
	if _, err := runCICDWorkflow(context.Background(), workflow, CICDRunOptions{
		Executor:    caller,
		AllowMutate: true,
	}); err != nil {
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
