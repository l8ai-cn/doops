package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// A modelProvider credential is the doagent model configuration for one
// target. The Gateway stores it encrypted and delivers it to the granted
// Agent, which merges it into doagent's fixed settings file. No mounted
// settings Secret is involved.

const modelProviderProbeTimeout = 90 * time.Second

// modelProviderPayload is the strict payload of a modelProvider credential.
// Provider entries use doagent's settings.json `provider` shape and are passed
// through unchanged.
type modelProviderPayload struct {
	Model         string                     `json:"model"`
	Provider      map[string]json.RawMessage `json:"provider"`
	ImageProvider string                     `json:"image_provider,omitempty"`
	ImageModel    string                     `json:"image_model,omitempty"`
}

func decodeModelProviderPayload(payload []byte) (modelProviderPayload, error) {
	var value modelProviderPayload
	if err := decodeStrictCredentialPayload(payload, &value); err != nil {
		return modelProviderPayload{}, ErrCredentialPayloadInvalid
	}
	value.Model = strings.TrimSpace(value.Model)
	providerID, modelName, ok := strings.Cut(value.Model, "/")
	if !ok || strings.TrimSpace(providerID) == "" || strings.TrimSpace(modelName) == "" ||
		len(value.Provider) == 0 || len(value.Provider) > 32 {
		return modelProviderPayload{}, ErrCredentialPayloadInvalid
	}
	if _, exists := value.Provider[providerID]; !exists {
		return modelProviderPayload{}, ErrCredentialPayloadInvalid
	}
	for id, raw := range value.Provider {
		if !validCredentialMetadataToken(id, 64) || strings.Contains(id, "/") {
			return modelProviderPayload{}, ErrCredentialPayloadInvalid
		}
		var entry struct {
			Options struct {
				APIKey  string `json:"apiKey"`
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		}
		if err := json.Unmarshal(raw, &entry); err != nil {
			return modelProviderPayload{}, ErrCredentialPayloadInvalid
		}
		key := strings.TrimSpace(entry.Options.APIKey)
		// The Gateway is the only key source: file, env and keyring references
		// would make the Agent depend on host state the Gateway cannot verify.
		if key == "" || strings.HasPrefix(key, "{") {
			return modelProviderPayload{}, ErrCredentialPayloadInvalid
		}
		if entry.Options.BaseURL != "" && !validCredentialEndpoint(entry.Options.BaseURL, false) {
			return modelProviderPayload{}, ErrCredentialPayloadInvalid
		}
	}
	if value.ImageProvider != "" {
		if _, exists := value.Provider[strings.TrimSpace(value.ImageProvider)]; !exists {
			return modelProviderPayload{}, ErrCredentialPayloadInvalid
		}
	}
	return value, nil
}

// modelProviderRecord marks which credential version supplied the model in
// doagent's settings file. Only metadata is stored here.
type modelProviderRecord struct {
	CredentialID string `json:"credential_id"`
	GrantID      string `json:"grant_id"`
	VersionID    string `json:"version_id"`
	Digest       string `json:"digest"`
}

func modelProviderRecordPath() string {
	return filepath.Join(filepath.Dir(doagentSettingsPath()), "doops-model-provider.json")
}

func readModelProviderRecord() (modelProviderRecord, bool) {
	data, err := os.ReadFile(modelProviderRecordPath())
	if err != nil {
		return modelProviderRecord{}, false
	}
	var record modelProviderRecord
	if json.Unmarshal(data, &record) != nil || record.CredentialID == "" || record.GrantID == "" {
		return modelProviderRecord{}, false
	}
	return record, true
}

// doopsMCPToolTimeoutSeconds is how long doagent waits for one MCP tool call
// before it stops the server. Tools that mutate (Helm) budget their own work to
// finish inside it; see helmToolBudget.
const doopsMCPToolTimeoutSeconds = 1800

// Runtime MCP servers and permissions every DoOps doagent session needs. They
// are owned by the Agent image, not by any credential.
var doagentRuntimeMCPServers = []map[string]any{
	doagentRuntimeMCPServer("doops"),
	doagentRuntimeMCPServer("dosql"),
}

func doagentRuntimeMCPServer(domain string) map[string]any {
	return map[string]any{
		"name": domain, "command": "/app/doops-agent",
		"args": []string{"mcp-stdio", "--domain", domain},
		"env":  map[string]string{}, "cwd": nil, "status": "Unknown",
		"capabilities": []string{"tools"}, "auto_start": true,
		"tool_timeout_secs": doopsMCPToolTimeoutSeconds, "required": true, "trusted_result_attestation": true,
	}
}

var doagentRuntimePermissions = map[string]any{
	"default_mode": "restricted",
	"presets": map[string]any{
		"always_allow": []string{"mcp_doops_k8s_read", "mcp_dosql_read"},
		"always_deny":  []string{},
	},
	"timeout_seconds": 30,
}

// modelSettingsKeys are the settings fields a modelProvider credential owns.
var modelSettingsKeys = []string{"model", "provider", "model_tiers", "default_models", "image_provider", "image_model"}

// EnsureDoagentBaseSettings writes doagent's fixed settings file with the
// runtime MCP servers and permissions. Model fields survive only when a
// Gateway-delivered credential recorded them; anything else (for example a
// key copied from a previously mounted Secret) is dropped, so a stale provider
// is never used. The Gateway delivers the model after the Agent connects.
func EnsureDoagentBaseSettings() error {
	settings := map[string]any{}
	if _, delivered := readModelProviderRecord(); delivered {
		if data, err := os.ReadFile(doagentSettingsPath()); err == nil {
			var existing map[string]any
			if json.Unmarshal(data, &existing) == nil {
				for _, key := range modelSettingsKeys {
					if value, ok := existing[key]; ok {
						settings[key] = value
					}
				}
			}
		}
	} else {
		_ = os.Remove(modelProviderRecordPath())
	}
	return writeDoagentSettings(settings)
}

func writeDoagentSettings(model map[string]any) error {
	settings := map[string]any{}
	for key, value := range model {
		settings[key] = value
	}
	settings["mcp_servers"] = doagentRuntimeMCPServers
	settings["permissions"] = doagentRuntimePermissions
	encoded, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	defer clear(encoded)
	return writeFileAtomic0600(doagentSettingsPath(), append(encoded, '\n'))
}

func writeFileAtomic0600(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// probeModelProvider calls the model once through doagent. The payload goes
// on stdin; doagent prints only status and a stable error category.
var probeModelProvider = func(ctx context.Context, payload modelProviderPayload) error {
	input, err := json.Marshal(map[string]any{"model": payload.Model, "provider": payload.Provider})
	if err != nil {
		return ErrCredentialPayloadInvalid
	}
	defer clear(input)
	ctx, cancel := context.WithTimeout(ctx, modelProviderProbeTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "do-agent", "model", "probe", "--timeout-secs", "60")
	command.Stdin = bytes.NewReader(input)
	output, runErr := command.Output()
	var report struct {
		Status        string `json:"status"`
		ErrorCategory string `json:"errorCategory"`
	}
	if json.Unmarshal(bytes.TrimSpace(output), &report) != nil {
		if runErr != nil {
			return errors.New("model_probe_unavailable")
		}
		return errors.New("model_probe_result_invalid")
	}
	if report.Status == "verified" && runErr == nil {
		return nil
	}
	if validCredentialErrorCategory(report.ErrorCategory) && strings.HasPrefix(report.ErrorCategory, "model_") {
		return errors.New(report.ErrorCategory)
	}
	return errors.New("model_probe_failed")
}

func materializeModelProviderCredential(ctx context.Context, req CredentialMaterializeRequest) (CredentialMaterialization, error) {
	if req.Scope.Use != CredentialUseModelProvider {
		return CredentialMaterialization{}, errors.New("credential_use_mismatch")
	}
	payload, err := decodeModelProviderPayload(req.Payload)
	if err != nil {
		return CredentialMaterialization{}, err
	}
	if err := probeModelProvider(ctx, payload); err != nil {
		return CredentialMaterialization{}, err
	}
	sum := sha256.Sum256(req.Payload)
	digest := "sha256:" + hex.EncodeToString(sum[:])
	model := map[string]any{"model": payload.Model, "provider": payload.Provider}
	if payload.ImageProvider != "" {
		model["image_provider"] = payload.ImageProvider
	}
	if payload.ImageModel != "" {
		model["image_model"] = payload.ImageModel
	}
	if err := writeDoagentSettings(model); err != nil {
		return CredentialMaterialization{}, errors.New("model_settings_write_failed")
	}
	record, _ := json.Marshal(modelProviderRecord{
		CredentialID: req.CredentialID, GrantID: req.GrantID, VersionID: req.VersionID, Digest: digest,
	})
	if err := writeFileAtomic0600(modelProviderRecordPath(), record); err != nil {
		return CredentialMaterialization{}, errors.New("credential_target_outcome_unknown")
	}
	result := CredentialMaterialization{
		CredentialID: req.CredentialID, GrantID: req.GrantID, VersionID: req.VersionID,
		ResourceName: credentialSecretName(req.CredentialID, req.GrantID),
		Namespace:    req.Scope.Namespace, Status: "verified", Digest: digest,
		ResourceVersion: "settings-" + strings.TrimPrefix(digest, "sha256:")[:16],
	}
	// No PreviousVersionID: the settings file holds one model at a time and the
	// Gateway restores a version by delivering it again, not from this report.
	return result, nil
}

// cleanupModelProviderCredential removes the delivered model when the
// settings file still carries this credential grant (and version, unless any
// version is requested). It reports whether it owned the cleanup, so the caller
// does not look for a Kubernetes Secret a model credential never creates. A
// settings file owned by another grant is left alone.
func cleanupModelProviderCredential(req CredentialCleanupRequest) (bool, error) {
	record, delivered := readModelProviderRecord()
	if !delivered || record.CredentialID != req.CredentialID || record.GrantID != req.GrantID {
		return false, nil
	}
	if !req.AnyVersion && record.VersionID != req.VersionID {
		// Another version of this grant is live; this version has nothing to remove.
		return true, nil
	}
	if err := writeDoagentSettings(nil); err != nil {
		return false, errors.New("credential_target_outcome_unknown")
	}
	if err := os.Remove(modelProviderRecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("credential_target_outcome_unknown")
	}
	return true, nil
}

// modelProviderPayloadFromSettings turns a doagent settings.json (for example a
// previously mounted model Secret) into a modelProvider payload. Only the model
// fields are kept; runtime MCP servers, permissions and other host settings are
// owned by the Agent image and are dropped.
func modelProviderPayloadFromSettings(settings []byte) ([]byte, error) {
	var source struct {
		Model         string                     `json:"model"`
		Provider      map[string]json.RawMessage `json:"provider"`
		ImageProvider string                     `json:"image_provider"`
		ImageModel    string                     `json:"image_model"`
	}
	if err := json.Unmarshal(settings, &source); err != nil {
		return nil, ErrCredentialPayloadInvalid
	}
	value := map[string]any{"model": strings.TrimSpace(source.Model), "provider": source.Provider}
	if source.ImageProvider != "" {
		value["image_provider"] = source.ImageProvider
	}
	if source.ImageModel != "" {
		value["image_model"] = source.ImageModel
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, ErrCredentialPayloadInvalid
	}
	if _, err := decodeModelProviderPayload(payload); err != nil {
		clear(payload)
		return nil, ErrCredentialPayloadInvalid
	}
	return payload, nil
}

// credentialImportFormatMatchesType pairs each Secret import format with the
// credential type it produces.
func credentialImportFormatMatchesType(format string, credentialType CredentialType) bool {
	switch format {
	case "":
		return credentialType == CredentialTypeOpaque
	case credentialImportFormatDoagentSettings:
		return credentialType == CredentialTypeModelProvider
	default:
		return false
	}
}
