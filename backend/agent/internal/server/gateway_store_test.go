package server

import (
	"testing"
	"time"
)

func TestGatewayStoreTokenKindIsolationAndPermissions(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("alice")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	userToken, err := store.CreateToken(CreateTokenRequest{
		Kind:      TokenKindUser,
		UserID:    user.ID,
		Name:      "alice laptop",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create user token: %v", err)
	}
	agentToken, err := store.CreateToken(CreateTokenRequest{
		Kind:     TokenKindAgent,
		Name:     "dev local agent",
		Cluster:  "dev",
		Instance: "local",
	})
	if err != nil {
		t.Fatalf("create agent token: %v", err)
	}

	agentAuth, err := store.VerifyAgentToken(agentToken.Plaintext)
	if err != nil {
		t.Fatalf("verify agent token: %v", err)
	}
	if agentAuth.Cluster != "dev" || agentAuth.Instance != "local" {
		t.Fatalf("agent token scope mismatch: %#v", agentAuth)
	}

	if _, err := store.VerifyUserToken(agentToken.Plaintext); err == nil {
		t.Fatal("agent token must not authorize user operations")
	}
	if _, err := store.VerifyAgentToken(userToken.Plaintext); err == nil {
		t.Fatal("user token must not authorize agent registration")
	}

	if err := store.GrantUser(user.ID, ScopeGrant{
		Cluster:  "dev",
		Instance: "local",
		Actions:  []GatewayAction{ActionExec, ActionRead, ActionTargetsList},
	}); err != nil {
		t.Fatalf("grant user: %v", err)
	}

	userAuth, err := store.VerifyUserToken(userToken.Plaintext)
	if err != nil {
		t.Fatalf("verify user token: %v", err)
	}
	if !store.UserCan(userAuth.UserID, "dev", "local", ActionExec) {
		t.Fatal("expected alice to execute dev/local")
	}
	if store.UserCan(userAuth.UserID, "prod", "local", ActionExec) {
		t.Fatal("alice must not execute prod/local")
	}
	if store.UserCan(userAuth.UserID, "dev", "local", ActionWrite) {
		t.Fatal("targets:list must not imply write access")
	}
}

func TestGatewayStoreUserDefaultsToNoTargetAccess(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("operator")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	token, err := store.CreateToken(CreateTokenRequest{
		Kind:   TokenKindUser,
		UserID: user.ID,
		Name:   "operator laptop",
	})
	if err != nil {
		t.Fatalf("create user token: %v", err)
	}
	auth, err := store.VerifyUserToken(token.Plaintext)
	if err != nil {
		t.Fatalf("verify user token: %v", err)
	}

	for _, action := range defaultGatewayUserActions {
		if store.UserCan(auth.UserID, "any-cluster", "any-node", action) {
			t.Fatalf("new user without grants must not have target action %q", action)
		}
	}
	if store.UserCan(auth.UserID, "any-cluster", "any-node", ActionAdmin) {
		t.Fatal("default target access must not imply gateway admin access")
	}
}

func TestGatewayStoreListUsersDoesNotDeadlockWithSingleSQLiteConnection(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	alice, err := store.CreateUser("alice")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := store.CreateUser("bob")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}
	if err := store.GrantUser(alice.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionAdmin}}); err != nil {
		t.Fatalf("grant alice: %v", err)
	}
	if err := store.GrantUser(bob.ID, ScopeGrant{Cluster: "dev", Instance: "node", Actions: []GatewayAction{ActionRead}}); err != nil {
		t.Fatalf("grant bob: %v", err)
	}

	type result struct {
		users []UserSummary
		err   error
	}
	done := make(chan result, 1)
	go func() {
		users, err := store.ListUsers()
		done <- result{users: users, err: err}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("list users: %v", got.err)
		}
		if len(got.users) != 2 {
			t.Fatalf("expected two users, got %#v", got.users)
		}
		byName := map[string]UserSummary{}
		for _, u := range got.users {
			byName[u.Name] = u
		}
		if !byName["alice"].IsAdmin || byName["alice"].GrantCount != 1 {
			t.Fatalf("alice summary mismatch: %#v", byName["alice"])
		}
		if byName["bob"].IsAdmin || byName["bob"].GrantCount != 1 {
			t.Fatalf("bob summary mismatch: %#v", byName["bob"])
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("ListUsers deadlocked while rows were still open")
	}
}

