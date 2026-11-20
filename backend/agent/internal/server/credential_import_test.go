package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const importCanary = "synthetic-import-p@ss:/word"

type credentialImportPausedBody struct {
	io.Reader
	started chan struct{}
	release <-chan struct{}
}

func (body *credentialImportPausedBody) Read(p []byte) (int, error) {
	if body.started != nil {
		close(body.started)
		body.started = nil
		<-body.release
	}
	return body.Reader.Read(p)
}

type credentialLockWaitContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *credentialLockWaitContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func credentialImportFixture(t *testing.T) (*executionLifecycleFixture, string) {
	t.Helper()
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("i", 32))
	bin, argsLog := installCredentialTestCommands(t)
	sourceBin := t.TempDir()
	source, err := json.Marshal(map[string]any{
		"metadata": map[string]string{"uid": "source-uid", "resourceVersion": "7"},
		"type":     "Opaque",
		"data": map[string]string{
			"DB_PASSWORD":     base64.StdEncoding.EncodeToString([]byte(importCanary)),
			"SOURCE_URL":      base64.StdEncoding.EncodeToString([]byte("jdbc:mysql://mysql.test:3306/test")),
			"SOURCE_USERNAME": base64.StdEncoding.EncodeToString([]byte("test-user")),
			"SOURCE_PASSWORD": base64.StdEncoding.EncodeToString([]byte(importCanary)),
			"UNSELECTED":      base64.StdEncoding.EncodeToString([]byte("never-import-this")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sourceBin, "source.json")
	if err := os.WriteFile(sourcePath, source, 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
if [ "$1" = "get" ] && [ "$2" = "secret" ] && [ "$3" = "source-db" ]; then
  printf 'read source-db\n' >> "$DOOPS_TEST_ARGS_LOG"
  cat "$DOOPS_TEST_IMPORT_SOURCE"
else
  exec "$DOOPS_TEST_IMPORT_KUBECTL" "$@"
fi
`
	if err := os.WriteFile(filepath.Join(sourceBin, "kubectl"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOOPS_TEST_IMPORT_SOURCE", sourcePath)
	t.Setenv("DOOPS_TEST_IMPORT_KUBECTL", filepath.Join(bin, "kubectl"))
	t.Setenv("PATH", sourceBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{OperationTimeout: 5 * time.Second})
	agent := &Gateway{
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	conn := dialFakeGatewayAgent(t, f.server.URL, "dev", "local")
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.ServeGatewayWebSocketConnWithReady(conn, "credential-functional", nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("credential Agent did not stop")
		}
		requireEventually(t, 3*time.Second, func() bool { return f.hub.client.registry.Get("dev", "local") == nil })
	})
	waitForGatewayAgent(t, f.hub, "dev", "local")
	return f, argsLog
}

func TestCredentialImportKeyMapTransformsRuntimeKeys(t *testing.T) {
	f, argsLog := credentialImportFixture(t)
	var resource CredentialResource
	json.Unmarshal(f.http(t, f.admin, http.MethodPost, "/v1/credentials",
		`{"name":"mapped-db","scope":"personal","type":"opaque"}`, http.StatusOK), &resource)
	body := map[string]any{
		"session_id": "mapped-import", "cluster": "dev", "instance": "local",
		"namespace": "apps", "secret_name": "source-db",
		"keys": []string{"SOURCE_URL", "SOURCE_USERNAME", "SOURCE_PASSWORD"},
		"key_map": map[string]string{
			"SOURCE_URL":      "SPRING_DATASOURCE_URL",
			"SOURCE_USERNAME": "SPRING_DATASOURCE_USERNAME",
			"SOURCE_PASSWORD": "SPRING_DATASOURCE_PASSWORD",
		},
	}
	encoded, _ := json.Marshal(body)
	f.http(t, f.admin, http.MethodPost, "/v1/credentials/"+resource.ID+"/import-secret",
		string(encoded), http.StatusOK)
	versions, err := f.store.CredentialVersions(resource.ID)
	if err != nil || len(versions) != 1 {
		t.Fatalf("mapped import did not stage one version: %v %#v", err, versions)
	}
	payload, err := f.store.ResolveCredentialPayload(resource.ID, versions[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(payload)
	var decoded struct {
		Data map[string]string `json:"data"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"SPRING_DATASOURCE_URL":      "jdbc:mysql://mysql.test:3306/test",
		"SPRING_DATASOURCE_USERNAME": "test-user",
		"SPRING_DATASOURCE_PASSWORD": importCanary,
	}
	if !reflect.DeepEqual(decoded.Data, want) {
		t.Fatalf("mapped payload = %#v, want %#v", decoded.Data, want)
	}
	if logged, _ := os.ReadFile(argsLog); !strings.Contains(string(logged), "read source-db") {
		t.Fatal("mapped import did not read the source Secret through the Agent")
	}
}

func TestCredentialImportHTTPThroughRealAgent(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
	}{
		{"success", http.StatusOK},
		{"missing_key", http.StatusBadGateway},
		{"missing_encryption_key", http.StatusServiceUnavailable},
		{"audit_unavailable", http.StatusInternalServerError},
		{"owner_with_target", http.StatusOK},
		{"owner_without_target", http.StatusForbidden},
		{"inline_password", http.StatusBadRequest},
		{"query", http.StatusBadRequest},
		{"empty_query", http.StatusBadRequest},
		{"unknown_field", http.StatusBadRequest},
		{"revoked", http.StatusConflict},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, argsLog := credentialImportFixture(t)
			token := f.admin
			switch scenario.name {
			case "owner_with_target":
				token = f.user(t, "owner", ScopeGrant{Cluster: "dev", Instance: "local"})
			case "owner_without_target":
				token = f.user(t, "owner", ScopeGrant{Cluster: "dev", Instance: "other"})
			}
			var resource CredentialResource
			json.Unmarshal(f.http(t, token, http.MethodPost, "/v1/credentials",
				`{"name":"import-db","scope":"personal","type":"opaque"}`, http.StatusOK), &resource)
			body := map[string]any{
				"session_id": "import-test", "cluster": "dev", "instance": "local",
				"namespace": "apps", "secret_name": "source-db", "keys": []string{"DB_PASSWORD"},
				"connection_uri": "postgres://app@db:5432/app", "password_key": "DB_PASSWORD",
			}
			switch scenario.name {
			case "missing_key":
				body["keys"], body["password_key"] = []string{"MISSING"}, "MISSING"
			case "missing_encryption_key":
				t.Setenv("DOOPS_GATEWAY_SECRET_KEY", "")
			case "audit_unavailable":
				if _, err := f.store.db.Exec(`CREATE TRIGGER deny_import BEFORE INSERT ON audit_events
					WHEN NEW.action = 'credential:rotate' BEGIN SELECT RAISE(FAIL, 'synthetic failure'); END`); err != nil {
					t.Fatal(err)
				}
			case "inline_password":
				body["connection_uri"] = "postgres://app:inline-secret@db/app"
			case "query":
				body["connection_uri"] = "postgres://app@db/app?sslmode=require"
			case "empty_query":
				body["connection_uri"] = "postgres://app@db/app?"
			case "unknown_field":
				body["payload"] = "not-accepted"
			case "revoked":
				f.http(t, token, http.MethodPost, "/v1/credentials/"+resource.ID+"/revoke", "", http.StatusOK)
			}
			encoded, _ := json.Marshal(body)
			response := f.http(t, token, http.MethodPost, "/v1/credentials/"+resource.ID+"/import-secret",
				string(encoded), scenario.status)
			versions, err := f.store.CredentialVersions(resource.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario.status == http.StatusOK {
				if len(versions) != 1 || versions[0].State != CredentialVersionStaged {
					t.Fatalf("import did not stage exactly one version: %#v", versions)
				}
				payload, err := f.store.ResolveCredentialPayload(resource.ID, versions[0].ID)
				if err != nil {
					t.Fatal(err)
				}
				defer clear(payload)
				var decoded struct {
					Data map[string]string `json:"data"`
				}
				if json.Unmarshal(payload, &decoded) != nil || len(decoded.Data) != 1 {
					t.Fatal("imported unselected source fields")
				}
				connection, err := url.Parse(decoded.Data["connection_uri"])
				if err != nil || connection.User == nil {
					t.Fatal("imported connection URI is invalid")
				}
				if password, _ := connection.User.Password(); password != importCanary {
					t.Fatal("Agent did not preserve password encoding")
				}
			} else if len(versions) != 0 {
				t.Fatal("failed import persisted a version")
			}
			logged, _ := os.ReadFile(argsLog)
			if scenario.status != http.StatusOK && scenario.name != "missing_key" && len(logged) != 0 {
				t.Fatalf("denied request reached source Secret: %s", logged)
			}
			audits, err := f.store.ListAudit(100)
			if err != nil {
				t.Fatal(err)
			}
			auditJSON, _ := json.Marshal(audits)
			for _, output := range [][]byte{response, logged, auditJSON} {
				if strings.Contains(string(output), importCanary) || strings.Contains(string(output), url.QueryEscape(importCanary)) {
					t.Fatal("credential payload escaped into public output or audit")
				}
			}
		})
	}
}

