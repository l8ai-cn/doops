package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

const inventoryTestPath = "/v1/assets/machines"

type machineFixture struct {
	path                string
	store               *GatewayStore
	server              *httptest.Server
	admin, user, userID string
}

func (f *machineFixture) open(t *testing.T) {
	t.Helper()
	store, err := OpenGatewayStore(f.path)
	if err != nil {
		t.Fatal(err)
	}
	hub := NewGatewayHub(store, GatewayHubOptions{})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	f.store, f.server = store, srv
	t.Cleanup(func() { srv.Close(); store.Close() })
}
func newMachineFixture(t *testing.T) *machineFixture {
	t.Helper()
	f := &machineFixture{path: filepath.Join(t.TempDir(), "gateway.db")}
	f.open(t)
	for _, name := range []string{"inventory-admin", "operator"} {
		user, err := f.store.CreateUser(name)
		if err != nil {
			t.Fatal(err)
		}
		token, err := f.store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID})
		if err != nil {
			t.Fatal(err)
		}
		if name == "operator" {
			f.user, f.userID = token.Plaintext, user.ID
		} else {
			f.admin = token.Plaintext
			if err := f.store.SetUserAdmin(user.ID, true); err != nil {
				t.Fatal(err)
			}
		}
	}
	f.request(t, f.admin, "GET", "", nil, 503) // Gateway startup must never create the inventory schema.
	migration, err := os.ReadFile("../../../docs/migrations/machine_inventory.sql")
	if err != nil {
		t.Fatal(err)
	}
	f.exec(t, string(migration))
	f.exec(t, string(migration))
	return f
}
func (f *machineFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.store.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
func (f *machineFixture) request(t *testing.T, token, method, query string, body any, expect int, alternatives ...int) []byte {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := body.([]byte); ok {
		payload = raw
	}
	req, err := http.NewRequest(method, f.server.URL+inventoryTestPath+query, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := f.server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if (res.StatusCode != expect && !slices.Contains(alternatives, res.StatusCode)) || !json.Valid(data) {
		t.Fatalf("%s %s: HTTP %d, want %d, body %.400s", method, query, res.StatusCode, expect, data)
	}
	if res.StatusCode >= 400 {
		var failure struct{ Code string }
		if json.Unmarshal(data, &failure) != nil || !regexp.MustCompile(`^[a-z][a-z0-9_]+$`).MatchString(failure.Code) {
			t.Fatalf("missing translation code in %s", data)
		}
	}
	return data
}
func (f *machineFixture) list(t *testing.T, token string) []inventoryMachine {
	t.Helper()
	data := f.request(t, token, "GET", "", nil, 200)
	var list struct{ Machines []inventoryMachine }
	if err := json.Unmarshal(data, &list); err != nil || list.Machines == nil {
		t.Fatalf("bad list %s: %v", data, err)
	}
	return list.Machines
}
func (f *machineFixture) preview(t *testing.T, csv string) inventoryBatch {
	t.Helper()
	data := f.request(t, f.admin, "POST", "", map[string]string{"action": "preview", "csv": csv}, 200)
	var b inventoryBatch
	if err := json.Unmarshal(data, &b); err != nil || !isLowerHex(b.PreviewToken, 64) {
		t.Fatalf("invalid preview: %s", data)
	}
	return b
}
func confirmationRequest(csv, token string) map[string]string {
	return map[string]string{"action": "import", "csv": csv, "preview_token": token}
}
func (f *machineFixture) batch(t *testing.T, action, csv string, want ...string) inventoryBatch {
	t.Helper()
	batch := f.preview(t, csv)
	if action == "import" {
		data := f.request(t, f.admin, "POST", "", confirmationRequest(csv, batch.PreviewToken), 200)
		batch = inventoryBatch{}
		if err := json.Unmarshal(data, &batch); err != nil {
			t.Fatal(err)
		}
	}
	if len(batch.Rows) != len(want) || len(batch.BatchID) != 64 {
		t.Fatalf("bad batch: %+v", batch)
	}
	for i, row := range batch.Rows {
		if row.Line != i+2 || row.Message == "" || row.Code == "" || row.Status != want[i] {
			t.Fatalf("line %d: got %+v, want %s", i+2, row, want[i])
		}
	}
	return batch
}
func (f *machineFixture) save(t *testing.T, action string, m machineInventoryInput, revision int, want int) inventoryMachine {
	t.Helper()
	req := map[string]any{"action": action, "machine": m}
	if revision != 0 {
		req["expected_revision"] = revision
	}
	data := f.request(t, f.admin, "POST", "", req, want)
	var saved struct{ Machine inventoryMachine }
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	return saved.Machine
}

func TestMachineInventoryManualRevisionAndTargetVisibility(t *testing.T) {
	f := newMachineFixture(t)
	f.request(t, "", "GET", "", nil, 401)
	f.request(t, f.admin, "PUT", "", nil, 405)
	host := machineInventoryInput{AssetID: "phy-01", Name: "private host", Kind: "physical", Cluster: "ops", Instance: "private"}
	vm := machineInventoryInput{AssetID: "vm-07", Name: "报表服务", Kind: "vm", HostAssetID: host.AssetID, Cluster: "ops", Instance: "reports"}
	f.save(t, "create", host, 0, 400) // A target name or a grant alone cannot register an agent.
	for _, instance := range []string{"private", "reports"} {
		if err := f.store.MarkAgentOnline(AgentStatus{Cluster: "ops", Instance: instance}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.MarkAgentOffline("ops", "reports", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "ops", Instance: "*"}); err != nil {
		t.Fatal(err)
	}
	if row := f.save(t, "create", host, 0, 200); row.Revision != 1 || row.TargetStatus != "online" {
		t.Fatalf("host: %+v", row)
	}
	saved := f.save(t, "create", vm, 0, 200)
	if saved.TargetStatus != "offline" || saved.LastSeen == "" || saved.HostAssetID != host.AssetID {
		t.Fatalf("VM: %+v", saved)
	}
	f.save(t, "create", vm, 0, 409)
	f.batch(t, "import", "kind,host_asset_id,name,asset_id\nvm,phy-01,报表服务,vm-07\nphysical,,private host,phy-01\n", "duplicate", "duplicate")
	unbound := machineInventoryInput{AssetID: "unbound", Name: vm.Name, Kind: "vm"}
	f.save(t, "create", unbound, 0, 200)
	if got := f.list(t, f.user); len(got) != 2 || got[1].HostAssetID != host.AssetID {
		t.Fatalf("wildcard did not match both bound targets: %+v", got)
	}
	f.exec(t, `DELETE FROM grants WHERE user_id=? AND instance='*'`, f.userID)
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "ops", Instance: "reports"}); err != nil {
		t.Fatal(err)
	}
	data := f.request(t, f.user, "GET", "", nil, 200)
	if !bytes.Contains(data, []byte(vm.AssetID)) || bytes.Contains(data, []byte(host.AssetID)) || bytes.Contains(data, []byte("host_asset_id")) || bytes.Contains(data, []byte(unbound.AssetID)) {
		t.Fatalf("host or unbound asset leaked: %s", data)
	}
	f.request(t, f.user, "POST", "", map[string]any{"action": "create", "machine": unbound}, 403)
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "ops", Instance: "private"}); err != nil {
		t.Fatal(err)
	}
	if got := f.list(t, f.user); len(got) != 2 || got[1].HostAssetID != host.AssetID {
		t.Fatalf("visible host missing: %+v", got)
	}
	f.exec(t, `DELETE FROM grants WHERE user_id=? AND instance='reports'`, f.userID)
	if got := f.list(t, f.user); len(got) != 1 || got[0].AssetID != host.AssetID {
		t.Fatalf("revocation: %+v", got)
	}
	host.Kind = "vm"
	f.save(t, "update", host, 1, 409)
	vm.Name = "updated"
	f.save(t, "update", vm, 0, 400)
	if row := f.save(t, "update", vm, 1, 200); row.Name != vm.Name || row.Revision != 2 {
		t.Fatalf("updated: %+v", row)
	}
	vm.Name = "stale"
	f.save(t, "update", vm, 1, 409)
	vm.HostAssetID = "missing"
	f.save(t, "update", vm, 2, 400)
	vm.HostAssetID = "unbound"
	f.save(t, "update", vm, 2, 400)
	rows := f.list(t, f.admin)
	if len(rows) != 3 || rows[2].Name != "updated" || rows[2].Revision != 2 {
		t.Fatalf("bad final state: %+v", rows)
	}
}

