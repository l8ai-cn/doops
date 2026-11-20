package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func inspectionTestHTTP(t *testing.T, f *machineFixture) func(string, string, any, int) map[string]any {
	t.Helper()
	return func(token, query string, body any, want int) map[string]any {
		t.Helper()
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		method := "GET"
		if body != nil {
			method = "POST"
		}
		req, err := http.NewRequest(method, f.server.URL+"/v1/assets/inspections"+query, bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := f.server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != want {
			t.Fatalf("%s %s: HTTP %d want %d: %s", method, query, resp.StatusCode, want, raw)
		}
		var result map[string]any
		if err = json.Unmarshal(raw, &result); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(query, "view=download") && resp.StatusCode == 200 && !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
			t.Fatal("not a report download")
		}
		return result
	}
}
func TestAssetInspectionExecutorHTTP(t *testing.T) {
	f := newMachineFixture(t)
	call := inspectionTestHTTP(t, f)
	call(f.admin, "", nil, 503)
	for _, file := range []string{"cluster_inventory", "business_inventory", "asset_inspection", "asset_inspection"} {
		raw, err := os.ReadFile("../../../docs/migrations/" + file + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, string(raw))
	}
	otherUser, other := createCredentialHTTPUser(t, f.store, "inspection-other", false)
	_, viewer := createCredentialHTTPUser(t, f.store, "inspection-viewer", false)
	for _, instance := range []string{"one", "two"} {
		f.exec(t, `INSERT INTO agent_status(cluster,instance,status,updated_at) VALUES('site',?,'online','')`, instance)
		f.exec(t, `INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES(?,?,'vm','site',?)`, instance, instance, instance)
		if err := f.store.GrantUser(f.userID, ScopeGrant{"site", instance}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.GrantUser(otherUser.ID, ScopeGrant{"site", "one"}); err != nil {
		t.Fatal(err)
	}
	var checks []string
	for _, c := range []string{"M1", "M2", "M3", "M4", "M5", "V1", "V2", "V3", "A1", "A2", "A3", "A4", "A5"} {
		checks = append(checks, c)
	}
	scope := []inspectionScope{{"one", "site", "one", checks}, {"two", "site", "two", []string{"M1"}}}
	now := time.Now().UTC()
	q := inspectionRequest{Action: "create", Name: "防守周检", Owner: "owner", Executor: f.userID, Zone: "UTC", Weekday: int(now.Weekday()), Clock: now.Format("15:04"), Scope: scope}
	call(f.user, "", q, 403)
	q.Action = "preview"
	call(f.admin, "", q, 200)
	q.Action = "create"
	plan := int(call(f.admin, "", q, 200)["plan_id"].(float64))
	q.Executor = otherUser.ID
	q.Scope = scope[:1]
	q.Name = "another executor"
	call(f.admin, "", q, 200)
	slot, err := inspectionSlot(now, "UTC", int(now.Weekday()), now.Format("15:04"))
	if err != nil {
		t.Fatal(err)
	}
	manual := int(call(f.admin, "", map[string]any{"action": "run", "plan_id": plan}, 200)["run_id"].(float64))
	if int(call(f.admin, "", map[string]any{"action": "run", "plan_id": plan}, 200)["run_id"].(float64)) != manual {
		t.Fatal("manual replay duplicated run")
	}
	f.exec(t, `UPDATE inspection_plans SET next_due=?`, slot.Unix())
	tick := map[string]any{"action": "tick"}
	if call(viewer, "", tick, 200)["cycles"] != float64(0) || call(f.user, "", tick, 200)["cycles"] != float64(1) || call(f.user, "", tick, 200)["cycles"] != float64(0) {
		t.Fatal("tick identity or replay")
	}
	if len(call(other, "?view=work", nil, 200)["work"].([]any)) != 0 {
		t.Fatal("advanced another executor")
	}
	var run int
	if err := f.store.db.QueryRow(`SELECT id FROM inspection_runs WHERE plan_id=? AND trigger_kind='scheduled'`, plan).Scan(&run); err != nil || run == manual {
		t.Fatal("manual preempted weekly slot", err)
	}
	report := fmt.Sprintf("?view=download&run_id=%d", run)
	call(viewer, report, nil, 404)
	if len(call(other, report, nil, 200)["items"].([]any)) != 13 {
		t.Fatal("mixed-target export")
	}
	if len(call(f.admin, report, nil, 200)["items"].([]any)) != 14 {
		t.Fatal("replay duplicated frozen manifest")
	}
	jobs := call(f.user, "?view=work", nil, 200)["work"].([]any)
	ids := map[string]int{}
	for _, entry := range jobs {
		v := entry.(map[string]any)
		if int(v["run_id"].(float64)) != run {
			continue
		}
		ids[v["instance"].(string)+v["check_id"].(string)] = int(v["item_id"].(float64))
	}
	e := map[string]any{"observed_at": time.Now().UTC(), "source": "test-fixture", "gap": "no source"}
	payload := func(id, gen int, e any) map[string]any {
		return map[string]any{"action": "evidence", "item_id": id, "generation": gen, "evidence": e}
	}
	call(other, "", payload(ids["oneM3"], 1, e), 403)
	call(f.user, "", payload(ids["oneM3"], 1, e), 200)
	call(f.user, "", payload(ids["oneM3"], 1, e), 200)
	e["gap"] = "different receipt"
	call(f.user, "", payload(ids["oneM3"], 1, e), 409)
	f.exec(t, `DELETE FROM grants WHERE user_id=? AND instance='two'`, f.userID)
	call(f.user, "", payload(ids["twoM1"], 1, e), 404)
	call(f.user, "", tick, 200)
	hidden := call(f.user, report, nil, 200)
	if len(hidden["items"].([]any)) != 13 {
		t.Fatal("revocation not enforced")
	}
	all := call(f.admin, report, nil, 200)
	var sawGap bool
	for _, entry := range all["items"].([]any) {
		i := entry.(map[string]any)
		if i["instance"] == "two" {
			sawGap = i["reason"] == "executor_grant_revoked"
		}
	}
	if !sawGap {
		t.Fatal("lost revoked target gap")
	}
	root := t.TempDir()
	folder := filepath.Join(root, "site", "one", "one")
	if err = os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	file := func(path string, value any) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "token"), []byte(f.user), 0600); err != nil {
		t.Fatal(err)
	}
	advisories := filepath.Join(t.TempDir(), "advisories.json")
	file(advisories, map[string]any{"issued_at": now.Add(-time.Hour), "valid_until": now.Add(time.Hour), "advisories": []any{map[string]any{"component": "pkg:generic/widget", "advisory": "TEST-ONLY-17", "kind": "vulnerability", "introduced": "1.0.0", "fixed": "2.10.0"}}})
	bom := func(v string) {
		file(filepath.Join(folder, "sbom.json"), map[string]any{"bomFormat": "CycloneDX", "metadata": map[string]any{"timestamp": time.Now().UTC(), "properties": []any{map[string]string{"name": "doops:asset_id", "value": "one"}, map[string]string{"name": "doops:instance", "value": "one"}, map[string]string{"name": "doops:cluster", "value": "site"}}}, "components": []any{map[string]string{"purl": "pkg:generic/widget@" + v, "version": v}}})
	}
	bom("2.9.9")
	file(filepath.Join(folder, "A2.json"), map[string]any{"check_id": "A2", "asset_id": "one", "cluster": "site", "instance": "one", "source": "fixture-db-status", "policy": "connection budget", "observed_at": time.Now().UTC(), "samples": 1, "violations": 0, "detail": "test connection count 1 of 10"})
	runner := func() {
		t.Helper()
		c := exec.Command("python3", "-B", "../../../inspection/runner.py")
		c.Env = append(os.Environ(), "INSPECTION_GATEWAY="+f.server.URL, "INSPECTION_TOKEN_FILE="+filepath.Join(root, "token"), "INSPECTION_EVIDENCE_ROOT="+root, "INSPECTION_ADVISORY_FILE="+advisories)
		log, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("runner %s %v", log, err)
		}
		t.Logf("runner %s", log)
	}
	runner()
	item := ids["oneV1"]
	call(other, "", map[string]any{"action": "repair", "item_id": item, "generation": 1, "memo": "test deployment record"}, 200)
	call(other, "", map[string]any{"action": "recheck", "item_id": item, "generation": 1}, 200)
	time.Sleep(1100 * time.Millisecond)
	bom("2.10.0")
	runner()
	all = call(other, report, nil, 200)
	var states []string
	var sawSummary bool
	for _, entry := range all["history"].([]any) {
		h := entry.(map[string]any)
		if int(h["item_id"].(float64)) == ids["oneA2"] {
			sawSummary = h["evidence_category"] == "collector_summary" && h["source"] == "fixture-db-status"
		}
		if int(h["item_id"].(float64)) == item {
			if h["evidence_category"] != "offline_advisory_comparison" || h["source"] != "deployed-cyclonedx" {
				t.Fatal("software evidence attribution", h)
			}
			states = append(states, h["status"].(string))
		}
	}
	if !sawSummary || strings.Join(states, ",") != "finding,pass" {
		t.Fatalf("version history: %v", states)
	}
	f.exec(t, `UPDATE inspection_plans SET next_due=? WHERE id=?`, slot.AddDate(0, 0, -7).Unix(), plan)
	call(f.user, "", tick, 200)
	var n int
	if err = f.store.db.QueryRow(`SELECT count(*) FROM inspection_items WHERE reason='scheduler_missed_window'`).Scan(&n); err != nil || n == 0 {
		t.Fatal("lost missed week", err)
	}
	expired := slot.AddDate(0, 0, -14).Unix()
	f.exec(t, `UPDATE users SET disabled=1 WHERE id=?`, f.userID)
	f.exec(t, `UPDATE inspection_plans SET next_due=? WHERE id=?`, expired, plan)
	adminList := call(f.admin, "", nil, 200)
	var overdue bool
	for _, row := range adminList["plans"].([]any) {
		p := row.(map[string]any)
		if int(p["plan_id"].(float64)) == plan {
			overdue = p["overdue"] == float64(1) && p["executor_disabled"] == float64(1)
		}
	}
	if !overdue {
		t.Fatal("admin did not see overdue disabled executor")
	}
	cmd := map[string]any{"action": "reconcile", "plan_id": plan, "due": expired}
	call(other, "", cmd, 403)
	rid := call(f.admin, "", cmd, 200)["run_id"]
	if call(f.admin, "", cmd, 200)["run_id"] != rid {
		t.Fatal("reconciliation is not idempotent")
	}
	book := call(f.admin, fmt.Sprintf("?view=report&run_id=%.0f", rid), nil, 200)
	if len(book["history"].([]any)) != 0 || len(book["items"].([]any)) != 14 {
		t.Fatal("reconciliation created evidence or lost items")
	}
	for _, row := range book["items"].([]any) {
		i := row.(map[string]any)
		if i["status"] != "missing" || i["reason"] != "executor_disabled" {
			t.Fatal("reconciliation created executable item", i)
		}
	}
	var audited int
	if err = f.store.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='inspection:reconcile' AND user_id<>? AND status='success'`, otherUser.ID).Scan(&audited); err != nil || audited != 2 {
		t.Fatal("missing reconciliation audit", err)
	}
	cmd["due"] = now.Add(7 * 24 * time.Hour).Unix()
	call(f.admin, "", cmd, 409)

}

// Drive the same scheduling transaction on opposite sides of a weekly due time,
// independent of the calendar date and wall clock of the machine running CI.
func TestAssetInspectionManualBeforeWeeklySlot(t *testing.T) {
	f := newMachineFixture(t)
	for _, name := range []string{"cluster_inventory", "business_inventory", "asset_inspection"} {
		raw, e := os.ReadFile("../../../docs/migrations/" + name + ".sql")
		if e != nil {
			t.Fatal(e)
		}
		f.exec(t, string(raw))
	}
	early := time.Date(2030, 1, 7, 8, 0, 0, 0, time.UTC)
	due := early.Add(time.Hour)
	f.exec(t, `INSERT INTO inspection_plans(id,name,owner,executor,timezone,weekday,clock,next_due) VALUES(1,'early','owner',?,'UTC',1,'09:00',?)`, f.userID, due.Unix())
	f.exec(t, `INSERT INTO inspection_scope VALUES(1,'asset','c','i','','M1')`)
	var tokenID string
	if err := f.store.db.QueryRow(`SELECT id FROM tokens WHERE user_id=?`, f.userID).Scan(&tokenID); err != nil {
		t.Fatal(err)
	}
	a := TokenAuth{UserID: f.userID, TokenID: tokenID}
	step := func(action string, at time.Time) {
		t.Helper()
		tx, err := f.store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, err = inspectionSchedule(tx, a, inspectionRequest{Action: action, Plan: 1}, at); err != nil {
			t.Fatal(err)
		}
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	step("run", early)
	step("run", early)
	step("tick", early)
	var n int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM inspection_runs`).Scan(&n); err != nil || n != 1 {
		t.Fatal("premature scheduled run", n, err)
	}
	step("tick", due)
	step("tick", due)
	if err := f.store.db.QueryRow(`SELECT count(DISTINCT trigger_kind) FROM inspection_runs WHERE due=?`, due.Unix()).Scan(&n); err != nil || n != 2 {
		t.Fatal("missing independent scheduled run", n, err)
	}
}
