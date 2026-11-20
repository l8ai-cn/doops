package server

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

const syntheticLogin = "synthetic-login-canary"

func newLoginImportFixture(t *testing.T) *machineFixture {
	t.Helper()
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", "synthetic inventory test key")
	return newMachineFixture(t)
}

func (f *machineFixture) loginRequest(t *testing.T, csv, token string, httpStatus int) inventoryBatch {
	t.Helper()
	req := map[string]string{"action": "preview", "scope": machineLoginScope, "csv": csv}
	if token != "" {
		req["action"], req["preview_token"] = "import", token
	}
	data := f.request(t, f.admin, "POST", "", req, httpStatus)
	if strings.Contains(string(data), syntheticLogin) || strings.Contains(string(data), inventoryBatchID(csv)) {
		t.Fatal("credential material or unkeyed CSV fingerprint in response")
	}
	var batch inventoryBatch
	if httpStatus == 200 {
		if err := json.Unmarshal(data, &batch); err != nil || batch.Scope != machineLoginScope || len(batch.Rows) == 0 {
			t.Fatal("missing credential import result")
		}
	}
	return batch
}

func checkLoginResult(t *testing.T, b inventoryBatch, status string, expected, saved, existing int, rows ...string) {
	t.Helper()
	r := b.CredentialResult
	if r.Status != status || r.Saved != saved || r.Expected == nil || *r.Expected != expected || r.Existing == nil || *r.Existing != existing || len(rows) != len(b.Rows) {
		t.Fatalf("incorrect %s counts or row count", status)
	}
	for i, want := range rows {
		if b.Rows[i].CredentialStatus != want {
			t.Fatalf("line %d credential_status = %q, want %q", b.Rows[i].Line, b.Rows[i].CredentialStatus, want)
		}
	}
}

func (f *machineFixture) checkLoginImportCount(t *testing.T, query string, want int) {
	t.Helper()
	var got int
	if err := f.store.db.QueryRow(query).Scan(&got); err != nil || got != want {
		t.Fatalf("%s = %d, want %d (%v)", query, got, want, err)
	}
}