func TestMachineInventoryCSVReorderConflictReplayAndReopen(t *testing.T) {
	f := newMachineFixture(t)
	const header = "asset_id,name,kind,address,cpu_count,memory_gib,disk_gib,host_asset_id\n"
	const host = "phy-01,同名,physical,192.0.2.10,32,256,2048,\n"
	const vm = "vm-07,同名,vm,192.0.2.10,4,16.5,120,phy-01\n"
	const invalid = "bad,Invalid,vm,,-1,,,\n"
	csv := header + vm + host + vm + invalid
	preview := f.batch(t, "preview", csv, "new", "new", "duplicate", "error")
	if len(f.list(t, f.admin)) != 0 {
		t.Fatal("preview wrote machines")
	}
	lookup := "?batch_id=" + preview.BatchID
	f.request(t, f.admin, "GET", lookup, nil, 404)
	f.request(t, f.user, "POST", "", confirmationRequest(csv, preview.PreviewToken), 403)
	imported := f.batch(t, "import", csv, "imported", "imported", "duplicate", "error")
	if imported.BatchID != preview.BatchID || imported.AttemptID == 0 || !strings.Contains(imported.Rows[3].Message, "cpu_count") {
		t.Fatalf("bad receipt %+v", imported)
	}
	stale := f.request(t, f.admin, "POST", "", confirmationRequest(csv, preview.PreviewToken), 409)
	if !bytes.Contains(stale, []byte(`"code":"preview_stale"`)) {
		t.Fatalf("unexpected replay response: %s", stale)
	}
	latest := f.request(t, f.admin, "GET", lookup, nil, 200)
	var stored inventoryBatch
	if err := json.Unmarshal(latest, &stored); err != nil || !reflect.DeepEqual(stored, imported) {
		t.Fatalf("stale replay replaced original receipt: %s", latest)
	}
	replay := f.batch(t, "import", csv, "duplicate", "duplicate", "duplicate", "error")
	if replay.BatchID != imported.BatchID || replay.AttemptID == imported.AttemptID {
		t.Fatal("missing attempt identity")
	}
	f.batch(t, "import", header+host+vm, "duplicate", "duplicate")
	conflict := f.batch(t, "import", header+strings.Replace(host, ",32,", ",8,", 1), "error")
	if row := conflict.Rows[0]; row.Code != "registered_asset_conflict" || row.Field != "cpu_count" {
		t.Fatalf("localizable conflict: %+v", row)
	}
	f.batch(t, "import", "asset_id,name,kind,host_asset_id\norphan,A,vm,reject\nreject,B,physical,\nreject,C,physical,\nself,A,vm,self\nvm-host,A,vm,vm-07\nphysical-with-host,A,physical,phy-01\nnew,B,vm,phy-01\n", "error", "error", "error", "error", "error", "error", "imported")
	f.server.Close()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	rows := f.list(t, f.admin)
	if len(rows) != 3 || rows[1].CPUCount == nil || *rows[1].CPUCount != 32 || rows[2].HostAssetID != "phy-01" || rows[2].MemoryGiB == nil || *rows[2].MemoryGiB != 16.5 {
		t.Fatalf("persisted inventory: %+v", rows)
	}
	f.request(t, f.user, "GET", lookup, nil, 403)
	for _, attempt := range []inventoryBatch{imported, replay} {
		data := f.request(t, f.admin, "GET", fmt.Sprintf("%s&attempt_id=%d", lookup, attempt.AttemptID), nil, 200)
		var got inventoryBatch
		if json.Unmarshal(data, &got) != nil || !reflect.DeepEqual(attempt, got) {
			t.Fatalf("reopened attempt: %s", data)
		}
	}
}

