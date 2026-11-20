package server

import (
	"database/sql"
	"errors"
)

const monitorSeriesKey = `cluster=? AND instance=? AND type=? AND asset_id=? AND namespace=? AND origin=? AND metric=? AND device_id=?`

func monitorKey(req monitorRequest, a monitorAsset, origin, metric, device string) []any {
	return []any{req.Cluster, req.Instance, a.Type, a.AssetID, a.Namespace, origin, metric, device}
}

func storeMonitorPoint(tx *sql.Tx, req monitorRequest, p monitorInput, origin string) error {
	keys := monitorKey(req, p.monitorAsset, origin, p.Metric, p.DeviceID)
	if _, err := tx.Exec(`INSERT INTO asset_monitor_series(cluster,instance,type,asset_id,namespace,origin,metric,device_id) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, keys...); err != nil {
		return err
	}
	var id int64
	if err := tx.QueryRow(`SELECT id FROM asset_monitor_series WHERE `+monitorSeriesKey, keys...).Scan(&id); err != nil {
		return err
	}
	var last sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(at_ms) FROM asset_monitor_samples WHERE series_id=?`, id).Scan(&last); err != nil {
		return err
	}
	if last.Valid && p.At <= last.Int64 {
		var same bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM asset_monitor_samples WHERE series_id=? AND at_ms=? AND status=? AND value IS ?)`, id, p.At, p.Status, p.Value).Scan(&same); err != nil {
			return err
		}
		if same {
			return nil
		}
		return monitorErr(409, "monitor_sample_conflict")
	}
	if _, err := tx.Exec(`INSERT INTO asset_monitor_samples(series_id,at_ms,status,value) VALUES(?,?,?,?)`, id, p.At, p.Status, p.Value); err != nil {
		return err
	}
	var threshold float64
	var duration, effective int64
	var since sql.NullInt64
	err := tx.QueryRow(`SELECT threshold,duration_seconds,effective_ms,first_above_ms FROM asset_monitor_rules WHERE series_id=?`, id).Scan(&threshold, &duration, &effective, &since)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || p.At < effective {
		return err
	}
	gap := last.Valid && p.At-last.Int64 > monitorGap.Milliseconds()
	if gap || p.Status != "ok" {
		ended := p.At
		if gap {
			ended = last.Int64 + monitorGap.Milliseconds()
		}
		if err := closeMonitorAlert(tx, id, ended, "interrupted"); err != nil {
			return err
		}
		since.Valid = false
	}
	if p.Status == "ok" && *p.Value > threshold {
		if !since.Valid {
			since = sql.NullInt64{Int64: p.At, Valid: true}
		}
		if p.At-since.Int64 >= duration*1000 {
			if _, err := tx.Exec(`INSERT INTO asset_monitor_alerts(series_id,opened_ms,threshold,duration_seconds,state) VALUES(?,?,?,?,'open') ON CONFLICT DO NOTHING`, id, p.At, threshold, duration); err != nil {
				return err
			}
		}
	} else {
		since.Valid = false
		if err := closeMonitorAlert(tx, id, p.At, "recovered"); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE asset_monitor_rules SET first_above_ms=? WHERE series_id=?`, since, id)
	return err
}
func closeMonitorAlert(tx *sql.Tx, id, at int64, state string) error {
	_, err := tx.Exec(`UPDATE asset_monitor_alerts SET ended_ms=?,state=? WHERE series_id=? AND state='open'`, at, state, id)
	return err
}
func saveMonitorRule(tx *sql.Tx, req monitorRequest, at int64) error {
	var id int64
	if err := tx.QueryRow(`SELECT id FROM asset_monitor_series WHERE `+monitorSeriesKey, monitorKey(req, req.monitorAsset, req.Origin, req.Metric, req.DeviceID)...).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return monitorErr(404, "monitor_series_not_found")
		}
		return err
	}
	result, err := tx.Exec(`INSERT INTO asset_monitor_rules(series_id,threshold,duration_seconds,effective_ms) VALUES(?,?,?,?) ON CONFLICT(series_id) DO UPDATE SET threshold=excluded.threshold,duration_seconds=excluded.duration_seconds,effective_ms=excluded.effective_ms,first_above_ms=NULL WHERE threshold<>excluded.threshold OR duration_seconds<>excluded.duration_seconds`, id, req.Threshold, req.Duration, at)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	// Preserve the actual end of an incident if its source stopped before the rule edit.
	var latest int64
	if err := tx.QueryRow(`SELECT MAX(at_ms) FROM asset_monitor_samples WHERE series_id=?`, id).Scan(&latest); err != nil {
		return err
	}
	if at-latest > monitorGap.Milliseconds() {
		return closeMonitorAlert(tx, id, latest+monitorGap.Milliseconds(), "interrupted")
	}
	return closeMonitorAlert(tx, id, at, "reconfigured")
}