func TestMachineInventoryLoginImportRoundTrip(t *testing.T) {
	f := newLoginImportFixture(t)
	if err := f.store.MarkAgentOnline(AgentStatus{Cluster: "ops", Instance: "registered"}); err != nil {
		t.Fatal(err)
	}
	registered := f.save(t, "create", machineInventoryInput{AssetID: "known", Name: "Registered name", Kind: "physical", Address: "192.0.2.25", Cluster: "ops", Instance: "registered"}, 0, 200)
	const csv = "asset_id,name,kind,host_asset_id,username,password\n" +
		"guest,Guest,vm,known,known,\" " + syntheticLogin + ",one \"\n" +
		"known,,,,guest," + syntheticLogin + "-two\n" +
		"guest,Guest,vm,known,other," + syntheticLogin + "-three\n" +
		"guest,Guest,vm,known,known,\" " + syntheticLogin + ",one \"\n"
	p := f.loginRequest(t, csv, "", 200)
	checkLoginResult(t, p, "preview", 3, 0, 0, "pending", "pending", "pending", "duplicate")
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_machines`, 1)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_import_attempts`, 0)
	f.loginRequest(t, strings.Replace(csv, "-two", "-changed", 1), p.PreviewToken, 409)
	f.request(t, f.user, "POST", "", map[string]string{"action": "import", "scope": machineLoginScope, "csv": csv, "preview_token": p.PreviewToken}, 403)
	b := f.loginRequest(t, csv, p.PreviewToken, 200)
	checkLoginResult(t, b, "saved", 3, 3, 0, "saved", "saved", "saved", "duplicate")
	if b.Rows[0].Status != "imported" || b.Rows[1].Status != "duplicate" || b.Rows[2].Status != "duplicate" || b.AttemptID <= 0 {
		t.Fatal("incorrect machine outcomes")
	}
	f.server.Close()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	query := fmt.Sprintf("?batch_id=%s&attempt_id=%d", b.BatchID, b.AttemptID)
	var history inventoryBatch
	data := f.request(t, f.admin, "GET", query, nil, 200)
	if err := json.Unmarshal(data, &history); err != nil || !reflect.DeepEqual(history, b) || strings.Contains(string(data), syntheticLogin) {
		t.Fatal("stored receipt differs from HTTP confirmation or exposes a password")
	}
	f.request(t, f.user, "GET", query, nil, 403)
	list := f.list(t, f.admin)
	if len(list) != 2 || list[0].HostAssetID != "known" || list[0].Cluster != "" || (!reflect.DeepEqual(list[1].machineInventoryInput, registered.machineInventoryInput) || list[1].Revision != registered.Revision) {
		t.Fatal("import changed existing metadata, host or target binding")
	}
	// Replanning after restart must compare the exact account, asset and password.
	f.loginRequest(t, csv, p.PreviewToken, 409)
	p = f.loginRequest(t, csv, "", 200)
	checkLoginResult(t, p, "preview", 0, 0, 3, "existing", "existing", "existing", "duplicate")
	checkLoginResult(t, f.loginRequest(t, csv, p.PreviewToken, 200), "saved", 0, 0, 3, "existing", "existing", "existing", "duplicate")
	const extra = "username,password,asset_id\nknown," + syntheticLogin + "-four,known\n"
	p = f.loginRequest(t, extra, "", 200)
	checkLoginResult(t, f.loginRequest(t, extra, p.PreviewToken, 200), "saved", 1, 1, 0, "saved")
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_machine_login_accounts WHERE (asset_id,username) IN (('known','guest'),('known','known'),('guest','known'),('guest','other'))`, 4)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM credential_versions WHERE instr(payload_ciphertext,'synthetic-login-canary')>0`, 0)
	f.exec(t, `UPDATE inventory_import_attempts SET receipt=json_set(receipt,'$.credential_result.saved',0) WHERE attempt_id=?`, b.AttemptID)
	f.request(t, f.admin, "GET", query, nil, 503)
	events, err := f.store.ListAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(events)
	if err != nil || strings.Contains(string(encoded), syntheticLogin) || strings.Contains(string(encoded), inventoryBatchID(csv)) {
		t.Fatal("credential material in audit")
	}
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM audit_events WHERE action='inventory:machine:import' AND status='success' AND correlation_id=(SELECT batch_id FROM inventory_import_attempts LIMIT 1)`, 2)
}

func TestMachineInventoryLoginImportRejectsEntireBatch(t *testing.T) {
	f := newLoginImportFixture(t)
	const prior = "asset_id,name,kind,username,password\nknown,Known,physical,ops," + syntheticLogin + "\n"
	p := f.loginRequest(t, prior, "", 200)
	f.loginRequest(t, prior, p.PreviewToken, 200)
	const prefix = "asset_id,name,kind,username,password\nfresh,Fresh,physical,ops," + syntheticLogin + "\n"
	for _, tc := range []struct{ rows, code string }{
		{"known,,,ops," + syntheticLogin + "-changed", "machine_login_conflict"},
		{"fresh,Fresh,physical,ops," + syntheticLogin + "-changed", "machine_login_conflict"},
		{"unknown,,,ops," + syntheticLogin, "machine_not_registered"},
		{"fresh,,,other," + syntheticLogin, "machine_not_registered"},
		{"known,Changed,physical,other," + syntheticLogin, "registered_asset_conflict"},
		{"known,,,other,", "machine_login_required"},
		{"known,,,," + syntheticLogin, "invalid_machine_username"},
	} {
		csv := prefix + tc.rows + "\n"
		p := f.loginRequest(t, csv, "", 200)
		if len(p.Rows) != 2 || p.Rows[1].Code != tc.code || p.Rows[1].Status != "error" || p.Rows[1].CredentialStatus != "error" {
			t.Fatalf("missing row error %s", tc.code)
		}
		f.loginRequest(t, csv, p.PreviewToken, 422)
	}
	p = f.loginRequest(t, prior, "", 200)
	checkLoginResult(t, p, "preview", 0, 0, 1, "existing")
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_machines`, 1)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_import_attempts`, 1)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_machine_login_accounts`, 1)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM credential_versions`, 1)
	for _, col := range []string{"cluster", "instance", "credential_id"} {
		f.loginRequest(t, "asset_id,username,password,"+col+"\nknown,ops,"+syntheticLogin+",trusted\n", "", 400)
	}
}

func TestMachineInventoryLoginImportRollsBack(t *testing.T) {
	f := newLoginImportFixture(t)
	const csv = "asset_id,name,kind,username,password\none,One,physical,ops," + syntheticLogin + "\ntwo,Two,physical,ops," + syntheticLogin + "-two\n"
	p := f.loginRequest(t, csv, "", 200)
	originalKey := os.Getenv("DOOPS_GATEWAY_SECRET_KEY")
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", "")
	f.loginRequest(t, csv, "", 503)
	f.loginRequest(t, csv, p.PreviewToken, 503)
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", "synthetic second key")
	q := f.loginRequest(t, csv, "", 200)
	if q.BatchID == p.BatchID || q.PreviewToken == p.PreviewToken {
		t.Fatal("both identifiers must be keyed")
	}
	f.loginRequest(t, csv, p.PreviewToken, 409)
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", originalKey)
	for _, tc := range []struct {
		fault  string
		status int
	}{
		{`BEFORE INSERT ON inventory_machines WHEN NEW.asset_id='two'`, 422},
		{`BEFORE INSERT ON inventory_machine_login_accounts WHEN NEW.asset_id='two'`, 503},
		{`BEFORE INSERT ON inventory_import_attempts`, 503},
		{`BEFORE UPDATE ON audit_events WHEN NEW.status='success'`, 503},
	} {
		f.exec(t, "CREATE TRIGGER interrupt_login_import "+tc.fault+" BEGIN SELECT RAISE(ABORT,'"+syntheticLogin+"'); END")
		f.loginRequest(t, csv, p.PreviewToken, tc.status)
		f.exec(t, `DROP TRIGGER interrupt_login_import`)
		for _, table := range []string{"inventory_machines", "inventory_machine_login_accounts", "inventory_machine_login_key", "credentials", "credential_versions"} {
			f.checkLoginImportCount(t, "SELECT COUNT(*) FROM "+table, 0)
		}
		f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_import_attempts`, 0)
		f.checkLoginImportCount(t, `SELECT COUNT(*) FROM audit_events WHERE status='success' AND action='inventory:machine:import'`, 0)
		checkLoginResult(t, f.loginRequest(t, csv, "", 200), "preview", 2, 0, 0, "pending", "pending")
	}
	// SQLite can skip an insert without returning an error. Do not issue a
	// saved receipt when a credential mapping lacks a readable version.
	f.exec(t, `CREATE TRIGGER skip_version BEFORE INSERT ON credential_versions WHEN EXISTS(SELECT 1 FROM inventory_machine_login_accounts) BEGIN SELECT RAISE(IGNORE); END`)
	f.loginRequest(t, csv, p.PreviewToken, 503)
	f.exec(t, `DROP TRIGGER skip_version`)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_machine_login_accounts`, 0)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM inventory_import_attempts`, 0)
	checkLoginResult(t, f.loginRequest(t, csv, p.PreviewToken, 200), "saved", 2, 2, 0, "saved", "saved")
}

func TestMachineInventoryLoginImportPreservesPasswordBytes(t *testing.T) {
	f := newLoginImportFixture(t)
	const csv = "asset_id,name,kind,username,password\r\nknown,Known,vm,account,\"" + syntheticLogin + "\r\nsecond\nthird\"\r\n"
	p := f.loginRequest(t, csv, "", 200)
	f.loginRequest(t, csv, p.PreviewToken, 200)
	checkLoginResult(t, f.loginRequest(t, csv, "", 200), "preview", 0, 0, 1, "existing")
	q := f.loginRequest(t, strings.Replace(csv, "\r\nsecond", "\nsecond", 1), "", 200)
	if q.Rows[0].Code != "machine_login_conflict" {
		t.Fatal("passwords with different line endings compared equal")
	}
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", "synthetic wrong key after save")
	f.loginRequest(t, csv, "", 503)
	f.loginRequest(t, strings.Replace(csv, "account", "another", 1), "", 503)
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM credential_versions`, 1)
}
