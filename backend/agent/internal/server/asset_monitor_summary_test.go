package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Four source targets, two inventory cluster IDs, and one revoked operator.
// Data is local to the test database. Exercise the registered Gateway HTTP routes.
func monitorDutyFixture(t *testing.T) (*executionLifecycleFixture, CreatedToken, int64) {
	t.Helper()
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	applyInventoryMigration(t, f)
	ddl, err := os.ReadFile("../../../docs/migrations/asset_monitor.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	for _, src := range []struct{ instance, id, status string }{{"east-a", "CLS-A", "online"}, {"east-b", "CLS-A", "offline"}, {"empty", "CLS-A", "online"}, {"private", "CLS-B", "online"}} {
		if _, err := f.store.db.Exec(`INSERT INTO agent_status(cluster,instance,status,updated_at) VALUES('dev',?,?, 'now')`, src.instance, src.status); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.Exec(`INSERT INTO cluster_inventory_sources(cluster,instance,asset_id,name,verified_at,verified_by,scope) VALUES('dev',?,?,?,'now','test','list_pods')`, src.instance, src.id, src.id); err != nil {
			t.Fatal(err)
		}
	}
	op := f.user(t, "duty-read", ScopeGrant{Cluster: "dev", Instance: "east-a"})
	auth, err := f.store.VerifyUserToken(op.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"east-b", "empty"} {
		if err = f.store.GrantUser(auth.UserID, ScopeGrant{Cluster: "dev", Instance: src}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UnixMilli() - 100
	old := now - 48*time.Hour.Milliseconds()
	add := func(instance, asset, metric, device, status string, at int64) {
		t.Helper()
		p := monitorInput{monitorAsset: monitorAsset{Type: "machine", AssetID: asset}, Metric: metric, Unit: monitorUnits[metric], DeviceID: device, monitorPoint: monitorPoint{At: at, Status: status}}
		if status == "ok" {
			v := 71.
			p.Value = &v
		}
		body, _ := json.Marshal(map[string]any{"cluster": "dev", "instance": instance, "action": "ingest", "samples": []monitorInput{p}})
		f.http(t, f.admin, "POST", monitorPath, string(body), 200)
	}
	add("east-a", "host", "gpu", gpuB, "missing", now)
	add("east-a", "na", "cpu", "", "not_applicable", old)
	add("private", "private-asset", "memory", "", "missing", now)
	for _, source := range []struct {
		instance string
		end      int64
	}{{"east-a", now}, {"east-b", old}} {
		add(source.instance, "host", "gpu", gpuA, "ok", source.end-7*time.Hour.Milliseconds())
		body, _ := json.Marshal(map[string]any{"action": "rule", "cluster": "dev", "instance": source.instance, "type": "machine", "asset_id": "host", "origin": "exporter", "metric": "gpu", "device_id": gpuA, "threshold": 50, "duration_seconds": 60})
		f.http(t, f.admin, "POST", monitorPath, string(body), 200)
		// Backdate this fixture's configured rule so a two-day outage can be exercised immediately.
		if _, err := f.store.db.Exec(`UPDATE asset_monitor_rules SET effective_ms=? WHERE series_id=(SELECT id FROM asset_monitor_series WHERE instance=? AND device_id=?)`, source.end-180000, source.instance, gpuA); err != nil {
			t.Fatal(err)
		}
		add(source.instance, "host", "gpu", gpuA, "ok", source.end-120000)
		add(source.instance, "host", "gpu", gpuA, "ok", source.end)
	}
	return f, op, old
}
func TestAssetMonitorDutySummaryHTTP(t *testing.T) {
	f, op, old := monitorDutyFixture(t)
	read := func(token CreatedToken, query string) monitorSummary {
		t.Helper()
		var s monitorSummary
		data := f.http(t, token, "GET", monitorPath+"?view=alerts"+query, "", 200)
		if err := json.Unmarshal(data, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	admin := read(f.admin, "")
	if admin.Open != 1 || admin.Missing != 3 || admin.Sources != 4 || len(admin.Items) != 0 {
		t.Fatalf("platform: %+v", admin)
	}
	s := read(op, "&cluster_id=CLS-A&state=missing")
	if s.Open != 1 || s.Missing != 2 || s.Unmonitored != 1 || s.Sources != 3 || len(s.Items) != 2 {
		t.Fatalf("cluster: %+v", s)
	}
	if s.Items[0].Reason != "missing" || s.Items[0].DeviceID != gpuB || s.Items[1].Reason != "stale" || s.Items[1].Connection != "offline" || s.Items[1].At != old {
		t.Fatalf("series identity: %+v", s.Items)
	}
	if other := read(op, "&cluster_id=CLS-B&state=missing"); other.Sources != 0 || other.Missing != 0 || len(other.Items) != 0 {
		t.Fatal("unauthorized cluster revealed")
	}
	if other := read(f.admin, "&cluster_id=CLS-B"); other.Missing != 1 || other.Open != 0 {
		t.Fatal("cluster scope broadened")
	}
	if open := read(op, "&state=open"); len(open.Items) != 1 || open.Items[0].Instance != "east-a" || open.Items[0].DeviceID != gpuA {
		t.Fatal("expired alert included in open")
	}
	q := url.Values{"cluster": {"dev"}, "instance": {"east-b"}, "cluster_id": {"CLS-A"}, "asset_id": {"host"}, "type": {"machine"}, "metric": {"gpu"}, "device_id": {gpuA}, "origin": {"exporter"}, "range": {"6h"}, "end_ms": {fmt.Sprint(old)}}
	path := monitorPath + "?" + q.Encode()
	var graph struct {
		From   int64           `json:"from_ms"`
		To     int64           `json:"to_ms"`
		Series []monitorSeries `json:"series"`
	}
	if err := json.Unmarshal(f.http(t, op, "GET", path, "", 200), &graph); err != nil {
		t.Fatal(err)
	}
	if graph.To != old || graph.To-graph.From != (6*time.Hour).Milliseconds() || len(graph.Series) != 1 || len(graph.Series[0].Points) != 2 {
		t.Fatalf("offline curve %+v", graph)
	}
	if _, err := f.store.db.Exec(`DELETE FROM grants WHERE user_id=(SELECT user_id FROM tokens WHERE id=?) AND instance='east-b'`, op.ID); err != nil {
		t.Fatal(err)
	}
	after := read(op, "&state=missing")
	if after.Missing != 1 || after.Sources != 2 || len(after.Items) != 1 || after.Items[0].Instance == "east-b" {
		t.Fatalf("revoked leak %+v", after)
	}
	f.http(t, op, "GET", path, "", 403)
	for _, invalid := range []string{"&cluster_id=", "&cluster_id=CLS-A&cluster_id=CLS-B", "&state=pending", "&state=open&page=0", "&instance=east-b"} {
		f.http(t, op, "GET", monitorPath+"?view=alerts"+invalid, "", 400)
	}
	// Exercise paging on repeated asset names from a single source with many series.
	var points []monitorInput
	for i := 0; i < monitorAlertPageSize+1; i++ {
		points = append(points, monitorInput{monitorAsset: monitorAsset{Type: "app", AssetID: fmt.Sprintf("pod-%02d", i)}, Metric: "cpu", Unit: "cores", monitorPoint: monitorPoint{At: time.Now().UnixMilli(), Status: "missing"}})
	}
	body, _ := json.Marshal(map[string]any{"action": "ingest", "cluster": "dev", "instance": "east-a", "samples": points})
	f.http(t, f.admin, "POST", monitorPath, string(body), 200)
	first, second := read(op, "&state=missing"), read(op, "&state=missing&page=2")
	if first.Missing != 27 || len(first.Items) != 25 || len(second.Items) != 2 || first.Items[24].AssetID == second.Items[0].AssetID {
		t.Fatalf("pagination: %d %d %d", first.Missing, len(first.Items), len(second.Items))
	}
	if strings.Contains(string(f.http(t, op, "GET", monitorPath+"?view=alerts&state=missing", "", 200)), "private-asset") {
		t.Fatal("private detail disclosed")
	}
}