func TestMachineInventoryBoundedInputAndMetadataOnlyAudit(t *testing.T) {
	f := newMachineFixture(t)
	const canary = "token: aaaaaaaaaa"
	header := "asset_id,name,kind\n"
	for _, csv := range []string{header + strings.Repeat("m,a,vm\n", inventoryCSVRows+1), strings.Repeat("x", inventoryCSVBytes+1)} {
		f.request(t, f.admin, "POST", "", confirmationRequest(csv, strings.Repeat("0", 64)), 413)
	}
	f.request(t, f.admin, "POST", "", []byte(strings.Repeat(" ", inventoryJSONBytes+1)), 413)
	for _, csv := range []string{"asset_id,name\n1,x", "asset_id,name,kind,token\na,n,vm," + canary, "asset_id,kind,name,name\na,vm,x,y", header + "good,n,physical\nbad,wrong-width\n", header + "a,\"bad,vm", header} {
		resp := f.request(t, f.admin, "POST", "", confirmationRequest(csv, strings.Repeat("0", 64)), 400)
		if bytes.Contains(resp, []byte(canary)) {
			t.Fatal("error echoed payload")
		}
	}
	for _, raw := range [][]byte{[]byte(`{"action":"create","machine":{"asset_id":"a","name":"x","kind":"vm","token":"` + canary + `"}}`), []byte(`{"action":"` + canary + `"}`), []byte(`{"action":"create","machine":{"asset_id":"a","name":"` + canary + `","kind":"vm"}}`), []byte(`{"action":"create","machine":{"asset_id":"a","name":"x","kind":"vm","address":"http://user:pw@host"}}`), []byte(`{"action":"preview","csv":"a"}{}`), {0xff}} {
		resp := f.request(t, f.admin, "POST", "", raw, 400)
		if bytes.Contains(resp, []byte(canary)) {
			t.Fatal("invalid body echoed canary")
		}
	}
	batch := f.batch(t, "import", header+canary+",valid,vm\nasset,"+canary+",vm\n", "error", "error")
	if batch.Rows[0].AssetID != "" || batch.Rows[0].Code != "invalid_asset_id" || batch.Rows[1].Code != "invalid_name" {
		t.Fatal("canary ID in receipt")
	}
	receipt := f.request(t, f.admin, "GET", "?batch_id="+batch.BatchID, nil, 200)
	if bytes.Contains(receipt, []byte(canary)) {
		t.Fatal("canary in retained receipt")
	}
	f.save(t, "create", machineInventoryInput{AssetID: "vm", Name: "synthetic-private-label", Kind: "vm", Address: "private-address.example"}, 0, 200)
	events, err := f.store.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(events)
	for _, s := range []string{canary, "synthetic-private-label", "private-address.example", header} {
		if bytes.Contains(encoded, []byte(s)) {
			t.Fatal("audit contains input values")
		}
	}
	if len(events) == 0 || events[0].UserID == "" || events[0].TokenID == "" || events[0].CommandSummary != "asset_id=vm" || events[0].Status != "success" {
		t.Fatalf("missing audit evidence: %+v", events)
	}
	invalid := f.request(t, f.admin, "POST", "", map[string]string{"action": "unrecognized_value"}, 400)
	var failure doopsErrorResponse
	if json.Unmarshal(invalid, &failure) != nil || failure.Code != "invalid_inventory_request" {
		t.Fatalf("invalid action code: %s", invalid)
	}
	last, err := f.store.ListAudit(1)
	if err != nil || len(last) != 1 || last[0].Action != "inventory:machine:request" || last[0].Error != "invalid_inventory_request" {
		t.Fatalf("invalid action audit: %+v %v", last, err)
	}
	if rows := f.list(t, f.admin); len(rows) != 1 {
		t.Fatalf("invalid requests wrote machines: %+v", rows)
	}
}

