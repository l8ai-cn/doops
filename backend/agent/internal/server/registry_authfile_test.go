package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const registryAuthCanary = "synthetic-registry-auth-canary"

// registryAuthFileEntries reads the Agent's registry authfile.
func registryAuthFileEntries(t *testing.T, path string) map[string]map[string]string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry authfile: %v", err)
	}
	var document struct {
		Auths map[string]map[string]string `json:"auths"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("parse registry authfile: %v", err)
	}
	return document.Auths
}

func registryAuthRecordEntries(t *testing.T, path string) []registryAuthOwner {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read registry auth record: %v", err)
	}
	var record registryAuthRecord
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("parse registry auth record: %v", err)
	}
	return record.Entries
}

// A registry credential must leave the Agent with an authfile an OCI tool can
// read, because that is the only way `registry sync` and `image copy` work
// without an operator writing the file by hand.
func TestRegistryCredentialMaterializationDeliversAuthfile(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)
	recordPath := registryAuthRecordPath()

	entry := map[string]string{
		"username": "deploy",
		"password": registryAuthCanary,
		"auth":     "c3ludGhldGlj",
	}
	if err := deliverRegistryAuthEntry("registry.example.com:30002", entry, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatalf("deliver registry auth entry: %v", err)
	}

	auths := registryAuthFileEntries(t, authfile)
	delivered, ok := auths["registry.example.com:30002"]
	if !ok {
		t.Fatalf("authfile does not carry the delivered registry: %#v", auths)
	}
	if delivered["username"] != "deploy" || delivered["password"] != registryAuthCanary {
		t.Fatalf("unexpected authfile entry: %#v", delivered)
	}

	owners := registryAuthRecordEntries(t, recordPath)
	if len(owners) != 1 || owners[0].Server != "registry.example.com:30002" ||
		owners[0].CredentialID != "cred_a" || owners[0].GrantID != "grant_a" || owners[0].VersionID != "ver_a" {
		t.Fatalf("unexpected registry auth ownership record: %#v", owners)
	}
}

func TestRegistryOwnershipWriteFailureRestoresAuthfile(t *testing.T) {
	path := useRegistryAuthFileFixture(t)
	server := "registry.example.com"
	if err := deliverRegistryAuthEntry(server, map[string]string{"username": "original"}, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatal(err)
	}
	previous := registryAuthRecordPathOverride
	registryAuthRecordPathOverride = t.TempDir()
	t.Cleanup(func() { registryAuthRecordPathOverride = previous })
	if err := deliverRegistryAuthEntry(server, map[string]string{"username": "replacement"}, "cred_a", "grant_a", "ver_b"); err == nil {
		t.Fatal("expected ownership persistence failure")
	}
	if got := registryAuthFileEntries(t, path)[server]["username"]; got != "original" {
		t.Fatal("the previous auth entry was not restored")
	}
}

// The authfile is shared: a second registry credential adds to it without
// dropping the first one.
func TestRegistryAuthfileMergesMultipleRegistries(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)

	if err := deliverRegistryAuthEntry("first.example.com", map[string]string{"username": "one", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatalf("deliver first registry: %v", err)
	}
	if err := deliverRegistryAuthEntry("second.example.com", map[string]string{"username": "two", "password": registryAuthCanary}, "cred_b", "grant_b", "ver_b"); err != nil {
		t.Fatalf("deliver second registry: %v", err)
	}

	auths := registryAuthFileEntries(t, authfile)
	if len(auths) != 2 {
		t.Fatalf("authfile holds %d registries, want 2: %#v", len(auths), auths)
	}
	if auths["first.example.com"]["username"] != "one" || auths["second.example.com"]["username"] != "two" {
		t.Fatalf("unexpected authfile contents: %#v", auths)
	}
}

// An entry the Agent did not deliver must survive a delivery and a cleanup.
func TestRegistryAuthfileKeepsUnmanagedEntries(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)

	if err := os.WriteFile(authfile, []byte(
		`{"auths":{"operator.example.com":{"username":"operator","password":"operator-secret"}}}`), 0600); err != nil {
		t.Fatalf("seed operator authfile: %v", err)
	}
	if err := deliverRegistryAuthEntry("managed.example.com", map[string]string{"username": "deploy", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatalf("deliver registry auth entry: %v", err)
	}
	auths := registryAuthFileEntries(t, authfile)
	if _, ok := auths["operator.example.com"]; !ok {
		t.Fatalf("delivery dropped an unmanaged entry: %#v", auths)
	}

	if err := cleanupRegistryAuthEntries(CredentialCleanupRequest{
		CredentialID: "cred_a", GrantID: "grant_a", VersionID: "ver_a", AnyVersion: true,
	}); err != nil {
		t.Fatalf("cleanup registry auth entries: %v", err)
	}
	auths = registryAuthFileEntries(t, authfile)
	if _, ok := auths["managed.example.com"]; ok {
		t.Fatalf("cleanup left the delivered entry: %#v", auths)
	}
	if _, ok := auths["operator.example.com"]; !ok {
		t.Fatalf("cleanup dropped an unmanaged entry: %#v", auths)
	}
}

// Cleanup removes only the entries of the grant it names.
func TestRegistryAuthfileCleanupIsGrantScoped(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)

	if err := deliverRegistryAuthEntry("one.example.com", map[string]string{"username": "one", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatalf("deliver first registry: %v", err)
	}
	if err := deliverRegistryAuthEntry("two.example.com", map[string]string{"username": "two", "password": registryAuthCanary}, "cred_b", "grant_b", "ver_b"); err != nil {
		t.Fatalf("deliver second registry: %v", err)
	}

	if err := cleanupRegistryAuthEntries(CredentialCleanupRequest{
		CredentialID: "cred_a", GrantID: "grant_a", VersionID: "ver_a", AnyVersion: true,
	}); err != nil {
		t.Fatalf("cleanup registry auth entries: %v", err)
	}
	auths := registryAuthFileEntries(t, authfile)
	if _, ok := auths["one.example.com"]; ok {
		t.Fatalf("cleanup left the removed grant's entry: %#v", auths)
	}
	if _, ok := auths["two.example.com"]; !ok {
		t.Fatalf("cleanup removed another grant's entry: %#v", auths)
	}
}

// A live version of a grant keeps its entry: cleaning an older version must
// not take the registry away from the version now in use.
func TestRegistryAuthfileCleanupKeepsLiveVersion(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)

	if err := deliverRegistryAuthEntry("one.example.com", map[string]string{"username": "one", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_new"); err != nil {
		t.Fatalf("deliver registry auth entry: %v", err)
	}
	if err := cleanupRegistryAuthEntries(CredentialCleanupRequest{
		CredentialID: "cred_a", GrantID: "grant_a", VersionID: "ver_old",
	}); err != nil {
		t.Fatalf("cleanup old version: %v", err)
	}
	auths := registryAuthFileEntries(t, authfile)
	if _, ok := auths["one.example.com"]; !ok {
		t.Fatalf("cleanup of a stale version removed the live entry: %#v", auths)
	}
}

// Rotating a grant to a different registry must not leave the old registry
// behind: the grant now owns exactly one server.
func TestRegistryAuthfileRotationDropsTheOldServer(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)

	if err := deliverRegistryAuthEntry("old.example.com", map[string]string{"username": "one", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatalf("deliver first registry: %v", err)
	}
	if err := deliverRegistryAuthEntry("new.example.com", map[string]string{"username": "two", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_b"); err != nil {
		t.Fatalf("deliver rotated registry: %v", err)
	}
	auths := registryAuthFileEntries(t, authfile)
	if _, ok := auths["old.example.com"]; ok {
		t.Fatalf("rotation left the old registry behind: %#v", auths)
	}
	if _, ok := auths["new.example.com"]; !ok {
		t.Fatalf("rotation did not deliver the new registry: %#v", auths)
	}
}

// An authfile that cannot be parsed must fail the delivery rather than be
// silently replaced: the operator's own entries would be lost.
func TestRegistryAuthfileDeliveryRejectsUnreadableFile(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)

	if err := os.WriteFile(authfile, []byte("this is not json"), 0600); err != nil {
		t.Fatalf("seed broken authfile: %v", err)
	}
	err := deliverRegistryAuthEntry("one.example.com", map[string]string{"username": "one", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_a")
	if err == nil || err.Error() != "registry_authfile_invalid" {
		t.Fatalf("delivery to an unreadable authfile = %v, want registry_authfile_invalid", err)
	}
}

// The ownership record holds metadata only. A payload must never reach it.
func TestRegistryAuthRecordCarriesNoPayload(t *testing.T) {
	useRegistryAuthFileFixture(t)
	recordPath := registryAuthRecordPath()

	if err := deliverRegistryAuthEntry("one.example.com", map[string]string{"username": "one", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatalf("deliver registry auth entry: %v", err)
	}
	data, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatalf("read registry auth record: %v", err)
	}
	if strings.Contains(string(data), registryAuthCanary) {
		t.Fatalf("registry auth record exposed the credential payload: %s", data)
	}
}

// The authfile holds the credential, so it must not be readable by anyone but
// its owner.
func TestRegistryAuthFileIsOwnerOnly(t *testing.T) {
	authfile := useRegistryAuthFileFixture(t)

	if err := deliverRegistryAuthEntry("one.example.com", map[string]string{"username": "one", "password": registryAuthCanary}, "cred_a", "grant_a", "ver_a"); err != nil {
		t.Fatalf("deliver registry auth entry: %v", err)
	}
	info, err := os.Stat(authfile)
	if err != nil {
		t.Fatalf("stat registry authfile: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("registry authfile mode = %v, want 0600", info.Mode().Perm())
	}
}

// An authfile-only delivery restores the file without touching the Secret the
// kubelet pulls with.
func TestRegistryAuthOnlyMaterializationLeavesTheSecretAlone(t *testing.T) {
	useRegistryAuthFileFixture(t)
	request := CredentialMaterializeRequest{
		CredentialID: "cred_a", GrantID: "grant_a", VersionID: "ver_a",
		CredentialType: CredentialTypeRegistry,
		Scope: CredentialGrantScope{
			Cluster: "doops", Instance: "runner", Namespace: "kz-ops",
			Use: CredentialUseImagePull,
		},
	}
	evidence := registryAuthOnlyMaterialization(request, "one.example.com", "c3ludGhldGlj", "")
	if evidence.ResourceName != credentialSecretName(request.CredentialID, request.GrantID) {
		t.Fatalf("authfile-only evidence names %q", evidence.ResourceName)
	}
	if evidence.SecretType != "kubernetes.io/dockerconfigjson" {
		t.Fatalf("authfile-only evidence secret type = %q", evidence.SecretType)
	}
	if evidence.Status != "verified" {
		t.Fatalf("authfile-only evidence status = %q", evidence.Status)
	}
	if !strings.HasPrefix(evidence.ResourceVersion, "authfile-") {
		t.Fatalf("authfile-only evidence resource version = %q", evidence.ResourceVersion)
	}
}

// The delivery path must not pick a credential the target was not granted.
func TestRegistryAuthDeliveryIgnoresOtherTargets(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	if _, err := f.store.ActiveRegistryBindings("dev", "other"); err != nil {
		t.Fatalf("list registry bindings: %v", err)
	}
	bindings, err := f.store.ActiveRegistryBindings("dev", "local")
	if err != nil {
		t.Fatalf("list registry bindings: %v", err)
	}
	if len(bindings) != 0 {
		t.Fatalf("target without a registry grant reported %d bindings", len(bindings))
	}
}

// A target with no registry credential must not be given anything.
func TestRegistryAuthDeliveryWithoutCredentialsIsANoop(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	category := f.hub.deliverRegistryAuthFilesTo(context.Background(), "dev", "local")
	if category != "" {
		t.Fatalf("delivery without a registry credential = %q, want success", category)
	}
}

// The record file sits beside the authfile so a test can find it.
func TestRegistryAuthRecordPathFollowsTheAuthfile(t *testing.T) {
	useRegistryAuthFileFixture(t)
	if !strings.HasSuffix(registryAuthRecordPath(), "doops-registry-auth.json") {
		t.Fatalf("registry auth record path = %q", registryAuthRecordPath())
	}
	if filepath.Dir(registryAuthRecordPath()) != filepath.Dir(registryAuthFilePath()) {
		t.Fatalf("registry auth record is not beside the authfile")
	}
}
