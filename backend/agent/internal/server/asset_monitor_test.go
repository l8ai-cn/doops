package server

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const monitorPath = "/v1/assets/monitor"

func monitorFixture(t *testing.T) *executionLifecycleFixture {
	t.Helper()
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	srv := httptest.NewServer(http.HandlerFunc(f.hub.HandleAssetMonitor))
	t.Cleanup(srv.Close)
	f.server = srv
	ddl, err := os.ReadFile("../../../docs/migrations/asset_monitor.sql")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err = f.store.db.Exec(string(ddl)); err != nil {
			t.Fatal(err)
		}
	}
	return f
}
func monitorExporter(t *testing.T, f *executionLifecycleFixture, defaults ...string) func(CreatedToken, ...string) error {
	t.Helper()
	tls := httptest.NewTLSServer(http.HandlerFunc(f.hub.HandleAssetMonitor))
	t.Cleanup(tls.Close)
	caFile := filepath.Join(t.TempDir(), "test-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tls.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return func(token CreatedToken, args ...string) error {
		cmd := exec.Command("python3", append([]string{"../../../scripts/asset_monitor_collector.py", "--cluster", "dev", "--instance", "local", "--gateway", tls.URL}, append(defaults, args...)...)...)
		cmd.Env = append(os.Environ(), "SSL_CERT_FILE="+caFile)
		cmd.Stdin = strings.NewReader(token.Plaintext + "\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("collector: %s", out)
		}
		return nil
	}
}
func TestAssetMonitorGatewayAndExporter(t *testing.T) {
	f := monitorFixture(t)
	_, err := f.store.db.Exec(`INSERT INTO agent_status(cluster,instance,status,updated_at) VALUES('dev','local','offline','now'),('dev','private','offline','now')`)
	if err != nil {
		t.Fatal(err)
	}
	operator := f.user(t, "monitor-reader", ScopeGrant{Cluster: "dev", Instance: "local"})
	f.http(t, operator, "GET", monitorPath+"?cluster=dev&instance=private", "", 403)
	collect := monitorExporter(t, f, "--asset-id", "test-volume", "--type", "app", "--mount", "/")
	if err := collect(operator); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("untrusted writer: %v", err)
	}
	if err := collect(f.admin); err != nil {
		t.Fatal(err)
	}
	rule := `{"action":"rule","cluster":"dev","instance":"local","type":"app","asset_id":"test-volume","origin":"exporter","metric":"storage","device_id":"/","threshold":0,"duration_seconds":1}`
	f.http(t, operator, "POST", monitorPath, rule, 403)
	f.http(t, f.admin, "POST", monitorPath, rule, 200)
	if err = collect(f.admin, "--interval-seconds", "1", "--count", "3"); err != nil {
		t.Fatal(err)
	}
	query := monitorPath + "?cluster=dev&instance=local&type=app&asset_id=test-volume&origin=exporter&range=1h&metric=storage&device_id=%2F"
	read := func() monitorSeries {
		t.Helper()
		var result struct {
			Series []monitorSeries `json:"series"`
		}
		if err := json.Unmarshal(f.http(t, operator, "GET", query, "", 200), &result); err != nil || len(result.Series) != 1 {
			t.Fatalf("detail: %v", err)
		}
		return result.Series[0]
	}
	s := read()
	if len(s.Points) != 4 || len(s.Alerts) != 1 || s.Alerts[0].State != "open" || s.Unit != "bytes" || s.Latest.Value == nil || *s.Latest.Value <= 0 {
		t.Fatalf("expected real filesystem observations + alert: %+v", s)
	}
	write := func(at int64, status, value string, want int) {
		t.Helper()
		body := fmt.Sprintf(`{"action":"ingest","cluster":"dev","instance":"local","samples":[{"type":"app","asset_id":"test-volume","metric":"storage","device_id":"/","unit":"bytes","at_ms":%d,"status":"%s","value":%s}]}`, at, status, value)
		f.http(t, f.admin, "POST", monitorPath, body, want)
	}
	at := time.Now().UnixMilli()
	write(at, "missing", "null", 200)
	write(at, "missing", "null", 200)
	write(at, "ok", "0", 409)
	s = read()
	if s.Alerts[0].State != "interrupted" || s.Latest.Value != nil || len(s.Points) != 5 {
		t.Fatalf("gap or duplicate mishandled: %+v", s)
	}
	f.http(t, f.admin, "POST", monitorPath, strings.Replace(rule, `"duration_seconds":1`, `"duration_seconds":0`, 1), 400)
	write(at+60000, "ok", "1", 400)
	f.http(t, f.admin, "POST", monitorPath, `{"action":"collect","cluster":"dev","instance":"local","namespace":"orders","samples":[]}`, 400)
	f.http(t, operator, "POST", monitorPath, `{"action":"collect","cluster":"dev","instance":"local","namespace":"orders"}`, 503)
	f.http(t, f.admin, "POST", monitorPath, strings.Replace(rule, "local", "never-registered", 1), 404)
	f.http(t, operator, "GET", monitorPath+"?cluster=dev&instance=local&range=invalid", "", 400)
	if _, err = f.store.db.Exec(`DELETE FROM grants WHERE user_id=(SELECT user_id FROM tokens WHERE id=?)`, operator.ID); err != nil {
		t.Fatal(err)
	}
	f.http(t, operator, "GET", query, "", 403)
}
func TestAssetMonitorNativeTarget(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	ddl, err := os.ReadFile("../../../docs/migrations/asset_monitor.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.db.Exec(string(ddl)); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "kubectl"), []byte(`#!/bin/sh
[ "$*" = 'top pods --namespace orders' ] || exit 11
printf 'NAME CPU(cores) MEMORY(bytes)\norder-api 125m 37Mi\n'
`), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	inventoryTestAgent(t, f, "local")
	srv := httptest.NewServer(http.HandlerFunc(f.hub.HandleAssetMonitor))
	defer srv.Close()
	client := *f
	client.server = srv
	op := f.user(t, "sample-pods", ScopeGrant{Cluster: "dev", Instance: "local"})
	body := `{"action":"collect","cluster":"dev","instance":"local","namespace":"orders"}`
	client.http(t, op, "POST", monitorPath, body, 200)
	data := client.http(t, op, "GET", monitorPath+"?cluster=dev&instance=local&type=app&asset_id=order-api&namespace=orders&origin=k8s_top", "", 200)
	var d struct{ Series []monitorSeries }
	if err = json.Unmarshal(data, &d); err != nil || len(d.Series) != 2 || *d.Series[0].Latest.Value != 0.125 || *d.Series[1].Latest.Value != 37*1048576 {
		t.Fatalf("unexpected native series: %s", data)
	}
	client.http(t, op, "POST", monitorPath, strings.Replace(body, "local", "private", 1), 403)
}

func TestAssetMonitorScopeHistoryAndRecovery(t *testing.T) {
	f := monitorFixture(t)
	applyInventoryMigration(t, f)
	if _, err := f.store.db.Exec(`INSERT INTO agent_status(cluster,instance,updated_at) VALUES('dev','local','now'); INSERT INTO cluster_inventory_sources(cluster,instance,asset_id,name,verified_at,verified_by,scope) VALUES('dev','local','C-1','test','now','test','list_pods')`); err != nil {
		t.Fatal(err)
	}
	reader := f.user(t, "series-reader", ScopeGrant{Cluster: "dev", Instance: "local"})
	route := monitorRequest{Action: "ingest", Cluster: "dev", Instance: "local", ClusterID: "C-1"}
	start := time.Now().Add(-2 * time.Hour).UnixMilli()
	value := 2.0
	write := func(want int, points ...monitorInput) {
		t.Helper()
		body := map[string]any{"action": "ingest", "cluster": route.Cluster, "instance": route.Instance, "cluster_id": route.ClusterID, "samples": points}
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		f.http(t, f.admin, "POST", monitorPath, string(data), want)
	}
	for kind := range monitorKinds {
		for metric, unit := range monitorUnits {
			p := monitorInput{monitorAsset: monitorAsset{Type: kind, AssetID: "asset-1"}, Metric: metric, Unit: unit, monitorPoint: monitorPoint{At: start, Status: "ok", Value: &value}}
			if metric == "storage_total" || metric == "storage_available" {
				p.DeviceID = "/"
			}
			if metric == "gpu" {
				p.DeviceID = "GPU-11111111-1111-1111-1111-111111111111"
			}
			write(200, p)
		}
	}
	base := monitorPath + "?cluster=dev&instance=local&cluster_id=C-1"
	list := f.http(t, reader, "GET", base, "", 200)
	var catalog struct{ Assets []monitorAsset }
	if json.Unmarshal(list, &catalog) != nil || len(catalog.Assets) != 6 {
		t.Fatalf("six types: %s", list)
	}
	f.http(t, reader, "GET", strings.Replace(base, "C-1", "C-2", 1), "", 403)
	path := base + "&type=machine&asset_id=asset-1&origin=exporter"
	read := func(q string) []monitorSeries {
		t.Helper()
		var d struct{ Series []monitorSeries }
		if err := json.Unmarshal(f.http(t, reader, "GET", path+q, "", 200), &d); err != nil {
			t.Fatal(err)
		}
		return d.Series
	}
	for _, s := range read("&range=6h") {
		if len(s.Points) != 1 || s.Unit != monitorUnits[s.Metric] || !s.Stale {
			t.Fatalf("series %+v", s)
		}
	}
	if got := read("&range=1h&metric=cpu"); len(got) != 1 || len(got[0].Points) != 0 {
		t.Fatal("time filter")
	}
	p := monitorInput{monitorAsset: monitorAsset{Type: "machine", AssetID: "asset-1"}, Metric: "cpu", Unit: "cores", monitorPoint: monitorPoint{At: start + 1000, Value: &value, Status: "ok"}}
	bad := p
	bad.At = start
	write(200, bad)
	other := 9.
	bad.Value = &other
	write(409, p, bad)
	if len(read("&range=6h&metric=cpu")[0].Points) != 1 {
		t.Fatal("partial batch committed")
	}
	rule := `{"cluster":"dev","instance":"local","type":"machine","asset_id":"asset-1","origin":"exporter","metric":"cpu","action":"rule","threshold":1,"duration_seconds":1}`
	f.http(t, f.admin, "POST", monitorPath, rule, 200)
	// Move the fixture's rule effective time to exercise a six-minute gap without a wall-clock wait.
	if _, err := f.store.db.Exec(`UPDATE asset_monitor_rules SET effective_ms=?`, start); err != nil {
		t.Fatal(err)
	}
	write(200, p)
	p.At += 1000
	write(200, p)
	p.At += 1000
	low := 0.
	p.Value = &low
	write(200, p)
	p.At += 1000
	p.Value = &value
	write(200, p)
	p.At += 360000
	write(200, p)
	p.At += 1000
	write(200, p)
	alerts := read("&range=6h&metric=cpu")[0].Alerts
	if len(alerts) != 2 || alerts[1].State != "recovered" || alerts[0].State != "interrupted" || alerts[0].Opened != p.At {
		t.Fatalf("hold/reset/recovery: %+v", alerts)
	}
}

const gpuA = "GPU-11111111-1111-1111-1111-111111111111"
const gpuB = "GPU-22222222-2222-2222-2222-222222222222"

func monitorGPUFixture(t *testing.T) (*executionLifecycleFixture, func(CreatedToken, ...string) error) {
	t.Helper()
	f := monitorFixture(t)
	if _, err := f.store.db.Exec(`INSERT INTO agent_status(cluster,instance,updated_at) VALUES('dev','local','now')`); err != nil {
		t.Fatal(err)
	}
	return f, monitorExporter(t, f, "--type", "app", "--asset-id", "dual-host")
}
func monitorGPUSeries(t *testing.T, f *executionLifecycleFixture, device string) []monitorSeries {
	t.Helper()
	query := monitorPath + "?cluster=dev&instance=local&type=app&asset_id=dual-host&origin=exporter&metric=gpu"
	if device != "" {
		query += "&device_id=" + device
	}
	var result struct{ Series []monitorSeries }
	if err := json.Unmarshal(f.http(t, f.admin, "GET", query, "", 200), &result); err != nil {
		t.Fatal(err)
	}
	return result.Series
}
func TestAssetMonitorDualGPUCollectorHTTP(t *testing.T) {
	f, collect := monitorGPUFixture(t)
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FIRST_GPU", gpuA)
	t.Setenv("SECOND_GPU", gpuB)
	// The fixture replaces only the NVIDIA driver output. The production Python
	// collector, HTTPS bearer authentication, SQLite store and rule engine run normally.
	driver := `#!/bin/sh
[ "$1" = -i ] && [ "$4" = --format=csv,noheader,nounits ] || exit 2
case "$2:$3" in
  0:--query-gpu=uuid) echo "$FIRST_GPU";;
  1:--query-gpu=uuid) echo "$SECOND_GPU";;
  ` + gpuA + `:--query-gpu=uuid,utilization.gpu) echo '` + gpuA + `, 81';;
  ` + gpuB + `:--query-gpu=uuid,utilization.gpu) [ -z "$DROP_B" ] || exit 3; echo '` + gpuB + `, 12';;
  *) exit 2;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(driver), 0755); err != nil {
		t.Fatal(err)
	}
	if err := collect(f.admin, "--gpu", "0", "--gpu", "1"); err != nil {
		t.Fatal(err)
	}
	both := monitorGPUSeries(t, f, "")
	if len(both) != 2 || both[0].DeviceID != gpuA || both[1].DeviceID != gpuB || *both[0].Latest.Value != 81 || *both[1].Latest.Value != 12 {
		t.Fatalf("initial device split: %+v", both)
	}
	rule := `{"cluster":"dev","instance":"local","type":"app","asset_id":"dual-host","origin":"exporter","metric":"gpu","action":"rule","threshold":50,"duration_seconds":1`
	f.http(t, f.admin, "POST", monitorPath, rule+"}", 400)
	for _, id := range []string{gpuA, gpuB} {
		f.http(t, f.admin, "POST", monitorPath, rule+`,"device_id":"`+id+`"}`, 200)
	}
	t.Setenv("FIRST_GPU", gpuB)
	t.Setenv("SECOND_GPU", gpuA)
	if err := collect(f.admin, "--gpu", "0", "--gpu", "1", "--interval-seconds", "1", "--count", "3"); err != nil {
		t.Fatal(err)
	}
	av, bv := monitorGPUSeries(t, f, gpuA), monitorGPUSeries(t, f, gpuB)
	if len(av) != 1 || len(bv) != 1 || len(av[0].Alerts) != 1 || av[0].Alerts[0].State != "open" || len(bv[0].Alerts) != 0 || av[0].Rule == nil || bv[0].Rule == nil {
		t.Fatalf("device-specific alerts: %+v / %+v", av, bv)
	}
	for _, s := range []monitorSeries{av[0], bv[0]} {
		if len(s.Points) != 4 {
			t.Fatalf("points: %+v", s)
		}
		for _, p := range s.Points {
			if *p.Value != *s.Latest.Value {
				t.Fatal("index swap crossed device histories")
			}
		}
	}
	t.Setenv("DROP_B", "1")
	if err := collect(f.admin, "--gpu", gpuA, "--gpu", gpuB); err != nil {
		t.Fatal(err)
	}
	if monitorGPUSeries(t, f, gpuB)[0].Latest.Status != "missing" || monitorGPUSeries(t, f, gpuA)[0].Latest.Status != "ok" {
		t.Fatal("missing sample crossed device histories")
	}
	p := monitorInput{monitorAsset: monitorAsset{Type: "app", AssetID: "dual-host"}, Metric: "gpu", Unit: "%", monitorPoint: monitorPoint{At: time.Now().UnixMilli(), Status: "ok"}}
	v := 11.
	p.Value = &v
	post := func(points []monitorInput, want int) {
		body, _ := json.Marshal(map[string]any{"cluster": "dev", "instance": "local", "action": "ingest", "samples": points})
		f.http(t, f.admin, "POST", monitorPath, string(body), want)
	}
	post([]monitorInput{p}, 400)
	p.DeviceID = "0"
	post([]monitorInput{p}, 400)
	a, b := p, p
	a.DeviceID = gpuA
	b.DeviceID = gpuB
	post([]monitorInput{a, b}, 200)
	post([]monitorInput{a, b}, 200)
	if monitorGPUSeries(t, f, gpuA)[0].Latest.At != p.At || monitorGPUSeries(t, f, gpuB)[0].Latest.At != p.At {
		t.Fatal("same host and timestamp must accept both cards")
	}
}

func TestAssetMonitorDualGPUHardware(t *testing.T) {
	if _, err := exec.LookPath("nvidia-smi"); err != nil {
		t.Skip("no local NVIDIA driver")
	}
	out, err := exec.Command("nvidia-smi", "--query-gpu=uuid", "--format=csv,noheader,nounits").Output()
	if err != nil {
		t.Fatal(err)
	}
	ids := strings.Fields(string(out))
	if len(ids) < 2 {
		t.Skip("requires two local NVIDIA GPUs")
	}
	f, collect := monitorGPUFixture(t)
	if err := collect(f.admin, "--gpu", ids[0], "--gpu", ids[1]); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids[:2] {
		series := monitorGPUSeries(t, f, id)
		if len(series) != 1 || series[0].DeviceID != id || series[0].Latest.Status != "ok" || series[0].Latest.Value == nil {
			t.Fatalf("hardware sample for %s: %+v", id, series)
		}
	}
}
