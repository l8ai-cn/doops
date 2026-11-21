package server

import (
	"strings"
	"testing"
)

func TestDoSQLSchemesMapToEngines(t *testing.T) {
	for scheme, want := range map[string]string{
		"postgres": "postgres", "postgresql": "postgres",
		"mysql": "mysql", "mariadb": "mysql", "tidb": "mysql",
		"oracle": "oracle", "sqlserver": "mssql", "mssql": "mssql", "mongodb": "mongodb",
	} {
		got, ok := doSQLEngineForScheme(scheme)
		if !ok || got != want {
			t.Errorf("%s -> %q %v, want %s", scheme, got, ok, want)
		}
		if !doSQLSchemeSupported(scheme) {
			t.Errorf("import does not accept %s", scheme)
		}
	}
	if _, ok := doSQLEngineForScheme("ftp"); ok {
		t.Fatal("ftp accepted")
	}
}

func TestDoSQLMSSQLTarget(t *testing.T) {
	target, err := doSQLConnectionTarget("mssql", "app", map[string]string{"connection_uri": "sqlserver://sa:p%40ss@db.example:1444/app"})
	if err != nil || target.Driver != "sqlserver" || target.Address != "db.example:1444" || !strings.Contains(target.DSN, "database=app") {
		t.Fatalf("target: %#v %v", target, err)
	}
	if _, err := doSQLConnectionTarget("mssql", "other", map[string]string{"connection_uri": "sqlserver://sa:x@db/app"}); err == nil {
		t.Fatal("database mismatch accepted")
	}
	if got := doSQLQuoteIdentifier("mssql", "a]b"); got != "[a]]b]" {
		t.Fatalf("quote = %s", got)
	}
	if !strings.Contains(doSQLCopyInsert("mssql", "t", []string{"a", "b"}, 2), "(@p1, @p2), (@p3, @p4)") {
		t.Fatal("mssql placeholders wrong")
	}
	if doSQLCommitsImplicitly("mssql", "CREATE TABLE t (id int)") {
		t.Fatal("SQL Server DDL is transactional")
	}
	if _, err := doSQLConnectionTarget("mysql", "app", map[string]string{"connection_uri": "mariadb://u:p@db/app"}); err != nil {
		t.Fatalf("mariadb uri: %v", err)
	}
}

func TestCredentialImportSchemeNeedsADriverOnTheAgent(t *testing.T) {
	t.Setenv(doSQLExternalDirEnv, t.TempDir())
	req := credentialSecretImportRequest{
		SessionID: "s1", Cluster: "c", Instance: "i", Namespace: "ns", SecretName: "db",
		Keys: []string{"password"}, PasswordKey: "password", ConnectionURI: "extsql://user@db/app",
	}
	// The Gateway has no driver directory, so it accepts any well-formed scheme.
	if err := validateCredentialSecretImport(req); err != nil {
		t.Fatalf("well-formed scheme rejected by the Gateway: %v", err)
	}
	req.ConnectionURI = "FTP_://user@db/app"
	if err := validateCredentialSecretImport(req); err == nil {
		t.Fatal("malformed scheme accepted")
	}
	// The Agent decides whether a driver exists for it.
	if doSQLSchemeSupported("extsql") {
		t.Fatal("scheme without a driver reported as supported")
	}
	if !doSQLSchemeSupported("mysql") {
		t.Fatal("built-in scheme rejected")
	}
}
