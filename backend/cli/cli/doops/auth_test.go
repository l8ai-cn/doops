package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayAuthLoginURLUsesVersionedEndpoint(t *testing.T) {
	got, err := gatewayAuthLoginURL("https://gateway.example.com")
	if err != nil {
		t.Fatalf("build login url: %v", err)
	}
	want := "https://gateway.example.com/v1/auth/login"
	if got != want {
		t.Fatalf("login URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayLoginPreservesErrorAuthority(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"bare proxy response", "Unauthorized\n", "http_401"},
		{"structured login rejection", `{"code":"gateway_login_invalid_credentials","auth_domain":"gateway","message":"credentials rejected"}`, "gateway_login_invalid_credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer gateway.Close()
			_, err := GatewayLogin(gateway.URL, "synthetic-user", "synthetic-password", "auth-error-test")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("login error = %v, want %q", err, tc.want)
			}
			if tc.want == "http_401" && (strings.Contains(err.Error(), "gateway_login_invalid_credentials") || strings.Contains(err.Error(), "auth_domain=gateway")) {
				t.Fatalf("bare HTTP rejection invented Gateway authority: %v", err)
			}
		})
	}
}

func TestGatewayAuthLoginURLPreservesGatewayPathPrefix(t *testing.T) {
	got, err := gatewayAuthLoginURL("https://gateway.example.com/doops")
	if err != nil {
		t.Fatalf("build login url: %v", err)
	}
	want := "https://gateway.example.com/doops/v1/auth/login"
	if got != want {
		t.Fatalf("login URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayLoginURLRejectsExternalHTTPGatewayByDefault(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "")
	if _, err := gatewayAuthLoginURL("http://203.0.113.10:42222"); err == nil {
		t.Fatalf("expected external http gateway to be rejected by default")
	}
}

func TestGetAuthPathUsesCanonicalAgentSkillDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := GetAuthPath()
	want := filepath.Join(home, ".agent", "skills", "doops", "auth.json")
	if got != want {
		t.Fatalf("auth path mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestDoopsStatePathsUseCanonicalAgentSkillDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	stateDir := filepath.Join(home, ".agent", "skills", "doops")
	if got := doopsStateDir(); got != stateDir {
		t.Fatalf("state dir mismatch\nwant: %s\n got: %s", stateDir, got)
	}
	if got := GetAuthPath(); got != filepath.Join(stateDir, "auth.json") {
		t.Fatalf("auth path mismatch\nwant: %s\n got: %s", filepath.Join(stateDir, "auth.json"), got)
	}
	if got := defaultSessionStorePath(); got != filepath.Join(stateDir, "sessions.json") {
		t.Fatalf("session path mismatch\nwant: %s\n got: %s", filepath.Join(stateDir, "sessions.json"), got)
	}
	if got := historyLogPath(); got != filepath.Join(stateDir, "history-v2.jsonl") {
		t.Fatalf("history path mismatch\nwant: %s\n got: %s", filepath.Join(stateDir, "history-v2.jsonl"), got)
	}
}

func TestGatewayAuthKeyNormalizesEquivalentURLs(t *testing.T) {
	if got := gatewayAuthKey("HTTPS://Gateway.Example.com/"); got != "https://gateway.example.com" {
		t.Fatalf("gateway auth key mismatch: %q", got)
	}
	for _, equivalent := range []string{
		"wss://gateway.example.com",
		"https://gateway.example.com:443/",
		"https://user@gateway.example.com/",
	} {
		if got := gatewayAuthKey(equivalent); got != "https://gateway.example.com" {
			t.Fatalf("equivalent gateway %q normalized to %q", equivalent, got)
		}
	}
}

func TestResolveTokenSharesOneGatewayLoginAcrossTargets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	auth := &AuthStore{}
	auth.SetGateway("https://gateway.example.com", "shared-gateway-token")
	if err := auth.Save(); err != nil {
		t.Fatalf("save auth store: %v", err)
	}

	first := Server{
		Name:     "first",
		Gateway:  "https://gateway.example.com/",
		Cluster:  "cluster-a",
		Instance: "node-a",
	}
	second := Server{
		Name:     "second",
		Gateway:  "HTTPS://GATEWAY.EXAMPLE.COM",
		Cluster:  "cluster-b",
		Instance: "node-b",
	}
	if got, err := ResolveToken(first); err != nil || got != "shared-gateway-token" {
		t.Fatalf("first target did not use shared gateway token: %q", got)
	}
	if got, err := ResolveToken(second); err != nil || got != "shared-gateway-token" {
		t.Fatalf("second target did not use shared gateway token: %q", got)
	}
}

func TestResolveTokenRequiresGatewayLogin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	server := Server{
		Name:     "prod",
		Gateway:  "https://gateway.example.com",
		Cluster:  "prod",
		Instance: "node",
	}
	if _, err := ResolveToken(server); err == nil || !strings.Contains(err.Error(), "doops login --gateway https://gateway.example.com") {
		t.Fatalf("expected explicit login requirement, got %v", err)
	}
}

func TestLoadAuthRejectsMalformedStore(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := GetAuthPath()
	if err := os.WriteFile(path, []byte(`{"tokens":`), 0o600); err != nil {
		t.Fatalf("write malformed auth store: %v", err)
	}
	if _, err := LoadAuth(); err == nil || !strings.Contains(err.Error(), "parse Gateway auth store") {
		t.Fatalf("expected malformed auth store error, got %v", err)
	}
}

func TestLoadAuthMigratesLegacyTokenMapToCredentialMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := GetAuthPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir auth dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"tokens":{"https://gateway.example.com":"legacy-token"}}`), 0o600); err != nil {
		t.Fatalf("write legacy auth store: %v", err)
	}

	store, err := LoadAuth()
	if err != nil {
		t.Fatalf("load legacy auth store: %v", err)
	}
	record := store.GetGatewayRecord("https://gateway.example.com/")
	if record.Token != "legacy-token" || record.Source != "legacy-auth-json" {
		t.Fatalf("legacy metadata mismatch: %#v", record)
	}
	if err := store.Save(); err != nil {
		t.Fatalf("save migrated auth store: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read migrated auth store: %v", err)
	}
	var migrated struct {
		Version     int                    `json:"version"`
		Credentials map[string]GatewayAuth `json:"credentials"`
		Tokens      map[string]string      `json:"tokens"`
	}
	if err := json.Unmarshal(data, &migrated); err != nil {
		t.Fatalf("decode migrated auth store: %v", err)
	}
	if migrated.Version != authStoreVersion || len(migrated.Credentials) != 1 || migrated.Tokens != nil {
		t.Fatalf("legacy store was not rewritten as v2 credentials: %#v", migrated)
	}
}

func TestAuthStorePersistsTokenLifecycleMetadata(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	store := &AuthStore{}
	store.SetGatewayRecord("https://gateway.example.com", GatewayAuth{
		Token:     "synthetic-token",
		TokenID:   "tok_synthetic",
		Username:  "alice",
		Name:      "doops login",
		ExpiresAt: "2026-11-09T08:39:00Z",
		IssuedAt:  "2026-11-08T08:39:00Z",
		Source:    "password-login",
	})
	if err := store.Save(); err != nil {
		t.Fatalf("save auth metadata: %v", err)
	}
	loaded, err := LoadAuth()
	if err != nil {
		t.Fatalf("load auth metadata: %v", err)
	}
	if got := loaded.GetGatewayRecord("https://gateway.example.com"); got != store.GetGatewayRecord("https://gateway.example.com") {
		t.Fatalf("auth metadata changed after persistence: %#v", got)
	}
}

func TestAuthStoreRejectsUnknownOrAmbiguousFormats(t *testing.T) {
	for _, schema := range []string{
		`{"version":1,"credentials":{}}`,
		`{"version":3,"credentials":{}}`,
		`{"version":null,"credentials":{}}`,
		`{"version":2,"tokens":{}}`,
		`{"version":null,"tokens":{}}`,
		`{"tokens":null}`,
		`{"version":2,"credentials":null}`,
		`{"tokens":{},"credentials":null}`,
		`{"tokens":{"https://gw.example":"canary","https://gw.example:443/":"other"}}`,
	} {
		t.Run(schema, func(t *testing.T) {
			var store AuthStore
			if err := json.Unmarshal([]byte(schema), &store); err == nil {
				t.Fatal("ambiguous auth schema was accepted")
			}
		})
	}
}

func TestAuthStoreSaveReplacesACompletePrivateFile(t *testing.T) {
	t.Setenv("DOOPS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	store := &AuthStore{}
	store.SetGateway("https://gw.example", "first-synthetic-token")
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	path := GetAuthPath()
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(filepath.Dir(path), "original.json")
	if err := os.Link(path, linked); err != nil {
		t.Fatal(err)
	}
	store.SetGateway("https://gw.example", "second-synthetic-token")
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(linked)
	if err != nil || string(original) != string(before) {
		t.Fatal("saving truncated or modified the previously complete auth file")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("replacement auth file is not private")
	}
	replacement, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(replacement), "second-synthetic-token") {
		t.Fatal("replacement auth file omitted the new credential")
	}
	store.Version = 99
	if err := store.Save(); err == nil {
		t.Fatal("invalid store was saved")
	}
	afterFailure, err := os.ReadFile(path)
	if err != nil || string(afterFailure) != string(replacement) {
		t.Fatal("failed save changed the complete credential file")
	}
	if temps, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".auth-*.json")); err != nil || len(temps) != 0 {
		t.Fatal("saving left temporary credential files behind")
	}
}

func TestGatewayLoginReturnsTokenLifecycleMetadata(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":      "issued-token",
			"token_id":   "tok_issued",
			"token_type": "user",
			"username":   "alice",
			"name":       "doops login",
			"expires_at": "2026-11-09T08:39:00Z",
			"issued_at":  "2026-11-08T08:39:00Z",
		})
	}))
	defer gateway.Close()

	credential, err := GatewayLogin(gateway.URL, "alice", "synthetic-password", "doops login")
	if err != nil {
		t.Fatalf("gateway login: %v", err)
	}
	if credential.Token != "issued-token" || credential.TokenID != "tok_issued" ||
		credential.Username != "alice" || credential.ExpiresAt == "" || credential.IssuedAt == "" ||
		credential.Source != "password-login" {
		t.Fatalf("login metadata mismatch: %#v", credential)
	}
}

func TestLoadConfigIgnoresLegacyDotConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	legacyDir := filepath.Join(home, ".config", "doops")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("mkdir legacy config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(legacyDir, "config.json"), []byte(`{"servers":[{"name":"legacy-direct","ip":"127.0.0.1"}]}`), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}

	canonicalDir := filepath.Join(home, ".agent", "skills", "doops")
	if err := os.MkdirAll(canonicalDir, 0o755); err != nil {
		t.Fatalf("mkdir canonical config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(canonicalDir, "config.json"), []byte(`{"servers":[{"name":"gateway","gateway":"https://gw.example.com","cluster":"prod","instance":"node"}]}`), 0o600); err != nil {
		t.Fatalf("write canonical config: %v", err)
	}

	servers, _, err := LoadConfig()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if len(servers) != 1 || servers[0].Name != "gateway" {
		t.Fatalf("expected canonical config only, got %#v", servers)
	}
}

func TestGatewayAdminTokenCreatePostsVersionedEndpoint(t *testing.T) {
	var gotPath string
	var gotAuth string
	var gotBody gatewayAdminTokenCreateRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(gatewayAdminTokenCreateResponse{
			Token:     "issued-token",
			TokenID:   "tok_1",
			TokenType: "user",
			Username:  "alice",
		})
	}))
	defer ts.Close()

	resp, err := GatewayAdminTokenCreate(ts.URL, "admin-token", gatewayAdminTokenCreateRequest{
		User:    "alice",
		Name:    "cli-issued",
		Expires: "720h",
	})
	if err != nil {
		t.Fatalf("create admin token: %v", err)
	}
	if gotPath != "/v1/admin/tokens" {
		t.Fatalf("path mismatch: %s", gotPath)
	}
	if gotAuth != "Bearer admin-token" {
		t.Fatalf("auth header mismatch: %s", gotAuth)
	}
	if gotBody.User != "alice" || gotBody.Name != "cli-issued" || gotBody.Expires != "720h" {
		t.Fatalf("request body mismatch: %#v", gotBody)
	}
	if resp.Token != "issued-token" || resp.Username != "alice" {
		t.Fatalf("response mismatch: %#v", resp)
	}
}

func TestGatewayAdminTokenListAndRevokeUseMetadataOnly(t *testing.T) {
	var methods []string
	var queries []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer admin-token" {
			t.Fatalf("authorization mismatch")
		}
		methods = append(methods, r.Method)
		queries = append(queries, r.URL.RawQuery)
		switch r.Method {
		case http.MethodGet:
			_, _ = w.Write([]byte(`{"tokens":[{"id":"tok_1","kind":"user","user_name":"alice","name":"rotated","prefix":"dgw_user_abcd","revoked":false}]}`))
		case http.MethodDelete:
			_, _ = w.Write([]byte(`{"id":"tok_1","revoked":true}`))
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer ts.Close()

	tokens, err := GatewayAdminTokensList(ts.URL, "admin-token", "user")
	if err != nil {
		t.Fatalf("list admin tokens: %v", err)
	}
	if len(tokens) != 1 || tokens[0].ID != "tok_1" || tokens[0].Prefix != "dgw_user_abcd" {
		t.Fatalf("token metadata mismatch: %#v", tokens)
	}
	if err := GatewayAdminTokenRevoke(ts.URL, "admin-token", "tok_1"); err != nil {
		t.Fatalf("revoke admin token: %v", err)
	}
	if len(methods) != 2 || methods[0] != http.MethodGet || methods[1] != http.MethodDelete {
		t.Fatalf("methods mismatch: %#v", methods)
	}
	if queries[0] != "kind=user" || queries[1] != "id=tok_1" {
		t.Fatalf("queries mismatch: %#v", queries)
	}
}

func TestAdminTokenCreateValidatesSaveTargetBeforeIssuing(t *testing.T) {
	requests := 0
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "must not be called", http.StatusInternalServerError)
	}))
	defer gateway.Close()

	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	if err := os.WriteFile(config, []byte(`{"servers":[{
		"name":"source",
		"gateway":"`+gateway.URL+`",
		"cluster":"prod",
		"instance":"node"
	}]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	auth := AuthStore{}
	auth.SetGateway(gateway.URL, "admin-token")
	authData, err := json.Marshal(&auth)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "auth.json"), authData, 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}

	command := exec.Command(
		buildDoopsCLIBinary(t),
		"admin", "token", "create",
		"--target", "source",
		"--user", "codex",
		"--save-as", "missing",
	)
	command.Env = append(os.Environ(), "DOOPS_CONFIG="+config)
	output, err := command.CombinedOutput()
	if err == nil || !strings.Contains(string(output), `target "missing" is not configured`) {
		t.Fatalf("expected local save target validation, got err=%v output=%s", err, output)
	}
	if requests != 0 {
		t.Fatalf("token was issued before local validation: %d request(s)", requests)
	}
}
