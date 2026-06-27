package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

func TestGatewayLoginURLAllowsHTTPGatewayByDefault(t *testing.T) {
	got, err := gatewayAuthLoginURL("http://203.0.113.10:42222")
	if err != nil {
		t.Fatalf("http gateway login URL should be allowed by default: %v", err)
	}
	want := "http://203.0.113.10:42222/v1/auth/login"
	if got != want {
		t.Fatalf("login URL mismatch\nwant: %s\n got: %s", want, got)
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
	if got := historyLogPath(); got != filepath.Join(stateDir, "history.jsonl") {
		t.Fatalf("history path mismatch\nwant: %s\n got: %s", filepath.Join(stateDir, "history.jsonl"), got)
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
