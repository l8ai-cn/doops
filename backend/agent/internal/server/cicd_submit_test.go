package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/user/doops/agent/api"
)

func TestCICDSubmitUsesDedicatedGatewayAction(t *testing.T) {
	if got := actionForTool("doops_cicd_submit", json.RawMessage(`{"session_id":"release"}`)); got != ActionCICDSubmit {
		t.Fatalf("submit action = %q, want %q", got, ActionCICDSubmit)
	}
	if got := knownActions["cicd:submit"]; got != ActionCICDSubmit {
		t.Fatalf("known submit action = %q, want %q", got, ActionCICDSubmit)
	}
	for _, action := range defaultGatewayUserActions {
		if action == ActionCICDSubmit {
			t.Fatal("release submission must require an explicit cicd:submit grant")
		}
	}
}

func TestParseCICDReleaseSubmitRejectsCallerLocalAuthority(t *testing.T) {
	raw := mustJSON(t, api.CICDReleaseSubmitParams{
		SessionID: "release",
		Request: json.RawMessage(`{
			"apiVersion":"doops.sh/v3",
			"kind":"ReleaseRequest",
			"repositoryId":"repo_zhiyong",
			"revision":"0123456789abcdef0123456789abcdef01234567",
			"workflowPath":"deploy/workflows/test.yaml",
			"target":"gw-edu-coder",
			"allowMutate":true
		}`),
	})

	_, err := parseCICDReleaseSubmitParams(raw)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected caller target rejection, got %v", err)
	}
}

func TestParseCICDReleaseSubmitRequiresImmutableSourceAndRemoteWorkflow(t *testing.T) {
	raw := mustJSON(t, api.CICDReleaseSubmitParams{
		SessionID: "release",
		Request: json.RawMessage(`{
			"apiVersion":"doops.sh/v3",
			"kind":"ReleaseRequest",
			"repositoryId":"repo_zhiyong",
			"revision":"main",
			"workflowPath":"/tmp/test.yaml",
			"dryRun":true
		}`),
	})

	_, err := parseCICDReleaseSubmitParams(raw)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutable source rejection, got %v", err)
	}
}

