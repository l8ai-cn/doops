package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// A registry credential materializes a Kubernetes Secret for the kubelet to
// pull with, but the Agent's own OCI tools (skopeo, crane, ctr) read an
// authfile instead. Delivering that file here is what lets `doops registry
// sync` and `doops image copy` run without an authfile argument, and what
// gives a restarted Agent its registries back from the Gateway instead of
// from an operator writing the file again by hand.
//
// The path is the one every OCI tool already reads by default. The file is
// merged, never replaced: entries this Agent did not deliver stay untouched,
// and a sidecar record tracks which servers came from which grant so cleanup
// removes only those.
// registryAuthFile is the authfile every OCI tool reads by default. It is a
// variable solely so tests can point it at a fixture.
var registryAuthFile = "/root/.docker/config.json"

// registryAuthMutex serializes the read-modify-write of the shared authfile.
// Grants for different credentials are not covered by the Gateway's per-
// credential lock, so two deliveries can reach this file at once.
var registryAuthMutex sync.Mutex

func registryAuthFilePath() string {
	return registryAuthFile
}

// registryAuthRecordPathOverride lets a test point the ownership record
// somewhere other than beside the authfile.
var registryAuthRecordPathOverride string

func registryAuthRecordPath() string {
	if override := strings.TrimSpace(registryAuthRecordPathOverride); override != "" {
		return override
	}
	return filepath.Join(filepath.Dir(doagentSettingsPath()), "doops-registry-auth.json")
}

// registryAuthOwner records that one registry server in the authfile was
// delivered by one credential grant. Metadata only; never a payload.
type registryAuthOwner struct {
	Server       string `json:"server"`
	CredentialID string `json:"credential_id"`
	GrantID      string `json:"grant_id"`
	VersionID    string `json:"version_id"`
}

type registryAuthRecord struct {
	Entries []registryAuthOwner `json:"entries"`
}

func readRegistryAuthRecord() registryAuthRecord {
	data, err := os.ReadFile(registryAuthRecordPath())
	if err != nil {
		return registryAuthRecord{}
	}
	var record registryAuthRecord
	if json.Unmarshal(data, &record) != nil {
		return registryAuthRecord{}
	}
	owned := make([]registryAuthOwner, 0, len(record.Entries))
	for _, entry := range record.Entries {
		if strings.TrimSpace(entry.Server) == "" ||
			strings.TrimSpace(entry.CredentialID) == "" ||
			strings.TrimSpace(entry.GrantID) == "" {
			continue
		}
		owned = append(owned, entry)
	}
	return registryAuthRecord{Entries: owned}
}

// readRegistryAuthFile returns the authfile's auths map. A missing or
// unreadable file starts an empty one; a file that exists but does not parse is
// an error, so a delivery never silently discards an operator's entries.
func readRegistryAuthFile() (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	data, err := os.ReadFile(registryAuthFilePath())
	if errors.Is(err, os.ErrNotExist) {
		return map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
	}
	if err != nil {
		return nil, nil, errors.New("registry_authfile_unavailable")
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return map[string]json.RawMessage{}, map[string]json.RawMessage{}, nil
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(data, &document) != nil {
		return nil, nil, errors.New("registry_authfile_invalid")
	}
	auths := map[string]json.RawMessage{}
	if raw, ok := document["auths"]; ok {
		if json.Unmarshal(raw, &auths) != nil {
			return nil, nil, errors.New("registry_authfile_invalid")
		}
	}
	delete(document, "auths")
	return auths, document, nil
}

func writeRegistryAuthFile(auths map[string]json.RawMessage, rest map[string]json.RawMessage, record registryAuthRecord) error {
	document := make(map[string]any, len(rest)+1)
	for key, value := range rest {
		document[key] = value
	}
	// An empty auths map is kept so the file stays a valid authfile after the
	// last delivered registry is removed.
	document["auths"] = auths
	encoded, err := json.Marshal(document)
	if err != nil {
		return errors.New("registry_authfile_unavailable")
	}
	defer clear(encoded)
	previous, err := os.ReadFile(registryAuthFilePath())
	existed := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("registry_authfile_unavailable")
	}
	defer clear(previous)
	if err := writeFileAtomic0600(registryAuthFilePath(), append(encoded, '\n')); err != nil {
		return errors.New("registry_authfile_unavailable")
	}
	restore := func() error {
		var err error
		if existed {
			err = writeFileAtomic0600(registryAuthFilePath(), previous)
		} else {
			err = os.Remove(registryAuthFilePath())
		}
		if err != nil && (existed || !errors.Is(err, os.ErrNotExist)) {
			return errors.New("credential_target_outcome_unknown")
		}
		return errors.New("registry_authfile_unavailable")
	}
	sort.Slice(record.Entries, func(i, j int) bool {
		if record.Entries[i].Server != record.Entries[j].Server {
			return record.Entries[i].Server < record.Entries[j].Server
		}
		return record.Entries[i].GrantID < record.Entries[j].GrantID
	})
	encodedRecord, err := json.Marshal(record)
	if err != nil {
		return restore()
	}
	if len(record.Entries) == 0 {
		if err := os.Remove(registryAuthRecordPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return restore()
		}
		return nil
	}
	if err := writeFileAtomic0600(registryAuthRecordPath(), append(encodedRecord, '\n')); err != nil {
		return restore()
	}
	return nil
}

