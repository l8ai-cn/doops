package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentCredentialPlanToolParsesBoundWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	sessionRoot := filepath.Join(root, "credential-plan")
	if err := os.MkdirAll(filepath.Join(sessionRoot, "deploy"), 0755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	commit := strings.Repeat("a", 40)
	if err := os.WriteFile(filepath.Join(sessionRoot, ".doops-ready"), []byte(commit+"\n"), 0600); err != nil {
		t.Fatalf("write workspace binding: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionRoot, "deploy", "release.yaml"), []byte(`
apiVersion: doops.sh/v2
kind: DeploymentTemplate
metadata:
  name: oilan-release
spec:
  application: oilan
  environment: production
  configurationSource: deploy/environments.yaml
  credentialRefs:
    - name: cnb-registry
      use: imagePull
      namespace: oilan
      registryRepository: team/app
      registryReference: latest
      workload:
        kind: Deployment
        name: oilan-api
`), 0600); err != nil {
		t.Fatalf("write workflow: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionRoot, "deploy", "environments.yaml"), []byte(`
environments:
  production:
    target:
      name: oilan-prod
      cluster: oilan
      instance: prod
`), 0600); err != nil {
		t.Fatalf("write configuration: %v", err)
	}

	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callTool(t, conn, "doops_credential_plan", map[string]interface{}{
		"session_id":       "credential-plan",
		"workflow_path":    "deploy/release.yaml",
		"workspace_commit": commit,
	})
	var plan CredentialPlan
	if err := json.Unmarshal([]byte(result), &plan); err != nil {
		t.Fatalf("decode credential plan: %v", err)
	}
	if plan.Cluster != "oilan" || plan.Instance != "prod" || len(plan.CredentialRefs) != 1 {
		t.Fatalf("unexpected credential plan: %#v", plan)
	}
}

func TestAgentCredentialPlanToolRejectsWorkspaceCommitMismatch(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	sessionRoot := filepath.Join(root, "credential-plan")
	if err := os.MkdirAll(sessionRoot, 0755); err != nil {
		t.Fatalf("create workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sessionRoot, ".doops-ready"), []byte(strings.Repeat("a", 40)), 0600); err != nil {
		t.Fatalf("write workspace binding: %v", err)
	}

	_, err := handleCredentialPlanTool(mustJSON(t, map[string]interface{}{
		"session_id":       "credential-plan",
		"workflow_path":    "deploy/release.yaml",
		"workspace_commit": strings.Repeat("b", 40),
	}))
	if err == nil || !strings.Contains(err.Error(), "workspace commit mismatch") {
		t.Fatalf("workspace mismatch error = %v", err)
	}
}

func TestAgentCredentialMaterializeToolAcceptsBoundSession(t *testing.T) {
	bin, _ := installCredentialTestCommands(t)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_TEST_SECRET_TYPE", "Opaque")
	t.Setenv("DOOPS_TEST_SECRET_KEYS", "API_KEY")

	result, err := handleCredentialMaterializeTool(context.Background(), mustJSON(t, map[string]any{
		"session_id":      "credential-apply",
		"credential_id":   "cred_opaque",
		"version_id":      "ver_opaque",
		"credential_type": CredentialTypeOpaque,
		"use":             CredentialUseOpaqueSecret,
		"namespace":       "app",
		"required_keys":   []string{"API_KEY"},
		"payload":         map[string]any{"data": map[string]string{"API_KEY": "session-canary-secret"}},
	}))
	if err != nil {
		t.Fatalf("materialize tool with session binding: %v", err)
	}
	if strings.Contains(result, "session-canary-secret") {
		t.Fatalf("materialize result exposed credential payload: %s", result)
	}
}