func TestGatewayStoreTokenIDFastPathAndExpiredCleanup(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("alice")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	expired, err := store.CreateToken(CreateTokenRequest{
		Kind:      TokenKindUser,
		UserID:    user.ID,
		Name:      "expired",
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("create expired token: %v", err)
	}
	active, err := store.CreateToken(CreateTokenRequest{
		Kind:      TokenKindUser,
		UserID:    user.ID,
		Name:      "active",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("create active token: %v", err)
	}
	if got := tokenIDFromPlaintext(active.Plaintext); got != active.ID {
		t.Fatalf("token id parse mismatch: got %q want %q", got, active.ID)
	}
	if _, err := store.VerifyUserToken(expired.Plaintext); err == nil {
		t.Fatal("expired token must be rejected")
	}
	auth, err := store.VerifyUserToken(active.Plaintext)
	if err != nil {
		t.Fatalf("active token should verify: %v", err)
	}
	if auth.TokenID != active.ID {
		t.Fatalf("verify should return active token id, got %#v", auth)
	}
	deleted, err := store.DeleteExpiredTokens(time.Now())
	if err != nil {
		t.Fatalf("delete expired tokens: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected one expired token deleted, got %d", deleted)
	}
	if _, err := store.VerifyUserToken(active.Plaintext); err != nil {
		t.Fatalf("active token should survive cleanup: %v", err)
	}
}

func TestGatewayStoreUserPasswordLogin(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUserWithPassword(CreateUserRequest{Name: "alice", Password: "Ab123456"})
	if err != nil {
		t.Fatalf("create user with password: %v", err)
	}
	if user.PasswordHash == "" {
		t.Fatal("expected password hash to be stored")
	}

	if _, err := store.VerifyUserPassword("alice", "wrong"); err == nil {
		t.Fatal("wrong password must be rejected")
	}
	okUser, err := store.VerifyUserPassword("alice", "Ab123456")
	if err != nil {
		t.Fatalf("verify password: %v", err)
	}
	if okUser.ID != user.ID {
		t.Fatalf("unexpected user after verify: %#v", okUser)
	}

	if err := store.SetUserPassword(user.ID, "NewPass123"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if _, err := store.VerifyUserPassword("alice", "Ab123456"); err == nil {
		t.Fatal("old password must stop working")
	}
	if _, err := store.VerifyUserPassword("alice", "NewPass123"); err != nil {
		t.Fatalf("new password should work: %v", err)
	}
}

func TestGatewayStoreAuditCRUD(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("bob")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	started := time.Now().UTC()
	auditID, err := store.StartAudit(AuditEvent{
		UserID:         user.ID,
		TokenID:        "tok_123",
		Cluster:        "dev",
		Instance:       "local",
		Action:         ActionExec,
		Session:        "smoke",
		CommandSummary: "hostname",
		StartedAt:      started,
	})
	if err != nil {
		t.Fatalf("start audit: %v", err)
	}
	if err := store.FinishAudit(auditID, AuditFinish{
		Status:   "success",
		Tail:     "node-a\n",
		BytesIn:  12,
		BytesOut: 7,
		EndedAt:  started.Add(time.Second),
	}); err != nil {
		t.Fatalf("finish audit: %v", err)
	}

	events, err := store.ListAudit(10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected one audit event, got %d", len(events))
	}
	if events[0].Status != "success" || events[0].Tail != "node-a\n" {
		t.Fatalf("unexpected audit event: %#v", events[0])
	}
}

func TestGatewayStoreAuditFilterAndPurge(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	alice, err := store.CreateUser("alice")
	if err != nil {
		t.Fatalf("create alice: %v", err)
	}
	bob, err := store.CreateUser("bob")
	if err != nil {
		t.Fatalf("create bob: %v", err)
	}

	oldTime := time.Now().UTC().Add(-48 * time.Hour)
	newTime := time.Now().UTC()

	oldID, err := store.StartAudit(AuditEvent{
		UserID:         alice.ID,
		TokenID:        "tok_old",
		Cluster:        "doops-oilan",
		Instance:       "oilan-node",
		Action:         ActionExec,
		Session:        "old-session",
		CommandSummary: "hostname",
		StartedAt:      oldTime,
	})
	if err != nil {
		t.Fatalf("start old audit: %v", err)
	}
	if err := store.FinishAudit(oldID, AuditFinish{
		Status:   "success",
		Tail:     "old\n",
		BytesIn:  1,
		BytesOut: 4,
		EndedAt:  oldTime.Add(time.Second),
	}); err != nil {
		t.Fatalf("finish old audit: %v", err)
	}

	newID, err := store.StartAudit(AuditEvent{
		UserID:         bob.ID,
		TokenID:        "tok_new",
		Cluster:        "doops-edu",
		Instance:       "edu-coder",
		Action:         ActionAsk,
		Session:        "new-session",
		CommandSummary: "readonly check",
		StartedAt:      newTime,
	})
	if err != nil {
		t.Fatalf("start new audit: %v", err)
	}
	if err := store.FinishAudit(newID, AuditFinish{
		Status:   "error",
		Error:    "denied",
		Tail:     "new\n",
		BytesIn:  2,
		BytesOut: 4,
		EndedAt:  newTime.Add(time.Second),
	}); err != nil {
		t.Fatalf("finish new audit: %v", err)
	}

	filtered, err := store.ListAuditFiltered(AuditFilter{
		UserID:  bob.ID,
		Cluster: "doops-edu",
		Action:  ActionAsk,
		Limit:   10,
	})
	if err != nil {
		t.Fatalf("filter audit: %v", err)
	}
	if len(filtered) != 1 || filtered[0].Session != "new-session" {
		t.Fatalf("unexpected filtered audit rows: %#v", filtered)
	}

	deleted, err := store.DeleteAuditBefore(time.Now().UTC().Add(-24 * time.Hour))
	if err != nil {
		t.Fatalf("purge old audit: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("expected 1 deleted row, got %d", deleted)
	}

	remaining, err := store.ListAudit(10)
	if err != nil {
		t.Fatalf("list remaining audit: %v", err)
	}
	if len(remaining) != 1 || remaining[0].Session != "new-session" {
		t.Fatalf("unexpected remaining audit rows: %#v", remaining)
	}
}

func TestGatewayStoreAgentStatusLifecycle(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.MarkAgentOnline(AgentStatus{
		Cluster:  "dev",
		Instance: "local",
		TokenID:  "tok_agent",
		Remote:   "127.0.0.1:1234",
		LastSeen: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("mark online: %v", err)
	}
	agents, err := store.ListAgentStatus()
	if err != nil {
		t.Fatalf("list agents: %v", err)
	}
	if len(agents) != 1 || agents[0].Status != "online" {
		t.Fatalf("expected one online agent, got %#v", agents)
	}

	if err := store.MarkAgentOffline("dev", "local"); err != nil {
		t.Fatalf("mark offline: %v", err)
	}
	agents, err = store.ListAgentStatus()
	if err != nil {
		t.Fatalf("list agents after offline: %v", err)
	}
	if len(agents) != 1 || agents[0].Status != "offline" {
		t.Fatalf("expected offline agent retained in store, got %#v", agents)
	}
}
