package server

import (
	"database/sql"
	"strings"
)

// doSQLSchemeEngine maps a credential URI scheme to the engine whose driver and
// SQL rules DoSql applies. Databases that speak another engine's wire protocol
// share that engine's code path instead of getting a second implementation.
var doSQLSchemeEngine = map[string]string{
	"postgres": "postgres", "postgresql": "postgres",
	"mysql": "mysql", "mariadb": "mysql", "tidb": "mysql",
	"oracle":    "oracle",
	"sqlserver": "mssql", "mssql": "mssql",
	"mongodb": "mongodb",
}

// doSQLEngineForScheme is the one place that decides whether DoSql can open a
// scheme: a built-in engine, or an operator-installed external driver.
func doSQLEngineForScheme(scheme string) (string, bool) {
	scheme = strings.ToLower(scheme)
	if engine, ok := doSQLSchemeEngine[scheme]; ok {
		return engine, true
	}
	if _, err := doSQLExternalDriverPath(scheme); err == nil {
		return scheme, true
	}
	return "", false
}

func doSQLSchemeSupported(scheme string) bool {
	_, ok := doSQLEngineForScheme(scheme)
	return ok
}

func doSQLSchemesForEngine(engine string) []string {
	var schemes []string
	for scheme, name := range doSQLSchemeEngine {
		if name == engine {
			schemes = append(schemes, scheme)
		}
	}
	return schemes
}

// SQL Server has no read-only transaction mode in its driver. Read-only
// batches there never commit, so the transaction is always rolled back.
func doSQLTxOptions(engine string, readOnly bool) *sql.TxOptions {
	if engine != "postgres" && engine != "mysql" {
		return nil
	}
	return &sql.TxOptions{ReadOnly: readOnly}
}

// A multi-row INSERT is bounded by the driver's placeholder cap and, on SQL
// Server, by 1000 rows per VALUES list.
func doSQLCopyLimits(engine string) (maxParams, maxRows int) {
	if engine == "mssql" {
		return 2000, 1000
	}
	return doSQLCopyMaxParams, doSQLCopyBatchRows
}
