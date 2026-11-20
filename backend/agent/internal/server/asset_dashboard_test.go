package server

import (
	"encoding/json"
	"os"
	"testing"
)

func TestAssetDashboardUsesVisibleClusterIdentity(t *testing.T) {
	f := newNodeLinkFixture(t)
	migration, err := os.ReadFile("../../../docs/migrations/business_inventory.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.db.Exec(string(migration)); err != nil {
		t.Fatal(err)
	}
	// Different Agent routes lead to the same cluster, namespace and business system.
	_, err = f.store.db.Exec(`INSERT INTO cluster_inventory_namespaces SELECT cluster,instance,'shared','','','','2026-11-19T02:00:00Z','tester','list_pods' FROM cluster_inventory_sources;
 INSERT INTO inventory_node_links VALUES ('agents','one','node-a','vm-a');
 INSERT INTO inventory_business_systems VALUES ('shared','Shared','ops',1,1),('retired','Retired','ops',0,1);
 INSERT INTO inventory_business_deployments SELECT 'shared',cluster,instance,'shared','workload','','2026-11-19T02:00:00Z','tester' FROM cluster_inventory_sources;`)
	if err != nil {
		t.Fatal(err)
	}
	read := func(token CreatedToken) assetDashboard {
		t.Helper()
		raw := f.http(t, token, "GET", "/v1/assets/dashboard", "", 200)
		var d assetDashboard
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	admin := read(f.admin)
	if len(admin.Clusters) != 1 || admin.Machines != 2 || admin.Systems != 1 || len(admin.Unassigned) != 1 {
		t.Fatalf("incorrect platform counts: %+v", admin)
	}
	c := admin.Clusters[0]
	if c.Sources != 2 || c.Namespaces != 1 || c.Machines != 1 || c.Systems != 1 {
		t.Fatalf("incorrect cluster counts: %+v", c)
	}
	viewer := f.user(t, "dashboard-reader", ScopeGrant{Cluster: "agents", Instance: "one"})
	d := read(viewer)
	if len(d.Clusters) != 1 || d.Clusters[0].Sources != 1 || d.Clusters[0].Machines != 0 || d.Systems != 1 || len(d.Unassigned) != 0 {
		t.Fatalf("unexpected restricted counts: %+v", d)
	}
	u, err := f.store.VerifyUserToken(viewer.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.db.Exec(`DELETE FROM grants WHERE user_id=?`, u.UserID); err != nil {
		t.Fatal(err)
	}
	d = read(viewer)
	if len(d.Clusters) != 0 || d.Systems != 0 || d.Machines != 0 {
		t.Fatalf("revoked scope returned assets: %+v", d)
	}
}
