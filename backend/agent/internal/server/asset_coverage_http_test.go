package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

// Uses the registered Gateway route, real tokens, a loopback HTTP server and a
// temporary on-disk database. No production schema or data is touched.
func newCoverageFixture(t *testing.T) *machineFixture {
	t.Helper()
	f := newMachineFixture(t)
	for _, name := range []string{"cluster_inventory", "inventory_node_links", "business_inventory", "asset_coverage", "asset_coverage_registers"} {
		sql, err := os.ReadFile("../../../docs/migrations/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, string(sql))
	}
	return f
}
func coverageHTTP(t *testing.T, f *machineFixture, token, method, query, body string, want int) string {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+"/v1/assets/coverage"+query, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil || res.StatusCode != want {
		t.Fatalf("%s %s: HTTP %d want %d: %s (%v)", method, query, res.StatusCode, want, data, err)
	}
	if want == 200 && strings.Contains(query, "format=csv") && !strings.HasPrefix(res.Header.Get("Content-Type"), "text/csv") {
		t.Fatal(res.Header)
	}
	return string(data)
}
func coverageDecoded(t *testing.T, raw string) coverageReport {
	t.Helper()
	var report coverageReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatal(err)
	}
	return report
}
func TestAssetCoverageFreezeAndReauthorizeHTTP(t *testing.T) {
	f := newCoverageFixture(t)
	get := func(user, q string) string { return coverageHTTP(t, f, user, "GET", q, "", 200) }
	post := func(user, body string, want int) string { return coverageHTTP(t, f, user, "POST", "", body, want) }
	if r := coverageDecoded(t, get(f.admin, "")); r.State != "missing" || r.Accepted != nil {
		t.Fatal(r)
	}
	const empty = `{"action":"freeze","baseline_id":"empty"}`
	post(f.user, empty, 403)
	r := coverageDecoded(t, post(f.admin, empty, 200))
	if r.Accepted != nil || r.Metrics["machines"].Percent != nil || r.Metrics["systems"].Passed != nil {
		t.Fatal(r)
	}
	post(f.admin, empty, 409)
	f.exec(t, `INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance,active) VALUES ('M1','Host-Private','physical','ops','private',1),('M2','=SUM(1)','vm','ops','work',1),('M3','offline server','physical','ops','work',1),('M4','unbound','physical','','',1),('MR','retired','physical','','',0); UPDATE inventory_machines SET host_asset_id='M1' WHERE asset_id='M2'`)
	f.exec(t, `INSERT INTO cluster_inventory_sources(cluster,instance,asset_id,name,verified_at,verified_by,scope) VALUES ('ops','work','C1','cluster','2025-01-01T00:00:00Z','admin','list_namespaces'),('ops','private','C1','cluster','2025-01-01T00:00:00Z','admin','list_namespaces'); INSERT INTO inventory_node_links VALUES ('ops','work','node-a','M2'),('ops','private','hidden-node','M2'),('ops','private','hidden-node-b','M3')`)
	f.exec(t, `INSERT INTO inventory_business_systems VALUES ('B1','system','Team',1,1),('B2','missing','Team',1,1),('B3','retired business','Team',0,1); INSERT INTO inventory_business_deployments VALUES ('B1','ops','private','ns','private-app','','2025-01-01T00:00:00Z','admin'),('B1','ops','work','ns','public-app','','2025-01-01T00:00:00Z','admin')`)
	const freeze = `{"action":"freeze","baseline_id":"nov","verified_hosts":[{"machine_id":"M2","host_id":"M1","revision":1}]}`
	live := coverageDecoded(t, get(f.admin, "?baseline_id=live"))
	if live.Metrics["machines"].Covered != 2 {
		t.Fatal(live)
	}
	r = coverageDecoded(t, post(f.admin, freeze, 200))
	if r.Metrics["machines"].Total != 4 || r.Metrics["machines"].Covered != 3 || r.Metrics["machines"].Shortfall != 1 || r.Metrics["systems"].Total != 2 || r.Metrics["systems"].Covered != 1 || r.Accepted == nil || *r.Accepted {
		t.Fatalf("bad snapshot: %+v", r)
	}
	// Snapshot content is immutable even after the manager edits or deletes an edge.
	f.exec(t, `UPDATE inventory_machines SET name='renamed',host_asset_id=NULL,revision=2 WHERE asset_id='M2'; DELETE FROM inventory_node_links WHERE node_name='node-a'; UPDATE inventory_business_systems SET active=0 WHERE asset_id='B1'`)
	f.open(t)
	if historical := get(f.admin, "?baseline_id=nov"); !strings.Contains(historical, "=SUM(1)") || strings.Contains(historical, "renamed") {
		t.Fatal(historical)
	}
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "ops", Instance: "work"}); err != nil {
		t.Fatal(err)
	}
	visible := get(f.user, "?baseline_id=nov")
	for _, hidden := range []string{"Host-Private", "M1", "private-app", "hidden-node", "unbound"} {
		if strings.Contains(visible, hidden) {
			t.Fatalf("hidden %s in %s", hidden, visible)
		}
	}
	r = coverageDecoded(t, visible)
	if r.Metrics["machines"].Total != 2 || r.Metrics["machines"].Covered != 1 || r.Metrics["systems"].Covered != 1 || r.Scope != "visible" || r.Accepted != nil {
		t.Fatal(visible)
	}
	data := get(f.user, "?baseline_id=nov&kind=machines&status=covered&format=csv")
	cells, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(data, "\uFEFF"))).ReadAll()
	if err != nil || len(cells) != 2 || cells[1][4] != "M2" || cells[1][5] != "'=SUM(1)" || strings.Contains(data, "private") {
		t.Fatal(data, err)
	}
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "ops", Instance: "private"}); err != nil {
		t.Fatal(err)
	}
	r = coverageDecoded(t, get(f.user, "?baseline_id=nov"))
	if r.Metrics["machines"].Covered != 3 {
		t.Fatal(r)
	}
	f.exec(t, `DELETE FROM grants WHERE user_id=? AND instance='work'`, f.userID)
	visible = get(f.user, "?baseline_id=nov")
	if strings.Contains(visible, "M2") || strings.Contains(visible, "node-a") || strings.Contains(visible, "public-app") || !strings.Contains(visible, "private-app") {
		t.Fatal(visible)
	}
	f.exec(t, `DELETE FROM grants WHERE user_id=?`, f.userID)
	r = coverageDecoded(t, get(f.user, "?baseline_id=nov"))
	if len(r.Rows) != 0 || r.Metrics["systems"].Total != 0 {
		t.Fatal(r)
	}
	if strings.Count(get(f.user, "?baseline_id=nov&format=csv"), "\n") != 1 {
		t.Fatal("revoke failed in CSV")
	}
	post(f.admin, strings.Replace(freeze, `"nov"`, `"stale"`, 1), 409)
	f.exec(t, `CREATE TRIGGER fail_coverage_audit BEFORE UPDATE ON audit_events WHEN NEW.action='inventory:coverage:freeze' AND NEW.status='success' BEGIN SELECT RAISE(ABORT,'test audit outage'); END`)
	post(f.admin, `{"action":"freeze","baseline_id":"rollback"}`, 503)
	coverageHTTP(t, f, f.admin, "GET", "?baseline_id=rollback", "", 404)
	var n int
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='inventory:coverage:freeze' AND status='forbidden'`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action='inventory:coverage:export' AND status='success'`).Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	for _, q := range []string{"?scope=platform", "?kind=machines&kind=systems", "?format=x", "?status=", "?baseline_id=%ZZ"} {
		coverageHTTP(t, f, f.admin, "GET", q, "", 400)
	}
	coverageHTTP(t, f, "", "GET", "", "", 401)
	coverageHTTP(t, f, f.admin, "PUT", "", "", 405)
}
func TestAssetCoverageDoubleThresholdHTTP(t *testing.T) {
	f := newCoverageFixture(t)
	f.exec(t, `INSERT INTO cluster_inventory_sources(cluster,instance,asset_id,name,verified_at,verified_by,scope) VALUES ('ops','a','c','C','2025-01-01T00:00:00Z','admin','list_namespaces')`)
	for i := 1; i <= 5; i++ {
		id := fmt.Sprint(i)
		f.exec(t, `INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES(?,?,'physical','ops','a')`, id, id)
		f.exec(t, `INSERT INTO inventory_business_systems VALUES(?,?,'team',1,1)`, id, id)
		if i < 5 {
			f.exec(t, `INSERT INTO inventory_node_links VALUES('ops','a',?,?)`, "node-"+id, id)
			f.exec(t, `INSERT INTO inventory_business_deployments VALUES(?,'ops','a','ns',?,'','2025-01-01T00:00:00Z','admin')`, id, "app-"+id)
		}
	}
	r := coverageDecoded(t, coverageHTTP(t, f, f.admin, "POST", "", `{"action":"freeze","baseline_id":"boundary"}`, 200))
	for _, m := range r.Metrics {
		if m.Total != 5 || m.Covered != 4 || m.Shortfall != 0 || m.Percent == nil || *m.Percent != 80 {
			t.Fatal(m)
		}
	}
	if r.Accepted == nil || !*r.Accepted {
		t.Fatal(r.Accepted)
	}
	gap := coverageDecoded(t, coverageHTTP(t, f, f.admin, "GET", "?kind=systems&status=gap", "", 200))
	if len(gap.Rows) != 1 || gap.Rows[0].AssetID != "5" || gap.Metrics["machines"].Total != 5 {
		t.Fatal(gap)
	}
	f.exec(t, `UPDATE inventory_business_deployments SET verified_at='2999-01-01T00:00:00Z' WHERE asset_id='4'`)
	r = coverageDecoded(t, coverageHTTP(t, f, f.admin, "POST", "", `{"action":"freeze","baseline_id":"second"}`, 200))
	if r.Accepted == nil || *r.Accepted || r.Metrics["systems"].Shortfall != 1 {
		t.Fatal(r)
	}
	if !bytes.Contains([]byte(coverageHTTP(t, f, f.admin, "GET", "?baseline_id=boundary", "", 200)), []byte(`"accepted":true`)) {
		t.Fatal("old version changed")
	}
}

