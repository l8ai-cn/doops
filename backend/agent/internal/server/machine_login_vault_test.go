package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestMachineLoginVaultVisibilityAndIsolation(t *testing.T) {
	f := newLoginImportFixture(t)
	m := machineInventoryInput{AssetID: "bound-host", Name: "Host", Kind: "physical", Cluster: "ops", Instance: "staging"}
	if err := f.store.MarkAgentOnline(AgentStatus{Cluster: m.Cluster, Instance: m.Instance}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.GrantUser(f.userID, ScopeGrant{Cluster: "*", Instance: "*"}); err != nil {
		t.Fatal(err)
	}
	if before := f.save(t, "create", m, 0, 200); string(before.LoginAccounts) != "[]" {
		t.Fatal("admin should see an empty array for a new machine")
	}
	const password = "  synthetic-vault-pass  "
	const csv = "asset_id,username,password\nbound-host,zeta,\"" + password + "\"\nbound-host,alpha,\"" + password + "\"\n"
	preview := f.loginRequest(t, csv, "", 200)
	f.loginRequest(t, csv, preview.PreviewToken, 200)
	var credID, versionID, digest, ciphertext string
	if err := f.store.db.QueryRow(`SELECT a.credential_id,a.version_id,v.payload_digest,v.payload_ciphertext
 FROM inventory_machine_login_accounts a JOIN credential_versions v ON v.id=a.version_id WHERE a.username='alpha'`).Scan(&credID, &versionID, &digest, &ciphertext); err != nil {
		t.Fatal(err)
	}
	payload, err := f.store.decryptSecretWithAAD(ciphertext, machineLoginAAD(credID, versionID, digest, m.AssetID, "alpha"))
	if err != nil || validateCredentialPayload(CredentialTypeOpaque, []byte(payload)) != nil {
		t.Fatal("invalid encrypted opaque payload")
	}
	var login struct{ Data map[string]string }
	if json.Unmarshal([]byte(payload), &login) != nil || login.Data["password"] != password || login.Data["username"] != "alpha" || !strings.HasPrefix(ciphertext, "v2:") || strings.Contains(ciphertext, password) || digest == credentialPayloadDigest([]byte(payload)) || digest == credentialPayloadDigest([]byte(password)) {
		t.Fatal("password or digest storage invariant failed")
	}
	if plain, err := f.store.ResolveCredentialPayload(credID, versionID); !errors.Is(err, ErrCredentialVersionNotFound) || plain != nil {
		t.Fatal("generic payload resolution allowed")
	}
	for _, path := range []string{"", "/versions", "/grants", "/payload", "/revoke"} {
		method := http.MethodGet
		if path == "/grants" || path == "/payload" || path == "/revoke" {
			method = http.MethodPost
		}
		res := credentialHTTPRequest(t, f.server.Config.Handler, f.admin, method, "/v1/credentials/"+credID+path, nil)
		if res.Code != 404 {
			t.Fatalf("generic %s %s status = %d", method, path, res.Code)
		}
	}
	res := credentialHTTPRequest(t, f.server.Config.Handler, f.admin, http.MethodGet, "/v1/credentials", nil)
	if res.Code != 200 || strings.Contains(res.Body.String(), credID) {
		t.Fatal("generic credential list exposed machine login")
	}
	f.checkLoginImportCount(t, `SELECT COUNT(*) FROM credential_grants`, 0)
	f.batch(t, "preview", "asset_id,kind,name\nbound-host,physical,Host\n", "duplicate")
	adminList := f.list(t, f.admin)
	if len(adminList) != 1 {
		t.Fatal("missing visible machine")
	}
	var accounts []map[string]string
	if err := json.Unmarshal(adminList[0].LoginAccounts, &accounts); err != nil || len(accounts) != 2 || len(accounts[0]) != 2 || len(accounts[1]) != 2 || accounts[0]["username"] != "alpha" || accounts[0]["credential_id"] != credID || accounts[1]["username"] != "zeta" || accounts[1]["credential_id"] == "" {
		t.Fatal("wrong admin login metadata")
	}
	if detail := f.save(t, "update", m, 1, 200); !bytes.Equal(detail.LoginAccounts, adminList[0].LoginAccounts) {
		t.Fatal("admin detail disagrees with list")
	}
	user := string(f.request(t, f.user, "GET", "", nil, 200))
	if !strings.Contains(user, m.AssetID) || strings.Contains(user, "login_accounts") || strings.Contains(user, credID) || strings.Contains(user, "alpha") {
		t.Fatal("target operator saw login metadata or lost target visibility")
	}
	for _, token := range []string{f.admin, f.user} {
		data := string(f.request(t, token, "GET", "", nil, 200))
		for _, secret := range []string{password, digest, ciphertext, versionID} {
			if strings.Contains(data, secret) {
				t.Fatal("vault secret in inventory response")
			}
		}
	}
}

func TestMachineLoginVaultKeyAndCiphertextFailures(t *testing.T) {
	f := newLoginImportFixture(t)
	const key, csv = "synthetic vault test key", "asset_id,name,kind,username,password\nknown,Known,physical,ops,synthetic-pass\n"
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", key)
	preview := f.loginRequest(t, csv, "", 200)
	f.loginRequest(t, csv, preview.PreviewToken, 200)
	for _, key := range []string{"", "different synthetic key"} {
		t.Setenv("DOOPS_GATEWAY_SECRET_KEY", key)
		for _, text := range []string{csv, strings.Replace(csv, "known,Known", "new,New", 1)} {
			b := f.request(t, f.admin, "POST", "", map[string]string{"action": "preview", "scope": machineLoginScope, "csv": text}, 503)
			var failure doopsErrorResponse
			if json.Unmarshal(b, &failure) != nil || failure.Code != "machine_login_key_unavailable" {
				t.Fatal("incorrect missing/wrong key response")
			}
		}
	}
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", key)
	checkLoginResult(t, f.loginRequest(t, csv, "", 200), "preview", 0, 0, 1, "existing")
	changed := f.loginRequest(t, strings.Replace(csv, "synthetic-pass", "different-pass", 1), "", 200)
	if changed.Rows[0].Code != "machine_login_conflict" {
		t.Fatal("different password did not conflict")
	}
	f.exec(t, `UPDATE credential_versions SET payload_digest='tampered'`)
	data := f.request(t, f.admin, "POST", "", map[string]string{"action": "preview", "scope": machineLoginScope, "csv": csv}, 503)
	var failure doopsErrorResponse
	if json.Unmarshal(data, &failure) != nil || failure.Code != "machine_login_unavailable" {
		t.Fatal("unauthenticated ciphertext accepted or reported as a password conflict")
	}
}