func TestCredentialImportRechecksAdministrationAfterWaiting(t *testing.T) {
	f, argsLog := credentialImportFixture(t)
	var resource CredentialResource
	json.Unmarshal(f.http(t, f.admin, http.MethodPost, "/v1/credentials",
		`{"name":"revoke-admin","scope":"personal","type":"opaque"}`, http.StatusOK), &resource)
	unlock, err := f.hub.acquireCredentialMutationLocks(context.Background(), []string{resource.ID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if unlock != nil {
			unlock()
		}
	}()
	started, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	body := &credentialImportPausedBody{
		Reader: strings.NewReader(`{"session_id":"import-revoked","cluster":"dev","instance":"local",
			"namespace":"apps","secret_name":"source-db","keys":["DB_PASSWORD"]}`),
		started: started, release: release,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/credentials/"+resource.ID+"/import-secret", body).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer "+f.admin.Plaintext)
	response := httptest.NewRecorder()
	go func() {
		defer close(done)
		f.hub.HandleCredential(response, request)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		close(release)
		t.Fatal("import did not reach request parsing")
	}
	revokeErr := revokeAllAuthority(f.store, resource.OwnerID)
	close(release)
	unlock()
	unlock = nil
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("import did not finish after permission revocation")
	}
	if revokeErr != nil {
		t.Fatal(revokeErr)
	}
	if response.Code != http.StatusForbidden {
		t.Fatalf("revoked administrator imported Secret: HTTP %d", response.Code)
	}
	if logged, _ := os.ReadFile(argsLog); len(logged) != 0 {
		t.Fatal("revoked administrator read source Secret")
	}
}