func TestMachineInventoryImportRollbackAndFailedHost(t *testing.T) {
	f := newMachineFixture(t)
	csv := "asset_id,name,kind,host_asset_id\nguest,A,vm,host\nhost,B,physical,\nother,C,physical,\n"
	f.exec(t, `CREATE TRIGGER block_host BEFORE INSERT ON inventory_machines WHEN NEW.asset_id='host' BEGIN SELECT RAISE(ABORT,'synthetic host failure'); END`)
	out := f.batch(t, "import", csv, "error", "error", "imported")
	if !strings.Contains(out.Rows[0].Message, "host_asset_id") {
		t.Fatalf("dependent row: %+v", out.Rows[0])
	}
	f.exec(t, `DROP TRIGGER block_host`)
	token := f.preview(t, csv).PreviewToken
	for _, fault := range []string{`BEFORE INSERT ON audit_events`, `BEFORE UPDATE ON audit_events WHEN NEW.status='success'`, `BEFORE INSERT ON inventory_import_attempts`} {
		f.exec(t, "CREATE TRIGGER fail_closed "+fault+" BEGIN SELECT RAISE(ABORT,'synthetic persistence failure'); END")
		f.request(t, f.admin, "POST", "", confirmationRequest(csv, token), 503)
		f.exec(t, `DROP TRIGGER fail_closed`)
		rows := f.list(t, f.admin)
		if len(rows) != 1 || rows[0].AssetID != "other" {
			t.Fatalf("failed batch committed: %+v", rows)
		}
		var attempts int
		if err := f.store.db.QueryRow(`SELECT count(*) FROM inventory_import_attempts`).Scan(&attempts); err != nil || attempts != 1 {
			t.Fatalf("orphan receipt %d: %v", attempts, err)
		}
	}
	f.batch(t, "import", csv, "imported", "imported", "duplicate")
}

