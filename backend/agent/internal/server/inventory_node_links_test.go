package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const nodeLinksPath = "/v1/assets/node-links"

func newNodeLinkFixture(t *testing.T) *executionLifecycleFixture {
	t.Helper()
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	f.http(t, f.admin, "GET", nodeLinksPath, "", 503)
	for _, name := range []string{"machine_inventory.sql", "cluster_inventory.sql", "inventory_node_links.sql"} {
		schema, err := os.ReadFile(filepath.Join("../../../docs/migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.db.Exec(string(schema)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.Exec(`DELETE FROM grants;
 INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES ('vm-a','Same name','vm','hosts','a'),('vm-b','Same name','vm','','');
 INSERT INTO cluster_inventory_sources(asset_id,name,cluster,instance,scope,verified_at,verified_by) VALUES ('k1','Cluster','agents','one','list_pods','fixture','fixture'),('k1','Cluster','agents','two','list_pods','fixture','fixture');`); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestInventoryNodeLinksBothGrantsAndExactSource(t *testing.T) {
	f := newNodeLinkFixture(t)
	req := `{"action":"bind","asset_id":"k1","cluster":"agents","instance":"one","machine_id":"vm-a","node_name":"worker-0.test"}`
	post := func(token CreatedToken, body string, status int) {
		t.Helper()
		f.http(t, token, "POST", nodeLinksPath, body, status)
	}
	read := func(token CreatedToken, want ...nodeLinkKey) {
		t.Helper()
		data := f.http(t, token, "GET", nodeLinksPath, "", 200)
		var list struct{ Links []inventoryNodeLink }
		if err := json.Unmarshal(data, &list); err != nil || list.Links == nil || len(list.Links) != len(want) {
			t.Fatalf("unexpected list %s", data)
		}
		// Assert the entire wire shape so empty responses cannot leak names, IDs or totals.
		links := make([]inventoryNodeLink, len(want))
		for i, k := range want {
			links[i] = inventoryNodeLink{k, "manual"}
		}
		expected, _ := json.Marshal(map[string]any{"links": links})
		if strings.TrimSpace(string(data)) != string(expected) {
			t.Fatalf("got %s want %s", data, expected)
		}
	}
	f.http(t, CreatedToken{}, "GET", nodeLinksPath, "", 401)
	f.http(t, f.admin, "DELETE", nodeLinksPath, "", 405)
	reader := f.user(t, "reader", ScopeGrant{Cluster: "hosts", Instance: "*"})
	sourceOnly := f.user(t, "source-only", ScopeGrant{Cluster: "*", Instance: "one"})
	post(reader, req, 403)
	post(f.admin, strings.Replace(req, "worker-0.test", "Worker-0.test", 1), 400)
	post(f.admin, strings.Replace(req, "worker-0.test", "worker-*", 1), 400)
	post(f.admin, strings.Replace(req, "k1", "other-asset", 1), 404)
	post(f.admin, strings.Replace(req, "vm-a", "absent", 1), 404)
	post(f.admin, strings.Replace(req, "one", "unimported", 1), 404)
	post(f.admin, strings.Replace(req, "{", `{"source":"verified",`, 1), 400)
	post(f.admin, req, 200)
	post(f.admin, req, 409)
	post(f.admin, strings.Replace(req, "vm-a", "vm-b", 1), 409)
	reverse := strings.Replace(req, `"bind"`, `"unbind"`, 1)
	post(f.admin, strings.Replace(reverse, "vm-a", "vm-b", 1), 404)
	post(f.admin, strings.NewReplacer(`"one"`, `"two"`, `"vm-a"`, `"vm-b"`).Replace(req), 200)
	a := nodeLinkKey{"k1", "agents", "one", "worker-0.test", "vm-a"}
	b := nodeLinkKey{"k1", "agents", "two", "worker-0.test", "vm-b"}
	read(f.admin, a, b)
	read(reader)
	read(sourceOnly)
	user, err := f.store.VerifyUserToken(reader.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.GrantUser(user.UserID, ScopeGrant{Cluster: "*", Instance: "one"}); err != nil {
		t.Fatal(err)
	}
	read(reader, a)
	if _, err := f.store.db.Exec("DELETE FROM grants WHERE user_id=? AND cluster='hosts'", user.UserID); err != nil {
		t.Fatal(err)
	}
	read(reader)
	if err := f.store.GrantUser(user.UserID, ScopeGrant{Cluster: "*", Instance: "*"}); err != nil {
		t.Fatal(err)
	}
	read(reader, a) // */* does not expose the unbound machine associated with source two.
	post(reader, reverse, 403)
	post(f.admin, reverse, 200)
	read(f.admin, b)
	read(reader)
	var n int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM grants WHERE user_id=(SELECT user_id FROM tokens WHERE id=?)`, f.admin.ID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("admin acquired grants: %d, %v", n, err)
	}
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='inventory:node:unbind' AND status='success' AND cluster='agents' AND instance='one' AND command_summary LIKE '%machine_id=vm-a node_name=worker-0.test source=manual%'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("missing unbind audit: %d, %v", n, err)
	}
}

func TestInventoryNodeLinksAtomicAuditAndConstraints(t *testing.T) {
	f := newNodeLinkFixture(t)
	bind := `{"action":"bind","asset_id":"k1","cluster":"agents","instance":"one","machine_id":"vm-a","node_name":"node"}`
	unbind := strings.Replace(bind, `"bind"`, `"unbind"`, 1)
	exec := func(q string) {
		t.Helper()
		if _, err := f.store.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	count := func(want int) {
		t.Helper()
		var n int
		if err := f.store.db.QueryRow(`SELECT count(*) FROM inventory_node_links`).Scan(&n); err != nil || n != want {
			t.Fatalf("links=%d want=%d err=%v", n, want, err)
		}
	}
	for _, fault := range []string{"BEFORE INSERT ON audit_events", "BEFORE UPDATE OF status ON audit_events WHEN NEW.status='success'"} {
		exec("CREATE TRIGGER broken_audit " + fault + " BEGIN SELECT RAISE(ABORT,'synthetic-audit-detail'); END")
		data := f.http(t, f.admin, "POST", nodeLinksPath, bind, 503)
		if strings.Contains(string(data), "synthetic-audit-detail") {
			t.Fatal("internal audit error exposed")
		}
		count(0)
		exec("DROP TRIGGER broken_audit")
	}
	f.http(t, f.admin, "POST", nodeLinksPath, bind, 200)
	exec("CREATE TRIGGER broken_audit BEFORE UPDATE OF status ON audit_events WHEN NEW.status='success' BEGIN SELECT RAISE(ABORT,'synthetic'); END")
	f.http(t, f.admin, "POST", nodeLinksPath, unbind, 503)
	count(1)
	exec("DROP TRIGGER broken_audit")
	for _, query := range []string{
		`DELETE FROM inventory_machines WHERE asset_id='vm-a'`, `UPDATE inventory_machines SET asset_id='other' WHERE asset_id='vm-a'`,
		`DELETE FROM cluster_inventory_sources WHERE instance='one'`, `UPDATE cluster_inventory_sources SET instance='other' WHERE instance='one'`,
		`UPDATE cluster_inventory_sources SET asset_id='new' WHERE instance='one'`, `UPDATE inventory_node_links SET machine_id='missing'`,
		`INSERT INTO inventory_node_links VALUES('agents','one','new','missing')`, `INSERT INTO inventory_node_links VALUES('agents','absent','new','vm-a')`, `INSERT INTO inventory_node_links VALUES('agents','one','node','vm-b')`,
	} {
		if _, err := f.store.db.Exec(query); err == nil {
			t.Fatalf("invalid change succeeded: %s", query)
		}
	}
	count(1)
	f.http(t, f.admin, "POST", nodeLinksPath, unbind, 200)
	count(0)
	// An ignored INSERT can return a previous LastInsertId from SQLite. A
	// prior successful receipt must never authorize an unaudited mutation.
	exec("CREATE TRIGGER broken_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(IGNORE); END")
	f.http(t, f.admin, "POST", nodeLinksPath, bind, 503)
	count(0)
	exec("DROP TRIGGER broken_audit")
	// Concurrent binds on separate Gateway connections contend for the same source and name.
	other, err := OpenGatewayStore(f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	auth, err := other.VerifyUserToken(f.admin.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for i, store := range []*GatewayStore{f.store, other} {
		machine := []string{"vm-a", "vm-b"}[i]
		go func() {
			results <- store.changeNodeLink(t.Context(), auth, nodeLinkRequest{"bind", nodeLinkKey{"k1", "agents", "one", "node", machine}}, true)
		}()
	}
	success, conflict := 0, 0
	for range 2 {
		err := <-results
		if err == nil {
			success++
		} else if nodeLinkProblem(err).status == http.StatusConflict {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("concurrent binds: success=%d conflict=%d", success, conflict)
	}
	count(1)
}
