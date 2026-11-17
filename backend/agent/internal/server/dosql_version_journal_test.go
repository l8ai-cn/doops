package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func newDoSQLJournalPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "versions", "postgres__orders.jsonl")
}

func TestDoSQLVersionJournalAssignsOrderedVersionsAndPairsOutcomes(t *testing.T) {
	path := newDoSQLJournalPath(t)
	one, err := beginDoSQLVersion(path, "dbop_a", "cred-a", "sha256:aa", []string{"CREATE TABLE t (id int)"})
	if err != nil || one != 1 {
		t.Fatalf("first version = %d, %v", one, err)
	}
	if err := endDoSQLVersion(path, one, "dbop_a", "cred-a", "sha256:aa",
		doSQLDirectResult{Status: "verified", Committed: true}, nil); err != nil {
		t.Fatal(err)
	}
	two, err := beginDoSQLVersion(path, "dbop_b", "cred-a", "sha256:bb", []string{"INSERT INTO t VALUES (1)"})
	if err != nil || two != 2 {
		t.Fatalf("second version = %d, %v", two, err)
	}
	if err := endDoSQLVersion(path, two, "dbop_b", "cred-a", "sha256:bb",
		doSQLDirectResult{Status: "failed"}, errors.New("dosql_statement_failed: statement 1: boom")); err != nil {
		t.Fatal(err)
	}

	summaries, err := listDoSQLVersions(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 ||
		summaries[0].Version != 1 || summaries[0].Status != "verified" || !summaries[0].Committed ||
		summaries[1].Version != 2 || summaries[1].Status != "failed" || summaries[1].Committed {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}
	if unfinished := doSQLVersionUnfinished(summaries); len(unfinished) != 0 {
		t.Fatalf("finished versions reported unfinished: %v", unfinished)
	}
}

func TestDoSQLVersionJournalReportsABeginWithoutAnEndAsUnfinished(t *testing.T) {
	path := newDoSQLJournalPath(t)
	one, err := beginDoSQLVersion(path, "dbop_a", "cred", "sha256:aa", []string{"DROP TABLE t"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulates a crash after the reservation but before the outcome was written.
	summaries, err := listDoSQLVersions(path)
	if err != nil {
		t.Fatal(err)
	}
	unfinished := doSQLVersionUnfinished(summaries)
	if len(unfinished) != 1 || unfinished[0] != one {
		t.Fatalf("unfinished = %v, want [%d]", unfinished, one)
	}
	// The next apply must still get a fresh number and not reuse the unfinished one.
	next, err := beginDoSQLVersion(path, "dbop_b", "cred", "sha256:bb", []string{"SELECT 1"})
	if err != nil || next != one+1 {
		t.Fatalf("version after an unfinished one = %d, %v", next, err)
	}
}

func TestDoSQLVersionJournalNormalizesInvalidUTF8BeforeHashing(t *testing.T) {
	path := newDoSQLJournalPath(t)
	invalidUTF8 := string([]byte{0xff, 0xfe})
	statement := "SELECT '" + invalidUTF8 + "'"
	one, err := beginDoSQLVersion(path, "dbop_"+invalidUTF8, "cred", "sha256:aa", []string{statement})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("\uFFFD")) {
		t.Fatalf("new entry was not stored as valid UTF-8: %q", raw)
	}
	if bytes.Contains(raw, []byte(invalidUTF8)) {
		t.Fatalf("new entry kept invalid UTF-8 bytes on disk: %q", raw)
	}
	// The write side must hash the same bytes that the read side verifies.
	var written doSQLVersionEntry
	if err := json.Unmarshal(bytes.TrimSpace(raw), &written); err != nil {
		t.Fatal(err)
	}
	if written.Hash != doSQLEntryHash(written) {
		t.Fatal("stored entry does not match its hash after a parse and reserialize")
	}
	if _, err := listDoSQLVersions(path); err != nil {
		t.Fatalf("journal with invalid UTF-8 input did not verify: %v", err)
	}
	if err := endDoSQLVersion(path, one, "dbop_"+invalidUTF8, "cred", "sha256:aa",
		doSQLDirectResult{Status: "verified"}, nil); err != nil {
		t.Fatal(err)
	}
	two, err := beginDoSQLVersion(path, "dbop_next", "cred", "sha256:bb", []string{"SELECT 1"})
	if err != nil || two != one+1 {
		t.Fatalf("version after invalid UTF-8 entry = %d, %v", two, err)
	}
	summaries, err := listDoSQLVersions(path)
	if err != nil {
		t.Fatalf("journal did not verify after appending: %v", err)
	}
	if len(summaries) != 2 || summaries[0].Status != "verified" {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}
}

// legacyDoSQLEntryHash reproduces the old writer exactly: it hashes the raw
// json.Marshal output, which escapes invalid UTF-8 bytes as \ufffd.
func legacyDoSQLEntryHash(entry doSQLVersionEntry) string {
	entry.Hash = ""
	encoded, _ := json.Marshal(entry)
	sum := sha256.Sum256(append([]byte(entry.PrevHash+"\n"), encoded...))
	return hex.EncodeToString(sum[:])
}

func TestDoSQLVersionJournalVerifiesHistoricalEscapedReplacementEntry(t *testing.T) {
	path := newDoSQLJournalPath(t)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	entry := doSQLVersionEntry{
		Version: 1, Event: doSQLJournalBegin, OperationID: "dbop_legacy", Credential: "cred",
		Fingerprint: "sha256:aa", Statements: []string{"SELECT '" + string([]byte{0xff, 0xfe}) + "'"},
		RecordedAt: "2026-01-01T00:00:00Z",
	}
	entry.Hash = legacyDoSQLEntryHash(entry)
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`\ufffd`)) {
		t.Fatalf("test entry is not in the historical escaped form: %s", encoded)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	summaries, err := listDoSQLVersions(path)
	if err != nil {
		t.Fatalf("historical entry did not verify from its original bytes: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Status != "unfinished" {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}
	// The historical entry is untouched and a new entry must still link to it.
	next, err := beginDoSQLVersion(path, "dbop_next", "cred", "sha256:bb", []string{"SELECT 2"})
	if err != nil || next != 2 {
		t.Fatalf("version after historical entry = %d, %v", next, err)
	}
	if _, err := listDoSQLVersions(path); err != nil {
		t.Fatalf("chain broke after appending to a historical journal: %v", err)
	}
}

func TestDoSQLVersionJournalTreatsBeginWithoutEndAsUnfinishedNotCorrupt(t *testing.T) {
	path := newDoSQLJournalPath(t)
	one, err := beginDoSQLVersion(path, "dbop_a", "cred", "sha256:aa", []string{"DROP TABLE t"})
	if err != nil {
		t.Fatal(err)
	}
	summaries, err := listDoSQLVersions(path)
	if err != nil {
		t.Fatalf("an unfinished begin was reported as corruption: %v", err)
	}
	if len(summaries) != 1 || summaries[0].Version != one || summaries[0].Status != "unfinished" {
		t.Fatalf("unexpected summaries: %#v", summaries)
	}
	next, err := beginDoSQLVersion(path, "dbop_b", "cred", "sha256:bb", []string{"SELECT 1"})
	if err != nil || next != one+1 {
		t.Fatalf("version after unfinished begin = %d, %v", next, err)
	}
	if _, err := listDoSQLVersions(path); err != nil {
		t.Fatalf("second apply on an unfinished journal was blocked: %v", err)
	}
}

func TestDoSQLVersionJournalConcurrentAppliesGetDistinctVersions(t *testing.T) {
	path := newDoSQLJournalPath(t)
	const workers = 24
	versions := make([]int64, workers)
	var wg sync.WaitGroup
	for index := 0; index < workers; index++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			version, err := beginDoSQLVersion(path, "dbop_"+randomHex(4), "cred", "sha256:aa", []string{"UPDATE t SET a = 1"})
			if err != nil {
				t.Errorf("begin: %v", err)
				return
			}
			versions[index] = version
		}()
	}
	wg.Wait()
	seen := map[int64]bool{}
	for _, version := range versions {
		if version < 1 || version > workers || seen[version] {
			t.Fatalf("versions are not a distinct 1..%d range: %v", workers, versions)
		}
		seen[version] = true
	}
	// A broken chain would make this fail: every concurrent append must have linked.
	if _, err := listDoSQLVersions(path); err != nil {
		t.Fatalf("hash chain broke under concurrency: %v", err)
	}
}

func TestDoSQLVersionJournalDetectsEditedAndDeletedEntries(t *testing.T) {
	build := func(t *testing.T) (string, []string) {
		path := newDoSQLJournalPath(t)
		for _, statement := range []string{"CREATE TABLE a (id int)", "INSERT INTO a VALUES (1)"} {
			version, err := beginDoSQLVersion(path, "dbop_"+randomHex(3), "cred", "sha256:aa", []string{statement})
			if err != nil {
				t.Fatal(err)
			}
			if err := endDoSQLVersion(path, version, "dbop_x", "cred", "sha256:aa",
				doSQLDirectResult{Status: "verified", Committed: true}, nil); err != nil {
				t.Fatal(err)
			}
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return path, strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}
	write := func(t *testing.T, path string, lines []string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("edited statement", func(t *testing.T) {
		path, lines := build(t)
		lines[0] = strings.Replace(lines[0], "CREATE TABLE a", "DROP TABLE a", 1)
		write(t, path, lines)
		if _, err := listDoSQLVersions(path); err == nil || !strings.Contains(err.Error(), "dosql_version_journal_corrupt") {
			t.Fatalf("edit was not detected: %v", err)
		}
	})
	t.Run("deleted middle entry", func(t *testing.T) {
		path, lines := build(t)
		write(t, path, append(append([]string{}, lines[:1]...), lines[2:]...))
		if _, err := listDoSQLVersions(path); err == nil || !strings.Contains(err.Error(), "dosql_version_journal_corrupt") {
			t.Fatalf("deletion was not detected: %v", err)
		}
	})
	t.Run("reordered entries", func(t *testing.T) {
		path, lines := build(t)
		lines[1], lines[2] = lines[2], lines[1]
		write(t, path, lines)
		if _, err := listDoSQLVersions(path); err == nil {
			t.Fatal("reordering was not detected")
		}
	})
	t.Run("a corrupt journal also blocks the next apply", func(t *testing.T) {
		path, lines := build(t)
		lines[0] = strings.Replace(lines[0], "CREATE TABLE a", "DROP TABLE a", 1)
		write(t, path, lines)
		if _, err := beginDoSQLVersion(path, "dbop_new", "cred", "sha256:aa", []string{"SELECT 1"}); err == nil {
			t.Fatal("apply was allowed to start on a tampered journal")
		}
	})
	t.Run("a lost end record shows up as unfinished", func(t *testing.T) {
		path, lines := build(t)
		write(t, path, lines[:len(lines)-1])
		summaries, err := listDoSQLVersions(path)
		if err != nil {
			t.Fatal(err)
		}
		if unfinished := doSQLVersionUnfinished(summaries); len(unfinished) != 1 || unfinished[0] != 2 {
			t.Fatalf("unfinished = %v, want [2]", unfinished)
		}
	})
}

func TestDoSQLVersionJournalFailsClosedWhenItCannotBeWritten(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "versions")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := beginDoSQLVersion(filepath.Join(blocker, "x.jsonl"), "dbop_a", "cred", "sha256:aa", []string{"SELECT 1"}); err == nil ||
		!strings.Contains(err.Error(), "dosql_version_journal_unavailable") {
		t.Fatalf("unwritable journal error = %v", err)
	}
}

func TestDoSQLVersionJournalNeverStoresSecretsOrUnboundedStatements(t *testing.T) {
	path := newDoSQLJournalPath(t)
	canary := "postgres://app:synthetic-journal-canary@db.invalid/app"
	long := "INSERT INTO t VALUES ('" + strings.Repeat("x", doSQLJournalStmtLimit*2) + "')"
	version, err := beginDoSQLVersion(path, "dbop_a", "cred", "sha256:aa", []string{"SELECT '" + canary + "'", long})
	if err != nil {
		t.Fatal(err)
	}
	if err := endDoSQLVersion(path, version, "dbop_a", "cred", "sha256:aa", doSQLDirectResult{Status: "failed"},
		errors.New("dosql_connect_failed: dial "+canary)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-journal-canary") {
		t.Fatalf("journal stored a credential-shaped value: %s", raw)
	}
	if len(raw) > 4*doSQLJournalStmtLimit {
		t.Fatalf("journal stored an unbounded statement: %d bytes", len(raw))
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal permissions = %v, %v", info, err)
	}
}

func TestDoSQLVersionJournalPathIsPerEngineAndDatabase(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(t.TempDir(), "ws"))
	path := func(engine, database string) string {
		t.Helper()
		path, err := doSQLVersionJournalPath(doSQLTarget{Engine: engine, Address: "db.test:1234", Database: database})
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	pg, my := path("postgres", "testdb"), path("mysql", "testdb")
	if pg == my {
		t.Fatal("two engines with the same database name share one version history")
	}
	if strings.Contains(filepath.Base(path("postgres", "../../etc/passwd")), "/") {
		t.Fatal("database name escaped the journal directory")
	}
	if filepath.Dir(path("postgres", "../../etc/passwd")) != filepath.Dir(pg) {
		t.Fatal("a hostile database name changed the journal directory")
	}
	if path("postgres", "a/b") == path("postgres", "a_b") {
		t.Fatal("database names that previously sanitized identically share a journal")
	}
}

func TestDoSQLVersionIdentityNormalizesEndpointsAndIgnoresCredentials(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(t.TempDir(), "ws"))
	for _, tc := range []struct{ engine, scheme, port string }{
		{"postgres", "postgres", "5432"}, {"mysql", "mysql", "3306"},
		{"oracle", "oracle", "1521"}, {"mssql", "sqlserver", "1433"},
		{"mongodb", "mongodb", "27017"},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			first, err := doSQLConnectionTarget(tc.engine, "app", map[string]string{
				"connection_uri": tc.scheme + "://old:synthetic-old@DB.test/app",
			})
			if err != nil {
				t.Fatal(err)
			}
			rotated, err := doSQLConnectionTarget(tc.engine, "app", map[string]string{
				"connection_uri": tc.scheme + "://new:synthetic-new@db.test:" + tc.port + "/app",
			})
			if err != nil {
				t.Fatal(err)
			}
			a, err := doSQLVersionJournalPath(first)
			if err != nil {
				t.Fatal(err)
			}
			b, err := doSQLVersionJournalPath(rotated)
			if err != nil || a != b {
				t.Fatalf("credential rotation/default port changed identity: %q %q %v", a, b, err)
			}
			if strings.Contains(a, "synthetic") || strings.Contains(a, "DB.test") || strings.Contains(a, "db.test") {
				t.Fatal("journal path contains connection material")
			}
		})
	}
	a, err := doSQLConnectionTarget("mongodb", "app", map[string]string{"connection_uri": "mongodb://u:p@db-a.test,db-b.test/app"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := doSQLConnectionTarget("mongodb", "app", map[string]string{"connection_uri": "mongodb://v:q@db-b.test:27017,db-a.test:27017/app"})
	if err != nil {
		t.Fatal(err)
	}
	pathA, err := doSQLVersionJournalPath(a)
	if err != nil {
		t.Fatal(err)
	}
	pathB, err := doSQLVersionJournalPath(b)
	if err != nil || pathA != pathB {
		t.Fatalf("MongoDB seed order/default ports changed identity: %q %q %v", pathA, pathB, err)
	}
	ext, err := doSQLExternalTarget("extsql", "app", "extsql://u:p@db.test/app")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := doSQLVersionJournalPath(ext); err == nil || !strings.Contains(err.Error(), "explicit numeric port") {
		t.Fatalf("external engine guessed a default port: %v", err)
	}
	ext.Address = "DB.test:05432"
	first, err := doSQLVersionJournalPath(ext)
	if err != nil {
		t.Fatal(err)
	}
	ext.Address, ext.DSN = "db.test:5432", "extsql://new:synthetic-new@db.test:5432/app"
	second, err := doSQLVersionJournalPath(ext)
	if err != nil || first != second {
		t.Fatalf("explicit external endpoint changed identity: %q %q %v", first, second, err)
	}
}

func TestDoSQLVersionJournalsIsolateEndpointsAndPreserveRotationHistory(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(t.TempDir(), "ws"))
	a := doSQLTarget{Engine: "postgres", Database: "app", Address: "db-a.test:5432"}
	b := doSQLTarget{Engine: "postgres", Database: "app", Address: "db-b.test:5432"}
	pathA, err := doSQLVersionJournalPath(a)
	if err != nil {
		t.Fatal(err)
	}
	pathB, err := doSQLVersionJournalPath(b)
	if err != nil || pathA == pathB {
		t.Fatalf("distinct endpoints collided: %v", err)
	}
	for _, path := range []string{pathA, pathB} {
		if version, err := beginDoSQLVersion(path, "first", "credential-v1", "fp", []string{"UPDATE t SET a=1"}); err != nil || version != 1 {
			t.Fatalf("first version = %d, %v", version, err)
		}
	}
	a.DSN = "postgres://new:synthetic-rotation@db-a.test:5432/app"
	rotated, err := doSQLVersionJournalPath(a)
	if err != nil || rotated != pathA {
		t.Fatalf("rotation selected a new journal: %v", err)
	}
	if version, err := beginDoSQLVersion(rotated, "rotated", "credential-v2", "fp", []string{"UPDATE t SET a=2"}); err != nil || version != 2 {
		t.Fatalf("rotation version = %d, %v", version, err)
	}
	if err := os.WriteFile(pathA, []byte("corrupt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := beginDoSQLVersion(pathA, "blocked", "credential-v2", "fp", nil); err == nil {
		t.Fatal("corrupt history was accepted")
	}
	if version, err := beginDoSQLVersion(pathB, "independent", "credential-b", "fp", nil); err != nil || version != 2 {
		t.Fatalf("another endpoint's corrupt journal blocked B: %d, %v", version, err)
	}
}

func TestDoSQLVersionJournalRequiresExplicitLegacyMigration(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(root, "ws"))
	target := doSQLTarget{Engine: "postgres", Database: "app", Address: "db-a.test:5432"}
	path, err := doSQLVersionJournalPath(target)
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(root, ".doops-dosql-versions", "postgres__app.jsonl")
	if _, err := beginDoSQLVersion(legacy, "legacy-op", "old-binding", "fp", nil); err != nil {
		t.Fatal(err)
	}
	for _, address := range []string{"db-a.test:5432", "db-b.test:5432"} {
		target.Address = address
		if got, err := doSQLVersionJournalPath(target); got != "" || err == nil || !strings.Contains(err.Error(), "dosql_version_journal_migration_required") {
			t.Fatalf("legacy journal was silently adopted for %s: %q %v", address, got, err)
		}
	}
	// Explicit operator migration after verifying the entire old file's owner.
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(legacy, path); err != nil {
		t.Fatal(err)
	}
	target.Address = "db-a.test:5432"
	if got, err := doSQLVersionJournalPath(target); err != nil || got != path {
		t.Fatalf("explicit migration rejected: %q %v", got, err)
	}
	if version, err := beginDoSQLVersion(path, "new-op", "rotated-binding", "fp", nil); err != nil || version != 2 {
		t.Fatalf("migration lost historical numbering: %d, %v", version, err)
	}
}