func TestMachineInventoryConcurrentGateways(t *testing.T) {
	f := newMachineFixture(t)
	second := *f
	second.open(t)
	csv := "asset_id,name,kind\nshared,vm,vm\n"
	token := f.preview(t, csv).PreviewToken
	got := make([]string, 2)
	t.Run("simultaneous confirmations", func(t *testing.T) {
		for i, fixture := range []*machineFixture{f, &second} {
			t.Run(fmt.Sprintf("gateway-%d", i), func(t *testing.T) {
				t.Parallel()
				data := fixture.request(t, fixture.admin, "POST", "", confirmationRequest(csv, token), 200, 409)
				if bytes.Contains(data, []byte(`"code":"preview_stale"`)) {
					got[i] = "stale"
					return
				}
				var b inventoryBatch
				if err := json.Unmarshal(data, &b); err != nil || len(b.Rows) != 1 {
					t.Fatalf("response: %s", data)
				}
				got[i] = b.Rows[0].Status
			})
		}
	})
	slices.Sort(got)
	if !slices.Equal(got, []string{"imported", "stale"}) || len(f.list(t, f.admin)) != 1 {
		t.Fatalf("concurrent results: %v", got)
	}
}

func TestMachineInventoryPreviewBeforeConcurrentHostRegistration(t *testing.T) {
	f := newMachineFixture(t)
	other := *f
	other.open(t)
	if err := other.store.SetUserAdmin(other.userID, true); err != nil {
		t.Fatal(err)
	}
	const csv = "asset_id,name,kind,host_asset_id\nguest,Guest,vm,missing-host\nindependent,Other,physical,\n"
	plan := f.batch(t, "preview", csv, "error", "new")
	if plan.PreviewToken != other.preview(t, csv).PreviewToken {
		t.Fatal("identical snapshots produced different tokens")
	}
	other.request(t, other.user, "POST", "", map[string]any{"action": "create", "machine": machineInventoryInput{AssetID: "missing-host", Name: "Host", Kind: "physical"}}, 200)
	response := f.request(t, f.admin, "POST", "", confirmationRequest(csv, plan.PreviewToken), 409)
	if !bytes.Contains(response, []byte(`"code":"preview_stale"`)) {
		t.Fatalf("stale response: %s", response)
	}
	if rows := f.list(t, f.admin); len(rows) != 1 || rows[0].AssetID != "missing-host" {
		t.Fatalf("stale confirmation wrote assets: %+v", rows)
	}
	f.request(t, f.admin, "GET", "?batch_id="+plan.BatchID, nil, 404)
	current := f.batch(t, "preview", csv, "new", "new")
	if current.PreviewToken == plan.PreviewToken || current.BatchID != plan.BatchID {
		t.Fatal("host change was not reflected in preview identity")
	}
	response = f.request(t, f.admin, "POST", "", confirmationRequest(csv, current.PreviewToken), 200)
	var receipt inventoryBatch
	if json.Unmarshal(response, &receipt) != nil || len(receipt.Rows) != 2 || receipt.Rows[0].Status != "imported" || receipt.Rows[1].Status != "imported" || len(f.list(t, f.admin)) != 3 {
		t.Fatalf("fresh confirmation: %s", response)
	}
}

