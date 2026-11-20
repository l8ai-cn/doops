package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const monitorGap = 5 * time.Minute

var monitorUnits = map[string]string{"cpu": "cores", "memory": "bytes", "gpu": "%", "network": "bytes/s", "requests": "req/s", "storage": "bytes", "storage_total": "bytes", "storage_available": "bytes"}
var monitorKinds = map[string]bool{"machine": true, "os": true, "kernel": true, "process": true, "app": true, "database": true}
var monitorRanges = map[string]time.Duration{"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour}

type monitorAsset struct {
	Type      string `json:"type"`
	AssetID   string `json:"asset_id"`
	Namespace string `json:"namespace"`
	Origin    string `json:"origin"`
}
type monitorPoint struct {
	At     int64    `json:"at_ms"`
	Status string   `json:"status"`
	Value  *float64 `json:"value"`
}

var monitorGPUUUID = regexp.MustCompile(`^GPU-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var monitorInterface = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`)

func validMonitorDevice(metric, device string) bool {
	switch metric {
	case "gpu":
		return monitorGPUUUID.MatchString(device)
	case "network":
		return device == "" || monitorInterface.MatchString(device)
	case "storage", "storage_total", "storage_available":
		if device == "" {
			return metric == "storage"
		}
		return len(device) < 4096 && path.IsAbs(device) && path.Clean(device) == device && strings.IndexFunc(device, unicode.IsControl) < 0
	default:
		return device == ""
	}
}

type monitorInput struct {
	DeviceID string `json:"device_id,omitempty"`
	monitorAsset
	Metric string `json:"metric"`
	Unit   string `json:"unit"`
	monitorPoint
}
type monitorRule struct {
	Threshold *float64 `json:"threshold"`
	Duration  int      `json:"duration_seconds"`
}
type monitorRequest struct {
	DeviceID  string `json:"device_id,omitempty"`
	Action    string `json:"action"`
	Cluster   string `json:"cluster"`
	Instance  string `json:"instance"`
	ClusterID string `json:"cluster_id,omitempty"`
	monitorAsset
	Metric string `json:"metric"`
	monitorRule
	Samples []monitorInput `json:"samples"`
}

func monitorID(s string) bool                  { return inventoryKeyPattern.MatchString(s) && inventoryMetadata(s) }
func monitorErr(status int, code string) error { return inventoryFailure{status, code} }
func monitorValidAsset(a monitorAsset) bool {
	return monitorKinds[a.Type] && monitorID(a.AssetID) && (a.Namespace == "" || inventoryNamespacePattern.MatchString(a.Namespace))
}
func monitorValidValue(metric string, v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && (metric != "gpu" || v <= 100)
}
func (p monitorInput) valid(now time.Time) bool {
	if !validMonitorDevice(p.Metric, p.DeviceID) || !monitorValidAsset(p.monitorAsset) || monitorUnits[p.Metric] == "" || p.Unit != monitorUnits[p.Metric] || p.At <= 0 || p.At > now.UnixMilli() {
		return false
	}
	switch p.Status {
	case "ok":
		return p.Value != nil && monitorValidValue(p.Metric, *p.Value)
	case "missing", "not_applicable":
		return p.Value == nil
	}
	return false
}

// Authentication and exact target authorization run inside each read/write transaction.
// An optional inventory asset ID further restricts the target; it never grants access.
func (req monitorRequest) authorize(ctx context.Context, tx *sql.Tx, auth TokenAuth, admin bool) error {
	if !monitorID(req.Cluster) || !monitorID(req.Instance) || (req.ClusterID != "" && !monitorID(req.ClusterID)) {
		return monitorErr(400, "invalid_monitor_target")
	}
	action := "discover"
	if admin {
		action = "import"
	}
	if err := authorizeInventory(ctx, tx, auth, inventoryRequest{Action: action, Cluster: req.Cluster, Instance: req.Instance}); err != nil {
		return err
	}
	var known bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM agent_status WHERE cluster=? AND instance=? AND (?='' OR archived_at=''))`, req.Cluster, req.Instance, req.Action).Scan(&known); err != nil {
		return err
	}
	if !known {
		return monitorErr(404, "monitor_target_unknown")
	}
	if req.ClusterID != "" {
		var bound bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM cluster_inventory_sources WHERE cluster=? AND instance=? AND asset_id=?)`, req.Cluster, req.Instance, req.ClusterID).Scan(&bound); err != nil {
			return err
		}
		if !bound {
			return monitorErr(403, "monitor_source_outside_cluster")
		}
	}
	return nil
}
func (h *GatewayHub) HandleAssetMonitor(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r, err)
		return
	}
	var result any
	switch r.Method {
	case http.MethodGet:
		result, err = h.store.readAssetMonitor(r.Context(), auth, r.URL.Query())
	case http.MethodPost:
		var req monitorRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.DisallowUnknownFields()
		if dec.Decode(&req) != nil || dec.Decode(new(any)) != io.EOF {
			err = monitorErr(400, "invalid_monitor_json")
		} else {
			result, err = h.changeAssetMonitor(r.Context(), auth, req)
		}
	default:
		w.Header().Set("Allow", "GET, POST")
		err = monitorErr(405, "method_not_allowed")
	}
	if err != nil {
		e := inventoryFailure{503, "asset_monitor_unavailable"}
		var known inventoryFailure
		if errors.As(err, &known) {
			e = known
		}
		writeDoopsError(w, e.status, "", doopsErrorResponse{Error: e.code, Code: e.code, Message: e.code})
		return
	}
	writeJSONHTTP(w, result)
}
func (h *GatewayHub) changeAssetMonitor(ctx context.Context, auth TokenAuth, req monitorRequest) (any, error) {
	native := req.Action == "collect"
	if !native && req.Action != "ingest" && req.Action != "rule" {
		return nil, monitorErr(400, "invalid_monitor_action")
	}
	// External assertions and rule edits require a platform administrator. A target reader
	// may trigger only the fixed, server-selected Agent collection with no caller samples.
	pre, err := h.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	err = req.authorize(ctx, pre, auth, !native)
	pre.Rollback()
	if err != nil {
		return nil, err
	}
	session := "monitor-" + randomHex(10)
	id, err := h.store.StartAudit(AuditEvent{UserID: auth.UserID, TokenID: auth.TokenID, Cluster: req.Cluster, Instance: req.Instance, Action: GatewayAction("asset_monitor:" + req.Action), Session: session})
	if err != nil {
		return nil, err
	}
	result, err := h.applyAssetMonitor(ctx, auth, req, session, id)
	if err != nil {
		if auditErr := h.store.FinishAudit(id, AuditFinish{Status: "error", Error: "asset_monitor_request_failed"}); auditErr != nil {
			return nil, auditErr
		}
	}
	return result, err
}
func (h *GatewayHub) applyAssetMonitor(ctx context.Context, auth TokenAuth, req monitorRequest, session string, audit int64) (any, error) {
	native := req.Action == "collect"
	samples := req.Samples
	origin := "exporter"
	var generation uint64
	switch req.Action {
	case "collect":
		if !inventoryNamespacePattern.MatchString(req.Namespace) || samples != nil || req.Type != "" || req.AssetID != "" || req.Origin != "" || req.Metric != "" || req.DeviceID != "" || req.Threshold != nil || req.Duration != 0 {
			return nil, monitorErr(400, "invalid_monitor_collection")
		}
		agent := h.getAgent(req.Cluster, req.Instance)
		if agent == nil {
			return nil, monitorErr(503, "target_offline")
		}
		generation = agent.Generation
		var err error
		samples, err = h.collectMonitorPods(ctx, auth, req, session)
		if err != nil {
			return nil, err
		}
		origin = "k8s_top"
	case "ingest":
		if len(samples) == 0 || len(samples) > 4096 || req.monitorAsset != (monitorAsset{}) || req.Metric != "" || req.DeviceID != "" || req.Threshold != nil || req.Duration != 0 {
			return nil, monitorErr(400, "invalid_monitor_batch")
		}
	case "rule":
		if samples != nil || !validMonitorDevice(req.Metric, req.DeviceID) || !monitorValidAsset(req.monitorAsset) || (req.Origin != "exporter" && req.Origin != "k8s_top") || monitorUnits[req.Metric] == "" || req.Threshold == nil || !monitorValidValue(req.Metric, *req.Threshold) || req.Duration < 1 || req.Duration > 604800 {
			return nil, monitorErr(400, "invalid_monitor_rule")
		}
	}
	now := time.Now()
	for _, p := range samples {
		if p.Origin != "" || !p.valid(now) {
			return nil, monitorErr(400, "invalid_monitor_sample")
		}
	}
	tx, err := h.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`UPDATE audit_events SET status='executing' WHERE id=?`, audit); err != nil {
		return nil, err
	}
	if err = req.authorize(ctx, tx, auth, !native); err != nil {
		return nil, err
	}
	if native {
		var online bool
		if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM agent_status WHERE cluster=? AND instance=? AND generation=? AND status='online')`, req.Cluster, req.Instance, generation).Scan(&online); err != nil {
			return nil, err
		}
		if !online {
			return nil, monitorErr(409, "monitor_target_changed")
		}
	}
	if req.Action == "rule" {
		err = saveMonitorRule(tx, req, now.UnixMilli())
	} else {
		for _, p := range samples {
			if err = storeMonitorPoint(tx, req, p, origin); err != nil {
				break
			}
		}
	}
	if err != nil {
		return nil, err
	}
	if err = finishAuditWith(tx, audit, AuditFinish{Status: "success"}); err != nil {
		return nil, err
	}
	return map[string]any{"action": req.Action, "accepted": len(samples)}, tx.Commit()
}
