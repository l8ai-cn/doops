package server

import (
	"net"
	"net/url"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/microsoft/go-mssqldb/msdsn"
	"github.com/sijms/go-ora/v2/configurations"
	"go.mongodb.org/mongo-driver/v2/x/mongo/driver/connstring"
)

func TestDoSQLTargetRejectsURIIdentityOverrides(t *testing.T) {
	for _, tc := range []struct {
		engine, scheme string
		options        []string
	}{
		{"postgres", "postgres", []string{"host", "hostaddr", "port", "dbname", "database", "service", "servicefile"}},
		{"mssql", "sqlserver", []string{"server", "port", "database", "failoverpartner", "failoverport"}},
		{"oracle", "oracle", []string{"connStr", "server", "service name", "SID", "instance name", "unix socket"}},
		{"mongodb", "mongodb", []string{"host", "port", "database", "dbname"}},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			for _, option := range tc.options {
				for _, key := range []string{option, strings.ToUpper(option)} {
					uri := tc.scheme + "://u:synthetic@db-a.test/app?" + url.QueryEscape(key) + "=db-b.test"
					_, err := doSQLConnectionTarget(tc.engine, "app", map[string]string{"connection_uri": uri})
					if err == nil || !strings.Contains(err.Error(), "dosql_connection_uri_target_override_unsupported") {
						t.Errorf("target override %q accepted: %v", key, err)
					}
				}
			}
		})
	}
	for _, suffix := range []string{"/", "?host=%zz", "?x=1;host=db-b.test", "#other"} {
		if _, err := doSQLConnectionTarget("postgres", "app", map[string]string{
			"connection_uri": "postgres://u:p@db-a.test/app" + suffix,
		}); err == nil {
			t.Errorf("ambiguous path/query accepted: %q", suffix)
		}
	}
}

func TestDoSQLTargetMatchesDriverEffectiveDestination(t *testing.T) {
	// No connection is opened. These are the linked drivers' real parsers.
	// Isolate pgx from local service/password files and demonstrate that an
	// environment port cannot override the endpoint used by the journal.
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGSERVICEFILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PGPORT", "6543")
	t.Setenv("PGHOST", "environment.test")
	t.Setenv("PGDATABASE", "environment_database")
	for _, tc := range []struct{ engine, scheme, port, query string }{
		{"postgres", "postgres", "5432", "?sslmode=disable"},
		{"mysql", "mysql", "3306", "?host=ignored.test&port=1234&dbname=ignored"},
		{"mssql", "sqlserver", "1433", "?encrypt=disable"},
		{"oracle", "oracle", "1521", ""},
		{"mongodb", "mongodb", "27017", "?authSource=admin"},
	} {
		t.Run(tc.scheme, func(t *testing.T) {
			target, err := doSQLConnectionTarget(tc.engine, "app", map[string]string{
				"connection_uri": tc.scheme + "://u:synthetic@db.test/app" + tc.query,
			})
			if err != nil {
				t.Fatal(err)
			}
			var address, database string
			switch tc.engine {
			case "postgres":
				config, err := pgconn.ParseConfig(target.DSN)
				if err != nil {
					t.Fatal("pgx rejected the generated DSN")
				}
				address, database = net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port))), config.Database
				for _, alternate := range config.Fallbacks {
					if alternate.Host != config.Host || alternate.Port != config.Port {
						t.Fatal("pgx can connect to an unjournaled alternate endpoint")
					}
				}
			case "mysql":
				config, err := mysql.ParseDSN(target.DSN)
				if err != nil {
					t.Fatal("mysql rejected the generated DSN")
				}
				address, database = config.Addr, config.DBName
			case "mssql":
				config, err := msdsn.Parse(target.DSN)
				if err != nil {
					t.Fatal("mssql rejected the generated DSN")
				}
				address, database = net.JoinHostPort(config.Host, strconv.FormatUint(config.Port, 10)), config.Database
				if config.Instance != "" || config.FailOverPartner != "" {
					t.Fatal("mssql has an unjournaled alternate target")
				}
			case "oracle":
				config, err := configurations.ParseConfig(target.DSN)
				if err != nil || len(config.Servers) != 1 {
					t.Fatal("oracle rejected the generated DSN or added another endpoint")
				}
				address, database = net.JoinHostPort(config.Servers[0].Addr, strconv.Itoa(config.Servers[0].Port)), config.ServiceName
			case "mongodb":
				config, err := connstring.ParseAndValidate(target.DSN)
				if err != nil || len(config.Hosts) != 1 {
					t.Fatal("mongo rejected the generated DSN or added another endpoint")
				}
				address, database = config.Hosts[0], config.Database
				if config.AuthSource != "admin" {
					t.Fatal("authSource was lost")
				}
			}
			if address != target.Address || database != target.Database || address != "db.test:"+tc.port {
				t.Fatalf("driver destination %s/%s differs from journal destination %s/%s", address, database, target.Address, target.Database)
			}
		})
	}
}