func TestGatewayCICDSubmitFailsClosedBeforeContactingDeploymentTarget(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("release-operator")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := store.CreateToken(CreateTokenRequest{
		Kind:   TokenKindUser,
		UserID: user.ID,
		Name:   "release-operator",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{
		Cluster:  "control",
		Instance: "release-plane",
		Actions:  []GatewayAction{ActionCICDSubmit},
	}); err != nil {
		t.Fatalf("grant cicd submit: %v", err)
	}

	hub := NewGatewayHub(store, GatewayHubOptions{})
	params := mustJSON(t, api.ToolCallParams{
		Name: "doops_cicd_submit",
		Arguments: mustJSON(t, api.CICDReleaseSubmitParams{
			SessionID: "release-0123456789ab",
			Request: json.RawMessage(`{
				"apiVersion":"doops.sh/v3",
				"kind":"ReleaseRequest",
				"repositoryId":"repo_zhiyong",
				"revision":"0123456789abcdef0123456789abcdef01234567",
				"workflowPath":"deploy/workflows/test.yaml",
				"allowMutate":true
			}`),
		}),
	})

	var response api.JSONRPCResponse
	err = hub.handleGatewayToolCall(tokenAuthForTest(t, store, token.Plaintext), "control", "release-plane", api.JSONRPCRequest{
		ID:     1,
		Params: params,
	}, func([]byte) error {
		t.Fatal("submission must not relay to a deployment target before the multi-Ops compiler is configured")
		return nil
	}, func(value interface{}) error {
		response = value.(api.JSONRPCResponse)
		return nil
	})
	if err != nil {
		t.Fatalf("handle cicd submit: %v", err)
	}
	if response.Result == nil {
		t.Fatalf("expected fail-closed tool result, got %#v", response)
	}
	result := response.Result.(map[string]interface{})
	content := result["content"].([]map[string]interface{})
	if len(content) == 0 || !strings.Contains(content[0]["text"].(string), "remote multi-Ops compiler is not configured") {
		t.Fatalf("expected compiler configuration failure, got %#v", response)
	}

	events, err := store.ListAuditFiltered(AuditFilter{Action: ActionCICDSubmit, Limit: 1})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 || events[0].Status != "blocked" || !strings.Contains(events[0].Error, "compiler") {
		t.Fatalf("expected blocked cicd submit audit, got %#v", events)
	}
}

func TestGatewayCICDSubmitRejectsNonAcceptedCompilerResult(t *testing.T) {
	store, token, hub := newCICDSubmitTestGateway(t)
	defer store.Close()
	hub.SetCICDReleaseSubmitter(func(context.Context, cicdReleaseSubmission) (CICDReleaseResult, error) {
		return CICDReleaseResult{
			ReleaseID: "release-20260712-0123456789ab",
			Status:    "Blocked",
		}, nil
	})

	response := callCICDSubmitForTest(t, hub, store, token.Plaintext, validCICDSubmitToolCall(t))
	if response.Result == nil {
		t.Fatalf("expected fail-closed tool result, got %#v", response)
	}
	result := response.Result.(map[string]interface{})
	content := result["content"].([]map[string]interface{})
	if len(content) == 0 || !strings.Contains(content[0]["text"].(string), "status") {
		t.Fatalf("expected non-accepted release result rejection, got %#v", response)
	}

	events, err := store.ListAuditFiltered(AuditFilter{Action: ActionCICDSubmit, Limit: 1})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 || events[0].Status != "blocked" {
		t.Fatalf("expected blocked non-accepted submission audit, got %#v", events)
	}
}

func TestGatewayCICDSubmitAuditsInvalidRequest(t *testing.T) {
	store, token, hub := newCICDSubmitTestGateway(t)
	defer store.Close()

	params := mustJSON(t, api.ToolCallParams{
		Name: "doops_cicd_submit",
		Arguments: mustJSON(t, api.CICDReleaseSubmitParams{
			SessionID: "release-invalid",
			Request: json.RawMessage(`{
				"apiVersion":"doops.sh/v3",
				"kind":"ReleaseRequest",
				"repositoryId":"repo_zhiyong",
				"revision":"main",
				"workflowPath":"deploy/workflows/test.yaml",
				"dryRun":true
			}`),
		}),
	})
	response := callCICDSubmitForTest(t, hub, store, token.Plaintext, params)
	if response.Error == nil || !strings.Contains(response.Error.Message, "immutable") {
		t.Fatalf("expected invalid release request error, got %#v", response)
	}

	events, err := store.ListAuditFiltered(AuditFilter{Action: ActionCICDSubmit, Limit: 1})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 || events[0].Status != "invalid" || !strings.Contains(events[0].Error, "immutable") {
		t.Fatalf("expected invalid submission audit, got %#v", events)
	}
}

func newCICDSubmitTestGateway(t *testing.T) (*GatewayStore, CreatedToken, *GatewayHub) {
	t.Helper()
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	user, err := store.CreateUser("release-operator")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := store.CreateToken(CreateTokenRequest{
		Kind:   TokenKindUser,
		UserID: user.ID,
		Name:   "release-operator",
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{
		Cluster:  "control",
		Instance: "release-plane",
		Actions:  []GatewayAction{ActionCICDSubmit},
	}); err != nil {
		t.Fatalf("grant cicd submit: %v", err)
	}
	return store, token, NewGatewayHub(store, GatewayHubOptions{})
}

func validCICDSubmitToolCall(t *testing.T) json.RawMessage {
	t.Helper()
	return mustJSON(t, api.ToolCallParams{
		Name: "doops_cicd_submit",
		Arguments: mustJSON(t, api.CICDReleaseSubmitParams{
			SessionID: "release-0123456789ab",
			Request: json.RawMessage(`{
				"apiVersion":"doops.sh/v3",
				"kind":"ReleaseRequest",
				"repositoryId":"repo_zhiyong",
				"revision":"0123456789abcdef0123456789abcdef01234567",
				"workflowPath":"deploy/workflows/test.yaml",
				"allowMutate":true
			}`),
		}),
	})
}

func callCICDSubmitForTest(t *testing.T, hub *GatewayHub, store *GatewayStore, plaintext string, params json.RawMessage) api.JSONRPCResponse {
	t.Helper()
	var response api.JSONRPCResponse
	err := hub.handleGatewayToolCall(tokenAuthForTest(t, store, plaintext), "control", "release-plane", api.JSONRPCRequest{
		ID:     1,
		Params: params,
	}, func([]byte) error {
		t.Fatal("release submission must not relay to a deployment target")
		return nil
	}, func(value interface{}) error {
		response = value.(api.JSONRPCResponse)
		return nil
	})
	if err != nil {
		t.Fatalf("handle cicd submit: %v", err)
	}
	return response
}

func tokenAuthForTest(t *testing.T, store *GatewayStore, plaintext string) TokenAuth {
	t.Helper()
	auth, err := store.VerifyUserToken(plaintext)
	if err != nil {
		t.Fatalf("verify user token: %v", err)
	}
	return auth
}