func TestMachineInventoryPreviewTokenBindsCSV(t *testing.T) {
	f := newMachineFixture(t)
	csv := "asset_id,name,kind\na,One,vm\nb,Two,vm\n"
	plan := f.batch(t, "preview", csv, "new", "new")
	for _, input := range []map[string]string{{"action": "import", "csv": csv}, confirmationRequest(csv, "")} {
		r := f.request(t, f.admin, "POST", "", input, 400)
		if !bytes.Contains(r, []byte(`"code":"preview_token_required"`)) {
			t.Fatalf("missing token response: %s", r)
		}
	}
	for _, modified := range []string{strings.Replace(csv, "One", "Other", 1), "asset_id,name,kind\nb,Two,vm\na,One,vm\n"} {
		f.request(t, f.admin, "POST", "", confirmationRequest(modified, plan.PreviewToken), 409)
	}
	f.request(t, f.admin, "POST", "", confirmationRequest(csv, plan.BatchID), 409)
	if len(f.list(t, f.admin)) != 0 {
		t.Fatal("token mismatch created assets")
	}
	f.save(t, "create", machineInventoryInput{AssetID: "unrelated", Name: "Unaffected", Kind: "physical"}, 0, 200)
	f.request(t, f.admin, "POST", "", confirmationRequest(csv, plan.PreviewToken), 200)
}

func TestMachineInventoryRejectsLoginCredentials(t *testing.T) {
	f := newMachineFixture(t)
	const metadata = "asset_id,name,kind\nfresh,Fresh,vm\n"
	const canary = "synthetic-login-canary-a12f"
	const code = "machine_login_credentials_not_supported"
	token := f.preview(t, metadata).PreviewToken
	rejected := 0
	check := func(t *testing.T, request map[string]string, wantCode string) {
		t.Helper()
		data := f.request(t, f.admin, "POST", "", request, 422)
		var failure doopsErrorResponse
		if json.Unmarshal(data, &failure) != nil || failure.Code != wantCode || failure.Error != wantCode || bytes.Contains(data, []byte(canary)) {
			t.Fatal("wrong credential rejection or payload echoed")
		}
		rejected++
	}
	for _, action := range []string{"preview", "import"} {
		t.Run(action, func(t *testing.T) {
			req := map[string]string{"action": action, "scope": "machine_and_credentials", "csv": metadata}
			check(t, req, "machine_login_required") // Report absent credentials even before confirming.
			if action == "import" {
				req["preview_token"] = token
				check(t, req, "machine_login_required")
			}
			delete(req, "scope")
			for _, column := range []string{"username", "account", " SSH_User ", "Password", "登录账号", "服务器初始帐户", "服务器初始账户", "服务器初始密码", "服务器初始帐户/密码"} {
				req["csv"] = "asset_id,name,kind,unrecognized," + column + "\nfresh,Fresh,vm,," + canary + "\n"
				check(t, req, code)
			}
			req["scope"] = "machine_metadata"
			req["csv"] = "asset_id,name,kind,password\nfresh,Fresh,vm," + canary + "\n"
			delete(req, "preview_token")
			check(t, req, code)
		})
	}
	data := f.request(t, f.admin, "POST", "", map[string]string{"action": "preview", "csv": metadata, "scope": canary}, 400)
	if bytes.Contains(data, []byte(canary)) {
		t.Fatal("unknown scope echoed")
	}
	for _, header := range []string{"asset_id,name,kind,password_hint", "asset_id,name,kind,kind", "asset_id,name"} {
		data := f.request(t, f.admin, "POST", "", map[string]string{"action": "preview", "csv": header}, 400)
		var failure doopsErrorResponse
		if err := json.Unmarshal(data, &failure); err != nil || failure.Code != "invalid_csv_header" {
			t.Fatalf("unexpected header rejection: %s", data)
		}
	}
	var machines, attempts, credentials, bindings int
	if err := f.store.db.QueryRow(`SELECT (SELECT count(*) FROM inventory_machines), (SELECT count(*) FROM inventory_import_attempts), (SELECT count(*) FROM credentials), (SELECT count(*) FROM credential_grants)`).Scan(&machines, &attempts, &credentials, &bindings); err != nil || machines != 0 || attempts != 0 || credentials != 0 || bindings != 0 {
		t.Fatalf("rejection wrote inventory or credentials: %d %d %d %d (%v)", machines, attempts, credentials, bindings, err)
	}
	events, err := f.store.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	failures := 0
	for _, event := range events {
		if event.Status == "error" && (event.Error == code || event.Error == "machine_login_required") {
			if event.CorrelationID != "" {
				t.Fatal("unvalidated CSV fingerprint in audit")
			}
			failures++
		}
	}
	encoded, err := json.Marshal(events)
	if err != nil || failures != rejected || bytes.Contains(encoded, []byte(canary)) {
		t.Fatalf("credential rejection audit count %d, want %d, or audit exposed input", failures, rejected)
	}
}

