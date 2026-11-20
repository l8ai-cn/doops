package server

import (
	"database/sql"
	"fmt"
	"time"
)

// dueInWeek uses Monday as the calendar boundary. For a missing DST wall minute,
// run at the first valid wall minute after it. A repeated minute gets one slot.
func inspectionSlot(now time.Time, zone string, weekday int, clock string) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" || zone == "Local" || weekday < 0 || weekday > 6 {
		return time.Time{}, inspectErr(400, "invalid IANA timezone or weekday (Sunday=0)")
	}
	hm, err := time.Parse("15:04", clock)
	if err != nil {
		return time.Time{}, inspectErr(400, "clock must be HH:MM")
	}
	local := now.In(loc)
	offset := (int(local.Weekday()) + 6) % 7
	date := local.AddDate(0, 0, (weekday+6)%7-offset)
	midnight := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, loc)
	start := midnight.Add(-3 * time.Hour)
	want := hm.Hour()*60 + hm.Minute()
	for i := 0; i < 30*60; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if at.Year() == date.Year() && at.YearDay() == date.YearDay() && at.Hour()*60+at.Minute() >= want {
			return at, nil
		}
	}
	return time.Time{}, inspectErr(400, "no valid wall time on scheduled day")
}
func followingInspection(slot time.Time, zone string, wd int, clock string) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, err
	}
	return inspectionSlot(slot.In(loc).AddDate(0, 0, 7), zone, wd, clock)
}
func inspectionPlan(tx *sql.Tx, q inspectionRequest, now time.Time) (any, error) {
	if !inspectText(q.Name, 128) || !inspectText(q.Owner, 120) || !safeMachineID(q.Executor) || len(q.Scope) < 1 || len(q.Scope) > 64 {
		return nil, inspectErr(400, "name, owner, executor user ID and 1–64 scoped assets required")
	}
	slot, err := inspectionSlot(now, q.Zone, q.Weekday, q.Clock)
	if err != nil {
		return nil, err
	}
	if slot.Before(now) {
		slot, err = followingInspection(slot, q.Zone, q.Weekday, q.Clock)
		if err != nil {
			return nil, err
		}
	}
	var enabled bool
	if err = tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM users WHERE id=? AND disabled=0)`, q.Executor).Scan(&enabled); err != nil {
		return nil, err
	}
	if !enabled {
		return nil, inspectErr(400, "executor account unavailable")
	}
	var id int64
	if q.Action == "create" {
		err = tx.QueryRow(`INSERT INTO inspection_plans(name,owner,executor,timezone,weekday,clock,next_due) VALUES(?,?,?,?,?,?,?) RETURNING id`, q.Name, q.Owner, q.Executor, q.Zone, q.Weekday, q.Clock, slot.Unix()).Scan(&id)
		if err != nil {
			return nil, err
		}
	}
	seen := map[[4]string]bool{}
	preview := []map[string]any{}
	for _, a := range q.Scope {
		if !safeMachineID(a.Asset) || !safeMachineID(a.Cluster) || !safeMachineID(a.Instance) || len(a.Checks) < 1 || len(a.Checks) > len(inspectionChecks) {
			return nil, inspectErr(400, "invalid asset, exact target or checklist")
		}
		var binding, permitted, online bool
		var clusterID string
		err = tx.QueryRow(`SELECT `+inspectBinding+`,`+inspectExecAllowed+`,`+inspectOnline+`,COALESCE((SELECT asset_id FROM cluster_inventory_sources c WHERE c.cluster=s.cluster AND c.instance=s.instance),'') FROM (SELECT ? AS asset_id,? AS cluster,? AS instance) s CROSS JOIN (SELECT ? AS executor) p`, a.Asset, a.Cluster, a.Instance, q.Executor).Scan(&binding, &permitted, &online, &clusterID)
		if err != nil {
			return nil, err
		}
		if !binding {
			return nil, inspectErr(400, "asset is not bound to the selected target")
		}
		if !permitted && q.Action == "create" {
			return nil, inspectErr(403, "executor has no grant on selected target")
		}
		for _, check := range a.Checks {
			k := [4]string{a.Asset, a.Cluster, a.Instance, check}
			if inspectionChecks[check] == "" || seen[k] {
				return nil, inspectErr(400, "unknown or duplicate checklist entry")
			}
			seen[k] = true
			if id != 0 {
				if _, err = tx.Exec(`INSERT INTO inspection_scope VALUES(?,?,?,?,?,?)`, id, a.Asset, a.Cluster, a.Instance, clusterID, check); err != nil {
					return nil, err
				}
			}
		}
		preview = append(preview, map[string]any{"asset_id": a.Asset, "cluster": a.Cluster, "instance": a.Instance, "checks": a.Checks, "authorized": permitted, "online": online})
	}
	return map[string]any{"plan_id": id, "next_due": slot.Unix(), "scope": preview}, nil
}
func inspectionFreeze(tx *sql.Tx, plan int64, due, now time.Time, mode string) (int64, error) {
	var id int64
	err := tx.QueryRow(`SELECT id FROM inspection_runs WHERE plan_id=? AND due=? AND (trigger_kind='manual')=?`, plan, due.Unix(), mode == "manual").Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	err = tx.QueryRow(`INSERT INTO inspection_runs(plan_id,due,frozen,trigger_kind) VALUES(?,?,?,?) RETURNING id`, plan, due.Unix(), now.Unix(), mode).Scan(&id)
	if err != nil {
		return 0, err
	}
	deadline := due.Add(24 * time.Hour)
	if mode == "manual" {
		deadline = now.Add(24 * time.Hour)
	}
	_, err = tx.Exec(`INSERT INTO inspection_items(run_id,asset_id,cluster,instance,cluster_id,check_id,opened,deadline,status,reason) SELECT ?,s.asset_id,s.cluster,s.instance,s.cluster_id,s.check_id,?,?,CASE WHEN ? OR `+inspectBlock+`<>'' THEN 'missing' ELSE 'pending' END,CASE WHEN ? THEN CASE WHEN NOT EXISTS(SELECT 1 FROM users u WHERE u.id=p.executor AND u.disabled=0) THEN 'executor_disabled' ELSE 'scheduler_missed_window' END ELSE `+inspectBlock+` END FROM inspection_scope s JOIN inspection_plans p ON p.id=s.plan_id WHERE p.id=?`, id, now.Unix(), deadline.Unix(), !deadline.After(now), !deadline.After(now), plan)
	return id, err
}
func inspectionSchedule(tx *sql.Tx, auth TokenAuth, q inspectionRequest, now time.Time) (any, error) {
	if q.Action == "tick" {
		var activeToken bool
		if err := tx.QueryRow(inventoryGrants+`SELECT EXISTS(SELECT 1 FROM actor)`, auth.UserID, auth.TokenID).Scan(&activeToken); err != nil {
			return nil, err
		}
		if !activeToken {
			return nil, inspectErr(403, "inactive inspection executor")
		}
		n := 0
		for ; n < 32; n++ {
			var id, due int64
			var zone, clock string
			var wd int
			err := tx.QueryRow(`SELECT id,next_due,timezone,weekday,clock FROM inspection_plans WHERE active=1 AND executor=? AND next_due<=? ORDER BY next_due,id LIMIT 1`, auth.UserID, now.Unix()).Scan(&id, &due, &zone, &wd, &clock)
			if err == sql.ErrNoRows {
				break
			}
			if err != nil {
				return nil, err
			}
			old := time.Unix(due, 0)
			if _, err = inspectionFreeze(tx, id, old, now, "scheduled"); err != nil {
				return nil, err
			}
			next, err := followingInspection(old, zone, wd, clock)
			if err != nil {
				return nil, err
			}
			if !next.After(old) {
				return nil, fmt.Errorf("schedule did not advance")
			}
			if _, err = tx.Exec(`UPDATE inspection_plans SET next_due=? WHERE id=?`, next.Unix(), id); err != nil {
				return nil, err
			}
		}
		_, err := tx.Exec(`UPDATE inspection_items AS s SET status='missing',reason=CASE WHEN s.deadline<=? THEN 'collection_deadline_elapsed' ELSE `+inspectBlock+` END FROM inspection_runs r JOIN inspection_plans p ON p.id=r.plan_id WHERE s.run_id=r.id AND p.executor=? AND s.status='pending' AND (s.deadline<=? OR `+inspectBlock+`<>'')`, now.Unix(), auth.UserID, now.Unix())
		return map[string]int{"cycles": n}, err
	}
	var zone, clock string
	var wd, active int
	var nextDue int64
	err := tx.QueryRow(`SELECT timezone,weekday,clock,active,next_due FROM inspection_plans WHERE id=?`, q.Plan).Scan(&zone, &wd, &clock, &active, &nextDue)
	if err == sql.ErrNoRows {
		return nil, inspectErr(404, "plan not found")
	}
	if err != nil {
		return nil, err
	}
	if q.Action == "reconcile" {
		var id int64
		err = tx.QueryRow(`SELECT id FROM inspection_runs WHERE plan_id=? AND due=? AND trigger_kind<>'manual'`, q.Plan, q.Due).Scan(&id)
		if err == nil {
			return map[string]int64{"run_id": id}, nil
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		if active == 0 || nextDue != q.Due || now.Unix() < nextDue+86400 {
			return nil, inspectErr(409, "only the oldest expired collection window can be reconciled")
		}
		expired := time.Unix(nextDue, 0)
		id, err = inspectionFreeze(tx, q.Plan, expired, now, "reconciled")
		if err != nil {
			return nil, err
		}
		next, err := followingInspection(expired, zone, wd, clock)
		if err != nil {
			return nil, err
		}
		_, err = tx.Exec(`UPDATE inspection_plans SET next_due=? WHERE id=?`, next.Unix(), q.Plan)
		return map[string]int64{"run_id": id}, err
	}
	if q.Action == "pause" && active != 0 && now.Unix() >= nextDue+86400 {
		return nil, inspectErr(409, "reconcile overdue cycles before pausing")
	}
	slot, err := inspectionSlot(now, zone, wd, clock)
	if err != nil {
		return nil, err
	}
	if q.Action == "run" {
		if active == 0 {
			return nil, inspectErr(409, "plan paused")
		}
		id, err := inspectionFreeze(tx, q.Plan, slot, now, "manual")
		return map[string]int64{"run_id": id}, err
	}
	if q.Action == "pause" {
		_, err = tx.Exec(`UPDATE inspection_plans SET active=0 WHERE id=?`, q.Plan)
	} else if active == 0 {
		if slot.Before(now) {
			slot, err = followingInspection(slot, zone, wd, clock)
			if err != nil {
				return nil, err
			}
		}
		_, err = tx.Exec(`UPDATE inspection_plans SET active=1,next_due=? WHERE id=?`, slot.Unix(), q.Plan)
	}
	return map[string]int64{"plan_id": q.Plan}, err
}
