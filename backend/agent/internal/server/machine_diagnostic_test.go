package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func machineDiagnosticFixture(t *testing.T) (*executionLifecycleFixture, string, int64) {
	t.Helper()
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	for _, migration := range []string{"cluster_inventory", "machine_inventory", "inventory_node_links", "business_inventory", "asset_monitor", "machine_diagnostics"} {
		raw, err := os.ReadFile("../../../docs/migrations/" + migration + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.db.Exec(string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	t.Setenv("DOOPS_MACHINE_DIAGNOSTICS_ROOT", root)
	at := time.Now().Add(-time.Minute).UnixMilli()
	fp := strings.Repeat("a", 64)
	_, err := f.store.db.Exec(`INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES('host1','Host One','physical','east','a'),('host2','Host Two','physical','east','a'),('host3','Other','physical','east','b');
 INSERT INTO cluster_inventory_sources(cluster,instance,asset_id,name,verified_at,verified_by,scope) VALUES('east','a','source-a','East','2020-01-01','test','list_pods');
 INSERT INTO inventory_business_systems VALUES('sys','Orders','Team',1,1);
 INSERT INTO inventory_business_deployments VALUES('sys','east','a','orders','app','orders-db','2020-02-01T00:00:00Z','reviewer');
 INSERT INTO asset_monitor_series(id,cluster,instance,namespace,type,asset_id,origin,metric) VALUES(1,'east','a','orders','app','app','exporter','memory')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.db.Exec(`INSERT INTO machine_diagnostic_bindings VALUES('host1','east','a',?),('host2','east','a',?)`, fp, strings.Repeat("b", 64))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.db.Exec(`INSERT INTO asset_monitor_samples VALUES(1,?,'ok',500),(1,?,'ok',999)`, at-1000, at+5000)
	if err != nil {
		t.Fatal(err)
	}
	sample := diagnosticSample{AssetID: "host1", Cluster: "east", Instance: "a", Fingerprint: fp, At: at, Source: "host-procfs", OS: "linux", Version: "1", Kernel: "6.8", BootAt: at/1000 - 200, BootID: "12345678-1234-1234-1234-123456789012"}
	small, big, start := int64(1), int64(24<<20), int64(350)
	sample.Processes = []diagnosticProcess{{PID: 1, Name: "app", Start: &start, RSS: &small}, {PID: 22, Name: "worker", Start: &start, RSS: &big, Deployment: &diagnosticApp{SystemID: "sys", Namespace: "orders", Application: "app"}}, {PID: 23, Name: "app", Start: &start, RSS: &small, Deployment: &diagnosticApp{SystemID: "sys", Namespace: "billing", Application: "app"}}}
	raw, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "east/a/host1")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := strconv.FormatInt(at/60000%diagSlots, 10) + ".json"
	file := filepath.Join(dir, name)
	if err = os.WriteFile(file, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return f, file, at
}

func TestMachineDiagnosticsHTTP(t *testing.T) {
	f, file, at := machineDiagnosticFixture(t)
	reader := f.user(t, "diag-reader", ScopeGrant{Cluster: "east", Instance: "a"})
	url := "/v1/assets/machine-diagnostics?cluster=east&instance=a&asset_id=host1&range=6h&to_ms=" + strconv.FormatInt(at, 10)
	get := func(url string, status int) string {
		t.Helper()
		return string(f.http(t, reader, "GET", url, "", status))
	}
	var details struct {
		Sample diagnosticSample `json:"sample"`
		From   int64            `json:"from_ms"`
		To     int64            `json:"to_ms"`
	}
	if err := json.Unmarshal([]byte(get(url, 200)), &details); err != nil {
		t.Fatal(err)
	}
	p := details.Sample.Processes
	if len(p) != 3 || p[0].PID != 22 || p[0].Deployment == nil || p[1].Deployment != nil || p[2].Deployment != nil || details.To != at || details.To-details.From != 6*time.Hour.Milliseconds() {
		t.Fatalf("incorrect scope or ordering %+v", details)
	}
	detail := url + "&at_ms=" + strconv.FormatInt(at, 10) + "&pid=22&start_ticks=350"
	app := get(detail, 200)
	if !strings.Contains(app, "orders-db") || !strings.Contains(app, `"value":500`) || strings.Contains(app, `"value":999`) || strings.Contains(app, "host_sha256") {
		t.Fatalf("application not scoped to exact window: %s", app)
	}
	get(strings.Replace(detail, "350", "351", 1), 404)
	get(strings.Replace(detail, "pid=22", "pid=1", 1), 404)
	get(strings.Replace(url, "instance=a", "instance=b", 1), 403)
	get(strings.Replace(url, "host1", "host3", 1), 404)
	get(url+"&range=7d", 400)
	f.http(t, reader, "PUT", url, "{}", 405)
	if gap := get(strings.Replace(url, "host1", "host2", 1), 200); !strings.Contains(gap, "sample_missing") {
		t.Fatal(gap)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(filepath.Dir(filepath.Dir(file)), "host2", filepath.Base(file))
	if err := os.MkdirAll(filepath.Dir(wrong), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrong, []byte(strings.Replace(string(data), `"host1"`, `"host2"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	get(strings.Replace(url, "host1", "host2", 1), 503)
	if _, err = f.store.db.Exec(`DELETE FROM inventory_business_deployments`); err != nil {
		t.Fatal(err)
	}
	get(detail, 404)
	if _, err := f.store.db.Exec(`DELETE FROM grants WHERE cluster='east' AND instance='a'`); err != nil {
		t.Fatal(err)
	}
	get(url, 403)
}

func TestMachineDiagnosticsComparisonHTTP(t *testing.T) {
	f, file, at := machineDiagnosticFixture(t)
	reader := f.user(t, "diag-compare", ScopeGrant{Cluster: "east", Instance: "a"})
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var current diagnosticSample
	if err = json.Unmarshal(data, &current); err != nil {
		t.Fatal(err)
	}
	count := func(n int64) *int64 { return &n }
	current.CPUTotal = count(200)
	current.CPUIdle = count(80)
	save := func(v diagnosticSample) string {
		t.Helper()
		p := filepath.Join(filepath.Dir(file), strconv.FormatInt(v.At/60000%diagSlots, 10)+".json")
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(p, b, 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	save(current)
	for _, tc := range []struct {
		status      string
		minutes     int64
		boot        string
		total, idle *int64
		percentage  float64
	}{
		{"ok", 1, current.BootID, count(100), count(30), 50},
		{"unknown", 1, current.BootID, nil, nil, 0},
		{"gap", 6, current.BootID, count(100), count(30), 0},
		{"reboot", 1, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", count(100), count(30), 0},
		{"counter_discontinuity", 1, current.BootID, count(201), count(80), 0},
	} {
		t.Run(tc.status, func(t *testing.T) {
			prev := current
			prev.At -= tc.minutes * 60000
			prev.BootAt = prev.At/1000 - 100
			prev.BootID = tc.boot
			prev.CPUTotal, prev.CPUIdle = tc.total, tc.idle
			p := save(prev)
			defer os.Remove(p)
			body := f.http(t, reader, "GET", "/v1/assets/machine-diagnostics?asset_id=host1&cluster=east&instance=a&range=1h&to_ms="+strconv.FormatInt(at, 10), "", 200)
			var report struct {
				Comparison struct {
					Status   string
					Percent  *float64 `json:"cpu_percent"`
					Previous struct {
						At   int64  `json:"at_ms"`
						Used *int64 `json:"memory_used_bytes"`
					}
				}
			}
			if err := json.Unmarshal(body, &report); err != nil {
				t.Fatal(err)
			}
			c := report.Comparison
			if c.Status != tc.status || c.Previous.At != prev.At || c.Previous.Used != nil || (c.Percent != nil) != (tc.status == "ok") || (c.Percent != nil && *c.Percent != tc.percentage) {
				t.Fatalf("unexpected comparison: %+v", c)
			}
		})
	}
}
