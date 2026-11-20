package server

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/user/doops/agent/api"
)

func TestDoSQLReadUsesBoundCredentialMetadataAndReturnsEvidenceSummary(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "ws")
	session := "sql-read"
	if err := os.MkdirAll(filepath.Join(workspaceRoot, session), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOOPS_WORKSPACE_ROOT", workspaceRoot)
	t.Setenv("DOOPS_GATEWAY_CLUSTER", "cluster-a")
	t.Setenv("DOOPS_GATEWAY_INSTANCE", "node-a")

	binding := api.AgentCredentialBinding{
		Name: "orders-db", Use: string(CredentialUseDatabase), Namespace: "apps", ResourceName: "cred-secret",
		SecretType: "Opaque", Keys: []string{"connection_uri"}, ResourceVersion: "7",
		CredentialID: "cred_123456789012345678901234", GrantID: "credgrant_123456789012345678901234", VersionID: "credver_123456789012345678901234",
		DatabaseAssetID: "dbscan_cluster-a_node-a_apps_orders-postgres_postgres", EnvironmentID: "prod", DatabaseEngine: "postgres", DatabaseName: "orders",
	}
	leasePath := dosqlCredentialLeasePath(session)
	if err := os.MkdirAll(filepath.Dir(leasePath), 0700); err != nil {
		t.Fatal(err)
	}
	leaseJSON, _ := json.Marshal(dosqlCredentialLease{SessionID: session, Bindings: []api.AgentCredentialBinding{binding}})
	if err := os.WriteFile(leasePath, append(leaseJSON, '\n'), 0600); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	secretJSON := `{"metadata":{"resourceVersion":"7","labels":{"doops.sh/credential-id":"cred_123456789012345678901234","doops.sh/credential-grant-id":"credgrant_123456789012345678901234"},"annotations":{"doops.sh/credential-version":"credver_123456789012345678901234"}},"data":{"connection_uri":"` + base64.StdEncoding.EncodeToString([]byte("postgres://db-user:db-secret@db/orders")) + `"}}`
	writeExecutable(t, kubectl, "#!/bin/sh\ncase \"$*\" in *pods,services*) printf '%s' '{\"items\":[{\"kind\":\"StatefulSet\",\"metadata\":{\"namespace\":\"apps\",\"name\":\"orders-postgres\"},\"spec\":{\"template\":{\"spec\":{\"containers\":[{\"image\":\"postgres:16\",\"env\":[{\"name\":\"POSTGRES_DB\",\"value\":\"orders\"}]}]}}}}]}';; *) printf '%s' '"+secretJSON+"';; esac\n")
	node := filepath.Join(bin, "node")
	writeExecutable(t, node, "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = \"--output\" ]; then out=\"$2\"; fi; shift; done\nprintf '%s' '{\"status\":\"succeeded\",\"result\":{\"status\":\"verified\",\"executionResult\":{\"affectedRows\":1,\"transactionId\":\"dbtx_test\"}}}' > \"$out\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOSQL_NODE", node)

	request := map[string]interface{}{
		"session_id": session, "operation": "query", "credential_ref": "orders-db", "database_asset_id": "dbscan_cluster-a_node-a_apps_orders-postgres_postgres",
		"environment_id": "prod", "project_id": "ops", "engine": "postgres", "database_name": "orders",
		"statement": "select count(*) from orders",
	}
	encoded, _ := json.Marshal(request)
	result, err := handleDoSQLTool(t.Context(), encoded)
	if err != nil {
		t.Fatalf("read query: %v", err)
	}
	if !strings.Contains(result, `"status":"verified"`) || !strings.Contains(result, "dbtx_test") {
		t.Fatalf("unexpected bounded result: %s", result)
	}
	if strings.Contains(result, "db-secret") || strings.Contains(result, "postgres://") {
		t.Fatalf("credential payload leaked into result: %s", result)
	}
}

func TestDoSQLExecutionRequestUsesIntegratedGoRunner(t *testing.T) {
	binding := api.AgentCredentialBinding{
		Use: string(CredentialUseDatabase), Namespace: "apps", ResourceName: "database-secret",
		Keys: []string{"connection_uri"}, SecretType: "Opaque", ResourceVersion: "7",
	}
	params := doSQLParams{Engine: "postgres", DatabaseName: "orders", Statement: "SELECT 1"}
	request, env, err := buildDoSQLExecutionRequest(params, binding, "dbop_test", "/tmp/journal", "/tmp/evidence")
	if err != nil {
		t.Fatal(err)
	}
	adapter, ok := request["adapter"].(map[string]interface{})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if !ok || adapter["type"] != "doops-go" || adapter["runnerPath"] != executable {
		t.Fatalf("unexpected integrated adapter: %#v", request["adapter"])
	}
	if len(env) != 0 {
		t.Fatalf("Go runner must not receive credential payload env: %#v", env)
	}
	if adapter["clientImage"] != "postgres:16" {
		t.Fatalf("PostgreSQL must use a PostgreSQL client image: %#v", adapter)
	}
}