func TestCredentialMutationRechecksAuthorityAfterLockWait(t *testing.T) {
	for _, scenario := range []string{"payload-token", "verify-token", "verify-target"} {
		t.Run(scenario, func(t *testing.T) {
			f, argsLog := credentialImportFixture(t)
			var resource CredentialResource
			json.Unmarshal(f.http(t, f.admin, http.MethodPost, "/v1/credentials",
				`{"name":"queued-db","scope":"personal","type":"opaque"}`, http.StatusOK), &resource)
			base := "/v1/credentials/" + resource.ID
			var version CredentialVersion
			json.Unmarshal(f.http(t, f.admin, http.MethodPut, base+"/payload",
				`{"data":{"connection_uri":"postgres://app:synthetic-queued@db/app"}}`, http.StatusOK), &version)
			scope := CredentialGrantScope{Cluster: "dev", Instance: "local", Namespace: "apps", Use: CredentialUseDatabase}
			grantBody, _ := json.Marshal(map[string]any{"scope": scope})
			f.http(t, f.admin, http.MethodPost, base+"/grants", string(grantBody), http.StatusOK)
			method, endpoint, body := http.MethodPut, base+"/payload", `{"data":{"key":"synthetic-queued-new"}}`
			if strings.HasPrefix(scenario, "verify-") {
				method, endpoint = http.MethodPost, base+"/verify"
				encoded, _ := json.Marshal(map[string]any{
					"session_id": "queued-verify", "version_id": version.ID, "scope": scope,
				})
				body = string(encoded)
			}
			unlock, err := f.hub.acquireCredentialMutationLocks(context.Background(), []string{resource.ID})
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if unlock != nil {
					unlock()
				}
			}()
			deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx := &credentialLockWaitContext{Context: deadline, waiting: make(chan struct{})}
			request := httptest.NewRequest(method, endpoint, strings.NewReader(body)).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+f.admin.Plaintext)
			response, done := httptest.NewRecorder(), make(chan struct{})
			go func() {
				defer close(done)
				f.hub.HandleCredential(response, request)
			}()
			select {
			case <-ctx.waiting:
			case <-done:
				t.Fatalf("request did not wait for the credential lock: %d", response.Code)
			case <-deadline.Done():
				t.Fatal("request did not enter lock acquisition")
			}
			if scenario == "verify-target" {
				err = revokeAllAuthority(f.store, resource.OwnerID)
			} else {
				err = f.store.RevokeToken(f.admin.ID)
			}
			unlock()
			unlock = nil
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case <-deadline.Done():
				t.Fatal("credential request did not finish")
			}
			if response.Code != http.StatusForbidden {
				t.Fatalf("revoked authority was accepted: HTTP %d", response.Code)
			}
			versions, err := f.store.CredentialVersions(resource.ID)
			if err != nil || len(versions) != 1 {
				t.Fatal("revoked request persisted another version")
			}
			if logged, _ := os.ReadFile(argsLog); len(logged) != 0 {
				t.Fatal("revoked request reached the target")
			}
		})
	}
}

