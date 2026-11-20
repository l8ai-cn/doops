package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the Gateway and Agent without requiring a Kubernetes cluster in CI.
// A denied list must preserve the last complete snapshot; reusing a workload name
// with a new UID must never inherit the previous application's binding.
func TestAssetTopologyRuntimeRefreshAndRecreation(t *testing.T) {
	const path = "/v1/assets/topology"
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	for _, name := range []string{"cluster_inventory", "machine_inventory", "inventory_node_links", "business_inventory", "asset_runtime_topology"} {
		b, err := os.ReadFile("../../../docs/migrations/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.db.Exec(string(b)); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	t.Setenv("TOPOLOGY_FIXTURE", root)
	t.Setenv("PATH", root+string(os.PathListSeparator)+os.Getenv("PATH"))
	script := `#!/bin/sh
set -eu
test "$1" = get && test "$3" = --namespace && test "$4" = orders && test "$5" = --output || exit 42
printf 'runtime-begin\n'
case "$2" in
 deployments) printf 'orders\tapi\t%s\t\t\n' "$(cat "$TOPOLOGY_FIXTURE/uid")" ;;
 replicasets|statefulsets|daemonsets|jobs|cronjobs) ;;
 pods) if test -e "$TOPOLOGY_FIXTURE/deny"; then echo 'Error from server (Forbidden): pods is forbidden' >&2; exit 1; fi ;;
 *) exit 42 ;;
esac
printf 'runtime-end\n'
`
	write := func(name, text string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("kubectl", script, 0700)
	write("uid", "old-uid", 0600)
	closeAgent := inventoryTestAgent(t, f, "local")
	if _, err := f.store.db.Exec(`
INSERT INTO cluster_inventory_sources(asset_id,name,cluster,instance,scope,verified_at,verified_by)
 VALUES ('east','East','dev','local','list_pods','2026-11-01','QA');
INSERT INTO cluster_inventory_namespaces(cluster,instance,name,uid,scope,verified_at,verified_by,namespace_observed_at,pods_listed_at)
 VALUES ('dev','local','orders','ns-uid','list_pods','2026-11-01','QA','','2026-11-01');
INSERT INTO inventory_business_systems VALUES ('svc','Orders','Ops',1,1);
INSERT INTO inventory_business_deployments
 VALUES ('svc','dev','local','orders','api','','2026-11-01','QA')`); err != nil {
		t.Fatal(err)
	}
	viewer := f.user(t, "topology-reader", ScopeGrant{Cluster: "dev", Instance: "local"})
	key := runtimeKey{AssetID: "east", Cluster: "dev", Instance: "local", Namespace: "orders"}
	post := func(who CreatedToken, r runtimeRequest, status int) string {
		t.Helper()
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return string(f.http(t, who, "POST", path, string(b), status))
	}
	read := func() runtimeScope {
		t.Helper()
		var g assetTopology
		if err := json.Unmarshal(f.http(t, viewer, "GET", path, "", 200), &g); err != nil || len(g.Runtime) != 1 {
			t.Fatalf("runtime graph: %v, %v", g.Runtime, err)
		}
		return g.Runtime[0]
	}
	collect := runtimeRequest{runtimeKey: key, Action: "collect"}
	post(viewer, collect, 200)
	first := read()
	if len(first.Workloads) != 1 || first.Workloads[0].UID != "old-uid" || first.Revision == "" {
		t.Fatal(first)
	}
	bind := runtimeRequest{runtimeKey: key, Action: "bind", Application: "api", Workload: "old-uid", Revision: first.Revision}
	post(viewer, bind, 403)
	post(f.admin, bind, 200)
	write("deny", "", 0600)
	if got := post(viewer, collect, 403); !strings.Contains(got, "runtime_kubernetes_denied") {
		t.Fatal(got)
	}
	preserved := read()
	if preserved.Revision != first.Revision || preserved.At != first.At || len(preserved.Bindings) != 1 {
		t.Fatal(preserved)
	}
	if err := os.Remove(filepath.Join(root, "deny")); err != nil {
		t.Fatal(err)
	}
	write("uid", "new-uid", 0600)
	post(viewer, collect, 200)
	updated := read()
	if updated.Revision == first.Revision || len(updated.Bindings) != 0 || len(updated.Workloads) != 1 || updated.Workloads[0].UID != "new-uid" {
		t.Fatal(updated)
	}
	post(f.admin, bind, 409)
	bind.Revision = updated.Revision
	post(f.admin, bind, 409)
	closeAgent()
	post(viewer, collect, 503)
	offline := read()
	if offline.Status != "offline" || offline.Revision != updated.Revision {
		t.Fatal(offline)
	}
}
