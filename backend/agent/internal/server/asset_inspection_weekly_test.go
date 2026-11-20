package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func weeklyFixture(t *testing.T) *machineFixture {
	t.Helper()
	f := newMachineFixture(t)
	for _, name := range []string{"cluster_inventory", "business_inventory", "asset_inspection"} {
		b, err := os.ReadFile("../../../docs/migrations/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		f.exec(t, string(b))
	}
	return f
}
func TestAssetInspectionWeeklyScopeAndCalendar(t *testing.T) {
	f := weeklyFixture(t)
	now := time.Date(2026, 11, 16, 0, 30, 0, 0, time.UTC)
	slot := now.Add(-32 * time.Hour).Truncate(time.Hour).Unix()
	next := slot + 7*86400
	_, op := createCredentialHTTPUser(t, f.store, "weekly-east-reader", false)
	var viewer TokenAuth
	if err := f.store.db.QueryRow(`SELECT u.id,t.id FROM users u JOIN tokens t ON t.user_id=u.id WHERE u.name='weekly-east-reader'`).Scan(&viewer.UserID, &viewer.TokenID); err != nil {
		t.Fatal(err)
	}
	var admin TokenAuth
	if err := f.store.db.QueryRow(`SELECT u.id,t.id FROM users u JOIN tokens t ON t.user_id=u.id WHERE u.name='inventory-admin'`).Scan(&admin.UserID, &admin.TokenID); err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{"east", "west"} {
		f.exec(t, `INSERT INTO agent_status(cluster,instance,status,updated_at) VALUES(?,'node','online','')`, src)
		if err := f.store.GrantUser(f.userID, ScopeGrant{src, "node"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.GrantUser(viewer.UserID, ScopeGrant{"east", "node"}); err != nil {
		t.Fatal(err)
	}
	addPlan := func(id int, cursor int64, z string, active int) {
		t.Helper()
		f.exec(t, `INSERT INTO inspection_plans(id,name,owner,executor,timezone,weekday,clock,next_due,active) VALUES(?,?,'owner',?,?,6,'08:00',?,?)`, id, fmt.Sprint("plan-", id), f.userID, z, cursor, active)
	}
	addScope := func(plan int, id, src, check string) {
		t.Helper()
		f.exec(t, `INSERT INTO inspection_scope VALUES(?,?,?,'node',?,?)`, plan, id, src, src, check)
		f.exec(t, `INSERT OR IGNORE INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES(?,?,'vm',?,'node')`, id, id, src)
	}
	addRun := func(id, plan int, due int64, kind string) {
		t.Helper()
		f.exec(t, `INSERT INTO inspection_runs VALUES(?,?,?,?,?)`, id, plan, due, due, kind)
	}
	addItem := func(run int, id, src, check, status string) {
		t.Helper()
		f.exec(t, `INSERT INTO inspection_items(run_id,asset_id,cluster,instance,cluster_id,check_id,opened,deadline,status) VALUES(?,?,?,'node',?,?,?,?,?)`, run, id, src, src, check, slot, now.Unix()+86400, status)
	}
	addPlan(1, next, "America/Los_Angeles", 0)
	addRun(1, 1, slot, "scheduled")
	for _, entry := range [][3]string{{"A", "M1", "pass"}, {"B", "A1", "insufficient"}, {"C", "M1", "finding"}, {"C", "A1", "pending"}, {"D", "A1", "missing"}} {
		id, check, status := entry[0], entry[1], entry[2]
		src := "east"
		if id == "D" {
			src = "west"
		}
		addScope(1, id, src, check)
		addItem(1, id, src, check, status)
	}
	addPlan(2, slot, "America/Los_Angeles", 1)
	addScope(2, "E", "east", "M1")
	addRun(2, 2, slot, "manual")
	addItem(2, "E", "east", "M1", "pass")
	addPlan(3, now.Add(2*time.Hour).Unix(), "UTC", 1)
	addScope(3, "F", "east", "M1")
	addScope(3, "A", "east", "M1")
	addPlan(4, next, "UTC", 0)
	addRun(4, 4, slot, "scheduled")
	addScope(4, "last-week", "east", "M1")
	addItem(4, "last-week", "east", "M1", "pass")
	get := func(a TokenAuth, scope string) inspectionWeekSummary {
		t.Helper()
		tx, err := f.store.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		s, err := inspectionWeekly(tx, a, scope, now)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := get(admin, "")
	if s.Planned != 6 || s.Completed != 0 || s.Missed != 2 || s.Partial != 3 || s.Pending != 1 {
		t.Fatalf("weekly counts %+v", s)
	}
	found := false
	for _, r := range s.Rows {
		if r.Asset == "E" {
			found = r.Run == 0 && r.ReconcileDue == slot && r.Missing == 1
		}
	}
	if !found {
		t.Fatal("manual masked overdue plan")
	}
	for _, query := range []struct {
		a     TokenAuth
		scope string
	}{{viewer, ""}, {admin, "east"}} {
		s = get(query.a, query.scope)
		if s.Planned != 5 || s.Missed != 1 || len(s.Rows) != 6 {
			t.Fatalf("scope drift %+v", s)
		}
	}
	if s = get(viewer, "west"); s.Planned != 0 || len(s.Rows) > 0 {
		t.Fatal("restricted cluster leaked")
	}
	r := inspectionTestHTTP(t, f)(op, "?view=weekly&cluster_id=west", nil, 200)
	if r["planned"] != float64(0) {
		t.Fatal(r)
	}
}

func TestAssetInspectionScheduledRunnerReportProof(t *testing.T) {
	f := weeklyFixture(t)
	call := inspectionTestHTTP(t, f)
	f.exec(t, `INSERT INTO agent_status(cluster,instance,status,updated_at) VALUES('local','gateway','online','')`)
	f.exec(t, `INSERT INTO inventory_machines(asset_id,name,kind,cluster,instance) VALUES('QA-DB','local test DB','vm','local','gateway')`)
	if err := f.store.GrantUser(f.userID, ScopeGrant{"local", "gateway"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	slot := now.Truncate(time.Minute)
	p := call(f.admin, "", inspectionRequest{Action: "create", Name: "Local database weekly check", Owner: "QA", Executor: f.userID, Zone: "UTC", Weekday: int(now.Weekday()), Clock: slot.Format("15:04"), Scope: []inspectionScope{{"QA-DB", "local", "gateway", []string{"A2"}}}}, 200)
	id := int(p["plan_id"].(float64))
	manual := call(f.admin, "", map[string]any{"action": "run", "plan_id": id}, 200)["run_id"]
	f.exec(t, `UPDATE inspection_plans SET next_due=? WHERE id=?`, slot.Unix(), id)
	dir := t.TempDir()
	folder := filepath.Join(dir, "local", "gateway", "QA-DB")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(dir, "executor")
	if err := os.WriteFile(token, []byte(f.user), 0600); err != nil {
		t.Fatal(err)
	}
	// Publish a real passive, read-only database measurement after the runner's
	// tick. Forward every request to the real temporary Gateway without changing responses.
	remote, err := url.Parse(f.server.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(remote)
	logged := make(chan string, 12)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logged <- r.Method + " " + r.URL.RequestURI()
		if r.Method == "GET" && r.URL.Query().Get("view") == "work" {
			var result string
			if err := f.store.db.QueryRow("PRAGMA quick_check").Scan(&result); err != nil {
				http.Error(w, "local measurement failed", 500)
				return
			}
			violations := 0
			if result != "ok" {
				violations = 1
			}
			raw, err := json.Marshal(map[string]any{"asset_id": "QA-DB", "cluster": "local", "instance": "gateway", "check_id": "A2", "source": "sqlite-quick-check", "observed_at": time.Now().UTC(), "policy": "quick_check returns ok", "detail": result, "samples": 1, "violations": violations})
			if err != nil || os.WriteFile(filepath.Join(folder, "A2.json"), raw, 0600) != nil {
				http.Error(w, "publish local measurement failed", 500)
				return
			}
		}
		proxy.ServeHTTP(w, r)
	}))
	defer bridge.Close()
	runner := exec.Command("python3", "-B", "../../../inspection/runner.py")
	runner.Env = append(os.Environ(), "INSPECTION_GATEWAY="+bridge.URL, "INSPECTION_TOKEN_FILE="+token, "INSPECTION_EVIDENCE_ROOT="+dir)
	out, err := runner.CombinedOutput()
	if err != nil {
		t.Fatalf("runner: %v %s", err, out)
	}
	if len(logged) != 4 || <-logged != "POST /v1/assets/inspections" || <-logged != "GET /v1/assets/inspections?view=work" {
		t.Fatal("unexpected external runner sequence")
	}
	weekly := call(f.user, "?view=weekly", nil, 200)
	if weekly["planned"] != float64(1) || weekly["completed"] != float64(1) {
		t.Fatal(weekly)
	}
	row := weekly["rows"].([]any)[0].(map[string]any)
	scheduled := row["run_id"]
	if scheduled == manual || row["trigger_kind"] != "scheduled" {
		t.Fatal("manual run incorrectly credited")
	}
	verify := func(run any, want bool) {
		t.Helper()
		report := call(f.user, fmt.Sprintf("?view=download&run_id=%.0f", run), nil, 200)
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "report.json")
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("python3", "-B", "../../../test/asset_inspection_scheduled_evidence.py", path)
		out, err := cmd.CombinedOutput()
		if (err == nil) != want {
			t.Fatalf("verifier: %s %v", out, err)
		}
		if want {
			var proof struct {
				Kind  string `json:"trigger_kind"`
				Count int    `json:"original_observed_items"`
			}
			if err := json.Unmarshal(out, &proof); err != nil || proof.Kind != "scheduled" || proof.Count != 1 || !strings.Contains(string(out), "sqlite-quick-check") {
				t.Fatalf("invalid proof %s", out)
			}
			t.Logf("external runner -> scheduled run %.0f -> scoped report; read-only SQLite observation confirmed", run)
		}
	}
	verify(scheduled, true)
	verify(manual, false)
}
