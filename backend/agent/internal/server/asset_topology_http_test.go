package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestAssetTopologyHTTPVisibleEvidenceAndRevocation(t *testing.T) {
	const path = "/v1/assets/topology"
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	f.http(t, CreatedToken{}, "GET", path, "", 401)
	f.http(t, f.admin, "DELETE", path, "", 405)
	f.http(t, f.admin, "GET", path, "", 503)
	for _, name := range []string{"cluster_inventory", "machine_inventory", "inventory_node_links", "business_inventory", "asset_runtime_topology"} {
		migration, err := os.ReadFile("../../../docs/migrations/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.db.Exec(string(migration)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.Exec(`INSERT INTO cluster_inventory_sources(asset_id,name,cluster,instance,scope,verified_at,verified_by) VALUES
 ('east','East','dev','agent-a','list_pods','2026-11-01T00:00:00Z','tester'),('east','East','dev','agent-b','list_pods','2026-11-02T00:00:00Z','tester'),('west','West','dev','west-only','list_pods','2026-11-03T00:00:00Z','tester');
 INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES ('hidden-host','restricted host','physical','dev','agent-b'),('standalone','independent machine','physical','',''),('other-machine','West','physical','dev','west-only');
 INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance,host_asset_id) VALUES ('vm','authorized VM','vm','dev','agent-a','hidden-host');
 INSERT INTO inventory_node_links VALUES ('dev','agent-a','linked-node','vm'),('dev','agent-a','node-hidden-endpoint','hidden-host'),('dev','agent-b','node-hidden-source','vm');
 INSERT INTO inventory_business_systems VALUES ('sys','Orders','Ops',1,1),('sys-b','Billing','Ops',1,1),('sys-west','West App','Ops',1,1);
 INSERT INTO inventory_business_deployments VALUES
 ('sys','dev','agent-a','orders','order-api','shared-db','2026-11-01T00:00:00Z','tester'),('sys','dev','agent-b','orders','order-api','shared-db','2026-11-02T00:00:00Z','tester'),
 ('sys-b','dev','agent-b','billing','billing-api','shared-db','2026-11-02T00:00:00Z','tester'),('sys-west','dev','west-only','orders','order-api','shared-db','2026-11-03T00:00:00Z','tester'),('sys','dev','agent-a','orders','unverified-app','unverified-db','','tester');`); err != nil {
		t.Fatal(err)
	}
	read := func(token CreatedToken, query string) (assetTopology, string) {
		t.Helper()
		wire := f.http(t, token, "GET", path+query, "", 200)
		var g assetTopology
		if err := json.Unmarshal(wire, &g); err != nil || g.Nodes == nil || g.Edges == nil {
			t.Fatalf("invalid graph: %s, %v", wire, err)
		}
		ids := map[string]bool{}
		for _, n := range g.Nodes {
			if ids[n.ID] {
				t.Fatal("duplicate", n.ID)
			}
			ids[n.ID] = true
		}
		for _, e := range g.Edges {
			if !ids[e.From] || !ids[e.To] || len(e.Evidence) == 0 {
				t.Fatal("dangling or unattributed edge", e)
			}
		}
		return g, string(wire)
	}
	dependencies := func(g assetTopology, scope string) map[string][]topologyEvidence {
		t.Helper()
		got := map[string][]topologyEvidence{}
		for _, e := range g.Edges {
			if e.Kind == "uses" && e.To == topologyID("database", "cluster", scope, "shared-db") {
				got[e.From] = e.Evidence
			}
		}
		return got
	}
	assertAbsent := func(body string, values ...string) {
		t.Helper()
		for _, v := range values {
			if strings.Contains(body, v) {
				t.Fatalf("response exposed %q", v)
			}
		}
	}
	platform, wire := read(f.admin, "")
	if !strings.Contains(wire, "standalone") || len(dependencies(platform, "west")) != 1 {
		t.Fatal("admin missing isolated machine or independent cluster database")
	}
	assertAbsent(wire, "unverified-app", "unverified-db")
	for _, q := range []string{"?cluster_id=", "?cluster_id=east&cluster_id=west", "?scope=east", "?cluster_id=%ZZ"} {
		f.http(t, f.admin, "GET", path+q, "", 400)
	}
	operator := f.user(t, "topology-reader", ScopeGrant{Cluster: "dev", Instance: "agent-a"})
	auth, err := f.store.VerifyUserToken(operator.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.GrantUser(auth.UserID, ScopeGrant{Cluster: "dev", Instance: "agent-b"}); err != nil {
		t.Fatal(err)
	}
	first, wire := read(operator, "?cluster_id=east")
	before := dependencies(first, "east")
	order := topologyID("application", "cluster", "east", "orders", "order-api")
	if len(before) != 2 || len(before[order]) != 2 || !strings.Contains(wire, "hosted_on") || !strings.Contains(wire, "maps_to") {
		t.Fatal("expected both authorized dependencies, providers and physical relations", wire)
	}
	assertAbsent(wire, "west-only", "standalone", "unverified")
	unknown := f.http(t, operator, "GET", path+"?cluster_id=unknown", "", 404)
	private := f.http(t, operator, "GET", path+"?cluster_id=west", "", 404)
	if string(private) != string(unknown) {
		t.Fatal("cluster existence oracle")
	}
	if _, err = f.store.db.Exec(`DELETE FROM grants WHERE user_id=? AND instance='agent-b'`, auth.UserID); err != nil {
		t.Fatal(err)
	}
	second, wire := read(operator, "?cluster_id=east")
	after := dependencies(second, "east")
	if len(after) != 1 || len(after[order]) != 1 || after[order][0].Instance != "agent-a" {
		t.Fatal("unexpected dependencies after revocation", after)
	}
	assertAbsent(wire, "agent-b", "billing", "sys-b", "hidden-host", "node-hidden-endpoint", "node-hidden-source", "hosted_on", "2026-11-02", "standalone")
	if !strings.Contains(wire, "linked-node") {
		t.Fatal("authorized Node mapping removed")
	}
	if _, err = f.store.db.Exec(`DELETE FROM grants WHERE user_id=?`, auth.UserID); err != nil {
		t.Fatal(err)
	}
	empty, _ := read(operator, "")
	if len(empty.Nodes) != 0 || len(empty.Edges) != 0 {
		t.Fatal("nonempty graph after all grants revoked")
	}
	f.http(t, operator, "GET", path+"?cluster_id=east", "", 404)
}
