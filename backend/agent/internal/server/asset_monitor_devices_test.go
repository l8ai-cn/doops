package server

import (
	"encoding/json"
	"fmt"
	"net/url"
	"testing"
	"time"
)

func TestAssetMonitorIndependentDeviceSeriesHTTP(t *testing.T) {
	f := monitorFixture(t)
	if _, err := f.store.db.Exec(`INSERT INTO agent_status(cluster,instance,updated_at) VALUES('dev','local','now')`); err != nil {
		t.Fatal(err)
	}
	type channel struct {
		metric, device string
		value          float64
	}
	channels := []channel{{"network", "", 13}, {"network", "eth0", 71}, {"network", "eth1", 12}, {"storage", "", 10}}
	for _, m := range []string{"storage_total", "storage", "storage_available"} {
		channels = append(channels, channel{m, "/", 73}, channel{m, "/mnt/data 2", 15})
	}
	start := time.Now().Add(-time.Minute).UnixMilli()
	write := func(at int64) {
		var batch []monitorInput
		for _, c := range channels {
			batch = append(batch, monitorInput{monitorAsset: monitorAsset{Type: "machine", AssetID: "multi"}, Metric: c.metric, Unit: monitorUnits[c.metric], DeviceID: c.device, monitorPoint: monitorPoint{At: at, Status: "ok", Value: &c.value}})
		}
		raw, err := json.Marshal(map[string]any{"action": "ingest", "cluster": "dev", "instance": "local", "samples": batch})
		if err != nil {
			t.Fatal(err)
		}
		f.http(t, f.admin, "POST", monitorPath, string(raw), 200)
	}
	write(start)
	for _, c := range channels {
		rule := fmt.Sprintf(`{"action":"rule","cluster":"dev","instance":"local","type":"machine","asset_id":"multi","origin":"exporter","metric":%q,"device_id":%q,"threshold":50,"duration_seconds":1}`, c.metric, c.device)
		f.http(t, f.admin, "POST", monitorPath, rule, 200)
	}
	first := time.Now().UnixMilli()
	write(first)
	time.Sleep(time.Second)
	last := time.Now().UnixMilli()
	write(last)
	write(last)
	read := func(query string) []monitorSeries {
		t.Helper()
		var detail struct{ Series []monitorSeries }
		raw := f.http(t, f.admin, "GET", monitorPath+"?cluster=dev&instance=local&type=machine&asset_id=multi&origin=exporter"+query, "", 200)
		if err := json.Unmarshal(raw, &detail); err != nil {
			t.Fatal(err)
		}
		return detail.Series
	}
	if all := read(""); len(all) != len(channels) {
		t.Fatalf("expected %d independent series, got %d", len(channels), len(all))
	}
	for _, c := range channels {
		series := read("&metric=" + c.metric + "&device_id=" + url.QueryEscape(c.device))
		if len(series) != 1 {
			t.Fatalf("%+v: %d series", c, len(series))
		}
		s := series[0]
		if s.DeviceID != c.device || s.Metric != c.metric || len(s.Points) != 3 || s.Latest.Value == nil || *s.Latest.Value != c.value || s.Rule == nil {
			t.Fatalf("series differs for %+v: %+v", c, s)
		}
		if c.value > 50 && (len(s.Alerts) != 1 || s.Alerts[0].State != "open") || c.value <= 50 && len(s.Alerts) != 0 {
			t.Fatalf("alert crossed devices for %+v: %+v", c, s.Alerts)
		}
	}
	for _, q := range []string{"metric=network&device_id=%2Fdev", "metric=storage&device_id=relative", "metric=storage&device_id=%2Fmnt%2F..%2Ffoo", "metric=memory&device_id=eth0"} {
		f.http(t, f.admin, "GET", monitorPath+"?cluster=dev&instance=local&"+q, "", 400)
	}
}
