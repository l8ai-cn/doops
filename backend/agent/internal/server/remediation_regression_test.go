package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRemediationWorkspaceRegularFileRoundTrip(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	write, _ := json.Marshal(map[string]string{"session_id": "local-review", "path": "notes/check.txt", "content": "wave"})
	if _, err := handleFileWrite(write); err != nil {
		t.Fatal(err)
	}
	read, _ := json.Marshal(map[string]string{"session_id": "local-review", "path": "notes/check.txt"})
	if got, err := handleFileRead(read); got != "wave" || err != nil {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestRemediationWorkspaceFailedExchangePreservesDestination(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(destination, 0700); err != nil {
		t.Fatal(err)
	}
	if err := exchangeWorkspaceDirectories(filepath.Join(root, "unavailable"), destination); err == nil {
		t.Fatal("missing replacement should fail")
	}
	info, err := os.Stat(destination)
	if err != nil || !info.IsDir() {
		t.Fatalf("destination was lost: %v", err)
	}
}

func TestRemediationMongoPolicyChecksKeysRatherThanValues(t *testing.T) {
	if err := validateMongoDocumentKeys(json.RawMessage(`{"items":[{"label":"$where"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestRemediationEdgeRegistrationDoesNotPublishFailedSession(t *testing.T) {
	store := openCredentialTestStore(t)
	registry, err := NewEdgeRegistry(store)
	if err != nil {
		t.Fatal(err)
	}
	session := &edgeGatewaySession{cluster: "review", instance: "node"}
	if _, err := store.db.Exec(`CREATE TRIGGER reject_test_edge_registration BEFORE INSERT ON edge_agent_status BEGIN SELECT RAISE(FAIL, 'synthetic fault'); END`); err != nil {
		t.Fatal(err)
	}
	if err := registry.register(session); err == nil {
		t.Fatal("registration should report persistence failure")
	}
	if count := registry.SessionCount(session.cluster, session.instance); count != 0 {
		t.Fatalf("failed registration left %d sessions", count)
	}
	if _, err := store.db.Exec("DROP TRIGGER reject_test_edge_registration"); err != nil {
		t.Fatal(err)
	}
	if err := registry.register(session); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < defaultEdgeMaxSessionsPerTarget; i++ {
		if err := registry.register(&edgeGatewaySession{cluster: session.cluster, instance: session.instance}); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.register(&edgeGatewaySession{cluster: session.cluster, instance: session.instance}); err == nil {
		t.Fatal("registration exceeded capacity")
	}
}

func TestRemediationDirectSQLQuotedWordsRemainReadable(t *testing.T) {
	if !directReadOnlyStatement("postgres", "SELECT 'update', 'into'") {
		t.Fatal("quoted text must not be classified as SQL keywords")
	}
}