// registryAuthPendingDelivery is a registry authfile write that waits until
// the Kubernetes Secret has been verified.
type registryAuthPendingDelivery struct {
	server       string
	entry        map[string]string
	credentialID string
	grantID      string
	versionID    string
}

// registryAuthOnlyMaterialization builds the evidence for a delivery that only
// restored the Agent's authfile. The Kubernetes Secret was left untouched, so
// the evidence reports the Secret the credential owns without claiming a new
// resource version for it.
func registryAuthOnlyMaterialization(req CredentialMaterializeRequest, server, auth, digest string) CredentialMaterialization {
	sum := sha256.Sum256([]byte(server + "\x00" + auth))
	return CredentialMaterialization{
		CredentialID: req.CredentialID,
		VersionID:    req.VersionID,
		ResourceName: credentialSecretName(req.CredentialID, req.GrantID),
		Namespace:    req.Scope.Namespace,
		SecretType:   "kubernetes.io/dockerconfigjson",
		Keys:         []string{".dockerconfigjson"},
		// The authfile is the artifact that was actually written. Its digest
		// identifies that file, not the Secret, which was not touched.
		ResourceVersion: "authfile-" + hex.EncodeToString(sum[:8]),
		Digest:          digest,
		Status:          "verified",
	}
}

// deliverRegistryAuthEntry merges one registry's authentication into the
// Agent's authfile under the given grant's ownership. Rotating a grant to a
// different server drops the server it used to own.
func deliverRegistryAuthEntry(server string, entry map[string]string, credentialID, grantID, versionID string) error {
	server = strings.TrimSpace(server)
	credentialID = strings.TrimSpace(credentialID)
	grantID = strings.TrimSpace(grantID)
	versionID = strings.TrimSpace(versionID)
	if server == "" || len(entry) == 0 || credentialID == "" || grantID == "" || versionID == "" {
		return errors.New("registry_authfile_request_invalid")
	}
	encodedEntry, err := json.Marshal(entry)
	if err != nil {
		return errors.New("registry_authfile_request_invalid")
	}

	registryAuthMutex.Lock()
	defer registryAuthMutex.Unlock()

	auths, rest, err := readRegistryAuthFile()
	if err != nil {
		return err
	}
	record := readRegistryAuthRecord()
	kept := make([]registryAuthOwner, 0, len(record.Entries)+1)
	for _, owner := range record.Entries {
		if owner.CredentialID == credentialID && owner.GrantID == grantID {
			// This grant's previous server is no longer owned by it.
			if owner.Server != server {
				delete(auths, owner.Server)
			}
			continue
		}
		kept = append(kept, owner)
	}
	auths[server] = encodedEntry
	kept = append(kept, registryAuthOwner{
		Server: server, CredentialID: credentialID, GrantID: grantID, VersionID: versionID,
	})
	return writeRegistryAuthFile(auths, rest, registryAuthRecord{Entries: kept})
}

// cleanupRegistryAuthEntries removes the authfile entries this grant delivered.
// Unlike the Helm and model-provider cleanups it does not own the whole
// cleanup: a registry credential also has a Kubernetes Secret to remove, so the
// caller carries on afterwards.
func cleanupRegistryAuthEntries(req CredentialCleanupRequest) error {
	credentialID := strings.TrimSpace(req.CredentialID)
	grantID := strings.TrimSpace(req.GrantID)
	if credentialID == "" || grantID == "" {
		return errors.New("registry_authfile_request_invalid")
	}

	registryAuthMutex.Lock()
	defer registryAuthMutex.Unlock()

	record := readRegistryAuthRecord()
	if len(record.Entries) == 0 {
		return nil
	}
	kept := make([]registryAuthOwner, 0, len(record.Entries))
	removed := false
	for _, owner := range record.Entries {
		if owner.CredentialID != credentialID || owner.GrantID != grantID {
			kept = append(kept, owner)
			continue
		}
		if !req.AnyVersion && owner.VersionID != strings.TrimSpace(req.VersionID) {
			// Another version of this grant is live; this version owns nothing.
			kept = append(kept, owner)
			continue
		}
		removed = true
	}
	if !removed {
		return nil
	}
	auths, rest, err := readRegistryAuthFile()
	if err != nil {
		return err
	}
	stillOwned := make(map[string]struct{}, len(kept))
	for _, owner := range kept {
		stillOwned[owner.Server] = struct{}{}
	}
	for _, owner := range record.Entries {
		if owner.CredentialID != credentialID || owner.GrantID != grantID {
			continue
		}
		if _, ok := stillOwned[owner.Server]; ok {
			continue
		}
		delete(auths, owner.Server)
	}
	return writeRegistryAuthFile(auths, rest, registryAuthRecord{Entries: kept})
}
