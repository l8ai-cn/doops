package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

const diagSlots int64 = 8 * 24 * 60

type diagnosticApp struct {
	SystemID    string `json:"system_id"`
	Namespace   string `json:"namespace"`
	Application string `json:"application"`
}
type diagnosticProcess struct {
	PID        int            `json:"pid"`
	Start      *int64         `json:"start_ticks"`
	RSS        *int64         `json:"rss_bytes"`
	Name       string         `json:"name"`
	Deployment *diagnosticApp `json:"deployment,omitempty"`
}
type diagnosticResources struct {
	CPUTotal    *int64 `json:"cpu_total_ticks,omitempty"`
	CPUIdle     *int64 `json:"cpu_idle_ticks,omitempty"`
	MemoryTotal *int64 `json:"memory_total_bytes,omitempty"`
	MemoryUsed  *int64 `json:"memory_used_bytes,omitempty"`
}

func validDiagnosticPair(total, part *int64) bool {
	return (total == nil && part == nil) || (total != nil && part != nil && *total > 0 && *part >= 0 && *part <= *total)
}

type diagnosticSample struct {
	diagnosticResources
	AssetID     string              `json:"asset_id"`
	Cluster     string              `json:"cluster"`
	Instance    string              `json:"instance"`
	Fingerprint string              `json:"host_sha256,omitempty"`
	Source      string              `json:"source"`
	At          int64               `json:"at_ms"`
	OS          string              `json:"os"`
	Version     string              `json:"version"`
	Kernel      string              `json:"kernel"`
	BootID      string              `json:"boot_id"`
	BootAt      int64               `json:"boot_at"`
	OOM         *int64              `json:"oom_kills"`
	Exited      int                 `json:"exited"`
	Processes   []diagnosticProcess `json:"processes"`
}

var diagBootID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var diagVersion = regexp.MustCompile(`^[a-zA-Z0-9._:+~-]{1,128}$`)
var diagComm = regexp.MustCompile(`^(?:[\pL\pN_. +:/@-]{1,31}|\[redacted\])$`)

func (v *diagnosticSample) valid(id, cluster, instance, fp string) bool {
	if !validDiagnosticPair(v.CPUTotal, v.CPUIdle) || !validDiagnosticPair(v.MemoryTotal, v.MemoryUsed) || v.AssetID != id || v.Cluster != cluster || v.Instance != instance || v.Fingerprint != fp || v.Source != "host-procfs" || v.At <= 0 || v.BootAt < 1 || v.BootAt > v.At/1000 || !diagBootID.MatchString(v.BootID) || !diagVersion.MatchString(v.OS) || (v.Version != "" && !diagVersion.MatchString(v.Version)) || !diagVersion.MatchString(v.Kernel) || v.Exited < 0 || (v.OOM != nil && *v.OOM < 0) || len(v.Processes) > 32768 {
		return false
	}
	seen := make(map[int]bool, len(v.Processes))
	for _, p := range v.Processes {
		if p.PID <= 0 || seen[p.PID] || p.Start == nil || *p.Start < 0 || p.RSS == nil || *p.RSS < 0 || !diagComm.MatchString(p.Name) || !inventoryMetadata(p.Name) {
			return false
		}
		seen[p.PID] = true
	}
	return seen[1]
}