func TestDoSQLPostgresMultiHostPreservesDriverEndpoints(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(t.TempDir(), "ws"))
	t.Setenv("PGSERVICE", "")
	t.Setenv("PGPASSFILE", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("PGPORT", "5432")
	endpoints := func(config *pgconn.Config) []string {
		result := []string{net.JoinHostPort(config.Host, strconv.Itoa(int(config.Port)))}
		for _, alternate := range config.Fallbacks {
			result = append(result, net.JoinHostPort(alternate.Host, strconv.Itoa(int(alternate.Port))))
		}
		return result
	}
	var defaultPath string
	for _, authority := range []string{
		"db-a.test,db-b.test",
		"db-a.test:5432,db-b.test:5432",
		"db-a.test:5433,db-b.test:5434",
		"db-a.test,db-b.test:5433",
		"db-a.test:5433,db-b.test,db-c.test:5434",
	} {
		t.Run(authority, func(t *testing.T) {
			raw := "postgres://synthetic:synthetic@" + authority + "/app?sslmode=disable"
			original, err := pgconn.ParseConfig(raw)
			if err != nil {
				t.Fatal("pgx rejected the original multi-host URI")
			}
			target, err := doSQLConnectionTarget("postgres", "app", map[string]string{"connection_uri": raw})
			if err != nil {
				t.Fatal(err)
			}
			effective, err := pgconn.ParseConfig(target.DSN)
			if err != nil {
				t.Fatal("pgx rejected the rewritten multi-host URI")
			}
			if !reflect.DeepEqual(endpoints(original), endpoints(effective)) ||
				strings.Join(endpoints(effective), ",") != target.Address || effective.Database != target.Database {
				t.Fatalf("driver endpoints changed: original=%v effective=%v journal=%s", endpoints(original), endpoints(effective), target.Address)
			}
			path, err := doSQLVersionJournalPath(target)
			if err != nil {
				t.Fatal(err)
			}
			if target.Address == "db-a.test:5432,db-b.test:5432" {
				if defaultPath != "" && path != defaultPath {
					t.Fatal("omitted/explicit default ports selected different journals")
				}
				defaultPath = path
			}
		})
	}
	for _, authority := range []string{
		"db-a.test:5432,db-b.test", "db-a.test:5433,db-b.test",
	} {
		// A host that omits its port cannot be paired by pgx on any supported
		// toolchain, so this stays a rejection the Agent must mirror.
		raw := "postgres://synthetic:synthetic@" + authority + "/app?sslmode=disable"
		if _, err := pgconn.ParseConfig(raw); err == nil {
			t.Fatalf("driver now supports %q; extend positive coverage", authority)
		}
		if _, err := doSQLConnectionTarget("postgres", "app", map[string]string{"connection_uri": raw}); err == nil {
			t.Fatalf("accepted an authority the linked driver rejects: %q", authority)
		}
	}
	for _, authority := range []string{"[::1]:5432,[::2]:5433"} {
		// net/url's bracketed-IPv6 host validation tightened after Go 1.24:
		// Go 1.24 accepts a bracketed IPv6 multi-host authority and pgx splits it
		// into the same endpoints, while newer toolchains reject the repeated '['
		// before pgx can split it. Keep the contract independent of the stdlib
		// version instead of one toolchain's parser: the Agent accepts exactly the
		// authority the linked driver accepts and journals that driver's endpoints.
		t.Run(authority, func(t *testing.T) {
			raw := "postgres://synthetic:synthetic@" + authority + "/app?sslmode=disable"
			original, driverErr := pgconn.ParseConfig(raw)
			target, targetErr := doSQLConnectionTarget("postgres", "app", map[string]string{"connection_uri": raw})
			if driverErr != nil {
				if targetErr == nil {
					t.Fatalf("accepted an authority the linked driver rejects: %q", authority)
				}
				return
			}
			if targetErr != nil {
				t.Fatalf("rejected an authority the linked driver accepts: %q: %v", authority, targetErr)
			}
			effective, err := pgconn.ParseConfig(target.DSN)
			if err != nil {
				t.Fatal("pgx rejected the rewritten multi-host URI")
			}
			if !reflect.DeepEqual(endpoints(original), endpoints(effective)) ||
				strings.Join(endpoints(effective), ",") != target.Address || effective.Database != target.Database {
				t.Fatalf("driver endpoints changed: original=%v effective=%v journal=%s", endpoints(original), endpoints(effective), target.Address)
			}
			if _, err := doSQLVersionJournalPath(target); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDoSQLExternalTargetPreservesDriverOptions(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(t.TempDir(), "ws"))
	raw := "extsql://u:synthetic@db.test:1234/app?ssl=true&timeout=5"
	target, err := doSQLExternalTarget("extsql", "app", raw)
	if err != nil || target.DSN != raw {
		t.Fatal("external driver options were rejected or rewritten")
	}
	path, err := doSQLVersionJournalPath(target)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := doSQLExternalTarget("extsql", "app", "extsql://v:rotated@db.test:1234/app?ssl=true&timeout=10")
	if err != nil {
		t.Fatal(err)
	}
	rotatedPath, err := doSQLVersionJournalPath(rotated)
	if err != nil || path != rotatedPath {
		t.Fatal("external credential/connection-option rotation changed the declared endpoint journal")
	}
}