func TestCredentialConfigureCLIThroughGatewayAndRealAgent(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "doops")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cli/doops")
	build.Dir = "../../../cli"
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	for _, scenario := range []string{"import", "stdin", "verify-failed", "target-exec-import"} {
		t.Run(scenario, func(t *testing.T) {
			f, argsLog := credentialImportFixture(t)
			actor := f.admin
			if scenario == "target-exec-import" {
				actor = f.user(t, "configure-target-exec", ScopeGrant{
					Cluster: "dev", Instance: "local",
				})
			}
			configRoot := t.TempDir()
			for name, value := range map[string]any{
				"config.json": map[string]any{"servers": []map[string]string{{
					"name": "functional", "gateway": f.server.URL, "cluster": "dev", "instance": "local",
				}}},
				"auth.json": map[string]any{"tokens": map[string]string{f.server.URL: actor.Plaintext}},
			} {
				data, _ := json.Marshal(value)
				if err := os.WriteFile(filepath.Join(configRoot, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("DOOPS_TEST_APPLY_MODE", "")
			if scenario == "verify-failed" {
				t.Setenv("DOOPS_TEST_APPLY_MODE", "fail_before")
			}
			name := "database-" + scenario
			args := []string{"-session", "configure-" + scenario, "credential", "configure",
				"--target", "functional", "--name", name, "--type", "opaque", "--scope", "personal",
				"--namespace", "apps", "--use", "database"}
			if scenario != "stdin" {
				args = append(args, "--from-secret", "source-db", "--keys", "DB_PASSWORD",
					"--password-key", "DB_PASSWORD", "--connection-uri", "postgres://app@db:5432/app")
			}
			runCtx, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			command := exec.CommandContext(runCtx, binary, args...)
			command.Env = append(os.Environ(), "DOOPS_CONFIG="+filepath.Join(configRoot, "config.json"))
			command.Stdin = strings.NewReader(`{"data":{"connection_uri":"postgres://app:synthetic-stdin@db/app"}}`)
			output, err := command.CombinedOutput()
			if (err != nil) != (scenario == "verify-failed") {
				t.Fatalf("configure result %v: %s", err, output)
			}
			var visible struct {
				Credentials []CredentialResource `json:"credentials"`
			}
			json.Unmarshal(f.http(t, f.admin, http.MethodGet,
				"/v1/credentials?cluster=dev&instance=local", "", http.StatusOK), &visible)
			found := false
			for _, resource := range visible.Credentials {
				found = found || resource.Name == name
			}
			if found != (scenario != "verify-failed") {
				t.Fatalf("target visibility does not match activation: %s", output)
			}
			if (strings.Contains(string(output), "phase=promoted")) != found {
				t.Fatalf("CLI activation claim is wrong: %s", output)
			}
			audits, _ := f.store.ListAudit(200)
			auditJSON, _ := json.Marshal(audits)
			logged, _ := os.ReadFile(argsLog)
			for _, public := range [][]byte{output, auditJSON, logged} {
				for _, secret := range []string{importCanary, url.QueryEscape(importCanary), "synthetic-stdin", "never-import-this"} {
					if strings.Contains(string(public), secret) {
						t.Fatal("payload escaped CLI, audit or process arguments")
					}
				}
			}
			for _, event := range audits {
				if event.UserID == "internal" || event.EndedAt.IsZero() {
					t.Fatalf("credential audit lost actor or terminal state: %#v", event)
				}
			}
			if len(f.hub.listActiveOperations()) != 0 {
				t.Fatal("configure left an active operation")
			}
			t.Logf("actual CLI output: %s", output)
		})
	}
}

// revokeAllAuthority removes every target grant and the platform admin flag.
func revokeAllAuthority(store *GatewayStore, userID string) error {
	if _, err := store.db.Exec("DELETE FROM grants WHERE user_id = ?", userID); err != nil {
		return err
	}
	_, err := store.db.Exec("UPDATE users SET is_admin = 0 WHERE id = ?", userID)
	return err
}
