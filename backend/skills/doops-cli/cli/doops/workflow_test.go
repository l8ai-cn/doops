package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeWorkflowCaller struct {
	toolName  string
	arguments map[string]interface{}
	output    string
	calls     int
	onCall    func()
}

func (f *fakeWorkflowCaller) CallStructured(toolName string, arguments map[string]interface{}, destination interface{}) error {
	f.calls++
	f.toolName = toolName
	f.arguments = arguments
	if f.onCall != nil {
		f.onCall()
	}
	return json.Unmarshal([]byte(f.output), destination)
}

func TestRunCICDCommandRoutesApplyThroughPushAndAsk(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create repository marker: %v", err)
	}
	workflow := filepath.Join(root, "backend", "deploy", "workflows", "oilan-agent-bootstrap.yaml")
	if err := os.MkdirAll(filepath.Dir(workflow), 0o755); err != nil {
		t.Fatalf("create workflow directory: %v", err)
	}
	if err := os.WriteFile(workflow, []byte("apiVersion: doops.sh/v2\nkind: DeploymentTemplate\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	caller := &fakeWorkflowCaller{output: `{
		"apiVersion":"doops.sh/v2",
		"kind":"DeploymentRun",
		"metadata":{"workspaceCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			"spec":{"mode":"apply"},
			"status":{
				"phase":"converged",
				"mutationCount":1,
				"resultDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"capabilities":{"source-observer":"available"},
				"evidence":[{
					"subject":"source",
					"module":"doops-source-observer",
					"toolCallId":"tool-source",
					"observedAt":"2026-07-14T00:00:00Z",
				"result":{"revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			}]
		}
	}`}
	var pushedRoot string
	var events []string
	var prepareBody struct {
		Cluster         string `json:"cluster"`
		Instance        string `json:"instance"`
		SessionID       string `json:"session_id"`
		WorkflowPath    string `json:"workflow_path"`
		WorkspaceCommit string `json:"workspace_commit"`
		Mode            string `json:"mode"`
	}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		events = append(events, "prepare")
		if r.Method != http.MethodPost || r.URL.Path != "/v1/credentials/prepare" {
			t.Fatalf("unexpected credential prepare request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer workflow-token" {
			t.Fatalf("credential prepare authorization mismatch: %q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&prepareBody); err != nil {
			t.Fatalf("decode credential prepare body: %v", err)
		}
		_, _ = fmt.Fprint(w, `{
			"apiVersion":"doops.sh/v2",
			"kind":"CredentialRun",
			"id":"credrun_123",
			"mode":"apply",
			"cluster":"doops-edu",
			"instance":"edu-coder",
			"materializations":[{
				"credentialId":"cred_registry",
				"versionId":"credver_1",
				"resourceName":"registry-pull",
				"namespace":"apps",
				"secretType":"kubernetes.io/dockerconfigjson",
				"keys":[".dockerconfigjson"],
				"resourceVersion":"42",
				"digest":"sha256:manifest",
				"fingerprint":"sha256:fingerprint",
				"expiresAt":"2026-08-01T00:00:00Z",
				"status":"verified"
			}]
		}`)
	}))
	defer gateway.Close()
	originalPush := pushWorkflowWorkspace
	originalOpen := openWorkflowAgent
	pushWorkflowWorkspace = func(_ Server, src, _ string, dryRun bool, _ []string, sessionID string) (string, error) {
		events = append(events, "push")
		pushedRoot = src
		if dryRun {
			t.Fatal("workspace push must occur before the agent apply")
		}
		if sessionID != "release-test" {
			t.Fatalf("unexpected session %q", sessionID)
		}
		return strings.Repeat("a", 40), nil
	}
	openWorkflowAgent = func(_ Server, _ *SessionStore, _ string, _ bool) (workflowAgentCaller, func()) {
		caller.onCall = func() { events = append(events, "prompt") }
		return caller, func() {}
	}
	t.Cleanup(func() {
		pushWorkflowWorkspace = originalPush
		openWorkflowAgent = originalOpen
	})

	err := runCICDCommand(
		context.Background(),
		[]string{
			"run",
			"-f", workflow,
			"-target", "agent-runtime",
			"--set", "releaseId=" + strings.Repeat("b", 40),
		},
		[]Server{{
			Name:     "agent-runtime",
			Gateway:  gateway.URL,
			Cluster:  "doops-edu",
			Instance: "edu-coder",
			Token:    "workflow-token",
		}},
		nil,
		NewSessionStore(),
		"release-test",
		false,
	)
	if err != nil {
		t.Fatalf("run workflow: %v", err)
	}
	if pushedRoot != root {
		t.Fatalf("workflow repository root mismatch: got %q want %q", pushedRoot, root)
	}
	if got, want := strings.Join(events, ","), "push,prepare,prompt"; got != want {
		t.Fatalf("workflow operation order mismatch: got %s want %s", got, want)
	}
	if prepareBody.Cluster != "doops-edu" ||
		prepareBody.Instance != "edu-coder" ||
		prepareBody.SessionID != "release-test" ||
		prepareBody.WorkflowPath != "backend/deploy/workflows/oilan-agent-bootstrap.yaml" ||
		prepareBody.WorkspaceCommit != strings.Repeat("a", 40) ||
		prepareBody.Mode != "apply" {
		t.Fatalf("credential prepare body mismatch: %#v", prepareBody)
	}
	if caller.toolName != "doops_agent_prompt" {
		t.Fatalf("expected generic Ask tool, got %q", caller.toolName)
	}
	if caller.arguments["workspace_commit"] != strings.Repeat("a", 40) {
		t.Fatalf("workspace commit not bound to Ask: %#v", caller.arguments)
	}
	if caller.arguments["response_format"] != "json" {
		t.Fatalf("Ask must request structured result: %#v", caller.arguments)
	}
	if caller.arguments["operation"] != "apply" {
		t.Fatalf("apply must use the explicit native operation: %#v", caller.arguments)
	}
	var instruction map[string]interface{}
	if err := json.Unmarshal([]byte(caller.arguments["instruction"].(string)), &instruction); err != nil {
		t.Fatalf("decode instruction: %v", err)
	}
	if instruction["skill"] != "$doops-cicd" ||
		instruction["executionMode"] != "apply" ||
		instruction["workflowPath"] != "backend/deploy/workflows/oilan-agent-bootstrap.yaml" {
		t.Fatalf("unexpected Skill instruction: %#v", instruction)
	}
	credentialRun, ok := instruction["credentialRun"].(map[string]interface{})
	if !ok {
		t.Fatalf("credential run is missing from instruction: %#v", instruction)
	}
	if credentialRun["id"] != "credrun_123" {
		t.Fatalf("credential run id missing from instruction: %#v", credentialRun)
	}
	materializations, ok := credentialRun["materializations"].([]interface{})
	if !ok || len(materializations) != 1 {
		t.Fatalf("credential materializations missing from instruction: %#v", credentialRun)
	}
	materialization, ok := materializations[0].(map[string]interface{})
	if !ok ||
		materialization["credentialId"] != "cred_registry" ||
		materialization["versionId"] != "credver_1" ||
		materialization["resourceName"] != "registry-pull" ||
		materialization["namespace"] != "apps" ||
		materialization["secretType"] != "kubernetes.io/dockerconfigjson" ||
		materialization["resourceVersion"] != "42" ||
		materialization["digest"] != "sha256:manifest" ||
		materialization["fingerprint"] != "sha256:fingerprint" ||
		materialization["expiresAt"] != "2026-08-01T00:00:00Z" ||
		materialization["status"] != "verified" {
		t.Fatalf("credential materialization mismatch: %#v", materialization)
	}
	if _, leaked := materialization["payload"]; leaked {
		t.Fatalf("credential materialization instruction must not include payload: %#v", materialization)
	}
	inputs := instruction["inputs"].(map[string]interface{})
	if inputs["releaseId"] != strings.Repeat("b", 40) {
		t.Fatalf("release input not forwarded: %#v", inputs)
	}
	resultContract := instruction["resultContract"].(map[string]interface{})
	if resultContract["apiVersion"] != "doops.sh/v2" ||
		resultContract["kind"] != "DeploymentRun" ||
		resultContract["requireEvidence"] != true {
		t.Fatalf("unexpected structured result contract: %#v", resultContract)
	}
}

func TestRunCICDCommandUsesDryRunCredentialPreparation(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create repository marker: %v", err)
	}
	workflow := filepath.Join(root, "release.yaml")
	if err := os.WriteFile(workflow, []byte(`apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: release
spec:
  application: app
  environment: production
  configurationSource: environments.yaml
`), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "environments.yaml"), []byte(`environments:
  production:
    target:
      name: agent-runtime
      cluster: doops-edu
      instance: edu-coder
`), 0o644); err != nil {
		t.Fatalf("write environment source: %v", err)
	}

	var mode string
	var declarationFieldsPresent bool
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/credentials/prepare" {
			t.Fatalf("unexpected credential prepare request: %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode credential prepare body: %v", err)
		}
		mode, _ = body["mode"].(string)
		_, workflowPresent := body["workflow_yaml"]
		_, configurationPresent := body["configuration_yaml"]
		declarationFieldsPresent = workflowPresent || configurationPresent
		_, _ = fmt.Fprint(w, `{
			"apiVersion":"doops.sh/v2",
			"kind":"CredentialRun",
			"id":"credrun_dry",
			"mode":"dry-run",
			"cluster":"doops-edu",
			"instance":"edu-coder",
			"materializations":[]
		}`)
	}))
	defer gateway.Close()

	caller := &fakeWorkflowCaller{output: workflowResultJSON("dry-run", 0)}
	originalPush := pushWorkflowWorkspace
	originalOpen := openWorkflowAgent
	var pushDryRun bool
	pushWorkflowWorkspace = func(_ Server, _ string, _ string, dryRun bool, _ []string, _ string) (string, error) {
		pushDryRun = dryRun
		return strings.Repeat("a", 40), nil
	}
	openWorkflowAgent = func(Server, *SessionStore, string, bool) (workflowAgentCaller, func()) {
		return caller, func() {}
	}
	t.Cleanup(func() {
		pushWorkflowWorkspace = originalPush
		openWorkflowAgent = originalOpen
	})

	err := runCICDCommand(
		context.Background(),
		[]string{"run", "-f", workflow, "-target", "agent-runtime", "--dry-run"},
		[]Server{{Name: "agent-runtime", Gateway: gateway.URL, Cluster: "doops-edu", Instance: "edu-coder"}},
		nil,
		NewSessionStore(),
		"release-test",
		false,
	)
	if err != nil {
		t.Fatalf("run dry-run workflow: %v", err)
	}
	if mode != "dry-run" {
		t.Fatalf("credential prepare mode mismatch: got %q", mode)
	}
	if pushDryRun || declarationFieldsPresent {
		t.Fatalf("dry-run must push the workspace and resolve declarations on the Agent")
	}
	if caller.arguments["operation"] != "ask" {
		t.Fatalf("dry-run must use ask operation: %#v", caller.arguments)
	}
}

func TestRunCICDCommandStopsBeforePromptWhenCredentialPrepareFails(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create repository marker: %v", err)
	}
	workflow := filepath.Join(root, "release.yaml")
	if err := os.WriteFile(workflow, []byte("kind: DeploymentTemplate\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "credential_grant_denied", http.StatusForbidden)
	}))
	defer gateway.Close()
	caller := &fakeWorkflowCaller{output: workflowResultJSON("apply", 1)}
	originalPush := pushWorkflowWorkspace
	originalOpen := openWorkflowAgent
	pushWorkflowWorkspace = func(Server, string, string, bool, []string, string) (string, error) {
		return strings.Repeat("a", 40), nil
	}
	openWorkflowAgent = func(Server, *SessionStore, string, bool) (workflowAgentCaller, func()) {
		return caller, func() {}
	}
	t.Cleanup(func() {
		pushWorkflowWorkspace = originalPush
		openWorkflowAgent = originalOpen
	})

	err := runCICDCommand(
		context.Background(),
		[]string{"run", "-f", workflow, "-target", "agent-runtime"},
		[]Server{{Name: "agent-runtime", Gateway: gateway.URL, Cluster: "doops-edu", Instance: "edu-coder"}},
		nil,
		NewSessionStore(),
		"release-test",
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "credential prepare") {
		t.Fatalf("credential prepare failure must be returned, got %v", err)
	}
	if caller.calls != 0 {
		t.Fatalf("credential prepare failure must not call prompt, got %d calls", caller.calls)
	}
}

func workflowResultJSON(mode string, mutationCount int) string {
	return fmt.Sprintf(`{
		"apiVersion":"doops.sh/v2",
		"kind":"DeploymentRun",
		"metadata":{"workspaceCommit":"%s"},
		"spec":{"mode":%q},
		"status":{
			"phase":"planned",
			"mutationCount":%d,
			"resultDigest":"sha256:%s",
			"capabilities":{"source-observer":"available"},
			"evidence":[{
				"subject":"source",
				"module":"doops-source-observer",
				"toolCallId":"tool-source",
				"observedAt":"2026-07-14T00:00:00Z",
				"result":{"revision":"immutable"}
			}]
		}
	}`, strings.Repeat("a", 40), mode, mutationCount, strings.Repeat("a", 64))
}

func TestValidateWorkflowResultRejectsSyntheticResultsAndDryRunMutation(t *testing.T) {
	for name, result := range map[string]string{
		"synthetic admitted": `{
			"apiVersion":"doops.sh/v2",
			"kind":"DeploymentRun",
			"spec":{"mode":"dry-run"},
			"status":{"phase":"admitted","mutationCount":0,"evidence":[]}
		}`,
		"dry-run mutation": `{
			"apiVersion":"doops.sh/v2",
			"kind":"DeploymentRun",
			"spec":{"mode":"dry-run"},
			"status":{"phase":"planned","mutationCount":1,"evidence":[]}
		}`,
		"missing evidence field": `{
			"apiVersion":"doops.sh/v2",
			"kind":"DeploymentRun",
			"spec":{"mode":"dry-run"},
			"status":{"phase":"blocked","mutationCount":0}
		}`,
		"empty evidence": `{
			"apiVersion":"doops.sh/v2",
			"kind":"DeploymentRun",
			"spec":{"mode":"dry-run"},
				"status":{"phase":"planned","mutationCount":0,"evidence":[]}
			}`,
		"unbound evidence": `{
				"apiVersion":"doops.sh/v2",
				"kind":"DeploymentRun",
				"metadata":{"workspaceCommit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				"spec":{"mode":"dry-run"},
				"status":{
					"phase":"planned",
					"mutationCount":0,
					"resultDigest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					"capabilities":{"source-observer":"available"},
					"evidence":[{
						"subject":"source",
						"module":"doops-source-observer",
						"observedAt":"2026-07-14T00:00:00Z",
						"result":{"revision":"immutable"}
					}]
				}
			}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateWorkflowResult(result, "dry-run", strings.Repeat("a", 40)); err == nil {
				t.Fatal("invalid or synthetic result must be rejected")
			}
		})
	}
}

func TestRunCICDCommandRequiresTargetBeforePush(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatalf("create repository marker: %v", err)
	}
	workflow := filepath.Join(root, "release.yaml")
	if err := os.WriteFile(workflow, []byte("kind: DeploymentTemplate\n"), 0o644); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	originalPush := pushWorkflowWorkspace
	pushes := 0
	pushWorkflowWorkspace = func(Server, string, string, bool, []string, string) (string, error) {
		pushes++
		return strings.Repeat("a", 40), nil
	}
	t.Cleanup(func() { pushWorkflowWorkspace = originalPush })
	server := []Server{{Name: "agent-runtime", Gateway: "https://gateway.example.com"}}

	for name, args := range map[string][]string{
		"missing target": {"run", "-f", workflow, "--dry-run"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := runCICDCommand(context.Background(), args, server, nil, NewSessionStore(), "release-test", false); err == nil {
				t.Fatal("invalid invocation must fail")
			}
		})
	}
	if pushes != 0 {
		t.Fatalf("invalid invocations must not push, got %d pushes", pushes)
	}
}
