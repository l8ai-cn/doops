package server

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// The reverse connection, enrollment, HTTP routes and SQLite store are real.
// Only the Linux host's published evidence is a fixture.
func TestMachineDiagnosticsClaimedTunnel(t *testing.T) {
	f, file, at := machineDiagnosticFixture(t)
	t.Setenv("DOOPS_AGENT_MACHINE_DIAGNOSTICS_ROOT", os.Getenv("DOOPS_MACHINE_DIAGNOSTICS_ROOT"))
	t.Setenv("DOOPS_MACHINE_DIAGNOSTICS_ROOT", "")
	if _, err := f.store.db.Exec(`DELETE FROM machine_diagnostic_bindings`); err != nil {
		t.Fatal(err)
	}
	reader := f.user(t, "diag-reader", ScopeGrant{Cluster: "east", Instance: "a"})
	admin := f.user(t, "diag-admin", ScopeGrant{Cluster: "east", Instance: "a"})
	if _, err := f.store.db.Exec(`UPDATE users SET is_admin=1 WHERE name='diag-admin'`); err != nil {
		t.Fatal(err)
	}
	const path = "/v1/assets/machine-diagnostics"
	const query = path + "?asset_id=host1&cluster=east&instance=a&range=1h"
	const refresh = `{"action":"refresh","asset_id":"host1","cluster":"east","instance":"a"}`
	fp := strings.Repeat("a", 64)
	bind := `{"action":"bind","asset_id":"host1","cluster":"east","instance":"a","host_sha256":"` + fp + `"}`
	f.http(t, reader, "POST", path, bind, 403)
	f.http(t, admin, "POST", path, bind, 409)
	enroll, err := f.store.CreateAgentEnrollment("east", "a")
	if err != nil {
		t.Fatal(err)
	}
	connect := func() *GatewayAgent {
		prior := f.hub.getAgent("east", "a")
		conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(f.server.URL, "http")+"/v1/agent/connect?cluster=east&instance=a", http.Header{"Authorization": {"Bearer " + enroll.Plaintext}})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() { defer close(done); NewGateway().ServeGatewayWebSocketConnWithReady(conn, "machine-diag", nil) }()
		t.Cleanup(func() { conn.Close(); <-done })
		requireEventually(t, 3*time.Second, func() bool { a := f.hub.getAgent("east", "a"); return a != nil && a != prior && a.TokenID == enroll.ID })
		return f.hub.getAgent("east", "a")
	}
	first := connect()
	f.http(t, admin, "POST", path, strings.Replace(bind, fp, strings.Repeat("c", 64), 1), 409)
	if body := f.http(t, admin, "GET", query, "", 200); !strings.Contains(string(body), "binding_missing") {
		t.Fatalf("bad fingerprint created a binding: %s", body)
	}
	// Two simultaneous confirmations produce exactly one binding and one successful audit.
	statuses := make(chan int, 2)
	for range 2 {
		go func() {
			req, _ := http.NewRequest("POST", f.server.URL+path, strings.NewReader(bind))
			req.Header.Set("Authorization", "Bearer "+admin.Plaintext)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				statuses <- 0
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			statuses <- resp.StatusCode
		}()
	}
	a, b := <-statuses, <-statuses
	if !(a == 200 && b == 409 || a == 409 && b == 200) {
		t.Fatalf("concurrent binding HTTP %d %d", a, b)
	}
	if body := f.http(t, reader, "POST", path, refresh, 200); !strings.Contains(string(body), `"new_sample":false`) {
		t.Fatalf("replayed evidence: %s", body)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var sample diagnosticSample
	if err = json.Unmarshal(raw, &sample); err != nil {
		t.Fatal(err)
	}
	publish := func(stamp int64) {
		sample.At = stamp
		data, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(filepath.Dir(file), strconv.FormatInt(stamp/60000%diagSlots, 10)+".json"), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// An older payload of the same minute may never replace newer evidence.
	publish(at - 1)
	f.http(t, reader, "POST", path, refresh, 409)
	publish(at)
	second := connect()
	if first.Generation >= second.Generation {
		t.Fatal("generation did not advance")
	}
	publish(time.Now().UnixMilli())
	f.http(t, reader, "POST", path, refresh, 200)
	if b := string(f.http(t, reader, "GET", query, "", 200)); !strings.Contains(b, `"generation":`+strconv.FormatUint(second.Generation, 10)) || strings.Contains(b, fp) {
		t.Fatalf("invalid sample provenance: %s", b)
	}
	if err = f.store.RevokeAgentEnrollment(enroll.ID); err != nil {
		t.Fatal(err)
	}
	f.http(t, reader, "POST", path, refresh, 409)
	if _, err = f.store.db.Exec(`DELETE FROM grants WHERE user_id=(SELECT id FROM users WHERE name='diag-reader')`); err != nil {
		t.Fatal(err)
	}
	f.http(t, reader, "GET", query, "", 403)
	f.http(t, reader, "POST", path, refresh, 403)
	if _, err = f.store.db.Exec(`UPDATE inventory_machines SET instance='b' WHERE asset_id='host1'`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = f.store.db.QueryRow(`SELECT (SELECT count(*) FROM machine_diagnostic_bindings)+(SELECT count(*) FROM machine_diagnostic_samples)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("rebind retained evidence: %d %v", n, err)
	}
	if err = f.store.db.QueryRow(`SELECT count(*) FROM audit_events WHERE session='machine-diagnostic' AND status='success' AND command_summary LIKE 'host1 generation=%'`).Scan(&n); err != nil || n != 3 {
		t.Fatalf("missing successful audit %d %v", n, err)
	}
}
