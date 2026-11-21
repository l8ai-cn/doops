package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialCLIManagesMetadataAndReadsPutPayloadOnlyFromStdin(t *testing.T) {
	const payloadCanary = "credential-payload-must-not-be-printed"
	var requests []credentialCLIRequest
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		requests = append(requests, credentialCLIRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Query:  r.URL.Query(),
			Auth:   r.Header.Get("Authorization"),
			Body:   body,
		})
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials":
			_, _ = w.Write([]byte(`{"id":"cred_123","name":"registry","scope":"personal","type":"registry","state":"active","payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/credentials":
			_, _ = w.Write([]byte(`{"credentials":[{"id":"cred_123","name":"registry","scope":"personal","type":"registry","state":"active","payload":"` + payloadCanary + `"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/v1/credentials/cred_123/payload":
			_, _ = w.Write([]byte(`{"id":"credver_456","credential_id":"cred_123","payload_digest":"sha256:abc","state":"staged","payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials/cred_123/grants":
			_, _ = w.Write([]byte(`{"id":"credgrant_789","credential_id":"cred_123","scope":{"cluster":"doops-edu","instance":"edu-coder","use":"imagePull"},"payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/credentials/cred_123/grants":
			_, _ = w.Write([]byte(`{"id":"credgrant_789","credential_id":"cred_123","state":"revoked","payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials/cred_123/verify":
			_, _ = w.Write([]byte(`{"status":"verified","evidence":[{"status":"verified","resourceVersion":"42","digest":"sha256:manifest","payload":"` + payloadCanary + `"}],"payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials/cred_123/promote":
			_, _ = w.Write([]byte(`{"credential_id":"cred_123","version_id":"credver_456","promoted":true,"payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/credentials/cred_123/revoke":
			_, _ = w.Write([]byte(`{"credential_id":"cred_123","revoked":true,"payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/credential-bundles":
			_, _ = w.Write([]byte(`{"id":"credbundle_1","name":"release","scope":"platform","state":"active","payload":"` + payloadCanary + `"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/credential-bundles":
			_, _ = w.Write([]byte(`{"id":"credbundle_1","name":"release","scope":"platform","state":"active","payload":"` + payloadCanary + `"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer gateway.Close()

	binary := buildDoopsCLIBinary(t)
	repositoryRoot := filepath.Clean(filepath.Join("..", "..", "..", "..", ".."))
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte(`{"servers":[{
		"name":"gw-edu-coder",
		"gateway":"`+gateway.URL+`",
		"cluster":"doops-edu",
		"instance":"edu-coder"
	}]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	auth := AuthStore{}
	auth.SetGateway(gateway.URL, "cli-token")
	authData, err := json.Marshal(&auth)
	if err != nil {
		t.Fatalf("marshal auth: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(config), "auth.json"), authData, 0o600); err != nil {
		t.Fatalf("write auth: %v", err)
	}
	run := func(stdin string, args ...string) string {
		t.Helper()
		command := exec.Command(binary, args...)
		command.Dir = repositoryRoot
		command.Env = append(os.Environ(),
			"DOOPS_CONFIG="+config,
			"DOOPS_CREDENTIAL_PAYLOAD="+payloadCanary,
		)
		command.Stdin = strings.NewReader(stdin)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("doops %s: %v\n%s", strings.Join(args, " "), err, output)
		}
		if strings.Contains(string(output), payloadCanary) {
			t.Fatalf("credential CLI output exposed payload: %s", output)
		}
		return string(output)
	}

	run("", "credential", "create", "--target", "gw-edu-coder", "--name", "registry", "--scope", "personal", "--type", "registry")
	run("", "credential", "list", "--target", "gw-edu-coder")
	managedOutput := run("", "credential", "list", "--target", "gw-edu-coder", "--view", "managed")
	if !strings.Contains(managedOutput, "bindings=not_loaded") || strings.Contains(managedOutput, "bindings=0") {
		t.Fatalf("managed credential list must disclose that bindings are not loaded: %s", managedOutput)
	}
	run(`{"server":"registry.example.com","username":"deploy","password":"stdin-wins"}`,
		"credential", "put", "--target", "gw-edu-coder", "--id", "cred_123")
	run("", "credential", "grant", "--target", "gw-edu-coder", "--id", "cred_123",
		"--namespace", "apps", "--use", "imagePull",
		"--workload-kind", "Deployment", "--workload-name", "api",
		"--image-repository", "team/api", "--image-digest", "sha256:manifest")
	run("", "credential", "grant", "--target", "gw-edu-coder", "--id", "cred_123",
		"--namespace", "agents", "--use", "database",
		"--required-keys", "connection_uri")
	run("", "-session", "credential-verify", "credential", "verify",
		"--target", "gw-edu-coder", "--id", "cred_123", "--version-id", "credver_456")
	run("", "-session", "credential-verify-database", "credential", "verify",
		"--target", "gw-edu-coder", "--id", "cred_123", "--version-id", "credver_456")
	run("", "credential", "promote", "--target", "gw-edu-coder", "--id", "cred_123", "--version-id", "credver_456")
	run("", "credential", "grant-revoke", "--target", "gw-edu-coder", "--id", "cred_123", "--grant-id", "credgrant_789")
	run("", "credential", "revoke", "--target", "gw-edu-coder", "--id", "cred_123")
	run(`{"name":"release","scope":"platform","items":[{"credential_id":"cred_123","use":"imagePull","namespace":"apps","workload":{"Kind":"Deployment","Name":"api"},"registry_repository":"team/api","registry_reference":"sha256:manifest"}]}`,
		"credential", "bundle-create", "--target", "gw-edu-coder")
	run("", "credential", "bundle-get", "--target", "gw-edu-coder", "--name", "release")

	command := exec.Command(binary, "credential", "put", "--target", "gw-edu-coder", "--id", "cred_123", "--payload", payloadCanary)
	command.Env = append(os.Environ(), "DOOPS_CONFIG="+config)
	command.Stdin = strings.NewReader(`{"password":"stdin-is-required"}`)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("put must reject payload flags, output: %s", output)
	}
	command = exec.Command(binary, "credential", "list", "--gateway", gateway.URL, "--token", "argv-token")
	command.Env = append(os.Environ(), "DOOPS_CONFIG="+config)
	if output, err := command.CombinedOutput(); err == nil {
		t.Fatalf("credential commands must reject token argv flags, output: %s", output)
	}

	if len(requests) != 13 {
		t.Fatalf("gateway request count mismatch: got %d want 13", len(requests))
	}
	for _, request := range requests {
		if request.Auth != "Bearer cli-token" {
			t.Fatalf("credential request auth mismatch: %#v", request)
		}
	}
	assertCredentialCLIJSON(t, requests[0], http.MethodPost, "/v1/credentials", map[string]any{
		"name": "registry", "scope": "personal", "type": "registry",
	})
	if requests[1].Method != http.MethodGet || requests[1].Path != "/v1/credentials" {
		t.Fatalf("credential list request mismatch: %#v", requests[1])
	}
	if requests[1].Query.Get("view") != "target" ||
		requests[1].Query.Get("cluster") != "doops-edu" ||
		requests[1].Query.Get("instance") != "edu-coder" {
		t.Fatalf("credential list target query mismatch: %#v", requests[1].Query)
	}
	assertCredentialCLIJSON(t, requests[3], http.MethodPut, "/v1/credentials/cred_123/payload", map[string]any{
		"server": "registry.example.com", "username": "deploy", "password": "stdin-wins",
	})
	if len(requests[3].Query) != 0 {
		t.Fatalf("credential put must not activate through a query parameter: %#v", requests[3].Query)
	}
	assertCredentialCLIJSON(t, requests[4], http.MethodPost, "/v1/credentials/cred_123/grants", map[string]any{
		"scope": map[string]any{
			"cluster":          "doops-edu",
			"instance":         "edu-coder",
			"namespace":        "apps",
			"use":              "imagePull",
			"workload":         map[string]any{"kind": "Deployment", "name": "api"},
			"image_repository": "team/api",
			"image_digest":     "sha256:manifest",
		},
	})
	assertCredentialCLIJSON(t, requests[5], http.MethodPost, "/v1/credentials/cred_123/grants", map[string]any{
		"scope": map[string]any{
			"cluster":       "doops-edu",
			"instance":      "edu-coder",
			"namespace":     "agents",
			"use":           "database",
			"workload":      map[string]any{"kind": "", "name": ""},
			"required_keys": []any{"connection_uri"},
		},
	})
	for index, session := range map[int]string{6: "credential-verify", 7: "credential-verify-database"} {
		// verify sends only the target; each binding keeps the scope stored at grant time.
		assertCredentialCLIJSON(t, requests[index], http.MethodPost, "/v1/credentials/cred_123/verify", map[string]any{
			"version_id": "credver_456",
			"session_id": session,
			"scope": map[string]any{
				"cluster":   "doops-edu",
				"instance":  "edu-coder",
				"namespace": "",
				"use":       "",
				"workload":  map[string]any{"kind": "", "name": ""},
			},
		})
	}
	assertCredentialCLIJSON(t, requests[8], http.MethodPost, "/v1/credentials/cred_123/promote", map[string]any{
		"version_id": "credver_456",
	})
	assertCredentialCLIJSON(t, requests[9], http.MethodDelete, "/v1/credentials/cred_123/grants", map[string]any{
		"grant_id": "credgrant_789",
	})
	if requests[10].Method != http.MethodPost || requests[10].Path != "/v1/credentials/cred_123/revoke" || len(requests[10].Body) != 0 {
		t.Fatalf("credential revoke request mismatch: %#v", requests[10])
	}
	if requests[11].Method != http.MethodPost || requests[11].Path != "/v1/credential-bundles" {
		t.Fatalf("credential bundle create request mismatch: %#v", requests[11])
	}
	if requests[12].Method != http.MethodGet || requests[12].Path != "/v1/credential-bundles" ||
		requests[12].Query.Get("name") != "release" {
		t.Fatalf("credential bundle get request mismatch: %#v", requests[12])
	}
}

func TestResolveCredentialGatewayRequiresConfiguredTarget(t *testing.T) {
	if _, _, err := resolveCredentialGateway("", nil, nil); err == nil || !strings.Contains(err.Error(), "--target is required") {
		t.Fatalf("expected target requirement, got %v", err)
	}
}

func TestCredentialCommandsRejectDuplicateTargetRouteFlags(t *testing.T) {
	servers := []Server{{Name: "target", Gateway: "https://gateway.example.com", Cluster: "cluster-a", Instance: "node-a"}}
	for _, args := range [][]string{
		{"--target", "target", "--id", "cred_123", "--cluster", "other"},
		{"--target", "target", "--id", "cred_123", "--instance", "other"},
	} {
		if err := runCredentialGrant(args, servers, nil, io.Discard); err == nil {
			t.Fatalf("credential grant accepted duplicate route flag: %v", args)
		}
	}
	for _, args := range [][]string{
		{"--target", "target", "--id", "cred_123", "--version-id", "credver_456", "--cluster", "other"},
		{"--target", "target", "--id", "cred_123", "--version-id", "credver_456", "--instance", "other"},
	} {
		if err := runCredentialVerify(args, "verify", servers, nil, io.Discard); err == nil {
			t.Fatalf("credential verify accepted duplicate route flag: %v", args)
		}
	}
}

func TestCredentialGatewayStructured401GuidesGatewayLogin(t *testing.T) {
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","code":"gateway_auth_expired","auth_domain":"gateway","message":"Gateway user token has expired","next_action":"Run doops login --gateway <gateway-url> for this Gateway; do not replace the Gateway token with a managed credential or business API key"}`))
	}))
	defer gateway.Close()

	_, err := gatewayCredentialRequest(http.MethodGet, gateway.URL, "stale-token", "/v1/credentials", nil, nil)
	if err == nil {
		t.Fatal("expected credential gateway unauthorized error")
	}
	message := err.Error()
	for _, want := range []string{"gateway_auth_expired", "auth_domain=gateway", "doops login --gateway <gateway-url>"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error missing %q: %s", want, message)
		}
	}
	if strings.Contains(message, "credential_invalid") {
		t.Fatalf("structured Gateway auth failure must not be classified as credential_invalid: %s", message)
	}
}

func TestCredentialCLIHelp(t *testing.T) {
	var output bytes.Buffer
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"-h"}} {
		output.Reset()
		if err := runCredentialCommand(args, "", nil, nil, strings.NewReader(""), &output); err != nil {
			t.Fatalf("credential help %v: %v", args, err)
		}
		if !strings.Contains(output.String(), "doops credential list") ||
			!strings.Contains(output.String(), "doops credential create") ||
			!strings.Contains(output.String(), "doops credential configure") ||
			!strings.Contains(output.String(), "doops credential preflight") ||
			!strings.Contains(output.String(), "doops credential verify") {
			t.Fatalf("credential help %v missing commands: %s", args, output.String())
		}
	}
	output.Reset()
	if err := runCredentialCommand(
		[]string{"create", "-h"}, "", nil, nil, strings.NewReader(""), &output,
	); err != nil {
		t.Fatalf("credential create help: %v", err)
	}
	if !strings.Contains(output.String(), "-name string") ||
		!strings.Contains(output.String(), "-target string") {
		t.Fatalf("credential create help missing flags: %s", output.String())
	}
}

type credentialCLIRequest struct {
	Method string
	Path   string
	Query  url.Values
	Auth   string
	Body   []byte
}

func buildDoopsCLIBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "doops")
	command := exec.Command("go", "build", "-o", binary, ".")
	command.Dir = "."
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build doops CLI: %v\n%s", err, output)
	}
	return binary
}

func assertCredentialCLIJSON(t *testing.T, request credentialCLIRequest, method, path string, want map[string]any) {
	t.Helper()
	if request.Method != method || request.Path != path {
		t.Fatalf("request route mismatch: %#v", request)
	}
	var got map[string]any
	if err := json.Unmarshal(request.Body, &got); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if !jsonEquivalent(got, want) {
		t.Fatalf("request body mismatch\ngot:  %#v\nwant: %#v", got, want)
	}
}

func jsonEquivalent(got, want map[string]any) bool {
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	return bytes.Equal(gotJSON, wantJSON)
}

// Scope flags verify used to need are dropped with a note; the call proceeds
// with the scope stored on each binding instead of failing.
func TestCredentialVerifyDropsRetiredScopeFlags(t *testing.T) {
	base := []string{"--target", "t", "--id", "cred_1", "--version-id", "credver_1"}
	for _, tc := range []struct {
		name string
		args []string
		note string
	}{
		{"value with equals", []string{"--namespace=apps"}, "--namespace ignored"},
		{"value as next word", []string{"--namespace", "apps"}, "--namespace ignored"},
		{"boolean-looking flag before another flag", []string{"--use", "--required-keys=a"}, "--required-keys ignored"},
		{"single dash", []string{"-image-digest", "sha256:x"}, "--image-digest ignored"},
		{"nothing retired", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			kept := dropRetiredCredentialVerifyFlags(append(append([]string(nil), base...), tc.args...), &out)
			if strings.Join(kept, " ") != strings.Join(base, " ") {
				t.Fatalf("kept = %q, want %q", kept, base)
			}
			if tc.note == "" && out.Len() != 0 || !strings.Contains(out.String(), tc.note) {
				t.Fatalf("note = %q, want %q", out.String(), tc.note)
			}
		})
	}
}
