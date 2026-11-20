package server

import (
	"database/sql"
	"sort"
	"time"
)

type weeklyItemCounts struct {
	Items        int `json:"items"`
	Checked      int `json:"checked"`
	Missing      int `json:"missing"`
	Insufficient int `json:"insufficient"`
}

func (n weeklyItemCounts) state() string {
	switch {
	case n.Missing > 0:
		return "missed"
	case n.Checked == n.Items:
		return "completed"
	case n.Checked+n.Insufficient > 0:
		return "partial"
	default:
		return "pending"
	}
}

type weeklyAssetRow struct {
	weeklyItemCounts
	Asset        string `json:"asset_id"`
	Plan         int64  `json:"plan_id"`
	Name         string `json:"name"`
	Timezone     string `json:"timezone"`
	Due          int64  `json:"due"`
	Run          int64  `json:"run_id,omitempty"`
	Trigger      string `json:"trigger_kind,omitempty"`
	Status       string `json:"status"`
	ReconcileDue int64  `json:"reconcile_due,omitempty"`
}
type inspectionWeekSummary struct {
	Planned   int              `json:"planned"`
	Completed int              `json:"completed"`
	Missed    int              `json:"missed"`
	Partial   int              `json:"partial"`
	Pending   int              `json:"pending"`
	Rows      []weeklyAssetRow `json:"rows"`
	AsOf      time.Time        `json:"as_of"`
}

func inspectionLocalWeek(now time.Time, zone string) (time.Time, time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	d := now.In(loc)
	monday := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc).AddDate(0, 0, -(int(d.Weekday())+6)%7)
	return monday, monday.AddDate(0, 0, 7), nil
}
func inspectionWeekly(tx *sql.Tx, a TokenAuth, scope string, now time.Time) (inspectionWeekSummary, error) {
	summary := inspectionWeekSummary{Rows: []weeklyAssetRow{}, AsOf: now.UTC()}
	args := []any{a.UserID, a.TokenID, scope, scope}
	filter := inspectAllowed + ` AND (?='' OR s.cluster_id=?)`
	// This UTC bound covers any local calendar week, including an offset transition.
	// The plan's local week below determines inclusion. Both queries use this read transaction.
	stmt := inventoryGrants + `, scoped AS (SELECT r.id AS run_id,r.plan_id,r.trigger_kind,r.due,p.name,p.timezone,s.asset_id,` + inspectionStateAt("?") + ` AS status ` + inspectionJoins + ` WHERE ` + filter + ` AND r.trigger_kind IN ('scheduled','reconciled') AND r.due BETWEEN ? AND ?) SELECT run_id,plan_id,trigger_kind,due,name,timezone,asset_id,count(*),sum(status IN ('pass','finding')),sum(status='missing'),sum(status='insufficient') FROM scoped GROUP BY run_id,asset_id`
	rows, err := tx.Query(stmt, a.UserID, a.TokenID, now.Unix(), scope, scope, now.Add(-8*24*time.Hour).Unix(), now.Add(8*24*time.Hour).Unix())
	if err != nil {
		return summary, err
	}
	defer rows.Close()
	frozen := map[int64]bool{}
	for rows.Next() {
		var row weeklyAssetRow
		if err := rows.Scan(&row.Run, &row.Plan, &row.Trigger, &row.Due, &row.Name, &row.Timezone, &row.Asset, &row.Items, &row.Checked, &row.Missing, &row.Insufficient); err != nil {
			return summary, err
		}
		start, end, err := inspectionLocalWeek(now, row.Timezone)
		if err != nil {
			return summary, err
		}
		if row.Due >= start.Unix() && row.Due < end.Unix() {
			frozen[row.Plan] = true
			summary.Rows = append(summary.Rows, row)
		}
	}
	if err := rows.Err(); err != nil {
		return summary, err
	}
	// The cursor can still point at a previous week when a runner has stopped. In
	// that case include this week's obligation as well, without writing a run.
	plans, err := tx.Query(inventoryGrants+`SELECT p.id,p.name,p.timezone,p.weekday,p.clock,p.next_due,s.asset_id,count(*),EXISTS(SELECT 1 FROM actor WHERE is_admin=1) FROM inspection_scope s JOIN inspection_plans p ON p.id=s.plan_id WHERE p.active=1 AND `+filter+` GROUP BY p.id,s.asset_id ORDER BY p.id`, args...)
	if err != nil {
		return summary, err
	}
	defer plans.Close()
	var lastPlan int64
	var due int64
	for plans.Next() {
		var row weeklyAssetRow
		var weekday int
		var clock string
		var cursor int64
		var admin bool
		if err := plans.Scan(&row.Plan, &row.Name, &row.Timezone, &weekday, &clock, &cursor, &row.Asset, &row.Items, &admin); err != nil {
			return summary, err
		}
		if frozen[row.Plan] {
			continue
		}
		if lastPlan != row.Plan {
			start, end, err := inspectionLocalWeek(now, row.Timezone)
			if err != nil {
				return summary, err
			}
			lastPlan = row.Plan
			due = cursor
			switch {
			case cursor >= end.Unix():
				due = 0
			case cursor < start.Unix():
				slot, err := inspectionSlot(now, row.Timezone, weekday, clock)
				if err != nil {
					return summary, err
				}
				due = slot.Unix()
			}
		}
		if due == 0 {
			continue
		}
		row.Due = due
		if due+86400 <= now.Unix() {
			row.Missing = row.Items
			if admin {
				row.ReconcileDue = cursor
			}
		}
		summary.Rows = append(summary.Rows, row)
	}
	if err := plans.Err(); err != nil {
		return summary, err
	}
	totals := map[string]weeklyItemCounts{}
	for i := range summary.Rows {
		r := &summary.Rows[i]
		r.Status = r.state()
		n := totals[r.Asset]
		n.Items += r.Items
		n.Missing += r.Missing
		n.Checked += r.Checked
		n.Insufficient += r.Insufficient
		totals[r.Asset] = n
	}
	summary.Planned = len(totals)
	for _, n := range totals {
		switch n.state() {
		case "missed":
			summary.Missed++
		case "completed":
			summary.Completed++
		case "partial":
			summary.Partial++
		case "pending":
			summary.Pending++
		}
	}
	sort.Slice(summary.Rows, func(i, j int) bool {
		x, y := summary.Rows[i], summary.Rows[j]
		if x.Due != y.Due {
			return x.Due < y.Due
		}
		if x.Plan != y.Plan {
			return x.Plan < y.Plan
		}
		return x.Asset < y.Asset
	})
	return summary, nil
}
