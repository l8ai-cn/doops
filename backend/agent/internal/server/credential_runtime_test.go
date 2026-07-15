package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCredentialPrepareAuthorizesParsedPlanAndDoesNotLeakPayload(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("p", 32))
	store := openCredentialTestStore(t)
	deployer, deployerToken := createCredentialHTTPUser(t, store, "deployer", false)
	admin := createCredentialTestUser(t, store, "credential-admin")
	if err := store.GrantUser(admin.ID, ScopeGrant{
		Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionCredentialGrant, ActionCredentialRotate},
	}); err != nil {
		t.Fatalf("grant credential admin: %v", err)
	}
	if err := store.GrantUser(deployer.ID, ScopeGrant{
		Cluster: "doops-edu", Instance: "edu-coder", Actions: []GatewayAction{ActionAsk, ActionCredentialUse},
	}); err != nil {
		t.Fatalf("grant deployer target actions: %v", err)
	}
	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name: "cnb-oci-pull", Scope: CredentialScopePlatform, Type: CredentialTypeRegistry, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	version, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"server":"registry.example.com","username":"deploy","password":"runtime-canary-secret"}`),
		CreatedBy:    admin.ID,
		Activate:     true,
	})
	if err != nil {
		t.Fatalf("put credential: %v", err)
	}
	_, err = store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: credential.ID,
		GranteeID:    deployer.ID,
		Cluster:      "doops-edu",
		Instance:     "edu-coder",
		Project:      "oilan",
		Environment:  "production",
		Template:     "oilan-agent-release",
		Namespace:    "kz-ops",
		Uses:         []CredentialUse{CredentialUseImagePull},
		CreatedBy:    admin.ID,
	})
	if err != nil {
		t.Fatalf("create credential grant: %v", err)
	}
	agentToken, err := store.CreateToken(CreateTokenRequest{
		Kind: TokenKindAgent, Name: "agent", Cluster: "doops-edu", Instance: "edu-coder",
	})
	if err != nil {
		t.Fatalf("create agent token: %v", err)
	}

	hub := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, OperationTimeout: time.Second})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	agentConn := dialFakeGatewayAgent(t, server.URL, agentToken.Plaintext, "doops-edu", "edu-coder")
	defer agentConn.Close()

	var materializeCalls atomic.Int64
	var cleanupCalls atomic.Int64
	var payloadObserved atomic.Bool
	go serveCredentialRuntimeAgent(t, agentConn, &materializeCalls, &cleanupCalls, &payloadObserved)
	waitForGatewayAgent(t, hub, "doops-edu", "edu-coder")

	dryRun := credentialHTTPRequest(t, mux, deployerToken, http.MethodPost, "/v1/credentials/prepare", map[string]any{
		"cluster":          "doops-edu",
		"instance":         "edu-coder",
		"session_id":       "release-1",
		"workflow_path":    "deploy/release.yaml",
		"workspace_commit": strings.Repeat("a", 40),
		"mode":             "dry-run",
	})
	if dryRun.Code != http.StatusOK {
		t.Fatalf("credential dry-run: HTTP %d: %s", dryRun.Code, dryRun.Body.String())
	}
	if materializeCalls.Load() != 0 {
		t.Fatalf("dry-run performed %d materializations", materializeCalls.Load())
	}

	apply := credentialHTTPRequest(t, mux, deployerToken, http.MethodPost, "/v1/credentials/prepare", map[string]any{
		"cluster":          "doops-edu",
		"instance":         "edu-coder",
		"session_id":       "release-1",
		"workflow_path":    "deploy/release.yaml",
		"workspace_commit": strings.Repeat("a", 40),
		"mode":             "apply",
	})
	if apply.Code != http.StatusOK {
		t.Fatalf("credential apply: HTTP %d: %s", apply.Code, apply.Body.String())
	}
	if materializeCalls.Load() != 1 || !payloadObserved.Load() {
		t.Fatalf("materialization calls=%d payloadObserved=%v", materializeCalls.Load(), payloadObserved.Load())
	}
	if strings.Contains(apply.Body.String(), "runtime-canary-secret") {
		t.Fatalf("credential apply response exposed payload: %s", apply.Body.String())
	}
	var run CredentialRun
	if err := json.Unmarshal(apply.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode credential run: %v", err)
	}
	if run.MutationCount != 1 || len(run.Materializations) != 1 ||
		run.Materializations[0].CredentialID != credential.ID ||
		run.Materializations[0].VersionID != version.ID {
		t.Fatalf("unexpected credential run: %#v", run)
	}
	staged, err := store.PutCredentialVersion(CredentialVersionPutRequest{
		CredentialID: credential.ID,
		Payload:      json.RawMessage(`{"server":"registry.example.com","username":"deploy","password":"rotated-canary-secret"}`),
		CreatedBy:    admin.ID,
	})
	if err != nil {
		t.Fatalf("put staged credential: %v", err)
	}
	verify := credentialHTTPRequest(t, mux, deployerToken, http.MethodPost,
		"/v1/credentials/"+credential.ID+"/verify", map[string]any{
			"version_id":       staged.ID,
			"cluster":          "doops-edu",
			"instance":         "edu-coder",
			"session_id":       "release-1",
			"workflow_path":    "deploy/release.yaml",
			"workspace_commit": strings.Repeat("a", 40),
		})
	if verify.Code != http.StatusForbidden {
		t.Fatalf("deployer credential verify = HTTP %d, want 403: %s", verify.Code, verify.Body.String())
	}
	_, adminToken := createCredentialHTTPUser(t, store, "credential-operator", true)
	verify = credentialHTTPRequest(t, mux, adminToken, http.MethodPost,
		"/v1/credentials/"+credential.ID+"/verify", map[string]any{
			"version_id":       staged.ID,
			"cluster":          "doops-edu",
			"instance":         "edu-coder",
			"session_id":       "release-1",
			"workflow_path":    "deploy/release.yaml",
			"workspace_commit": strings.Repeat("a", 40),
		})
	if verify.Code != http.StatusOK {
		t.Fatalf("credential verify: HTTP %d: %s", verify.Code, verify.Body.String())
	}
	promote := credentialHTTPRequest(t, mux, adminToken, http.MethodPost,
		"/v1/credentials/"+credential.ID+"/promote", map[string]any{"version_id": staged.ID})
	if promote.Code != http.StatusOK {
		t.Fatalf("credential promote: HTTP %d: %s", promote.Code, promote.Body.String())
	}
	active, err := store.ActiveCredentialVersion(credential.ID)
	if err != nil || active.ID != staged.ID {
		t.Fatalf("active credential after promotion = %#v, %v", active, err)
	}
	if materializeCalls.Load() != 4 {
		t.Fatalf("apply + verify/restore + promote materializations = %d, want 4", materializeCalls.Load())
	}
	revoke := credentialHTTPRequest(t, mux, adminToken, http.MethodPost,
		"/v1/credentials/"+credential.ID+"/revoke", nil)
	if revoke.Code != http.StatusOK {
		t.Fatalf("credential revoke: HTTP %d: %s", revoke.Code, revoke.Body.String())
	}
	revoked, err := store.CredentialByID(credential.ID)
	if err != nil || revoked.State != CredentialStateRevoked || cleanupCalls.Load() != 1 {
		t.Fatalf("credential revoke state=%#v cleanupCalls=%d err=%v", revoked, cleanupCalls.Load(), err)
	}
	audits, err := store.ListAudit(100)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	serialized, _ := json.Marshal(audits)
	if strings.Contains(string(serialized), "runtime-canary-secret") {
		t.Fatalf("credential audit exposed payload: %s", serialized)
	}
}

func TestCredentialPlanBundleResolvesToExactCredentialID(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("b", 32))
	store := openCredentialTestStore(t)
	admin := createCredentialTestUser(t, store, "admin")
	deployer := createCredentialTestUser(t, store, "deployer")
	if err := store.GrantUser(admin.ID, ScopeGrant{
		Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionCredentialGrant},
	}); err != nil {
		t.Fatalf("grant credential admin: %v", err)
	}
	credential, err := store.CreateCredential(CredentialCreateRequest{
		Name: "registry", Scope: CredentialScopePlatform, Type: CredentialTypeRegistry, CreatedBy: admin.ID,
	})
	if err != nil {
		t.Fatalf("create credential: %v", err)
	}
	_, err = store.CreateCredentialBundle(CredentialBundleCreateRequest{
		Name: "release-shared", Scope: CredentialScopePlatform, CreatedBy: admin.ID,
		Items: []CredentialBundleItem{{
			CredentialID:       credential.ID,
			Use:                CredentialUseImagePull,
			Namespace:          "apps",
			Workload:           CredentialPlanWorkload{Kind: "Deployment", Name: "api"},
			RegistryRepository: "team/api",
			RegistryReference:  "sha256:manifest",
		}},
	})
	if err != nil {
		t.Fatalf("create bundle: %v", err)
	}
	hub := NewGatewayHub(store, GatewayHubOptions{})
	refs, err := hub.resolveCredentialPlanReferences(deployer.ID, CredentialPlan{
		BundleRefs: []string{"release-shared"},
	})
	if err != nil {
		t.Fatalf("resolve bundle references: %v", err)
	}
	if len(refs) != 1 || refs[0].CredentialID != credential.ID || refs[0].Name != credential.Name {
		t.Fatalf("resolved references = %#v", refs)
	}
}

func TestCredentialPrepareGrantDenialStopsBeforeMaterialization(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("d", 32))
	store := openCredentialTestStore(t)
	deployer, deployerToken := createCredentialHTTPUser(t, store, "deployer", false)
	if err := store.GrantUser(deployer.ID, ScopeGrant{
		Cluster: "doops-edu", Instance: "edu-coder", Actions: []GatewayAction{ActionAsk, ActionCredentialUse},
	}); err != nil {
		t.Fatalf("grant deployer target actions: %v", err)
	}
	agentToken, err := store.CreateToken(CreateTokenRequest{
		Kind: TokenKindAgent, Name: "agent", Cluster: "doops-edu", Instance: "edu-coder",
	})
	if err != nil {
		t.Fatalf("create agent token: %v", err)
	}
	hub := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, OperationTimeout: time.Second})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	agentConn := dialFakeGatewayAgent(t, server.URL, agentToken.Plaintext, "doops-edu", "edu-coder")
	defer agentConn.Close()

	var materializeCalls atomic.Int64
	var cleanupCalls atomic.Int64
	var payloadObserved atomic.Bool
	go serveCredentialRuntimeAgent(t, agentConn, &materializeCalls, &cleanupCalls, &payloadObserved)
	waitForGatewayAgent(t, hub, "doops-edu", "edu-coder")

	response := credentialHTTPRequest(t, mux, deployerToken, http.MethodPost, "/v1/credentials/prepare", map[string]any{
		"cluster":          "doops-edu",
		"instance":         "edu-coder",
		"session_id":       "release-2",
		"workflow_path":    "deploy/release.yaml",
		"workspace_commit": strings.Repeat("b", 40),
		"mode":             "apply",
	})
	if response.Code != http.StatusForbidden {
		t.Fatalf("ungranted credential prepare = HTTP %d, want 403: %s", response.Code, response.Body.String())
	}
	if materializeCalls.Load() != 0 || payloadObserved.Load() {
		t.Fatalf("grant denial reached materializer: calls=%d payload=%v", materializeCalls.Load(), payloadObserved.Load())
	}
}

func serveCredentialRuntimeAgent(
	t *testing.T,
	conn *websocket.Conn,
	materializeCalls *atomic.Int64,
	cleanupCalls *atomic.Int64,
	payloadObserved *atomic.Bool,
) {
	t.Helper()
	for {
		var message map[string]any
		if err := conn.ReadJSON(&message); err != nil {
			return
		}
		switch message["method"] {
		case "initialize":
			_ = conn.WriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      message["id"],
				"result":  map[string]any{"protocolVersion": "2024-11-05"},
			})
		case "tools/call":
			params, _ := message["params"].(map[string]any)
			name, _ := params["name"].(string)
			arguments, _ := params["arguments"].(map[string]any)
			var result any
			switch name {
			case "doops_credential_plan":
				result = CredentialPlan{
					Template:    "oilan-agent-release",
					Project:     "oilan",
					Environment: "production",
					Target:      "gw-edu-coder",
					Cluster:     "doops-edu",
					Instance:    "edu-coder",
					CredentialRefs: []CredentialPlanReference{{
						Name:      "cnb-oci-pull",
						Use:       CredentialUseImagePull,
						Namespace: "kz-ops",
						Workload:  CredentialPlanWorkload{Kind: "Deployment", Name: "doops-agent-live"},
					}},
				}
			case "doops_credential_materialize":
				materializeCalls.Add(1)
				if strings.Contains(string(mustJSON(t, arguments)), "runtime-canary-secret") {
					payloadObserved.Store(true)
				}
				result = CredentialMaterialization{
					CredentialID:    stringValue(arguments["credential_id"]),
					VersionID:       stringValue(arguments["version_id"]),
					ResourceName:    "doops-cred-test",
					Namespace:       "kz-ops",
					SecretType:      "kubernetes.io/dockerconfigjson",
					Keys:            []string{".dockerconfigjson"},
					ResourceVersion: "42",
					Digest:          "sha256:manifest",
					Status:          "verified",
				}
			case "doops_credential_cleanup":
				cleanupCalls.Add(1)
				result = map[string]any{"status": "removed"}
			default:
				result = map[string]any{"error": "unexpected tool"}
			}
			raw, _ := json.Marshal(result)
			_ = conn.WriteJSON(map[string]any{
				"jsonrpc": "2.0",
				"id":      message["id"],
				"result": map[string]any{
					"content": []map[string]any{{"type": "text", "text": string(raw)}},
				},
			})
		}
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
