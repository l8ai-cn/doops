package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

const accountTestPassword = "synthetic-account-p@ss"

// accountCLI runs the compiled CLI against one isolated config directory, so the
// administrator and the provisioned user never share a login state.
type accountCLI struct {
	binary string
	config string
}

func (c accountCLI) run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, c.binary, args...)
	command.Env = append(os.Environ(), "DOOPS_CONFIG="+c.config)
	command.Stdin = strings.NewReader(stdin)
	output, err := command.CombinedOutput()
	return string(output), err
}

func writeAccountCLIConfig(t *testing.T, gateway, token string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]any{
		"config.json": map[string]any{"servers": []map[string]string{
			{"name": "allowed", "gateway": gateway, "cluster": "dev", "instance": "local"},
			{"name": "sibling", "gateway": gateway, "cluster": "dev", "instance": "sibling"},
		}},
	}
	if token != "" {
		files["auth.json"] = map[string]any{"tokens": map[string]string{gateway: token}}
	}
	for name, value := range files {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "config.json")
}

func accountPasswordLogin(t *testing.T, gateway, user, password string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": password, "name": "account-test"})
	response, err := http.Post(gateway+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var parsed struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(response.Body).Decode(&parsed)
	return response.StatusCode, parsed.Token
}

func accountOutputField(t *testing.T, output, key string) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(key) + `=(\S+)$`).FindStringSubmatch(output)
	if match == nil {
		t.Fatalf("output has no %s:\n%s", key, output)
	}
	return match[1]
}