func TestDoSQLReadReturnsBoundedQueryOutput(t *testing.T) {
	result := doSQLResultSummary(map[string]interface{}{
		"executionResult": map[string]interface{}{"stdout": "1\n", "transactionId": "dbtx_query"},
	}, "dbop_query", "/tmp/journal", "/tmp/evidence")
	if !strings.Contains(result, `"output":"1\n"`) || !strings.Contains(result, `"status":"verified"`) {
		t.Fatalf("verified SQL output is missing: %s", result)
	}
	result = doSQLResultSummary(map[string]interface{}{
		"executionResult": map[string]interface{}{"stdout": strings.Repeat("a", 40<<10)},
	}, "dbop_query", "/tmp/journal", "/tmp/evidence")
	if !strings.Contains(result, `"output_truncated":true`) || len(result) > 34<<10 {
		t.Fatal("SQL output is not bounded")
	}
}

func TestDoSQLRejectsDatabaseAssetMismatchAtAgentBoundary(t *testing.T) {
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "ws")
	session := "sql-denied"
	if err := os.MkdirAll(filepath.Join(workspaceRoot, session), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOOPS_WORKSPACE_ROOT", workspaceRoot)
	leasePath := dosqlCredentialLeasePath(session)
	if err := os.MkdirAll(filepath.Dir(leasePath), 0700); err != nil {
		t.Fatal(err)
	}
	binding := api.AgentCredentialBinding{Name: "orders-db", Use: string(CredentialUseDatabase), Namespace: "apps", ResourceName: "cred-secret", SecretType: "Opaque", Keys: []string{"connection_uri"}, ResourceVersion: "7", CredentialID: "cred_123456789012345678901234", GrantID: "credgrant_123456789012345678901234", VersionID: "credver_123456789012345678901234", DatabaseAssetID: "db_orders", EnvironmentID: "prod", DatabaseEngine: "postgres", DatabaseName: "orders"}
	leaseJSON, _ := json.Marshal(dosqlCredentialLease{SessionID: session, Bindings: []api.AgentCredentialBinding{binding}})
	if err := os.WriteFile(leasePath, append(leaseJSON, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	writeExecutable(t, kubectl, `#!/bin/sh
cat <<'JSON'
{"items":[{"kind":"Service","metadata":{"namespace":"data","name":"orders-postgres"},"spec":{"ports":[{"port":5432}]}},{"kind":"StatefulSet","metadata":{"namespace":"data","name":"orders-postgres"},"spec":{"template":{"spec":{"containers":[{"name":"postgres","image":"postgres:16","env":[{"name":"POSTGRES_DB","value":"orders"}]}]}}}}]}
JSON
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_GATEWAY_CLUSTER", "cluster-a")
	t.Setenv("DOOPS_GATEWAY_INSTANCE", "node-a")
	request, _ := json.Marshal(map[string]interface{}{"session_id": session, "operation": "query", "credential_ref": "orders-db", "database_asset_id": "db_other", "environment_id": "prod", "project_id": "ops", "engine": "postgres", "database_name": "orders", "statement": "select 1"})
	if _, err := handleDoSQLTool(t.Context(), request); err == nil || !strings.Contains(err.Error(), "database_asset_not_found") {
		t.Fatalf("database asset boundary was not enforced: %v", err)
	}
}

func TestDoSQLDiscoveryFindsDatabaseCandidatesWithoutReadingSecrets(t *testing.T) {
	bin := t.TempDir()
	kubectl := filepath.Join(bin, "kubectl")
	writeExecutable(t, kubectl, `#!/bin/sh
cat <<'JSON'
{"items":[
  {"kind":"Service","metadata":{"namespace":"data","name":"orders-postgres"},"spec":{"ports":[{"port":5432}]}},
  {"kind":"StatefulSet","metadata":{"namespace":"data","name":"orders-postgres"},"spec":{"template":{"spec":{"containers":[{"name":"postgres","image":"postgres:16","env":[{"name":"POSTGRES_DB","value":"orders"},{"name":"POSTGRES_PASSWORD","valueFrom":{"secretKeyRef":{"name":"db-secret","key":"password"}}}]}]}}}},
  {"kind":"StatefulSet","metadata":{"namespace":"data","name":"billing-mysql"},"spec":{"template":{"spec":{"containers":[{"name":"mysql","image":"mysql:8.4"}]}}}}
]}
JSON
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_GATEWAY_CLUSTER", "cluster-a")
	t.Setenv("DOOPS_GATEWAY_INSTANCE", "node-a")
	request, _ := json.Marshal(map[string]interface{}{"session_id": "discover", "operation": "discover"})
	result, err := handleDoSQLTool(t.Context(), request)
	if err != nil {
		t.Fatalf("discover databases: %v", err)
	}
	if strings.Contains(result, "POSTGRES_PASSWORD") || strings.Contains(result, "db-secret") {
		t.Fatalf("discovery leaked secret metadata: %s", result)
	}
	for _, expected := range []string{"dbscan_cluster-a_node-a_data_orders-postgres_postgres", "orders", "billing-mysql", "needs_database_name_or_credential"} {
		if !strings.Contains(result, expected) {
			t.Fatalf("discovery result missing %q: %s", expected, result)
		}
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverDatabaseCandidatesWhenKubectlMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := discoverDatabaseCandidates(t.Context())
	if err == nil || err.Error() != "dosql_discovery_unavailable" {
		t.Fatalf("kubectl failure must be dosql_discovery_unavailable, got %v", err)
	}
}