func readDiagnosticSample(rootPath, id, cluster, instance string, from, to, exact int64) (*diagnosticSample, error) {
	if rootPath == "" {
		return nil, monitorErr(503, "diagnostic_source_unconfigured")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	dir, err := root.OpenRoot(filepath.Join(cluster, instance, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	read := func(slot int64) (*diagnosticSample, error) {
		name := strconv.FormatInt(slot, 10) + ".json"
		info, err := dir.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() || info.Size() > 4<<20 {
			return nil, monitorErr(503, "invalid_host_sample")
		}
		f, err := dir.Open(name)
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() > 8<<20 {
			return nil, monitorErr(503, "invalid_host_sample")
		}
		var v diagnosticSample
		dec := json.NewDecoder(io.LimitReader(f, 8<<20+1))
		dec.DisallowUnknownFields()
		if dec.Decode(&v) != nil || dec.Decode(new(any)) != io.EOF || !diagFingerprint.MatchString(v.Fingerprint) || !v.valid(id, cluster, instance, v.Fingerprint) || v.At/60000%diagSlots != slot {
			return nil, monitorErr(503, "invalid_host_sample")
		}
		if v.At < from || v.At > to || (exact != 0 && v.At != exact) {
			return nil, nil
		}
		return &v, nil
	}
	if exact != 0 {
		return read(exact / 60000 % diagSlots)
	}
	handle, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	slots := make(map[int64]bool, len(entries))
	for _, file := range entries {
		name := file.Name()
		if filepath.Ext(name) != ".json" {
			continue
		}
		n, err := strconv.ParseInt(name[:len(name)-5], 10, 64)
		if err == nil && n >= 0 && n < diagSlots {
			slots[n] = true
		}
	}
	for minute := to / 60000; minute >= from/60000; minute-- {
		slot := minute % diagSlots
		if slots[slot] {
			v, err := read(slot)
			if err != nil || v != nil {
				return v, err
			}
		}
	}
	return nil, nil
}

type diagnosticMetric struct {
	Metric string   `json:"metric"`
	Device string   `json:"device"`
	Unit   string   `json:"unit"`
	At     int64    `json:"at_ms"`
	Value  *float64 `json:"value"`
	Status string   `json:"status"`
}

type diagnosticApplication struct {
	diagnosticApp
	Name      string             `json:"system_name"`
	Verified  string             `json:"verified_at"`
	Databases []string           `json:"databases"`
	Series    []diagnosticMetric `json:"series"`
}

func diagnosticDeployments(tx *sql.Tx, v *diagnosticSample) (map[diagnosticApp]*diagnosticApplication, error) {
	rows, err := tx.Query(`SELECT d.asset_id,s.name,d.namespace,d.application,d.database_name,d.verified_at FROM inventory_business_deployments d JOIN inventory_business_systems s USING(asset_id) WHERE cluster=? AND instance=? AND julianday(d.verified_at)<=julianday(?,'unixepoch') ORDER BY d.asset_id,d.namespace,d.application,d.database_name`, v.Cluster, v.Instance, float64(v.At)/1000)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	apps := map[diagnosticApp]*diagnosticApplication{}
	for rows.Next() {
		var k diagnosticApp
		var name, db, verified string
		if err := rows.Scan(&k.SystemID, &name, &k.Namespace, &k.Application, &db, &verified); err != nil {
			return nil, err
		}
		a := apps[k]
		if a == nil {
			a = &diagnosticApplication{diagnosticApp: k, Name: name, Verified: verified, Databases: []string{}, Series: []diagnosticMetric{}}
			apps[k] = a
		}
		if db != "" {
			a.Databases = append(a.Databases, db)
		}
		if verified > a.Verified {
			a.Verified = verified
		}
	}
	return apps, rows.Err()
}

func (h *GatewayHub) HandleMachineDiagnostics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	var result any
	switch r.Method {
	case http.MethodPost:
		result, err = h.collectMachineDiagnostic(r, auth)
	case http.MethodGet:
		result, err = h.store.machineDiagnostic(r.Context(), auth, r.URL.Query())
	default:
		w.Header().Set("Allow", "GET, POST")
		err = monitorErr(405, "method_not_allowed")
	}
	if err != nil {
		p := inventoryFailure{503, "machine_diagnostics_unavailable"}
		errors.As(err, &p)
		writeDoopsError(w, p.status, "", doopsErrorResponse{Error: p.code, Code: p.code, Message: p.code})
		return
	}
	writeJSONHTTP(w, result)
}

func (s *GatewayStore) machineDiagnostic(ctx context.Context, auth TokenAuth, q url.Values) (any, error) {
	bad := monitorErr(400, "invalid_diagnostic_query")
	for k, values := range q {
		switch k {
		case "cluster", "instance", "asset_id", "range", "to_ms", "at_ms", "pid", "start_ticks":
		default:
			return nil, bad
		}
		if len(values) != 1 {
			return nil, bad
		}
	}
	id, cluster, instance := q.Get("asset_id"), q.Get("cluster"), q.Get("instance")
	duration, ok := monitorRanges[q.Get("range")]
	if !ok || !safeMachineID(id) || !safeMachineID(cluster) || !safeMachineID(instance) {
		return nil, bad
	}
	now := time.Now().UnixMilli()
	to := now
	var err error
	if q.Has("to_ms") {
		to, err = strconv.ParseInt(q.Get("to_ms"), 10, 64)
		if err != nil || to > now || to < now-int64((7*24*time.Hour)/time.Millisecond) {
			return nil, bad
		}
	}
	from := to - duration.Milliseconds()
	var exact, pid, ticks int64
	if q.Has("pid") || q.Has("at_ms") || q.Has("start_ticks") {
		for name, dest := range map[string]*int64{"at_ms": &exact, "pid": &pid, "start_ticks": &ticks} {
			*dest, err = strconv.ParseInt(q.Get(name), 10, 64)
			if err != nil {
				return nil, bad
			}
		}
		if exact < from || exact > to || pid < 1 || ticks < 0 {
			return nil, bad
		}
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := authorizeInventory(ctx, tx, auth, inventoryRequest{Cluster: cluster, Instance: instance}); err != nil {
		return nil, err
	}
	var fp string
	err = tx.QueryRow(`SELECT COALESCE(b.host_sha256,'') FROM inventory_machines m LEFT JOIN machine_diagnostic_bindings b USING(asset_id,cluster,instance) WHERE m.asset_id=? AND m.cluster=? AND m.instance=?`, id, cluster, instance).Scan(&fp)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, monitorErr(404, "machine_target_unavailable")
	}
	if err != nil {
		return nil, err
	}
	root := os.Getenv("DOOPS_MACHINE_DIAGNOSTICS_ROOT")
	result := map[string]any{"from_ms": from, "to_ms": to, "sample": nil, "evidence_mode": "tunnel"}
	if root != "" {
		result["evidence_mode"] = "mount"
	}
	if fp == "" {
		result["gap"] = "binding_missing"
		return result, nil
	}
	var v *diagnosticSample
	if root != "" {
		v, err = readDiagnosticSample(root, id, cluster, instance, from, to, exact)
	} else {
		var generation uint64
		v, generation, err = cachedDiagnostic(tx, id, cluster, instance, fp, from, to, exact)
		if v != nil {
			result["generation"] = generation
		}
	}
	if err != nil {
		return nil, err
	}
	if v == nil {
		result["gap"] = "sample_missing"
		return result, nil
	}
	if !v.valid(id, cluster, instance, fp) {
		return nil, monitorErr(503, "invalid_host_sample")
	}
	if pid == 0 {
		var prev *diagnosticSample
		if root != "" {
			prev, err = readDiagnosticSample(root, id, cluster, instance, from, v.At-1, 0)
		} else {
			prev, _, err = cachedDiagnostic(tx, id, cluster, instance, fp, from, v.At-1, 0)
		}
		if err != nil {
			return nil, err
		}
		if prev != nil && !prev.valid(id, cluster, instance, fp) {
			return nil, monitorErr(503, "invalid_host_sample")
		}
		result["comparison"] = compareDiagnosticSamples(prev, v)
	}
	v.Fingerprint = ""
	apps, err := diagnosticDeployments(tx, v)
	if err != nil {
		return nil, err
	}
	var selected *diagnosticApplication
	for i := range v.Processes {
		p := &v.Processes[i]
		if p.Deployment == nil {
			continue
		}
		a := apps[*p.Deployment]
		if a == nil {
			p.Deployment = nil
			continue
		}
		if int64(p.PID) == pid && *p.Start == ticks {
			selected = a
		}
	}
	if pid > 0 {
		if selected == nil {
			return nil, monitorErr(404, "process_application_unavailable")
		}
		rows, err := tx.Query(`SELECT s.metric,s.device_id,p.at_ms,p.status,p.value FROM asset_monitor_series s JOIN asset_monitor_samples p ON p.series_id=s.id
 WHERE s.cluster=? AND s.instance=? AND s.namespace=? AND s.type='app' AND s.asset_id=? AND s.origin='exporter'
 AND p.at_ms=(SELECT MAX(at_ms) FROM asset_monitor_samples WHERE series_id=s.id AND at_ms BETWEEN ? AND ?) ORDER BY s.metric,s.device_id`, cluster, instance, selected.Namespace, selected.Application, from, to)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		for rows.Next() {
			var metric diagnosticMetric
			if err := rows.Scan(&metric.Metric, &metric.Device, &metric.At, &metric.Status, &metric.Value); err != nil {
				return nil, err
			}
			metric.Unit = monitorUnits[metric.Metric]
			selected.Series = append(selected.Series, metric)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		result["application"] = selected
	}
	sort.Slice(v.Processes, func(i, j int) bool {
		a, b := v.Processes[i], v.Processes[j]
		if *a.RSS == *b.RSS {
			return a.PID < b.PID
		}
		return *a.RSS > *b.RSS
	})
	result["sample"] = v
	return result, nil
}

func compareDiagnosticSamples(prev, current *diagnosticSample) map[string]any {
	result := map[string]any{"status": "first_sample"}
	if prev == nil {
		return result
	}
	result["previous"] = struct {
		At int64 `json:"at_ms"`
		diagnosticResources
	}{prev.At, prev.diagnosticResources}
	switch {
	case prev.BootID != current.BootID:
		result["status"] = "reboot"
	case current.At-prev.At > diagnosticFreshness.Milliseconds():
		result["status"] = "gap"
	case prev.CPUTotal == nil || current.CPUTotal == nil:
		result["status"] = "unknown"
	default:
		total, idle := *current.CPUTotal-*prev.CPUTotal, *current.CPUIdle-*prev.CPUIdle
		if total <= 0 || idle < 0 || idle > total {
			result["status"] = "counter_discontinuity"
		} else {
			result["status"] = "ok"
			result["cpu_percent"] = 100 * float64(total-idle) / float64(total)
		}
	}
	return result
}