// TestAccountProvisioningLifecycleThroughCLIGatewayAndRealAgent drives the same
// chain an administrator follows: create a user scoped to one target, let that
// user work there, prove every other boundary rejects them, rotate the password,
// and offboard them. Every step uses the compiled CLI or the public HTTP API.
func TestAccountProvisioningLifecycleThroughCLIGatewayAndRealAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(root, "workspaces"))
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", strings.Repeat("a", 32))
	binary := filepath.Join(root, "doops")
	build := exec.Command("go", "build", "-o", binary, "./cli/doops")
	build.Dir = "../../../cli"
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}

	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := &Gateway{
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	conn := dialFakeGatewayAgent(t, f.server.URL, "dev", "local")
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.ServeGatewayWebSocketConnWithReady(conn, "account-functional", nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("real Agent did not stop")
		}
	})
	waitForGatewayAgent(t, f.hub, "dev", "local")

	// The administrator owns a credential; the new user must never see it.
	f.http(t, f.admin, http.MethodPost, "/v1/credentials",
		`{"name":"admin-owned-canary","scope":"personal","type":"opaque"}`, http.StatusOK)

	admin := accountCLI{binary: binary, config: writeAccountCLIConfig(t, f.server.URL, f.admin.Plaintext)}
	const username = "account-functional-user"

	// Given an administrator, when they create a user granted only "allowed",
	// then the Gateway stores a non-admin user with exactly that one grant.
	created, err := admin.run(t, accountTestPassword, "admin", "user", "create", "--target", "allowed",
		"--user", username, "--password-stdin", "--grant-target", "allowed")
	if err != nil {
		t.Fatalf("admin user create: %v\n%s", err, created)
	}
	if strings.Contains(created, accountTestPassword) {
		t.Fatal("user create echoed the stdin password")
	}
	userID := accountOutputField(t, created, "user_id")
	apiKeyID := accountOutputField(t, created, "token_id")
	apiKey := accountOutputField(t, created, "token")
	if f.store.UserIsPlatformAdmin(userID) {
		t.Fatal("provisioned user became a platform admin")
	}
	grants, err := f.store.ListGrants(userID)
	if err != nil || len(grants) != 1 || grants[0].Cluster != "dev" || grants[0].Instance != "local" {
		t.Fatalf("grants = %#v, %v; want exactly dev/local", grants, err)
	}

	// Duplicate creation is refused before any Gateway mutation.
	if output, err := admin.run(t, accountTestPassword, "admin", "user", "create", "--target", "allowed",
		"--user", username, "--password-stdin", "--grant-target", "allowed"); err == nil || !strings.Contains(output, "user_exists") {
		t.Fatalf("duplicate user create was not refused: %v\n%s", err, output)
	}

	// The user can log in both ways: password (HTTP) and the issued API key (CLI stdin).
	status, passwordToken := accountPasswordLogin(t, f.server.URL, username, accountTestPassword)
	if status != http.StatusOK || passwordToken == "" {
		t.Fatalf("password login = HTTP %d", status)
	}
	user := accountCLI{binary: binary, config: writeAccountCLIConfig(t, f.server.URL, "")}
	if output, err := user.run(t, apiKey, "login", "--target", "allowed", "--token-stdin"); err != nil {
		t.Fatalf("API key login: %v\n%s", err, output)
	}

	for _, tc := range []struct {
		name    string
		args    []string
		allowed bool
		want    string
	}{
		{"targets on granted target", []string{"-session", "acct", "targets", "--target", "allowed"}, true, "local"},
		{"exec on granted target", []string{"-session", "acct", "exec", "--target", "allowed", "--cmd", "printf ACCOUNT_EXEC_OK"}, true, "ACCOUNT_EXEC_OK"},
		{"capabilities on granted target", []string{"-session", "acct", "capabilities", "--target", "allowed", "--require", "structuredSSH"}, true, "status=ready"},
		{"exec on sibling target", []string{"-session", "acct", "exec", "--target", "sibling", "--cmd", "hostname"}, false, "403"},
		{"admin user list", []string{"admin", "user", "list", "--target", "allowed"}, false, "403"},
		{"admin grant add", []string{"admin", "grant", "add", "--target", "allowed", "--scope-target", "sibling", "--user-id", userID}, false, "403"},
		{"admin token create", []string{"admin", "token", "create", "--target", "allowed", "--user", username}, false, "403"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := user.run(t, "", tc.args...)
			if (err == nil) != tc.allowed || !strings.Contains(output, tc.want) {
				t.Fatalf("allowed=%t err=%v, want %q:\n%s", tc.allowed, err, tc.want, output)
			}
		})
	}

	managed, err := user.run(t, "", "credential", "list", "--target", "allowed", "--view", "managed")
	if err != nil || strings.Contains(managed, "admin-owned-canary") {
		t.Fatalf("user saw another owner's credential: %v\n%s", err, managed)
	}

	// Password rotation: the old password stops working, the new one works.
	const rotated = "synthetic-account-rotated"
	if output, err := admin.run(t, rotated, "admin", "user", "password", "--target", "allowed",
		"--user-id", userID, "--password-stdin"); err != nil {
		t.Fatalf("admin user password: %v\n%s", err, output)
	}
	if status, _ := accountPasswordLogin(t, f.server.URL, username, accountTestPassword); status != http.StatusUnauthorized {
		t.Fatalf("old password login = HTTP %d, want 401", status)
	}
	if status, _ := accountPasswordLogin(t, f.server.URL, username, rotated); status != http.StatusOK {
		t.Fatalf("rotated password login = HTTP %d, want 200", status)
	}

	// Revoking the API key cuts the CLI off even though the user stays enabled.
	if output, err := admin.run(t, "", "admin", "token", "revoke", "--target", "allowed", "--id", apiKeyID); err != nil {
		t.Fatalf("admin token revoke: %v\n%s", err, output)
	}
	if output, err := user.run(t, "", "-session", "acct", "targets", "--target", "allowed"); err == nil || !strings.Contains(output, "gateway_auth_revoked") {
		t.Fatalf("revoked API key still works: %v\n%s", err, output)
	}

	// Disabling the user rejects new logins and previously issued tokens.
	if output, err := admin.run(t, "", "admin", "user", "disable", "--target", "allowed",
		"--user-id", userID, "--disabled=true"); err != nil {
		t.Fatalf("admin user disable: %v\n%s", err, output)
	}
	if status, _ := accountPasswordLogin(t, f.server.URL, username, rotated); status != http.StatusUnauthorized {
		t.Fatalf("disabled user password login = HTTP %d, want 401", status)
	}
	if _, err := f.store.VerifyUserToken(passwordToken); err == nil {
		t.Fatal("token issued before disable still verifies")
	}

	// Every administrator mutation is audited with the administrator as actor.
	adminAuth, err := f.store.VerifyUserToken(f.admin.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	events, err := f.store.ListAuditFiltered(AuditFilter{UserID: adminAuth.UserID, Action: ActionAdmin, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	routes := map[string]bool{}
	for _, event := range events {
		if strings.Contains(event.CommandSummary, accountTestPassword) || strings.Contains(event.CommandSummary, rotated) ||
			strings.Contains(event.CommandSummary, apiKey) {
			t.Fatalf("admin audit leaked a secret: %s", event.CommandSummary)
		}
		for _, route := range []string{"/v1/admin/users", "/v1/admin/grants", "/v1/admin/tokens",
			"/v1/admin/users/password", "/v1/admin/users/disable"} {
			if strings.Contains(event.CommandSummary, `"route":"`+route+`"`) {
				routes[route] = true
			}
		}
	}
	for _, route := range []string{"/v1/admin/users", "/v1/admin/grants", "/v1/admin/tokens",
		"/v1/admin/users/password", "/v1/admin/users/disable"} {
		if !routes[route] {
			t.Errorf("no admin audit event for %s", route)
		}
	}
}
