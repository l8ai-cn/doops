package server

import (
	"encoding/csv"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

func installCoverageRegisterFixture(t *testing.T, f *machineFixture) {
	t.Helper()
	f.exec(t, `INSERT INTO cluster_inventory_sources(cluster,instance,asset_id,name,verified_at,verified_by,scope) VALUES('route','east','C-E','same','2025-01-01T00:00:00Z','tester','list_namespaces'),('route','west','C-W','same','2025-01-01T00:00:00Z','tester','list_namespaces');
 INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES('M1','host east','physical','route','east'),('VM','private west','vm','route','west'),('NO-SCOPE','platform host','physical','',''); UPDATE inventory_machines SET host_asset_id='M1' WHERE asset_id='VM';
 INSERT INTO inventory_node_links VALUES('route','west','private-node','M1'); INSERT INTO inventory_business_systems VALUES('B1','shared','team',1,1),('SYS-OTHER','platform-only','team',1,1);
 INSERT INTO inventory_business_deployments VALUES('B1','route','east','ns','east-app','','2025-01-01T00:00:00Z','admin'),('B1','route','west','ns','west-secret','','2025-01-01T00:00:00Z','admin');`)
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "route", Instance: "east"}); err != nil {
		t.Fatal(err)
	}
}
func TestAssetCoverageRegisterScopeHTTP(t *testing.T) {
	f := newCoverageFixture(t)
	freeze := func(scope, id string, want int) coverageReport {
		t.Helper()
		body := map[string]any{"action": "freeze", "baseline_id": id}
		if scope != "" {
			body["cluster_id"] = scope
			body["expected_revision"] = 2
		}
		data, _ := json.Marshal(body)
		return coverageDecoded(t, coverageHTTP(t, f, f.admin, "POST", "", string(data), want))
	}
	f.exec(t, `DROP TABLE asset_coverage_registers; DROP TABLE asset_coverage_cluster_versions;
 INSERT INTO asset_coverage_baselines VALUES('legacy','2025-01-01T00:00:00Z','qa','[]')`)
	freeze("", "incomplete", 503)
	coverageHTTP(t, f, f.admin, "GET", "?summary=1", "", 503)
	coverageHTTP(t, f, f.admin, "GET", "?cluster_id=C-E", "", 503)
	ddl, err := os.ReadFile("../../../docs/migrations/asset_coverage_registers.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, string(ddl))
	installCoverageRegisterFixture(t, f)
	type E = coverageEntry
	east := coverageTarget{"route", "east"}
	save := func(token, scope, doc string, rev int64, entries []E, want int) {
		t.Helper()
		body := map[string]any{"action": "save_register", "register": coverageRegister{doc, entries, rev}}
		if scope != "" {
			body["cluster_id"] = scope
		}
		data, _ := json.Marshal(body)
		coverageHTTP(t, f, token, "POST", "", string(data), want)
	}
	get := func(token, query string) coverageReport {
		t.Helper()
		return coverageDecoded(t, coverageHTTP(t, f, token, "GET", query, "", 200))
	}
	if r := get(f.admin, "?cluster_id=C-E&baseline_id=legacy"); r.State != "scope_unavailable" || r.Accepted != nil {
		t.Fatal(r)
	}
	coverageHTTP(t, f, f.admin, "GET", "?cluster_id=C-E&baseline_id=legacy&format=csv", "", 409)
	if r := get(f.admin, "?cluster_id=C-E&baseline_id=live"); r.State != "register_missing" {
		t.Fatal(r)
	}
	eastEntries := []E{{"machines", "M1", "host east", east}, {"machines", "NEW", "unseen", east}, {"systems", "B1", "shared", east}, {"machines", "VM", "private west", coverageTarget{}}}
	westEntries := []E{{"systems", "B1", "shared", coverageTarget{}}, {"machines", "VM", "private", coverageTarget{}}}
	save(f.user, "C-E", "doc-east", 0, eastEntries, 403)
	save(f.admin, "C-W", "doc-west", 0, eastEntries, 400)
	save(f.admin, "C-E", "doc-east", 0, eastEntries, 200)
	save(f.admin, "C-E", "stale", 0, []E{}, 409)
	save(f.admin, "C-W", "doc-west", 0, westEntries, 200)
	save(f.admin, "", "doc-global", 0, []E{{"machines", "P", "platform manual", coverageTarget{}}}, 200)
	r := freeze("", "all-v2", 200)
	if r.Metrics["machines"].Total != 5 || r.Metrics["systems"].Total != 2 {
		t.Fatal(r)
	}
	eastQ := "?cluster_id=C-E&baseline_id=all-v2"
	r = get(f.admin, eastQ)
	if r.Metrics["machines"].Total != 3 || r.Metrics["machines"].Covered != 0 || r.Metrics["systems"].Total != 1 || r.Metrics["systems"].Covered != 1 || r.RegisterRevision != 1 {
		t.Fatal(r)
	}
	raw := coverageHTTP(t, f, f.user, "GET", eastQ, "", 200)
	for _, secret := range []string{"private west", "west-secret", "private-node", "doc-west", "NO-SCOPE", "platform manual"} {
		if strings.Contains(raw, secret) {
			t.Fatal(secret, raw)
		}
	}
	r = coverageDecoded(t, raw)
	if r.Metrics["machines"].Total != 2 || r.Accepted != nil {
		t.Fatal(r)
	}
	data := coverageHTTP(t, f, f.user, "GET", eastQ+"&kind=machines&status=gap&format=csv", "", 200)
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(data, "\uFEFF"))).ReadAll()
	if err != nil || len(rows) != 3 || rows[1][8] != "C-E" || rows[1][9] != "doc-east" {
		t.Fatal(data, err)
	}
	save(f.admin, "C-E", "doc-east-2", 1, eastEntries[:1], 200)
	coverageHTTP(t, f, f.admin, "POST", "", `{"action":"freeze","cluster_id":"C-E","baseline_id":"stale","expected_revision":1}`, 409)
	coverageHTTP(t, f, f.admin, "GET", "?cluster_id=C-E&baseline_id=stale", "", 404)
	r = freeze("C-E", "east-v3", 200)
	if r.Metrics["machines"].Total != 1 || r.Metrics["systems"].Total != 0 {
		t.Fatal(r)
	}
	if r = get(f.admin, "?summary=1"); r.Baseline.ID != "all-v2" {
		t.Fatal(r)
	}
	if r = get(f.admin, "?summary=1&cluster_id=C-E"); r.Baseline.ID != "east-v3" {
		t.Fatal(r)
	}
	if r = get(f.admin, eastQ); r.Metrics["machines"].Total != 3 || r.Document != "doc-east" {
		t.Fatal(r)
	}
	coverageHTTP(t, f, f.user, "GET", "?cluster_id=C-W", "", 404)
	coverageHTTP(t, f, f.admin, "GET", "?cluster_id=same", "", 404)
	coverageHTTP(t, f, f.admin, "GET", "?cluster_id=&summary=1", "", 400)
	coverageHTTP(t, f, f.admin, "GET", "?cluster_id=C-E&cluster_id=C-W", "", 400)
	f.exec(t, `DELETE FROM grants WHERE user_id=?`, f.userID)
	for _, q := range []string{eastQ, eastQ + "&format=csv", "?summary=1&cluster_id=C-E"} {
		coverageHTTP(t, f, f.user, "GET", q, "", 404)
	}
}

