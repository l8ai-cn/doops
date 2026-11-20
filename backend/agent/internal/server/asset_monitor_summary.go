package server

import (
	"context"
	"database/sql"
	"net/url"
	"strconv"
	"time"
)

const monitorAlertPageSize = 25

// Each row describes one source-qualified metric, including the physical GPU.
type monitorIncident struct {
	monitorAsset
	Cluster    string `json:"cluster"`
	Instance   string `json:"instance"`
	Connection string `json:"connection"`
	Metric     string `json:"metric"`
	DeviceID   string `json:"device_id,omitempty"`
	State      string `json:"state"`
	Reason     string `json:"reason"`
	At         int64  `json:"last_sample_ms"`
	Opened     *int64 `json:"opened_ms,omitempty"`
}
type monitorSummary struct {
	Open        int               `json:"open"`
	Missing     int               `json:"missing"`
	Sources     int               `json:"sources"`
	Unmonitored int               `json:"unmonitored_sources"`
	To          int64             `json:"to_ms"`
	PageSize    int               `json:"page_size"`
	Items       []monitorIncident `json:"items"`
}

// Resolve grants before considering either counts or identities. The latest point
// and open incident are indexed lookups; historical samples are never enumerated.
func (s *GatewayStore) readMonitorSummary(ctx context.Context, auth TokenAuth, q url.Values) (monitorSummary, error) {
	now := time.Now().UnixMilli()
	result := monitorSummary{To: now, PageSize: monitorAlertPageSize, Items: []monitorIncident{}}
	for k, v := range q {
		if (k != "view" && k != "cluster_id" && k != "state" && k != "page") || len(v) != 1 {
			return result, monitorErr(400, "invalid_monitor_summary_query")
		}
	}
	state := q.Get("state")
	if q.Get("view") != "alerts" || (q.Has("state") && state != "open" && state != "missing") || (q.Has("cluster_id") && !monitorID(q.Get("cluster_id"))) {
		return result, monitorErr(400, "invalid_monitor_summary_query")
	}
	page := int64(1)
	if q.Has("page") {
		n, err := strconv.ParseInt(q.Get("page"), 10, 32)
		if err != nil || n < 1 || state == "" {
			return result, monitorErr(400, "invalid_monitor_summary_page")
		}
		page = n
	}
	args := []any{auth.UserID, auth.TokenID}
	cte := inventoryGrants + `, sources AS (
 SELECT a.cluster,a.instance,a.status FROM agent_status a WHERE EXISTS(SELECT 1 FROM permitted p WHERE p.cluster IN ('*',a.cluster) AND p.instance IN ('*',a.instance))`
	if q.Has("cluster_id") {
		cte += ` AND EXISTS(SELECT 1 FROM cluster_inventory_sources c WHERE c.cluster=a.cluster AND c.instance=a.instance AND c.asset_id=?)`
		args = append(args, q.Get("cluster_id"))
	}
	cte += `), observed AS (
 SELECT s.id,s.type,s.asset_id,s.namespace,s.origin,s.cluster,s.instance,src.status AS connection,s.metric,s.device_id,p.at_ms,a.opened_ms,
 CASE WHEN p.status='not_applicable' THEN '' WHEN p.status='missing' THEN 'missing' WHEN p.at_ms<? THEN 'stale' WHEN a.opened_ms IS NOT NULL THEN 'threshold' ELSE '' END AS reason
 FROM sources src JOIN asset_monitor_series s ON s.cluster=src.cluster AND s.instance=src.instance
 JOIN asset_monitor_samples p ON p.series_id=s.id AND p.at_ms=(SELECT MAX(last.at_ms) FROM asset_monitor_samples last WHERE last.series_id=s.id)
 LEFT JOIN asset_monitor_alerts a ON a.series_id=s.id AND a.state='open'
 ), classified AS (SELECT *,CASE WHEN reason='threshold' THEN 'open' WHEN reason IN ('stale','missing') THEN 'missing' ELSE '' END AS state FROM observed) `
	args = append(args, monitorStaleCutoff(now))
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	err = tx.QueryRowContext(ctx, cte+`SELECT count(*) FILTER(WHERE state='open'),count(*) FILTER(WHERE state='missing'),(SELECT count(*) FROM sources),
 (SELECT count(*) FROM sources src WHERE NOT EXISTS(SELECT 1 FROM asset_monitor_series s WHERE s.cluster=src.cluster AND s.instance=src.instance)) FROM classified`, args...).Scan(&result.Open, &result.Missing, &result.Sources, &result.Unmonitored)
	if err != nil || state == "" {
		return result, err
	}
	rows, err := tx.QueryContext(ctx, cte+`SELECT type,asset_id,namespace,origin,cluster,instance,connection,metric,device_id,state,reason,at_ms,opened_ms FROM classified WHERE state=? ORDER BY id LIMIT ? OFFSET ?`, append(args, state, monitorAlertPageSize, (page-1)*monitorAlertPageSize)...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var item monitorIncident
		if err := rows.Scan(&item.Type, &item.AssetID, &item.Namespace, &item.Origin, &item.Cluster, &item.Instance, &item.Connection, &item.Metric, &item.DeviceID, &item.State, &item.Reason, &item.At, &item.Opened); err != nil {
			return result, err
		}
		result.Items = append(result.Items, item)
	}
	return result, rows.Err()
}
