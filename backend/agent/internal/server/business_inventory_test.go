package server

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestBusinessInventoryHTTP(t *testing.T) {
	const path = "/v1/assets/systems"
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	op := f.user(t, "system-reader", ScopeGrant{Cluster: "east", Instance: "a"})
	get := func(token CreatedToken) []inventoryBusinessSystem {
		t.Helper()
		var response struct {
			Systems []inventoryBusinessSystem `json:"systems"`
		}
		body := f.http(t, token, "GET", path, "", 200)
		if err := json.Unmarshal(body, &response); err != nil || response.Systems == nil {
			t.Fatalf("bad systems response %s", body)
		}
		return response.Systems
	}
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := f.store.db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	f.http(t, CreatedToken{}, "GET", path, "", 401)
	f.http(t, f.admin, "GET", path, "", 503)
	migration, err := os.ReadFile("../../../docs/migrations/business_inventory.sql")
	if err != nil {
		t.Fatal(err)
	}
	exec(string(migration))
	exec(string(migration))
	if len(get(f.admin)) != 0 {
		t.Fatal("expected empty ledger")
	}
	for _, instance := range []string{"a", "b"} {
		if err := f.store.MarkAgentOnline(AgentStatus{Cluster: "east", Instance: instance}); err != nil {
			t.Fatal(err)
		}
	}
	a := inventoryBusinessDeployment{Cluster: "east", Instance: "a", Namespace: "orders", Application: "orders-api", Database: "shared-pg"}
	b := inventoryBusinessDeployment{Cluster: "east", Instance: "b", Namespace: "billing", Application: "billing-api", Database: "shared-pg"}
	active := true
	sys := businessSystemInput{AssetID: "SYS-1", Name: "orders", Owner: "ops", Active: &active, Deployments: []inventoryBusinessDeployment{a, a, b}}
	post := func(token CreatedToken, input businessSystemInput, revision int64, status int) []byte {
		t.Helper()
		req := businessInventoryRequest{Action: "create", System: &input}
		if revision > 0 {
			req.Action = "update"
			req.ExpectedRevision = &revision
		}
		data, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		return f.http(t, token, "POST", path, string(data), status)
	}
	post(op, sys, 0, 403)
	post(f.admin, sys, 0, 200)
	post(f.admin, sys, 0, 409)
	standalone := sys
	standalone.AssetID = "SYS-2"
	standalone.Deployments = nil
	post(f.admin, standalone, 0, 200)
	private := sys
	private.AssetID = "SYS-3"
	private.Deployments = []inventoryBusinessDeployment{b}
	post(f.admin, private, 0, 200)
	rows := get(f.admin)
	if len(rows) != 3 || rows[0].Revision != 1 || len(rows[0].Deployments) != 2 || rows[0].Deployments[0].VerifiedAt == "" || len(rows[1].Deployments) != 0 {
		t.Fatalf("administrator ledger %+v", rows)
	}
	original := rows[0].Deployments[0].VerifiedAt
	visible := get(op)
	if len(visible) != 1 || visible[0].AssetID != "SYS-1" || len(visible[0].Deployments) != 1 || visible[0].Deployments[0].Application != "orders-api" {
		t.Fatalf("scoped ledger %+v", visible)
	}
	sys.Owner = "updated-owner"
	sys.Deployments = nil
	post(f.admin, sys, 1, 200)
	post(f.admin, sys, 1, 409)
	rows = get(f.admin)
	if rows[0].Revision != 2 || rows[0].Owner != sys.Owner || len(rows[0].Deployments) != 2 || rows[0].Deployments[0].VerifiedAt != original {
		t.Fatalf("metadata edit changed evidence %+v", rows[0])
	}
	unknown := a
	unknown.Instance = "absent"
	sys.Deployments = []inventoryBusinessDeployment{unknown}
	post(f.admin, sys, 2, 400)
	forged := a
	forged.VerifiedAt = "2020-01-01"
	sys.Deployments = []inventoryBusinessDeployment{forged}
	post(f.admin, sys, 2, 400)
	optional := a
	optional.Database = ""
	sys.Deployments = []inventoryBusinessDeployment{optional}
	exec(`CREATE TRIGGER reject_business_audit BEFORE UPDATE ON audit_events WHEN NEW.action='inventory:system:update' AND NEW.status='success' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
	post(f.admin, sys, 2, 503)
	rows = get(f.admin)
	if rows[0].Revision != 2 || len(rows[0].Deployments) != 2 || rows[0].Deployments[0].Database != "shared-pg" {
		t.Fatalf("partial write after rejected audit %+v", rows[0])
	}
	exec(`DROP TRIGGER reject_business_audit`)
	post(f.admin, sys, 2, 200)
	visible = get(op)
	if len(visible) != 1 || len(visible[0].Deployments) != 1 || visible[0].Deployments[0].Database != "" {
		t.Fatalf("replacement failed %+v", visible)
	}
	archived := b
	archived.Application = "archived-app"
	exec(`UPDATE agent_status SET archived_at='2026-01-01' WHERE cluster='east' AND instance='b'`)
	sys.Deployments = []inventoryBusinessDeployment{archived}
	post(f.admin, sys, 3, 400)
	auth, err := f.store.VerifyUserToken(op.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	exec(`DELETE FROM grants WHERE user_id=?`, auth.UserID)
	if len(get(op)) != 0 {
		t.Fatal("revoked target still visible")
	}
	sys.Deployments = []inventoryBusinessDeployment{}
	*sys.Active = false
	post(f.admin, sys, 3, 200)
	rows = get(f.admin)
	if rows[0].Active || len(rows[0].Deployments) != 0 || rows[0].Revision != 4 {
		t.Fatalf("retirement and removal failed %+v", rows[0])
	}
	var audited int
	err = f.store.db.QueryRow(`SELECT count(*) FROM audit_events WHERE action='inventory:system:update' AND command_summary LIKE '%relation_source=manual%' AND status='success'`).Scan(&audited)
	if err != nil || audited != 3 {
		t.Fatalf("unexpected committed audit count %d %v", audited, err)
	}
	if body := f.http(t, op, "POST", path, `{"action":"update","user_admin":true}`, 403); strings.Contains(string(body), "SYS-1") {
		t.Fatal("denied write disclosed record")
	}
}
