package server

import (
	"context"
	"database/sql"
	"net/url"
	"strconv"
	"time"
)

type monitorAlert struct {
	Opened    int64   `json:"opened_ms"`
	Ended     *int64  `json:"ended_ms"`
	State     string  `json:"state"`
	Threshold float64 `json:"threshold"`
	Duration  int     `json:"duration_seconds"`
}
type monitorSeries struct {
	DeviceID string         `json:"device_id,omitempty"`
	Metric   string         `json:"metric"`
	Unit     string         `json:"unit"`
	Latest   monitorPoint   `json:"latest"`
	Stale    bool           `json:"stale"`
	Points   []monitorPoint `json:"points"`
	Rule     *monitorRule   `json:"rule"`
	Alerts   []monitorAlert `json:"alerts"`
}

func (s *GatewayStore) readAssetMonitor(ctx context.Context, auth TokenAuth, q url.Values) (any, error) {
	if q.Has("view") {
		return s.readMonitorSummary(ctx, auth, q)
	}
	for k, v := range q {
		switch k {
		case "cluster", "instance", "cluster_id", "namespace", "type", "asset_id", "origin", "metric", "device_id", "range", "end_ms":
		default:
			return nil, monitorErr(400, "invalid_monitor_query")
		}
		if len(v) != 1 {
			return nil, monitorErr(400, "invalid_monitor_query")
		}
	}
	req := monitorRequest{Cluster: q.Get("cluster"), Instance: q.Get("instance"), ClusterID: q.Get("cluster_id"), monitorAsset: monitorAsset{Type: q.Get("type"), AssetID: q.Get("asset_id"), Namespace: q.Get("namespace"), Origin: q.Get("origin")}, Metric: q.Get("metric"), DeviceID: q.Get("device_id")}
	r := q.Get("range")
	if !q.Has("range") {
		r = "1h"
	}
	window, ok := monitorRanges[r]
	if !ok || (q.Has("device_id") && (req.Metric == "" || !validMonitorDevice(req.Metric, req.DeviceID))) || (req.Type != "" && !monitorKinds[req.Type]) || (req.Namespace != "" && !inventoryNamespacePattern.MatchString(req.Namespace)) || (req.Origin != "" && req.Origin != "k8s_top" && req.Origin != "exporter") || (req.Metric != "" && monitorUnits[req.Metric] == "") {
		return nil, monitorErr(400, "invalid_monitor_query")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err = req.authorize(ctx, tx, auth, false); err != nil {
		return nil, err
	}
	to := time.Now().UnixMilli()
	if q.Has("end_ms") {
		end, err := strconv.ParseInt(q.Get("end_ms"), 10, 64)
		if err != nil || end <= 0 || end > to || req.AssetID == "" {
			return nil, monitorErr(400, "invalid_monitor_window_end")
		}
		to = end
	}
	if req.AssetID == "" {
		rows, err := tx.Query(`SELECT DISTINCT type,asset_id,namespace,origin FROM asset_monitor_series WHERE cluster=? AND instance=? AND (?='' OR type=?) AND (?=0 OR namespace=?) AND (?='' OR origin=?) AND (?='' OR metric=?) AND (?=0 OR device_id=?) ORDER BY type,namespace,asset_id,origin`, req.Cluster, req.Instance, req.Type, req.Type, q.Has("namespace"), req.Namespace, req.Origin, req.Origin, req.Metric, req.Metric, q.Has("device_id"), req.DeviceID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		assets := []monitorAsset{}
		for rows.Next() {
			var a monitorAsset
			if err = rows.Scan(&a.Type, &a.AssetID, &a.Namespace, &a.Origin); err != nil {
				return nil, err
			}
			assets = append(assets, a)
		}
		return map[string]any{"assets": assets}, rows.Err()
	}
	if !monitorValidAsset(req.monitorAsset) || req.Origin == "" {
		return nil, monitorErr(400, "monitor_detail_requires_type_asset_origin")
	}
	from := to - window.Milliseconds()
	list := []monitorSeries{}
	rows, err := tx.Query(`SELECT id,metric,device_id FROM asset_monitor_series WHERE cluster=? AND instance=? AND type=? AND asset_id=? AND namespace=? AND origin=? AND (?='' OR metric=?) AND (?=0 OR device_id=?) ORDER BY metric,device_id`, req.Cluster, req.Instance, req.Type, req.AssetID, req.Namespace, req.Origin, req.Metric, req.Metric, q.Has("device_id"), req.DeviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var metric, device string
		if err := rows.Scan(&id, &metric, &device); err != nil {
			return nil, err
		}
		series, err := readMonitorSeries(tx, id, metric, from, to)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return nil, err
		}
		series.DeviceID = device
		list = append(list, series)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"from_ms": from, "to_ms": to, "max_gap_ms": monitorGap.Milliseconds(), "asset": req.monitorAsset, "series": list}, nil
}
func monitorStaleCutoff(at int64) int64 { return at - monitorGap.Milliseconds() }

func readMonitorSeries(tx *sql.Tx, id int64, metric string, from, to int64) (monitorSeries, error) {
	s := monitorSeries{Metric: metric, Unit: monitorUnits[metric], Points: []monitorPoint{}, Alerts: []monitorAlert{}}
	p := &s.Latest
	if err := tx.QueryRow(`SELECT at_ms,status,value FROM asset_monitor_samples WHERE series_id=? AND at_ms<=? ORDER BY at_ms DESC LIMIT 1`, id, to).Scan(&p.At, &p.Status, &p.Value); err != nil {
		return s, err
	}
	s.Stale = p.Status != "not_applicable" && p.At < monitorStaleCutoff(to)
	rows, err := tx.Query(`SELECT at_ms,status,value FROM asset_monitor_samples WHERE series_id=? AND at_ms BETWEEN ? AND ? ORDER BY at_ms LIMIT 25001`, id, from, to)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	for rows.Next() {
		var v monitorPoint
		if err = rows.Scan(&v.At, &v.Status, &v.Value); err != nil {
			return s, err
		}
		s.Points = append(s.Points, v)
	}
	if err = rows.Err(); err != nil {
		return s, err
	}
	if len(s.Points) > 25000 {
		return s, monitorErr(422, "monitor_window_too_dense")
	}
	var rule monitorRule
	err = tx.QueryRow(`SELECT threshold,duration_seconds FROM asset_monitor_rules WHERE series_id=?`, id).Scan(&rule.Threshold, &rule.Duration)
	if err == nil {
		s.Rule = &rule
	} else if err != sql.ErrNoRows {
		return s, err
	}
	logs, err := tx.Query(`SELECT opened_ms,ended_ms,state,threshold,duration_seconds FROM asset_monitor_alerts WHERE series_id=? AND (ended_ms IS NULL OR ended_ms>=?) AND opened_ms<=? ORDER BY opened_ms DESC`, id, from, to)
	if err != nil {
		return s, err
	}
	defer logs.Close()
	for logs.Next() {
		var a monitorAlert
		if err = logs.Scan(&a.Opened, &a.Ended, &a.State, &a.Threshold, &a.Duration); err != nil {
			return s, err
		}
		if a.Ended != nil && *a.Ended > to {
			a.Ended = nil
			a.State = "open"
		}
		if a.State == "open" && s.Stale {
			end := p.At + monitorGap.Milliseconds()
			a.State = "interrupted"
			a.Ended = &end
			if end < from {
				continue
			}
		}
		s.Alerts = append(s.Alerts, a)
	}
	return s, logs.Err()
}
