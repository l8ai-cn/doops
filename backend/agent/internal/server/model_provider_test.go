package server

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modelProviderCanary = "sk-synthetic-model-canary-0001"

func modelProviderTestPayload(key string) json.RawMessage {
	return json.RawMessage(`{"model":"openai/gpt-x","provider":{"openai":{"options":{"apiKey":"` +
		key + `","baseURL":"https://models.example.com/v1","kind":"openai"}}}}`)
}

// useModelProviderTestSettings points the fixed doagent settings path at a temp
// dir and replaces the real model call for the duration of a test.
func useModelProviderTestSettings(t *testing.T, probe func(context.Context, modelProviderPayload) error) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".agent", "settings.json")
	previousPath, previousProbe := doagentSettingsFile, probeModelProvider
	doagentSettingsFile = path
	if probe == nil {
		probe = func(context.Context, modelProviderPayload) error { return nil }
	}
	probeModelProvider = probe
	t.Cleanup(func() {
		doagentSettingsFile = previousPath
		probeModelProvider = previousProbe
	})
	return path
}

func modelProviderTestRequest(versionID string, payload json.RawMessage) CredentialMaterializeRequest {
	return CredentialMaterializeRequest{
		SessionID: "model-test", Cluster: "doops-test", Instance: "node-1",
		CredentialID: "cred_" + strings.Repeat("a", 24), GrantID: "credgrant_" + strings.Repeat("b", 24),
		VersionID: versionID, CredentialType: CredentialTypeModelProvider,
		Scope: CredentialGrantScope{
			Cluster: "doops-test", Instance: "node-1", Namespace: "default", Use: CredentialUseModelProvider,
		},
		Payload: payload,
	}
}

func readTestSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings are not JSON: %v", err)
	}
	return settings
}