func TestAssetCoverageLifecycleUpgradeAndSummaryHTTP(t *testing.T) {
	f := newMachineFixture(t)
	machine := machineInventoryInput{AssetID: "srv", Name: "线下待接入", Kind: "physical"}
	f.save(t, "create", machine, 0, 200)
	const retire = `{"action":"set_machine_active","machine_id":"srv","active":false,"expected_revision":1}`
	coverageHTTP(t, f, f.admin, "POST", "", retire, 503)
	if records := f.list(t, f.admin); len(records) != 1 || records[0].Revision != 1 {
		t.Fatal(records)
	}
	machine.Name = "当前登记名称"
	f.save(t, "update", machine, 1, 200)
	for _, name := range []string{"cluster_inventory", "inventory_node_links", "business_inventory", "asset_coverage", "asset_coverage_registers"} {
		ddl, err := os.ReadFile("../../../docs/migrations/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, string(ddl))
	}
	get := func(token, q string) coverageReport {
		return coverageDecoded(t, coverageHTTP(t, f, token, "GET", q, "", 200))
	}
	getSummary := func(token string) coverageReport {
		raw := coverageHTTP(t, f, token, "GET", "?summary=1", "", 200)
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &keys); err != nil {
			t.Fatal(err)
		}
		if len(keys) != 5 {
			t.Fatal(keys)
		}
		for _, k := range []string{"scope", "state", "baseline", "metrics", "accepted"} {
			if _, ok := keys[k]; !ok {
				t.Fatal(raw)
			}
		}
		return coverageDecoded(t, raw)
	}
	if initial := getSummary(f.admin); initial.State != "missing" || initial.Baseline != nil {
		t.Fatal(initial)
	}
	freeze := func(id string) {
		coverageHTTP(t, f, f.admin, "POST", "", fmt.Sprintf(`{"action":"freeze","baseline_id":%q}`, id), 200)
	}
	freeze("before")
	change := func(token string, active bool, rev, want int) string {
		return coverageHTTP(t, f, token, "POST", "", fmt.Sprintf(`{"action":"set_machine_active","machine_id":"srv","active":%t,"expected_revision":%d}`, active, rev), want)
	}
	change(f.user, false, 2, 403)
	change(f.admin, false, 1, 409)
	change(f.admin, true, 2, 409)
	coverageHTTP(t, f, f.admin, "POST", "", strings.Replace(retire, `"srv"`, `"absent"`, 1), 404)
	coverageHTTP(t, f, f.admin, "POST", "", `{"action":"set_machine_active","machine_id":"srv","expected_revision":2}`, 400)
	retired := coverageDecoded(t, change(f.admin, false, 2, 200))
	if retired.Metrics["machines"].Total != 0 || len(retired.Lifecycle) != 1 || retired.Lifecycle[0].Active || retired.Lifecycle[0].Revision != 3 {
		t.Fatal(retired)
	}
	change(f.admin, false, 2, 409)
	f.save(t, "update", machine, 2, 409)
	if m := f.list(t, f.admin); len(m) != 1 || m[0].Revision != 3 {
		t.Fatal(m)
	}
	if old := getSummary(f.admin); old.Baseline.ID != "before" || old.Metrics["machines"].Total != 1 {
		t.Fatal(old)
	}
	freeze("retired")
	if latest := getSummary(f.admin); latest.Baseline.ID != "retired" || latest.Metrics["machines"].Total != 0 {
		t.Fatal(latest)
	}
	hidden := get(f.user, "?baseline_id=live")
	if len(hidden.Lifecycle) > 0 {
		t.Fatal(hidden)
	}
	f.exec(t, `CREATE TRIGGER fail_lifecycle_audit BEFORE UPDATE ON audit_events WHEN NEW.action='inventory:coverage:set_machine_active' AND NEW.status='success' BEGIN SELECT RAISE(ABORT,'test audit outage'); END`)
	change(f.admin, true, 3, 503)
	stillRetired := get(f.admin, "?baseline_id=live")
	if stillRetired.Lifecycle[0].Revision != 3 || stillRetired.Lifecycle[0].Active {
		t.Fatal(stillRetired)
	}
	f.exec(t, `DROP TRIGGER fail_lifecycle_audit`)
	restored := coverageDecoded(t, change(f.admin, true, 3, 200))
	if restored.Lifecycle[0].Revision != 4 || restored.Metrics["machines"].Total != 1 {
		t.Fatal(restored)
	}
	if hist := get(f.admin, "?baseline_id=retired"); len(hist.Rows) != 0 || hist.Lifecycle != nil {
		t.Fatal(hist)
	}
	freeze("restored")
	if scoped := getSummary(f.user); scoped.Scope != "visible" || scoped.Metrics["machines"].Total != 0 || scoped.Accepted != nil {
		t.Fatal(scoped)
	}
	for _, query := range []string{"?summary=0", "?summary=1&kind=all", "?summary=1&format=csv", "?summary=1&summary=1"} {
		coverageHTTP(t, f, f.admin, "GET", query, "", 400)
	}
	var successes int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='inventory:coverage:set_machine_active' AND status='success' AND command_summary IN ('machine_id=srv active=true->false revision=2->3','machine_id=srv active=false->true revision=3->4')`).Scan(&successes); err != nil || successes != 2 {
		t.Fatal(successes, err)
	}
}