func TestAssetCoverageRegisterMembershipsAndEmptyExportHTTP(t *testing.T) {
	f := newCoverageFixture(t)
	installCoverageRegisterFixture(t, f)
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "route", Instance: "west"}); err != nil {
		t.Fatal(err)
	}
	save := func(scope, doc string, revision int, entries []coverageEntry, status int) {
		t.Helper()
		body, err := json.Marshal(map[string]any{"action": "save_register", "cluster_id": scope, "register": coverageRegister{doc, entries, int64(revision)}})
		if err != nil {
			t.Fatal(err)
		}
		coverageHTTP(t, f, f.admin, "POST", "", string(body), status)
	}
	east := coverageEntry{"machines", "DUAL", "east rack", coverageTarget{"route", "east"}}
	west := coverageEntry{"machines", "DUAL", "west rack", coverageTarget{"route", "west"}}
	save("C-E", "duplicate", 0, []coverageEntry{east, east}, 400)
	save("C-E", "east-doc", 0, []coverageEntry{east}, 200)
	save("C-W", "west-doc", 0, []coverageEntry{west}, 200)
	freeze := `{"action":"freeze","baseline_id":"batch"}`
	coverageHTTP(t, f, f.admin, "POST", "", freeze, 200)
	coverageHTTP(t, f, f.admin, "POST", "", freeze, 409)
	save("C-W", "west-next", 1, []coverageEntry{}, 200)
	query := "?baseline_id=batch&kind=machines&status=gap"
	for _, suffix := range []string{"", "&format=csv"} {
		got := coverageHTTP(t, f, f.user, "GET", query+suffix, "", 200)
		if !strings.Contains(got, "east rack") || strings.Contains(got, "west rack") || strings.Contains(got, "west-doc") {
			t.Fatal(got)
		}
		admin := coverageHTTP(t, f, f.admin, "GET", query+suffix, "", 200)
		if !strings.Contains(admin, "west rack") || !strings.Contains(admin, "west-doc") {
			t.Fatal(admin)
		}
	}
	readCSV := func(query string) [][]string {
		t.Helper()
		data := coverageHTTP(t, f, f.user, "GET", "?baseline_id=batch&cluster_id=C-E&kind=machines&format=csv&status="+query, "", 200)
		rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(data, "\uFEFF"))).ReadAll()
		if err != nil {
			t.Fatal(err)
		}
		return rows
	}
	full, empty := readCSV("gap"), readCSV("covered")
	if len(full) != 2 || len(empty) != 1 || !slices.Equal(full[0], empty[0]) || !slices.Contains(empty[0], "memberships") {
		t.Fatalf("CSV %v vs %v", full, empty)
	}
}