func TestModelProviderPayloadValidation(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"valid", string(modelProviderTestPayload(modelProviderCanary)), true},
		{"model without provider", `{"model":"gpt-x","provider":{"openai":{"options":{"apiKey":"k"}}}}`, false},
		{"model provider not configured", `{"model":"other/x","provider":{"openai":{"options":{"apiKey":"k"}}}}`, false},
		{"empty provider map", `{"model":"openai/x","provider":{}}`, false},
		{"missing api key", `{"model":"openai/x","provider":{"openai":{"options":{}}}}`, false},
		{"file reference key", `{"model":"openai/x","provider":{"openai":{"options":{"apiKey":"{file:/run/k}"}}}}`, false},
		{"env reference key", `{"model":"openai/x","provider":{"openai":{"options":{"apiKey":"{env:KEY}"}}}}`, false},
		{"plain http base url", `{"model":"openai/x","provider":{"openai":{"options":{"apiKey":"k","baseURL":"http://models.example.com"}}}}`, false},
		{"unknown top-level field", `{"model":"openai/x","provider":{"openai":{"options":{"apiKey":"k"}}},"mcp_servers":[]}`, false},
		{"image provider not configured", `{"model":"openai/x","provider":{"openai":{"options":{"apiKey":"k"}}},"image_provider":"other"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCredentialPayload(CredentialTypeModelProvider, json.RawMessage(tc.payload))
			if tc.valid && err != nil {
				t.Fatalf("valid payload rejected: %v", err)
			}
			if !tc.valid && !errors.Is(err, ErrCredentialPayloadInvalid) {
				t.Fatalf("invalid payload error = %v, want ErrCredentialPayloadInvalid", err)
			}
			if err != nil && strings.Contains(err.Error(), "k") && strings.Contains(err.Error(), modelProviderCanary) {
				t.Fatalf("validation error echoes payload: %v", err)
			}
		})
	}
}

func TestModelProviderMaterializeWritesFixedSettingsWithRuntimeServers(t *testing.T) {
	path := useModelProviderTestSettings(t, nil)
	result, err := materializeCredential(context.Background(),
		modelProviderTestRequest("credver_"+strings.Repeat("c", 24), modelProviderTestPayload(modelProviderCanary)))
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if result.Status != "verified" || !validCredentialSHA256Digest(result.Digest) || result.SecretType != "" {
		t.Fatalf("materialization metadata = %+v", result)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), modelProviderCanary) {
		t.Fatalf("materialization result carries the payload: %s", encoded)
	}
	settings := readTestSettings(t, path)
	if settings["model"] != "openai/gpt-x" {
		t.Fatalf("model = %v", settings["model"])
	}
	if servers, _ := settings["mcp_servers"].([]any); len(servers) != 2 {
		t.Fatalf("runtime MCP servers = %v", settings["mcp_servers"])
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode = %v, %v; want 0600", info, err)
	}
}

func TestModelProviderProbeFailureLeavesSettingsUntouched(t *testing.T) {
	path := useModelProviderTestSettings(t, func(context.Context, modelProviderPayload) error {
		return errors.New("model_auth_failed")
	})
	if err := EnsureDoagentBaseSettings(); err != nil {
		t.Fatalf("base settings: %v", err)
	}
	_, err := materializeCredential(context.Background(),
		modelProviderTestRequest("credver_"+strings.Repeat("c", 24), modelProviderTestPayload(modelProviderCanary)))
	if err == nil || err.Error() != "model_auth_failed" {
		t.Fatalf("materialize error = %v, want model_auth_failed", err)
	}
	settings := readTestSettings(t, path)
	if _, present := settings["model"]; present {
		t.Fatalf("a failed probe must not deliver a model: %v", settings)
	}
	if _, delivered := readModelProviderRecord(); delivered {
		t.Fatalf("a failed probe must not record a delivery")
	}
}

func TestModelProviderCleanupRecoversInterruptedDelivery(t *testing.T) {
	path := useModelProviderTestSettings(t, nil)
	if err := writeDoagentSettings(map[string]any{"model": "test-provider/test-model"}); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic0600(modelProviderPendingPath(), []byte("{}")); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupModelProviderCredential(CredentialCleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, found := readTestSettings(t, path)["model"]; found {
		t.Fatal("an interrupted model delivery survived cleanup")
	}
	if _, err := os.Stat(modelProviderPendingPath()); !os.IsNotExist(err) {
		t.Fatalf("pending marker survived cleanup: %v", err)
	}
}

func TestModelProviderUseMismatchIsRejected(t *testing.T) {
	useModelProviderTestSettings(t, nil)
	request := modelProviderTestRequest("credver_"+strings.Repeat("c", 24), modelProviderTestPayload(modelProviderCanary))
	request.Scope.Use = CredentialUseOpaqueSecret
	if _, err := materializeCredential(context.Background(), request); err == nil {
		t.Fatalf("modelProvider with a non-model use must be rejected")
	}
}

func TestModelProviderCleanupRemovesOnlyItsOwnDelivery(t *testing.T) {
	path := useModelProviderTestSettings(t, nil)
	version := "credver_" + strings.Repeat("c", 24)
	request := modelProviderTestRequest(version, modelProviderTestPayload(modelProviderCanary))
	if _, err := materializeCredential(context.Background(), request); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	other := CredentialCleanupRequest{
		SessionID: "model-test", CredentialID: "cred_" + strings.Repeat("d", 24),
		GrantID: request.GrantID, VersionID: version, Namespace: "default",
	}
	if cleaned, err := cleanupModelProviderCredential(other); err != nil || cleaned {
		t.Fatalf("cleanup of another credential = %v, %v; want untouched", cleaned, err)
	}
	if readTestSettings(t, path)["model"] != "openai/gpt-x" {
		t.Fatalf("another credential's cleanup removed this model")
	}

	own := CredentialCleanupRequest{
		SessionID: "model-test", CredentialID: request.CredentialID,
		GrantID: request.GrantID, AnyVersion: true, Namespace: "default",
	}
	if cleaned, err := cleanupModelProviderCredential(own); err != nil || !cleaned {
		t.Fatalf("cleanup = %v, %v", cleaned, err)
	}
	settings := readTestSettings(t, path)
	if _, present := settings["model"]; present {
		t.Fatalf("revoked model is still configured: %v", settings)
	}
	if _, present := settings["provider"]; present {
		t.Fatalf("revoked provider is still configured")
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), modelProviderCanary) {
		t.Fatalf("revoked key remains in the settings file")
	}
}

func TestEnsureBaseSettingsDropsModelNotDeliveredByGateway(t *testing.T) {
	path := useModelProviderTestSettings(t, nil)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := `{"model":"openai/stale","provider":{"openai":{"options":{"apiKey":"` + modelProviderCanary + `"}}}}`
	if err := os.WriteFile(path, []byte(stale), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDoagentBaseSettings(); err != nil {
		t.Fatalf("base settings: %v", err)
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), modelProviderCanary) || strings.Contains(string(data), "openai/stale") {
		t.Fatalf("a model the Gateway never delivered survived startup: %s", data)
	}

	if _, err := materializeCredential(context.Background(),
		modelProviderTestRequest("credver_"+strings.Repeat("c", 24), modelProviderTestPayload(modelProviderCanary))); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if err := EnsureDoagentBaseSettings(); err != nil {
		t.Fatalf("base settings after delivery: %v", err)
	}
	if readTestSettings(t, path)["model"] != "openai/gpt-x" {
		t.Fatalf("a Gateway-delivered model must survive restart until the Gateway redelivers")
	}
}

func TestModelProviderGrantIsOnePerTargetAndModelUseOnly(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("m", 32))
	store := openCredentialTestStore(t)
	owner := createCredentialTestUser(t, store, "model-owner")
	scope := CredentialGrantScope{Cluster: "doops-test", Instance: "node-1", Namespace: "default"}
	create := func(name string) CredentialResource {
		resource, err := store.CreateCredential(CredentialCreateRequest{
			Name: name, Scope: CredentialScopePersonal, Type: CredentialTypeModelProvider,
			OwnerID: owner.ID, CreatedBy: owner.ID,
		})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return resource
	}
	first, second := create("model-a"), create("model-b")

	wrongUse := scope
	wrongUse.Use = CredentialUseOpaqueSecret
	if _, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: first.ID, Scope: wrongUse, CreatedBy: owner.ID,
	}); err == nil || err.Error() != "credential_use_mismatch" {
		t.Fatalf("wrong use grant error = %v", err)
	}
	grant, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: first.ID, Scope: scope, CreatedBy: owner.ID,
	})
	if err != nil || grant.Scope.Use != CredentialUseModelProvider {
		t.Fatalf("first grant = %+v, %v", grant, err)
	}
	if _, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: first.ID, Scope: scope, CreatedBy: owner.ID,
	}); err != nil {
		t.Fatalf("re-granting the bound credential must stay idempotent: %v", err)
	}
	if _, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: second.ID, Scope: scope, CreatedBy: owner.ID,
	}); err == nil || err.Error() != "model_provider_already_bound" {
		t.Fatalf("second model provider on one target error = %v", err)
	}
	otherTarget := scope
	otherTarget.Instance = "node-2"
	if _, err := store.CreateCredentialGrant(CredentialGrantCreateRequest{
		CredentialID: second.ID, Scope: otherTarget, CreatedBy: owner.ID,
	}); err != nil {
		t.Fatalf("a different target may take another model provider: %v", err)
	}
	bindings, err := store.ActiveModelProviderBindings("doops-test", "node-1")
	if err != nil || len(bindings) != 1 || bindings[0].Resource.ID != first.ID {
		t.Fatalf("bindings = %+v, %v", bindings, err)
	}
}

func TestDoagentMetadataReportsAwaitingModelProviderWithoutModel(t *testing.T) {
	cases := []struct {
		name     string
		settings string
		want     string
	}{
		{"runtime servers only", `{"mcp_servers":[],"permissions":{}}`, "awaiting_model_provider"},
		{"model without provider", `{"model":"openai/m"}`, "unconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if err := os.WriteFile(path, []byte(tc.settings), 0600); err != nil {
				t.Fatal(err)
			}
			previous := doagentSettingsFile
			doagentSettingsFile = path
			t.Cleanup(func() { doagentSettingsFile = previous })
			status := inspectDoagentModelChannels()
			if status.status != tc.want {
				t.Fatalf("status = %q, want %q", status.status, tc.want)
			}
		})
	}
}

func TestModelProviderImportFromDoagentSettings(t *testing.T) {
	const canary = "synthetic-import-canary"
	cases := []struct {
		name     string
		settings string
		wantErr  bool
	}{
		{
			name: "keeps only model fields",
			settings: `{"model":"example/m","model_tiers":{"high":"example/m","default":"example/m","low":"example/m"},` +
				`"provider":{"example":{"options":{"apiKey":"` + canary + `","baseURL":"https://example.invalid/v1"}}},` +
				`"mcp_servers":[{"name":"other"}],"permissions":{"default_mode":"allow"},"image_provider":"example","image_model":"example/img"}`,
		},
		{name: "not json", settings: "not json", wantErr: true},
		{name: "no model", settings: `{"provider":{"example":{"options":{"apiKey":"` + canary + `"}}}}`, wantErr: true},
		{name: "file key reference", settings: `{"model":"example/m","provider":{"example":{"options":{"apiKey":"{file:/run/k}"}}}}`, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, err := modelProviderPayloadFromSettings([]byte(tc.settings))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got payload")
				}
				return
			}
			if err != nil {
				t.Fatalf("import: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(payload, &got); err != nil {
				t.Fatal(err)
			}
			for _, dropped := range []string{"model_tiers", "mcp_servers", "permissions"} {
				if _, ok := got[dropped]; ok {
					t.Fatalf("payload kept %s", dropped)
				}
			}
			if got["model"] != "example/m" || got["image_provider"] != "example" {
				t.Fatalf("payload fields = %v", got)
			}
		})
	}
}

func TestCredentialImportFormatSelectsTypeAndShape(t *testing.T) {
	base := credentialSecretImportRequest{
		SessionID: "import-format", Cluster: "dev", Instance: "local",
		Namespace: "apps", SecretName: "model-settings", Keys: []string{"settings.json"},
	}
	cases := []struct {
		name    string
		mutate  func(*credentialSecretImportRequest)
		wantErr bool
	}{
		{name: "doagent settings single key", mutate: func(r *credentialSecretImportRequest) { r.Format = "doagent_settings" }},
		{name: "doagent settings two keys", wantErr: true, mutate: func(r *credentialSecretImportRequest) {
			r.Format = "doagent_settings"
			r.Keys = []string{"a", "b"}
		}},
		{name: "doagent settings with key map", wantErr: true, mutate: func(r *credentialSecretImportRequest) {
			r.Format = "doagent_settings"
			r.KeyMap = map[string]string{"settings.json": "x"}
		}},
		{name: "unknown format", wantErr: true, mutate: func(r *credentialSecretImportRequest) { r.Format = "yaml" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base
			tc.mutate(&req)
			if err := validateCredentialSecretImport(req); (err != nil) != tc.wantErr {
				t.Fatalf("validate error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
	matches := []struct {
		format string
		typ    CredentialType
		want   bool
	}{
		{"", CredentialTypeOpaque, true},
		{"", CredentialTypeModelProvider, false},
		{"doagent_settings", CredentialTypeModelProvider, true},
		{"doagent_settings", CredentialTypeOpaque, false},
		{"yaml", CredentialTypeModelProvider, false},
	}
	for _, tc := range matches {
		if got := credentialImportFormatMatchesType(tc.format, tc.typ); got != tc.want {
			t.Fatalf("format %q type %q = %v, want %v", tc.format, tc.typ, got, tc.want)
		}
	}
}
