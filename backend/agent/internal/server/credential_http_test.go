package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCredentialHTTPPersonalOwnershipPlatformBoundaryAndPayloadRedaction(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("h", 32))
	store := openCredentialTestStore(t)
	owner, ownerToken := createCredentialHTTPUser(t, store, "owner", false)
	_, adminToken := createCredentialHTTPUser(t, store, "admin", true)
	hub := NewGatewayHub(store, GatewayHubOptions{})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)

	personal := credentialHTTPRequest(t, mux, ownerToken, http.MethodPost, "/v1/credentials", map[string]any{
		"name":  "owner-registry",
		"scope": "personal",
		"type":  "registry",
	})
	if personal.Code != http.StatusOK {
		t.Fatalf("create personal credential: HTTP %d: %s", personal.Code, personal.Body.String())
	}
	var personalBody CredentialResource
	if err := json.Unmarshal(personal.Body.Bytes(), &personalBody); err != nil {
		t.Fatalf("decode personal credential: %v", err)
	}
	if personalBody.OwnerID != owner.ID || personalBody.Scope != CredentialScopePersonal {
		t.Fatalf("unexpected personal credential: %#v", personalBody)
	}

	platformDenied := credentialHTTPRequest(t, mux, ownerToken, http.MethodPost, "/v1/credentials", map[string]any{
		"name":  "platform-registry",
		"scope": "platform",
		"type":  "registry",
	})
	if platformDenied.Code != http.StatusForbidden {
		t.Fatalf("non-admin platform create = HTTP %d, want 403", platformDenied.Code)
	}
	platform := credentialHTTPRequest(t, mux, adminToken, http.MethodPost, "/v1/credentials", map[string]any{
		"name":  "platform-registry",
		"scope": "platform",
		"type":  "registry",
	})
	if platform.Code != http.StatusOK {
		t.Fatalf("admin platform create: HTTP %d: %s", platform.Code, platform.Body.String())
	}

	put := credentialHTTPRequest(t, mux, ownerToken, http.MethodPut,
		"/v1/credentials/"+personalBody.ID+"/payload",
		json.RawMessage(`{"server":"registry.example.com","username":"owner","password":"http-canary-secret"}`))
	if put.Code != http.StatusOK {
		t.Fatalf("put personal payload: HTTP %d: %s", put.Code, put.Body.String())
	}
	if strings.Contains(put.Body.String(), "http-canary-secret") {
		t.Fatalf("payload response exposed credential value: %s", put.Body.String())
	}

	list := credentialHTTPRequest(t, mux, ownerToken, http.MethodGet, "/v1/credentials", nil)
	if list.Code != http.StatusOK {
		t.Fatalf("list credentials: HTTP %d: %s", list.Code, list.Body.String())
	}
	if strings.Contains(list.Body.String(), "http-canary-secret") {
		t.Fatalf("credential list exposed payload value: %s", list.Body.String())
	}
	audits, err := store.ListAudit(100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	auditJSON, err := json.Marshal(audits)
	if err != nil {
		t.Fatalf("marshal audits: %v", err)
	}
	if strings.Contains(string(auditJSON), "http-canary-secret") {
		t.Fatalf("credential audit exposed payload value: %s", auditJSON)
	}
}

func TestCredentialHTTPGrantRequiresOwnerTargetAuthority(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("a", 32))
	store := openCredentialTestStore(t)
	owner, ownerToken := createCredentialHTTPUser(t, store, "owner", false)
	deployer, deployerToken := createCredentialHTTPUser(t, store, "deployer", false)
	hub := NewGatewayHub(store, GatewayHubOptions{})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)

	created := credentialHTTPRequest(t, mux, ownerToken, http.MethodPost, "/v1/credentials", map[string]any{
		"name":  "owner-registry",
		"scope": "personal",
		"type":  "registry",
	})
	var credential CredentialResource
	if err := json.Unmarshal(created.Body.Bytes(), &credential); err != nil {
		t.Fatalf("decode credential: %v", err)
	}
	grantBody := map[string]any{
		"grantee_id":  deployer.ID,
		"cluster":     "doops-edu",
		"instance":    "edu-coder",
		"project":     "oilan",
		"environment": "production",
		"template":    "oilan-agent-release",
		"namespace":   "kz-ops",
		"uses":        []string{"imagePull"},
	}
	denied := credentialHTTPRequest(t, mux, ownerToken, http.MethodPost,
		"/v1/credentials/"+credential.ID+"/grants", grantBody)
	if denied.Code != http.StatusForbidden {
		t.Fatalf("owner without target authority grant = HTTP %d, want 403", denied.Code)
	}

	if err := store.GrantUser(owner.ID, ScopeGrant{
		Cluster: "doops-edu", Instance: "edu-coder", Actions: []GatewayAction{ActionAsk},
	}); err != nil {
		t.Fatalf("grant owner target authority: %v", err)
	}
	allowed := credentialHTTPRequest(t, mux, ownerToken, http.MethodPost,
		"/v1/credentials/"+credential.ID+"/grants", grantBody)
	if allowed.Code != http.StatusOK {
		t.Fatalf("owner with target authority grant: HTTP %d: %s", allowed.Code, allowed.Body.String())
	}
	var grant CredentialGrant
	if err := json.Unmarshal(allowed.Body.Bytes(), &grant); err != nil {
		t.Fatalf("decode grant: %v", err)
	}
	deniedRevoke := credentialHTTPRequest(t, mux, deployerToken, http.MethodDelete,
		"/v1/credentials/"+credential.ID+"/grants", map[string]any{"grant_id": grant.ID})
	if deniedRevoke.Code != http.StatusForbidden {
		t.Fatalf("grantee revoke = HTTP %d, want 403", deniedRevoke.Code)
	}
	revoked := credentialHTTPRequest(t, mux, ownerToken, http.MethodDelete,
		"/v1/credentials/"+credential.ID+"/grants", map[string]any{"grant_id": grant.ID})
	if revoked.Code != http.StatusOK {
		t.Fatalf("owner grant revoke: HTTP %d: %s", revoked.Code, revoked.Body.String())
	}
	active, err := store.ActiveCredentialGrants(credential.ID)
	if err != nil || len(active) != 0 {
		t.Fatalf("active grants after revoke = %#v, %v", active, err)
	}
}

func createCredentialHTTPUser(t *testing.T, store *GatewayStore, name string, admin bool) (GatewayUser, string) {
	t.Helper()
	user, err := store.CreateUser(name)
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	actions := []GatewayAction{ActionCredentialCreate, ActionCredentialMetadata, ActionCredentialUse}
	if admin {
		actions = append(actions, ActionCredentialGrant, ActionCredentialRotate, ActionCredentialRevoke, ActionCredentialAudit)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: actions}); err != nil {
		t.Fatalf("grant credential actions: %v", err)
	}
	token, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: name})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return user, token.Plaintext
}

func credentialHTTPRequest(
	t *testing.T,
	handler http.Handler,
	token, method, path string,
	body any,
) *httptest.ResponseRecorder {
	t.Helper()
	var payload []byte
	switch value := body.(type) {
	case nil:
	case json.RawMessage:
		payload = value
	default:
		var err error
		payload, err = json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal request: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	return recorder
}
