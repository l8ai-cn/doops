package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeWorkflowCaller struct {
	toolName  string
	arguments map[string]interface{}
	output    string
}

func (f *fakeWorkflowCaller) CallAndCapture(toolName string, arguments map[string]interface{}) (string, error) {
	f.toolName = toolName
	f.arguments = arguments
	return f.output, nil
}

func TestRunCICDCommandRoutesExistingTemplateThroughPushAndAsk(t *testing.T) {
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
		"spec":{"mode":"dry-run"},
		"status":{
			"phase":"planned",
			"mutationCount":0,
			"evidence":[{
				"subject":"source",
				"module":"doops-source-observer",
				"observedAt":"2026-07-14T00:00:00Z",
				"result":{"revision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
			}]
		}
	}`}
	var pushedRoot string
	originalPush := pushWorkflowWorkspace
	originalOpen := openWorkflowAgent
	pushWorkflowWorkspace = func(_ Server, src, _ string, dryRun bool, _ []string, sessionID string) (string, error) {
		pushedRoot = src
		if dryRun {
			t.Fatal("workspace push must occur before the agent dry-run")
		}
		if sessionID != "release-test" {
			t.Fatalf("unexpected session %q", sessionID)
		}
		return strings.Repeat("a", 40), nil
	}
	openWorkflowAgent = func(_ Server, _ *SessionStore, _ string, _ bool) (workflowAgentCaller, func()) {
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
			"--dry-run",
			"--set", "releaseId=" + strings.Repeat("b", 40),
		},
		[]Server{{Name: "agent-runtime", Gateway: "https://gateway.example.com"}},
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
	if caller.toolName != "doops_agent_prompt" {
		t.Fatalf("expected generic Ask tool, got %q", caller.toolName)
	}
	if caller.arguments["workspace_commit"] != strings.Repeat("a", 40) {
		t.Fatalf("workspace commit not bound to Ask: %#v", caller.arguments)
	}
	if caller.arguments["response_format"] != "json" {
		t.Fatalf("Ask must request structured result: %#v", caller.arguments)
	}
	var instruction map[string]interface{}
	if err := json.Unmarshal([]byte(caller.arguments["instruction"].(string)), &instruction); err != nil {
		t.Fatalf("decode instruction: %v", err)
	}
	if instruction["skill"] != "$doops-cicd" ||
		instruction["executionMode"] != "dry-run" ||
		instruction["mutationAuthorized"] != false ||
		instruction["workflowPath"] != "backend/deploy/workflows/oilan-agent-bootstrap.yaml" {
		t.Fatalf("unexpected Skill instruction: %#v", instruction)
	}
	inputs := instruction["inputs"].(map[string]interface{})
	if inputs["releaseId"] != strings.Repeat("b", 40) {
		t.Fatalf("release input not forwarded: %#v", inputs)
	}
}

func TestValidateWorkflowResultRejectsSyntheticAdmittedAndDryRunMutation(t *testing.T) {
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
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateWorkflowResult(result, "dry-run"); err == nil {
				t.Fatal("invalid or synthetic result must be rejected")
			}
		})
	}
}

func TestRunCICDCommandRequiresExplicitModeAndTargetBeforePush(t *testing.T) {
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
		"missing mode":   {"run", "-f", workflow, "-target", "agent-runtime"},
		"ambiguous mode": {"run", "-f", workflow, "-target", "agent-runtime", "--dry-run", "--allow-mutate"},
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