func TestMachineInventoryMetadataReceiptScope(t *testing.T) {
	f := newMachineFixture(t)
	_, err := f.store.CreateCredential(CredentialCreateRequest{Name: "existing", Scope: CredentialScopePersonal, Type: CredentialTypeRegistry, CreatedBy: f.userID, OwnerID: f.userID})
	if err != nil {
		t.Fatal(err)
	}
	checkScope := func(data []byte) inventoryBatch {
		t.Helper()
		var wire struct {
			Scope            string
			CredentialResult struct {
				Status string
				Saved  *int
			} `json:"credential_result"`
		}
		if err := json.Unmarshal(data, &wire); err != nil || wire.Scope != "machine_metadata" || wire.CredentialResult.Status != "not_imported" || wire.CredentialResult.Saved == nil || *wire.CredentialResult.Saved != 0 {
			t.Fatalf("missing metadata-only outcome: %s", data)
		}
		var batch inventoryBatch
		if err := json.Unmarshal(data, &batch); err != nil {
			t.Fatal(err)
		}
		return batch
	}
	const csv = "asset_id,name,kind\nm1,One,physical\n"
	req := map[string]string{"action": "preview", "csv": csv}
	preview := checkScope(f.request(t, f.admin, "POST", "", req, 200))
	req["scope"] = "machine_metadata"
	if got := checkScope(f.request(t, f.admin, "POST", "", req, 200)); !reflect.DeepEqual(preview, got) {
		t.Fatal("explicit metadata scope changed preview")
	}
	req["action"], req["preview_token"] = "import", preview.PreviewToken
	imported := checkScope(f.request(t, f.admin, "POST", "", req, 200))
	if len(imported.Rows) != 1 || imported.Rows[0].Status != "imported" || imported.AttemptID == 0 {
		t.Fatalf("invalid confirmation: %+v", imported)
	}
	stored := func() string {
		t.Helper()
		var raw string
		if err := f.store.db.QueryRow(`SELECT receipt FROM inventory_import_attempts WHERE attempt_id=?`, imported.AttemptID).Scan(&raw); err != nil {
			t.Fatal(err)
		}
		return raw
	}
	current := stored()
	checkScope([]byte(current))
	query := fmt.Sprintf("?batch_id=%s&attempt_id=%d", imported.BatchID, imported.AttemptID)
	if got := checkScope(f.request(t, f.admin, "GET", query, nil, 200)); !reflect.DeepEqual(imported, got) {
		t.Fatal("history differs from confirmation")
	}
	f.exec(t, `UPDATE inventory_import_attempts SET receipt=json_remove(receipt,'$.scope','$.credential_result') WHERE attempt_id=?`, imported.AttemptID)
	legacy := stored()
	if got := checkScope(f.request(t, f.admin, "GET", query, nil, 200)); !reflect.DeepEqual(imported, got) || stored() != legacy {
		t.Fatal("legacy receipt changed when read")
	}
	var credentials, bindings, machines int
	if err := f.store.db.QueryRow(`SELECT (SELECT count(*) FROM credentials), (SELECT count(*) FROM credential_grants), (SELECT count(*) FROM inventory_machines WHERE cluster='' AND instance='')`).Scan(&credentials, &bindings, &machines); err != nil || credentials != 1 || bindings != 0 || machines != 1 {
		t.Fatalf("unexpected credentials, grants or unbound machines: %d %d %d (%v)", credentials, bindings, machines, err)
	}
	for _, corrupt := range []string{"{", "null", "{}", strings.Replace(current, `"machine_metadata"`, `"machine_and_credentials"`, 1), strings.Replace(current, `"saved":0`, `"saved":3`, 1)} {
		f.exec(t, `UPDATE inventory_import_attempts SET receipt=? WHERE attempt_id=?`, corrupt, imported.AttemptID)
		data := f.request(t, f.admin, "GET", query, nil, 503)
		var failure doopsErrorResponse
		if err := json.Unmarshal(data, &failure); err != nil || failure.Code != "machine_inventory_unavailable" {
			t.Fatalf("unexpected historical receipt error: %s", data)
		}
	}
}
