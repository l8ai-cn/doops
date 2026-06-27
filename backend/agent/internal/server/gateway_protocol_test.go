package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sosedoff/gitkit"
	"github.com/user/doops/agent/api"
)

func TestGatewayHTTPListsTargetsForGrantedUserToken(t *testing.T) {
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
		Kind:   TokenKindUser,
		UserID: user.ID,
		Name:   "alice",
	})
	if err != nil {
		t.Fatalf("create user token: %v", err)
	}
	agentToken, err := store.CreateToken(CreateTokenRequest{
		Kind:     TokenKindAgent,
		Name:     "agent",
		Cluster:  "dev",
		Instance: "local",
	})
	if err != nil {
		t.Fatalf("create agent token: %v", err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionTargetsList}}); err != nil {
		t.Fatalf("grant targets list: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, LoginTokenTTL: 20 * time.Millisecond})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentWS := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/agent/connect?cluster=dev&instance=local"
	agentConn, _, err := websocket.DefaultDialer.Dial(agentWS, http.Header{"Authorization": []string{"Bearer " + agentToken.Plaintext}})
	if err != nil {
		t.Fatalf("dial agent websocket: %v", err)
	}
	defer agentConn.Close()
	go func() {
		for {
			var msg map[string]interface{}
			if err := agentConn.ReadJSON(&msg); err != nil {
				return
			}
			if msg["method"] == "initialize" {
				_ = agentConn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      msg["id"],
					"result": map[string]interface{}{
						"protocolVersion": "2024-11-05",
					},
				})
			}
		}
	}()

	resp, err := http.Get(ts.URL + "/v1/targets")
	if err != nil {
		t.Fatalf("get targets without token: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/targets", nil)
	req.Header.Set("Authorization", "Bearer "+agentToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get targets with agent token: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("agent token must not list targets, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/targets", nil)
	req.Header.Set("Authorization", "Bearer "+userToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get targets with default user token: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for default user token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGatewayAgentRegistrationRequiresMatchingToken(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	validToken, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err != nil {
		t.Fatalf("create valid agent token: %v", err)
	}
	wrongScopeToken, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "other", Cluster: "dev", Instance: "other"})
	if err != nil {
		t.Fatalf("create wrong-scope agent token: %v", err)
	}
	user, err := store.CreateUser("alice")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	userToken, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	if err != nil {
		t.Fatalf("create user token: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentWS := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/agent/connect?cluster=dev&instance=local"
	if _, resp, err := websocket.DefaultDialer.Dial(agentWS, nil); err == nil {
		t.Fatal("agent registration without token should fail")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		if resp == nil {
			t.Fatalf("expected HTTP 401 without token, got nil response: %v", err)
		}
		t.Fatalf("expected HTTP 401 without token, got %d", resp.StatusCode)
	}
	if _, resp, err := websocket.DefaultDialer.Dial(agentWS, http.Header{"Authorization": []string{"Bearer " + userToken.Plaintext}}); err == nil {
		t.Fatal("agent registration with user token should fail")
	} else if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		if resp == nil {
			t.Fatalf("expected HTTP 401 with user token, got nil response: %v", err)
		}
		t.Fatalf("expected HTTP 401 with user token, got %d", resp.StatusCode)
	}
	if _, resp, err := websocket.DefaultDialer.Dial(agentWS, http.Header{"Authorization": []string{"Bearer " + wrongScopeToken.Plaintext}}); err == nil {
		t.Fatal("agent registration with mismatched agent token should fail")
	} else if resp == nil || resp.StatusCode != http.StatusForbidden {
		if resp == nil {
			t.Fatalf("expected HTTP 403 with mismatched token, got nil response: %v", err)
		}
		t.Fatalf("expected HTTP 403 with mismatched token, got %d", resp.StatusCode)
	}

	agentConn, _, err := websocket.DefaultDialer.Dial(agentWS, http.Header{"Authorization": []string{"Bearer " + validToken.Plaintext}})
	if err != nil {
		t.Fatalf("agent registration with matching token should connect: %v", err)
	}
	defer agentConn.Close()
	go func() {
		for {
			var msg map[string]interface{}
			if err := agentConn.ReadJSON(&msg); err != nil {
				return
			}
			if msg["method"] == "initialize" {
				_ = agentConn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      msg["id"],
					"result": map[string]interface{}{
						"protocolVersion": "2024-11-05",
					},
				})
			}
		}
	}()

	requireEventually(t, time.Second, func() bool {
		return gw.getAgent("dev", "local") != nil
	})

	badWS := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/agent/connect?cluster=dev"
	_, resp, err := websocket.DefaultDialer.Dial(badWS, nil)
	if err == nil {
		t.Fatal("agent registration without instance should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusBadRequest {
		if resp == nil {
			t.Fatalf("expected HTTP 400 for missing instance, got nil response: %v", err)
		}
		t.Fatalf("expected HTTP 400 for missing instance, got %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/v1/targets")
	if err != nil {
		t.Fatalf("get targets without user token: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("targets must still require user token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func requireEventually(t *testing.T, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition was not met before timeout")
}

func TestGatewayGitHTTPRoutesByClusterAndInstance(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionPull}}); err != nil {
		t.Fatalf("grant pull: %v", err)
	}
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, MaxQueuedPerTarget: -1})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	requests := make(chan string, 1)
	go serveFakeGitAgent(t, agentConn, requests)

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/git/dev/local/release.git/info/refs?service=git-upload-pack", nil)
	req.SetBasicAuth("doops", userToken.Plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("git info refs through gateway: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if string(body) != "git-ok" {
		t.Fatalf("unexpected git response body: %q", body)
	}
	select {
	case got := <-requests:
		want := "GET /git/release.git/info/refs?service=git-upload-pack"
		if got != want {
			t.Fatalf("agent received wrong git request:\nwant %s\n got %s", want, got)
		}
	case <-time.After(time.Second):
		t.Fatal("agent did not receive git request")
	}
}

func TestGatewayGitHTTPWaitsForShortAgentReconnect(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionPull}}); err != nil {
		t.Fatalf("grant pull: %v", err)
	}
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, TargetReconnectGrace: 500 * time.Millisecond})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	reqDone := make(chan *http.Response, 1)
	reqErr := make(chan error, 1)
	go func() {
		url := gatewayGitTestURL(t, ts.URL, "dev", "local", "release", userToken.Plaintext) + "/info/refs?service=git-upload-pack"
		resp, err := http.Get(url)
		if err != nil {
			reqErr <- err
			return
		}
		reqDone <- resp
	}()

	time.Sleep(100 * time.Millisecond)
	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	requests := make(chan string, 1)
	go serveFakeGitAgent(t, agentConn, requests)

	select {
	case err := <-reqErr:
		t.Fatalf("git request failed: %v", err)
	case resp := <-reqDone:
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected request to wait for reconnect, got %d: %s", resp.StatusCode, body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("git request did not complete after agent reconnect")
	}
	select {
	case got := <-requests:
		if want := "GET /git/release.git/info/refs?service=git-upload-pack"; got != want {
			t.Fatalf("unexpected git request:\nwant %s\n got %s", want, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reconnected agent did not receive git request")
	}
}

func TestGatewayGitHTTPEndToEndPushAndFetch(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for gateway git e2e test")
	}

	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionPush, ActionPull}}); err != nil {
		t.Fatalf("grant push/pull: %v", err)
	}
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})

	hub := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, OperationTimeout: 10 * time.Second, MaxQueuedPerTarget: -1})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentGateway := NewGateway("0")
	service := gitkit.New(gitkit.Config{Dir: t.TempDir(), AutoCreate: true, AutoHooks: true})
	if err := service.Setup(); err != nil {
		t.Fatalf("setup gitkit service: %v", err)
	}
	agentGateway.gitHandler = http.StripPrefix("/git", service)

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	go agentGateway.ServeWebSocketConn(agentConn, "test-agent")
	waitForGatewayAgent(t, hub, "dev", "local")

	repoURL := gatewayGitTestURL(t, ts.URL, "dev", "local", "release", userToken.Plaintext)
	src := t.TempDir()
	runGitCommand(t, src, "init", "-b", "master")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("hello through gateway git\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runGitCommand(t, src, "add", "README.md")
	runGitCommand(t, src, "-c", "user.name=doops", "-c", "user.email=doops@example.com", "commit", "-m", "initial")
	commit := strings.TrimSpace(runGitCommand(t, src, "rev-parse", "HEAD"))

	pushOutput := runGitCommand(t, src, "push", repoURL, "HEAD:master")
	if !strings.Contains(pushOutput, "master") {
		t.Fatalf("push output should mention master branch, got:\n%s", pushOutput)
	}

	remoteRefs := runGitCommand(t, "", "ls-remote", repoURL, "refs/heads/master")
	if !strings.Contains(remoteRefs, commit) {
		t.Fatalf("ls-remote did not return pushed commit %s:\n%s", commit, remoteRefs)
	}
}

func TestGatewayGitHTTPConcurrentPushPullDifferentTargets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for gateway git e2e test")
	}

	hub, ts, tokens := newGatewayGitE2E(t, []gatewayGitE2ETarget{
		{Cluster: "dev-a", Instance: "node-a"},
		{Cluster: "dev-b", Instance: "node-b"},
	})
	defer ts.Close()
	for _, target := range tokens.Targets {
		conn := startGatewayGitTestAgent(t, hub, ts.URL, target.AgentToken, target.Cluster, target.Instance)
		defer conn.Close()
	}

	errCh := make(chan error, 2)
	for _, target := range tokens.Targets {
		target := target
		go func() {
			errCh <- pushCloneAndVerifyGatewayGit(ts.URL, target.Cluster, target.Instance, "parallel-"+target.Instance, target.UserToken, map[string][]byte{
				"README.md": []byte("hello from " + target.Instance + "\n"),
			})
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatewayGitHTTPConcurrentPushDifferentSessionsSameTarget(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for gateway git e2e test")
	}

	hub, ts, tokens := newGatewayGitE2E(t, []gatewayGitE2ETarget{
		{Cluster: "dev", Instance: "node"},
	})
	defer ts.Close()
	target := tokens.Targets[0]
	conn := startGatewayGitTestAgent(t, hub, ts.URL, target.AgentToken, target.Cluster, target.Instance)
	defer conn.Close()

	errCh := make(chan error, 2)
	for _, session := range []string{"parallel-a", "parallel-b"} {
		session := session
		go func() {
			errCh <- pushCloneAndVerifyGatewayGit(ts.URL, target.Cluster, target.Instance, session, target.UserToken, map[string][]byte{
				"README.md": []byte("hello from " + session + "\n"),
			})
		}()
	}
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatal(err)
		}
	}
}

func TestGatewayGitHTTPLargeFilePushAndPull(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required for gateway git e2e test")
	}

	hub, ts, tokens := newGatewayGitE2E(t, []gatewayGitE2ETarget{{Cluster: "dev-large", Instance: "node-large"}})
	defer ts.Close()
	target := tokens.Targets[0]
	conn := startGatewayGitTestAgent(t, hub, ts.URL, target.AgentToken, target.Cluster, target.Instance)
	defer conn.Close()

	large := makeDeterministicBytes(24 << 20)
	if err := pushCloneAndVerifyGatewayGit(ts.URL, target.Cluster, target.Instance, "large-file", target.UserToken, map[string][]byte{
		"assets/course-resource.bin": large,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestGatewayPasswordLoginIssuesUserToken(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUserWithPassword(CreateUserRequest{Name: "alice", Password: "Ab123456"})
	if err != nil {
		t.Fatalf("create password user: %v", err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionTargetsList}}); err != nil {
		t.Fatalf("grant targets list: %v", err)
	}
	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, LoginTokenTTL: 2 * time.Second})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/auth/login", "application/json", strings.NewReader(`{"username":"alice","password":"wrong"}`))
	if err != nil {
		t.Fatalf("post bad login: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password must be 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Post(ts.URL+"/v1/auth/login", "application/json", strings.NewReader(`{"username":"alice","password":"Ab123456","name":"test"}`))
	if err != nil {
		t.Fatalf("post login: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected login 200, got %d: %s", resp.StatusCode, body)
	}
	var parsed gatewayLoginResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("parse login response: %v", err)
	}
	if parsed.Token == "" || parsed.Username != "alice" || parsed.TokenType != string(TokenKindUser) {
		t.Fatalf("unexpected login response: %#v", parsed)
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/targets", nil)
	req.Header.Set("Authorization", "Bearer "+parsed.Token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get targets with login token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login token should authorize targets:list, got %d", resp.StatusCode)
	}
	time.Sleep(2100 * time.Millisecond)
	if _, err := store.VerifyUserToken(parsed.Token); err == nil {
		t.Fatal("password-login token must expire according to gateway LoginTokenTTL")
	}
}

func TestGatewayAuditHTTPRequiresAdminAndFiltersEvents(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	admin, _ := store.CreateUser("admin")
	adminToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: admin.ID, Name: "admin"})
	if err := store.GrantUser(admin.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionAdmin}}); err != nil {
		t.Fatalf("add admin grant: %v", err)
	}
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})

	id, err := store.StartAudit(AuditEvent{
		UserID:         user.ID,
		TokenID:        userToken.ID,
		Cluster:        "dev",
		Instance:       "local",
		Action:         ActionAsk,
		Session:        "deploy-1",
		CommandSummary: "deploy app",
		StartedAt:      time.Now().UTC().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("start audit: %v", err)
	}
	if err := store.FinishAudit(id, AuditFinish{Status: "success", Tail: "done", BytesOut: 4}); err != nil {
		t.Fatalf("finish audit: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/audit?session=deploy-1&limit=5", nil)
	req.Header.Set("Authorization", "Bearer "+userToken.Plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get audit as user: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected non-admin audit read to be forbidden, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/audit?session=deploy-1&limit=5", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get audit as admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected admin audit read to succeed, got %d", resp.StatusCode)
	}
	var body struct {
		Events []auditRecord `json:"events"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode audit response: %v", err)
	}
	if len(body.Events) != 1 || body.Events[0].Session != "deploy-1" || body.Events[0].Status != "success" {
		t.Fatalf("unexpected audit response: %#v", body.Events)
	}

	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/v1/audit?before="+time.Now().UTC().Add(time.Hour).Format(time.RFC3339), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete audit: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected admin audit purge to succeed, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestGatewayAdminTokenCreateRequiresAdminAndIssuesUserToken(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	admin, _ := store.CreateUser("admin")
	adminToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: admin.ID, Name: "admin"})
	if err := store.GrantUser(admin.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionAdmin}}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	alice, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: alice.ID, Name: "alice"})

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	reqBody := strings.NewReader(`{"user":"alice","name":"cli-issued","expires":"1h"}`)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/admin/tokens", reqBody)
	req.Header.Set("Authorization", "Bearer "+userToken.Plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create token as user: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected non-admin token create to be forbidden, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	reqBody = strings.NewReader(`{"user":"alice","name":"cli-issued","expires":"1h"}`)
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/admin/tokens", reqBody)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create token as admin: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected admin token create to succeed, got %d: %s", resp.StatusCode, body)
	}
	var body struct {
		Token     string `json:"token"`
		TokenID   string `json:"token_id"`
		TokenType string `json:"token_type"`
		Username  string `json:"username"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	if body.Token == "" || body.TokenID == "" || body.TokenType != string(TokenKindUser) || body.Username != "alice" {
		t.Fatalf("unexpected token response: %#v", body)
	}
	auth, err := store.VerifyUserToken(body.Token)
	if err != nil {
		t.Fatalf("created token should verify: %v", err)
	}
	if auth.UserID != alice.ID {
		t.Fatalf("created token user mismatch: want %s got %s", alice.ID, auth.UserID)
	}
	events, err := store.ListAuditFiltered(AuditFilter{Action: ActionAdmin, Limit: 10})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	if len(events) != 1 || events[0].Status != "success" || events[0].Session != "admin-token-create" {
		t.Fatalf("expected successful admin audit event, got %#v", events)
	}
}

func TestGatewayAdminReposCRUDAndTestAccess(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	admin, _ := store.CreateUser("admin")
	adminToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: admin.ID, Name: "admin"})
	if err := store.GrantUser(admin.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionAdmin}}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/admin/repos", nil)
	req.Header.Set("Authorization", "Bearer "+userToken.Plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list repos as user: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected non-admin repo list to be forbidden, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	reqBody := strings.NewReader(`{"name":"doops","url":"https://github.com/l8ai-cn/doops.git","branch":"main","username":"bot","password":"secret","description":"public repo"}`)
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/admin/repos", reqBody)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected repo create to succeed, got %d: %s", resp.StatusCode, body)
	}
	createBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read created repo body: %v", err)
	}
	if strings.Contains(string(createBody), "last_used_at") {
		t.Fatalf("unused repo response must omit last_used_at, got %s", createBody)
	}
	var created GitRepo
	if err := json.Unmarshal(createBody, &created); err != nil {
		t.Fatalf("decode created repo: %v", err)
	}
	if created.ID == "" || created.Name != "doops" || created.Branch != "main" || !created.HasPassword {
		t.Fatalf("unexpected created repo: %#v", created)
	}
	if created.PasswordHash != "" {
		t.Fatalf("repo response must not expose password hash: %#v", created)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/admin/repos", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	var listed struct {
		Repos []GitRepo `json:"repos"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		resp.Body.Close()
		t.Fatalf("decode repos: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(listed.Repos) != 1 || listed.Repos[0].ID != created.ID || !listed.Repos[0].HasPassword {
		t.Fatalf("unexpected repo list status=%d repos=%#v", resp.StatusCode, listed.Repos)
	}

	reqBody = strings.NewReader(`{"branch":"release","description":"updated"}`)
	req, _ = http.NewRequest(http.MethodPatch, ts.URL+"/v1/admin/repos?id="+url.QueryEscape(created.ID), reqBody)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("update repo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected repo update to succeed, got %d: %s", resp.StatusCode, body)
	}
	updateBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read updated repo body: %v", err)
	}
	if strings.Contains(string(updateBody), "last_used_at") {
		t.Fatalf("unused updated repo response must omit last_used_at, got %s", updateBody)
	}
	var updated GitRepo
	if err := json.Unmarshal(updateBody, &updated); err != nil {
		t.Fatalf("decode updated repo: %v", err)
	}
	if updated.Branch != "release" || updated.Description != "updated" || !updated.HasPassword {
		t.Fatalf("unexpected updated repo: %#v", updated)
	}

	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/v1/admin/repos/test?id="+url.QueryEscape(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("test repo: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected repo test to return JSON, got %d: %s", resp.StatusCode, body)
	}
	var tested struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tested); err != nil {
		t.Fatalf("decode repo test response: %v", err)
	}
	if !strings.Contains(tested.Message, "已保存") {
		t.Fatalf("expected saved repo test message, got %#v", tested)
	}

	req, _ = http.NewRequest(http.MethodDelete, ts.URL+"/v1/admin/repos?id="+url.QueryEscape(created.ID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected repo delete to succeed, got %d", resp.StatusCode)
	}

	req, _ = http.NewRequest(http.MethodGet, ts.URL+"/v1/admin/repos", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("list repos after delete: %v", err)
	}
	listed.Repos = nil
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil {
		resp.Body.Close()
		t.Fatalf("decode repos after delete: %v", err)
	}
	resp.Body.Close()
	if len(listed.Repos) != 0 {
		t.Fatalf("expected no repos after delete, got %#v", listed.Repos)
	}
}

func TestGatewayRPCForwardsAllowedActionAndRejectsDeniedAction(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, MaxQueuedPerTarget: -1})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	go serveFakeAgent(t, agentConn)

	clientWS := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/rpc?cluster=dev&instance=local"
	clientConn, _, err := websocket.DefaultDialer.Dial(clientWS, http.Header{"Authorization": []string{"Bearer " + userToken.Plaintext}})
	if err != nil {
		t.Fatalf("dial client websocket: %v", err)
	}
	defer clientConn.Close()
	writeGatewayJSON(t, clientConn, map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	readUntilID(t, clientConn, 1)

	args, _ := json.Marshal(map[string]interface{}{"command": "hostname", "session_id": "smoke"})
	writeGatewayJSON(t, clientConn, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "doops_shell",
			"arguments": json.RawMessage(args),
		},
	})
	final := readUntilID(t, clientConn, 2)
	if _, ok := final["result"]; !ok {
		t.Fatalf("expected forwarded result, got %#v", final)
	}

	writeArgs, _ := json.Marshal(map[string]interface{}{"path": "/tmp/nope", "content": "x"})
	writeGatewayJSON(t, clientConn, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "doops_file_write",
			"arguments": json.RawMessage(writeArgs),
		},
	})
	denied := readUntilID(t, clientConn, 3)
	if _, ok := denied["error"]; !ok {
		t.Fatalf("expected denied write error, got %#v", denied)
	}
	events, err := store.ListAudit(10)
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	foundForbiddenWrite := false
	for _, event := range events {
		if event.Action == ActionWrite && event.Status == "forbidden" {
			foundForbiddenWrite = true
		}
	}
	if !foundForbiddenWrite {
		t.Fatalf("expected forbidden write audit event, got %#v", events)
	}
}

func TestGatewayRPCDoesNotAllowClientActionOverrideForShell(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionInfo}}); err != nil {
		t.Fatalf("grant info: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, MaxQueuedPerTarget: -1})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	go serveFakeAgent(t, agentConn)

	clientConn := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientConn.Close()

	args, _ := json.Marshal(map[string]interface{}{
		"command":       "hostname",
		"session_id":    "smoke",
		"_doops_action": "info",
	})
	writeGatewayJSON(t, clientConn, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	denied := readUntilID(t, clientConn, 2)
	if errObj, ok := denied["error"].(map[string]interface{}); !ok || !strings.Contains(fmt.Sprint(errObj["message"]), "forbidden: exec") {
		t.Fatalf("expected shell to require exec permission despite override, got %#v", denied)
	}
}

func TestGatewayActionMappingUsesDedicatedToolsForCheckAndClean(t *testing.T) {
	if got := actionForTool("doops_shell", json.RawMessage(`{"_doops_action":"info"}`)); got != ActionExec {
		t.Fatalf("doops_shell must map to exec regardless of client override, got %q", got)
	}
	if got := actionForTool("doops_check_deployment", nil); got != ActionCheck {
		t.Fatalf("doops_check_deployment must map to check, got %q", got)
	}
	if got := actionForTool("doops_clean_workspace", nil); got != ActionClean {
		t.Fatalf("doops_clean_workspace must map to clean, got %q", got)
	}
}

func TestGatewayActionMappingUsesDedicatedToolForAgentUpgrade(t *testing.T) {
	if got := actionForTool("doops_agent_upgrade", nil); got != ActionAgentUpgrade {
		t.Fatalf("expected agent upgrade action, got %q", got)
	}
}

func TestGatewayActionMappingUsesDedicatedToolsForWorkspacePull(t *testing.T) {
	if got := actionForTool("doops_workspace_pull_begin", nil); got != ActionPull {
		t.Fatalf("expected workspace pull begin to map to pull, got %q", got)
	}
	if got := actionForTool("doops_workspace_pull_chunk", nil); got != ActionPull {
		t.Fatalf("expected workspace pull chunk to map to pull, got %q", got)
	}
}

func TestGatewayTargetBusySnapshotUsesOperationSlot(t *testing.T) {
	agent := &GatewayAgent{
		Cluster:   "dev",
		Instance:  "local",
		Key:       "dev/local",
		opSlot:    make(chan struct{}, 1),
		pending:   make(map[int64]chan gatewayWSMessage),
		resources: make(map[string]*agentResourceSlot),
	}
	agent.opSlot <- struct{}{}

	agent.setBusy(true)
	if got := agent.snapshot(); got.Busy {
		t.Fatalf("stale busy flag must not mark an idle target busy: %#v", got)
	}

	if err := agent.acquire(context.Background(), 0, time.Second); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if got := agent.snapshot(); !got.Busy || got.Status != "busy" || got.BusyReason != "exclusive_operation" {
		t.Fatalf("acquired target should be busy: %#v", got)
	}
	agent.release()
	if got := agent.snapshot(); got.Busy || got.Status != "idle" {
		t.Fatalf("released target should be idle: %#v", got)
	}
}

func TestGatewayTargetBusySnapshotUsesResourceLocks(t *testing.T) {
	agent := &GatewayAgent{
		Cluster:   "dev",
		Instance:  "local",
		Key:       "dev/local",
		opSlot:    make(chan struct{}, 1),
		pending:   make(map[int64]chan gatewayWSMessage),
		resources: make(map[string]*agentResourceSlot),
	}
	agent.opSlot <- struct{}{}

	if err := agent.acquireForAction(context.Background(), ActionPush, "workspace:release", 0, time.Second); err != nil {
		t.Fatalf("acquire resource: %v", err)
	}
	if got := agent.snapshot(); got.Busy || got.Status != "active" || got.ActiveOps != 1 || len(got.Resources) != 1 || got.Resources[0] != "workspace:release" {
		t.Fatalf("resource-locked target should be active but not target-busy: %#v", got)
	}
	agent.releaseForAction(ActionPush, "workspace:release")
	if got := agent.snapshot(); got.Busy || got.Status != "idle" {
		t.Fatalf("released resource target should be idle: %#v", got)
	}
}

func TestGatewayKeepsIdleAgentAliveWithPing(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})

	lease := 200 * time.Millisecond
	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: lease})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	go serveFakeAgent(t, agentConn)

	time.Sleep(lease * 4)
	targets := gw.ListTargets()
	if len(targets) != 1 {
		t.Fatalf("expected idle agent to stay online, got %d targets", len(targets))
	}
	if targets[0].Key != "dev/local" {
		t.Fatalf("unexpected target: %#v", targets[0])
	}
}

func TestGatewayRelayTimeoutDoesNotCloseAgentConnection(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, OperationTimeout: 25 * time.Millisecond})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	cancelSeen := make(chan struct{}, 1)
	go serveTimeoutFakeAgent(t, agentConn, cancelSeen)
	waitForGatewayAgent(t, gw, "dev", "local")
	agent := gw.getAgent("dev", "local")
	err = agent.relayToolCall(context.Background(), api.ToolCallParams{
		Name:      "doops_shell",
		Arguments: json.RawMessage(`{"session_id":"timeout","command":"sleep"}`),
	}, 25*time.Millisecond, func(gatewayWSMessage) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "operation timed out") {
		t.Fatalf("expected operation timeout, got %v", err)
	}
	select {
	case <-cancelSeen:
	case <-time.After(time.Second):
		t.Fatal("agent did not receive tools/cancel after timeout")
	}
	time.Sleep(50 * time.Millisecond)
	if gw.getAgent("dev", "local") == nil {
		t.Fatal("timed out operation must not unregister or close the agent connection")
	}
}

func TestGatewayAgentDeliverPendingBlocksInsteadOfDropping(t *testing.T) {
	agent := &GatewayAgent{closed: make(chan struct{})}
	ch := make(chan gatewayWSMessage, 1)
	ch <- gatewayWSMessage{Parsed: map[string]interface{}{"filled": true}}
	done := make(chan struct{})
	go func() {
		agent.deliverPending(gatewayWSMessage{Parsed: map[string]interface{}{"id": float64(2)}}, ch)
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("deliverPending returned while channel was full")
	case <-time.After(20 * time.Millisecond):
	}
	<-ch
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("deliverPending did not resume after channel space was available")
	}
	msg := <-ch
	if id, _ := msg.Parsed["id"].(float64); id != 2 {
		t.Fatalf("unexpected delivered message: %#v", msg.Parsed)
	}
}

func TestGatewayRPCRejectsSecondConcurrentOperationForSameSessionAsBusy(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, MaxQueuedPerTarget: -1})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	blockRelease := make(chan struct{})
	go serveBlockingFakeAgent(t, agentConn, blockRelease)

	clientWS := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/rpc?cluster=dev&instance=local"
	clientA, _, err := websocket.DefaultDialer.Dial(clientWS, http.Header{"Authorization": []string{"Bearer " + userToken.Plaintext}})
	if err != nil {
		t.Fatalf("dial client A: %v", err)
	}
	defer clientA.Close()
	clientB, _, err := websocket.DefaultDialer.Dial(clientWS, http.Header{"Authorization": []string{"Bearer " + userToken.Plaintext}})
	if err != nil {
		t.Fatalf("dial client B: %v", err)
	}
	defer clientB.Close()
	writeGatewayJSON(t, clientA, map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	readUntilID(t, clientA, 1)
	writeGatewayJSON(t, clientB, map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	readUntilID(t, clientB, 1)

	args, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "busy"})
	writeGatewayJSON(t, clientA, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	time.Sleep(200 * time.Millisecond)

	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	busy := readUntilID(t, clientB, 2)
	if errObj, ok := busy["error"].(map[string]interface{}); !ok || !strings.Contains(fmt.Sprint(errObj["message"]), "target busy") {
		t.Fatalf("expected target busy error, got %#v", busy)
	}
	close(blockRelease)
	readUntilID(t, clientA, 2)
}

func TestGatewayRPCAllowsConcurrentExecOperationsForDifferentSessions(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	release := make(chan struct{})
	started := make(chan string, 2)
	go serveSessionAwareBlockingFakeAgent(t, agentConn, started, release)

	clientA := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientA.Close()
	clientB := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientB.Close()

	argsA, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "session-a"})
	argsB, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "session-b"})
	writeGatewayJSON(t, clientA, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(argsA)},
	})
	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(argsB)},
	})

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case session := <-started:
			seen[session] = true
		case <-time.After(2 * time.Second):
			t.Fatal("expected both sessions to reach agent concurrently")
		}
	}
	if !seen["session-a"] || !seen["session-b"] {
		t.Fatalf("expected both sessions to start, got %#v", seen)
	}
	close(release)

	msgA := readGatewayNotification(t, clientA)
	msgB := readGatewayNotification(t, clientB)
	if got := notificationData(msgA); !strings.Contains(got, "session-a") {
		t.Fatalf("client A got wrong notification: %#v", msgA)
	}
	if got := notificationData(msgB); !strings.Contains(got, "session-b") {
		t.Fatalf("client B got wrong notification: %#v", msgB)
	}
	if resp := readUntilID(t, clientA, 2); resp["error"] != nil {
		t.Fatalf("client A final response failed: %#v", resp)
	}
	if resp := readUntilID(t, clientB, 2); resp["error"] != nil {
		t.Fatalf("client B final response failed: %#v", resp)
	}
}

func TestGatewayRPCDoesNotFallbackSessionNotificationsWithoutSessionID(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	release := make(chan struct{})
	started := make(chan string, 2)
	go serveSessionOrphanNotificationFakeAgent(t, agentConn, started, release)

	clientA := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientA.Close()
	clientB := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientB.Close()

	argsA, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "session-a"})
	argsB, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "session-b"})
	writeGatewayJSON(t, clientA, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(argsA)},
	})
	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(argsB)},
	})

	seen := map[string]bool{}
	for i := 0; i < 2; i++ {
		select {
		case session := <-started:
			seen[session] = true
		case <-time.After(2 * time.Second):
			t.Fatal("expected both sessions to reach agent concurrently")
		}
	}
	if !seen["session-a"] || !seen["session-b"] {
		t.Fatalf("expected both sessions to start, got %#v", seen)
	}
	close(release)

	for name, conn := range map[string]*websocket.Conn{"clientA": clientA, "clientB": clientB} {
		msg := readGatewayMessage(t, conn)
		if method, _ := msg["method"].(string); method == "notifications/message" {
			t.Fatalf("%s received orphan notification without session routing: %#v", name, msg)
		}
		if id, _ := msg["id"].(float64); id != 2 {
			t.Fatalf("%s expected final response id 2, got %#v", name, msg)
		}
	}
}

func TestGatewayResourceKeysUseSessionWorkspacePathAndTarget(t *testing.T) {
	if got := resourceKeyForTool(ActionExec, "doops_shell", json.RawMessage(`{"session_id":"exec-a"}`), "dev", "node"); got != "session:exec-a" {
		t.Fatalf("exec resource key mismatch: %q", got)
	}
	if got := resourceKeyForTool(ActionAsk, "doops_agent_prompt", json.RawMessage(`{"session_id":"ask-a"}`), "dev", "node"); got != "session:ask-a" {
		t.Fatalf("ask resource key mismatch: %q", got)
	}
	if got := resourceKeyForTool(ActionPush, "doops_workspace_begin", json.RawMessage(`{"session_id":"push-a"}`), "dev", "node"); got != "workspace:push-a" {
		t.Fatalf("push resource key mismatch: %q", got)
	}
	if got := resourceKeyForTool(ActionPull, "doops_workspace_pull_begin", json.RawMessage(`{"session_id":"pull-a"}`), "dev", "node"); got != "workspace:pull-a" {
		t.Fatalf("pull resource key mismatch: %q", got)
	}
	if got := resourceKeyForTool(ActionWrite, "doops_file_write", json.RawMessage(`{"path":"/tmp/x"}`), "dev", "node"); got != "path:/tmp/x" {
		t.Fatalf("write resource key mismatch: %q", got)
	}
	if got := resourceKeyForTool(ActionAgentUpgrade, "doops_agent_upgrade", json.RawMessage(`{}`), "dev", "node"); got != "target:dev/node" {
		t.Fatalf("upgrade resource key mismatch: %q", got)
	}
}

func TestGatewayRPCAllowsConcurrentReadOnlyOperationsForSameTarget(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionInfo}}); err != nil {
		t.Fatalf("grant info: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	release := make(chan struct{})
	started := make(chan int, 2)
	go serveConcurrentBlockingFakeAgent(t, agentConn, started, release)

	clientA := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientA.Close()
	clientB := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientB.Close()

	args, _ := json.Marshal(map[string]interface{}{"session_id": "info"})
	writeGatewayJSON(t, clientA, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_node_info", "arguments": json.RawMessage(args)},
	})
	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_node_info", "arguments": json.RawMessage(args)},
	})

	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("expected both read-only operations to reach agent concurrently")
		}
	}
	close(release)
	respA := readUntilID(t, clientA, 2)
	respB := readUntilID(t, clientB, 2)
	if _, ok := respA["error"]; ok {
		t.Fatalf("first info operation failed: %#v", respA)
	}
	if _, ok := respB["error"]; ok {
		t.Fatalf("second info operation failed: %#v", respB)
	}
}

func TestGatewayRPCQueuesSecondOperationForSameTarget(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{
		AgentLease:         time.Minute,
		TargetQueueTimeout: time.Second,
		MaxQueuedPerTarget: 1,
	})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	blockRelease := make(chan struct{})
	go serveBlockingFakeAgent(t, agentConn, blockRelease)

	clientA := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientA.Close()
	clientB := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientB.Close()

	args, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "queue"})
	writeGatewayJSON(t, clientA, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	time.Sleep(200 * time.Millisecond)
	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})

	close(blockRelease)
	readUntilID(t, clientA, 2)
	respB := readUntilID(t, clientB, 2)
	if _, ok := respB["error"]; ok {
		t.Fatalf("queued operation should succeed after first operation releases, got %#v", respB)
	}
}

func TestGatewayRPCDefaultDoesNotQueueBehindBusyTarget(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	blockRelease := make(chan struct{})
	go serveBlockingFakeAgent(t, agentConn, blockRelease)

	clientA := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientA.Close()
	clientB := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientB.Close()

	args, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "busy-default"})
	writeGatewayJSON(t, clientA, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	time.Sleep(200 * time.Millisecond)

	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	busy := readUntilID(t, clientB, 2)
	if errObj, ok := busy["error"].(map[string]interface{}); !ok || !strings.Contains(fmt.Sprint(errObj["message"]), "target busy") {
		t.Fatalf("expected default busy instead of hidden queue, got %#v", busy)
	}
	close(blockRelease)
	readUntilID(t, clientA, 2)
}

func TestGatewayRPCDrainsDisconnectedClientBeforeReleasingTarget(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	blockRelease := make(chan struct{})
	go serveBlockingFakeAgent(t, agentConn, blockRelease)

	clientA := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	args, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "disconnect-drain"})
	writeGatewayJSON(t, clientA, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	time.Sleep(200 * time.Millisecond)
	_ = clientA.Close()

	clientB := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer clientB.Close()
	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	busy := readUntilID(t, clientB, 2)
	if errObj, ok := busy["error"].(map[string]interface{}); !ok || !strings.Contains(fmt.Sprint(errObj["message"]), "target busy") {
		t.Fatalf("expected disconnected operation to keep target busy until agent responds, got %#v", busy)
	}

	close(blockRelease)
	time.Sleep(200 * time.Millisecond)
	writeGatewayJSON(t, clientB, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	resp := readUntilID(t, clientB, 3)
	if _, ok := resp["error"]; ok {
		t.Fatalf("expected target to be released after drained response, got %#v", resp)
	}
}

func TestGatewayGitClientCancelReleasesTarget(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "dev", Instance: "local", Actions: []GatewayAction{ActionPull}}); err != nil {
		t.Fatalf("grant pull: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, OperationTimeout: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	requests := make(chan int, 2)
	go serveStallingGitAgent(t, agentConn, requests)

	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstURL := gatewayGitTestURL(t, ts.URL, "dev", "local", "cancelled", userToken.Plaintext) + "/info/refs?service=git-upload-pack"
	firstReq, _ := http.NewRequestWithContext(firstCtx, http.MethodGet, firstURL, nil)
	firstDone := make(chan error, 1)
	go func() {
		resp, err := http.DefaultClient.Do(firstReq)
		if resp != nil {
			_ = resp.Body.Close()
		}
		firstDone <- err
	}()

	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("first git request did not reach fake agent")
	}
	cancelFirst()
	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled git request did not return")
	}

	secondURL := gatewayGitTestURL(t, ts.URL, "dev", "local", "second", userToken.Plaintext) + "/info/refs?service=git-upload-pack"
	resp, err := http.Get(secondURL)
	if err != nil {
		t.Fatalf("second git request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusConflict {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("target remained busy after cancelled git request: %s", body)
	}
	select {
	case <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("second git request did not reach fake agent")
	}
}

func TestGatewayAdminUnlockClearsBusyTarget(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	admin, _ := store.CreateUser("admin")
	adminToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: admin.ID, Name: "admin"})
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})
	if err := store.GrantUser(admin.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionAdmin}}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agentConn := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agentConn.Close()
	go serveFakeAgent(t, agentConn)
	waitForGatewayAgent(t, gw, "dev", "local")

	agent := gw.getAgent("dev", "local")
	if err := agent.acquire(context.Background(), 0, 0); err != nil {
		t.Fatalf("acquire target slot: %v", err)
	}
	if !agent.snapshot().Busy {
		t.Fatal("test setup expected busy target")
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/targets/unlock?cluster=dev&instance=local", nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("unlock request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("unlock failed with %d: %s", resp.StatusCode, body)
	}
	if agent.snapshot().Busy {
		t.Fatal("unlock should clear busy state immediately")
	}
}

func TestGatewayRPCEnforcesPerUserConcurrencyLimitAcrossTargets(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	user, _ := store.CreateUser("alice")
	userToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "alice"})
	devToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "dev-agent", Cluster: "dev", Instance: "local"})
	prodToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "prod-agent", Cluster: "prod", Instance: "local"})
	if err := store.GrantUser(user.ID, ScopeGrant{Cluster: "*", Instance: "local", Actions: []GatewayAction{ActionExec}}); err != nil {
		t.Fatalf("grant exec: %v", err)
	}

	gw := NewGatewayHub(store, GatewayHubOptions{
		AgentLease:              time.Minute,
		MaxConcurrentOperations: 4,
		MaxConcurrentPerUser:    1,
	})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	devAgent := dialFakeGatewayAgent(t, ts.URL, devToken.Plaintext, "dev", "local")
	defer devAgent.Close()
	prodAgent := dialFakeGatewayAgent(t, ts.URL, prodToken.Plaintext, "prod", "local")
	defer prodAgent.Close()
	release := make(chan struct{})
	go serveBlockingFakeAgent(t, devAgent, release)
	go serveFakeAgent(t, prodAgent)

	devClient := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "dev", "local")
	defer devClient.Close()
	prodClient := dialGatewayRPCClient(t, ts.URL, userToken.Plaintext, "prod", "local")
	defer prodClient.Close()

	args, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "limit"})
	writeGatewayJSON(t, devClient, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	time.Sleep(200 * time.Millisecond)

	writeGatewayJSON(t, prodClient, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})
	limited := readUntilID(t, prodClient, 2)
	if errObj, ok := limited["error"].(map[string]interface{}); !ok || !strings.Contains(fmt.Sprint(errObj["message"]), "user operation limit exceeded") {
		t.Fatalf("expected user operation limit error, got %#v", limited)
	}
	close(release)
	readUntilID(t, devClient, 2)
}

func TestGatewayAdminOperationsListAndCancel(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	admin, _ := store.CreateUser("admin")
	adminToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: admin.ID, Name: "admin"})
	if err := store.GrantUser(admin.ID, ScopeGrant{Cluster: "*", Instance: "*", Actions: []GatewayAction{ActionAdmin, ActionExec}}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	agentToken, _ := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: "dev", Instance: "local"})

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, OperationTimeout: time.Minute})
	mux := http.NewServeMux()
	gw.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	agent := dialFakeGatewayAgent(t, ts.URL, agentToken.Plaintext, "dev", "local")
	defer agent.Close()
	release := make(chan struct{})
	defer close(release)
	go serveBlockingFakeAgent(t, agent, release)

	client := dialGatewayRPCClient(t, ts.URL, adminToken.Plaintext, "dev", "local")
	defer client.Close()
	args, _ := json.Marshal(map[string]interface{}{"command": "sleep", "session_id": "cancel-me"})
	writeGatewayJSON(t, client, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params":  map[string]interface{}{"name": "doops_shell", "arguments": json.RawMessage(args)},
	})

	var opID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/admin/operations", nil)
		req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("list operations: %v", err)
		}
		var parsed struct {
			Operations []GatewayActiveOperation `json:"operations"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			resp.Body.Close()
			t.Fatalf("decode operations: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected operations list to succeed, got %d", resp.StatusCode)
		}
		if len(parsed.Operations) > 0 {
			op := parsed.Operations[0]
			if op.Session != "cancel-me" || op.Kind != "rpc" || op.Action != ActionExec {
				t.Fatalf("unexpected operation: %#v", op)
			}
			opID = op.ID
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if opID == "" {
		t.Fatal("expected active operation to be listed")
	}

	req, _ := http.NewRequest(http.MethodDelete, ts.URL+"/v1/admin/operations?id="+url.QueryEscape(opID), nil)
	req.Header.Set("Authorization", "Bearer "+adminToken.Plaintext)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("cancel operation: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected cancel to succeed, got %d", resp.StatusCode)
	}
	msg := readUntilID(t, client, 2)
	errObj, _ := msg["error"].(map[string]interface{})
	if !strings.Contains(fmt.Sprint(errObj["message"]), "operation canceled") {
		t.Fatalf("expected canceled client response, got %#v", msg)
	}
}

func TestGatewayStaleAgentDisconnectDoesNotMarkReplacementOffline(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	gw := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute})
	oldAgent := &GatewayAgent{
		Cluster:  "dev",
		Instance: "local",
		Key:      tunnelKey("dev", "local"),
		TokenID:  "old",
		Remote:   "old",
	}
	newAgent := &GatewayAgent{
		Cluster:  "dev",
		Instance: "local",
		Key:      tunnelKey("dev", "local"),
		TokenID:  "new",
		Remote:   "new",
	}

	gw.registerAgent(oldAgent)
	gw.registerAgent(newAgent)
	gw.unregisterAgent(oldAgent)

	targets := gw.ListTargets()
	if len(targets) != 1 || targets[0].TokenID != "new" {
		t.Fatalf("expected replacement agent to remain online in memory, got %#v", targets)
	}
	statuses, err := store.ListAgentStatus()
	if err != nil {
		t.Fatalf("list agent status: %v", err)
	}
	if len(statuses) != 1 || statuses[0].Status != "online" || statuses[0].TokenID != "new" {
		t.Fatalf("expected replacement agent to remain online in store, got %#v", statuses)
	}
}

func TestAuditTailBufferKeepsOnlyBoundedSuffix(t *testing.T) {
	buf := newAuditTailBuffer(16)
	buf.WriteString(strings.Repeat("a", 1024))
	if got := buf.Len(); got > 16 {
		t.Fatalf("tail buffer grew past bound: %d", got)
	}
	buf.WriteString("0123456789abcdef")
	buf.WriteString("XYZ")
	if got := buf.String(); got != "3456789abcdefXYZ" {
		t.Fatalf("unexpected tail suffix: %q", got)
	}
}

func dialFakeGatewayAgent(t *testing.T, serverURL, token, cluster, instance string) *websocket.Conn {
	t.Helper()
	agentWS := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/agent/connect?cluster=" + cluster + "&instance=" + instance
	header := http.Header{}
	if strings.TrimSpace(token) != "" {
		header.Set("Authorization", "Bearer "+token)
	}
	agentConn, _, err := websocket.DefaultDialer.Dial(agentWS, header)
	if err != nil {
		t.Fatalf("dial agent websocket: %v", err)
	}
	return agentConn
}

type gatewayGitE2ETarget struct {
	Cluster  string
	Instance string
}

type gatewayGitE2EToken struct {
	Cluster    string
	Instance   string
	UserToken  string
	AgentToken string
}

type gatewayGitE2ETokens struct {
	Targets []gatewayGitE2EToken
}

func newGatewayGitE2E(t *testing.T, targets []gatewayGitE2ETarget) (*GatewayHub, *httptest.Server, gatewayGitE2ETokens) {
	t.Helper()
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	hub := NewGatewayHub(store, GatewayHubOptions{AgentLease: time.Minute, OperationTimeout: 30 * time.Second, MaxQueuedPerTarget: -1})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	ts := httptest.NewServer(mux)

	out := gatewayGitE2ETokens{Targets: make([]gatewayGitE2EToken, 0, len(targets))}
	for idx, target := range targets {
		user, err := store.CreateUser(fmt.Sprintf("user-%d", idx))
		if err != nil {
			t.Fatalf("create user: %v", err)
		}
		userToken, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "git-e2e"})
		if err != nil {
			t.Fatalf("create user token: %v", err)
		}
		if err := store.GrantUser(user.ID, ScopeGrant{Cluster: target.Cluster, Instance: target.Instance, Actions: []GatewayAction{ActionPush, ActionPull}}); err != nil {
			t.Fatalf("grant push/pull: %v", err)
		}
		agentToken, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindAgent, Name: "agent", Cluster: target.Cluster, Instance: target.Instance})
		if err != nil {
			t.Fatalf("create agent token: %v", err)
		}
		out.Targets = append(out.Targets, gatewayGitE2EToken{
			Cluster:    target.Cluster,
			Instance:   target.Instance,
			UserToken:  userToken.Plaintext,
			AgentToken: agentToken.Plaintext,
		})
	}
	return hub, ts, out
}

func startGatewayGitTestAgent(t *testing.T, hub *GatewayHub, serverURL, token, cluster, instance string) *websocket.Conn {
	t.Helper()
	agentGateway := NewGateway("0")
	service := gitkit.New(gitkit.Config{Dir: t.TempDir(), AutoCreate: true, AutoHooks: true})
	if err := service.Setup(); err != nil {
		t.Fatalf("setup gitkit service: %v", err)
	}
	agentGateway.gitHandler = http.StripPrefix("/git", service)

	conn := dialFakeGatewayAgent(t, serverURL, token, cluster, instance)
	go agentGateway.ServeWebSocketConn(conn, "test-agent")
	waitForGatewayAgent(t, hub, cluster, instance)
	return conn
}

func waitForGatewayAgent(t *testing.T, hub *GatewayHub, cluster, instance string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if hub.getAgent(cluster, instance) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("gateway agent did not become online: %s/%s", cluster, instance)
}

func gatewayGitTestURL(t *testing.T, serverURL, cluster, instance, session, token string) string {
	t.Helper()
	u, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	u.User = url.UserPassword("doops", token)
	u.Path = "/v1/git/" + cluster + "/" + instance + "/" + session + ".git"
	u.RawQuery = ""
	return u.String()
}

func pushCloneAndVerifyGatewayGit(serverURL, cluster, instance, session, token string, files map[string][]byte) error {
	src, err := os.MkdirTemp("", "doops-git-src-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(src)
	for name, content := range files {
		path := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, content, 0644); err != nil {
			return err
		}
	}
	if _, err := runGitCommandE(src, "init", "-b", "master"); err != nil {
		return err
	}
	if _, err := runGitCommandE(src, "add", "."); err != nil {
		return err
	}
	if _, err := runGitCommandE(src, "-c", "user.name=doops", "-c", "user.email=doops@example.com", "commit", "-m", "sync"); err != nil {
		return err
	}
	commit, err := runGitCommandE(src, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	repoURL, err := gatewayGitURL(serverURL, cluster, instance, session, token)
	if err != nil {
		return err
	}
	if _, err := runGitCommandE(src, "push", repoURL, "HEAD:master"); err != nil {
		return err
	}
	refs, err := runGitCommandE("", "ls-remote", repoURL, "refs/heads/master")
	if err != nil {
		return err
	}
	if !strings.Contains(refs, strings.TrimSpace(commit)) {
		return fmt.Errorf("remote refs do not contain pushed commit %s: %s", strings.TrimSpace(commit), refs)
	}
	dest, err := os.MkdirTemp("", "doops-git-clone-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dest)
	if _, err := runGitCommandE("", "clone", "--quiet", "--branch", "master", repoURL, dest); err != nil {
		return err
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(dest, name))
		if err != nil {
			return err
		}
		if sha256.Sum256(got) != sha256.Sum256(want) {
			return fmt.Errorf("cloned file checksum mismatch: %s", name)
		}
	}
	return nil
}

func gatewayGitURL(serverURL, cluster, instance, session, token string) (string, error) {
	u, err := url.Parse(serverURL)
	if err != nil {
		return "", err
	}
	u.User = url.UserPassword("doops", token)
	u.Path = "/v1/git/" + cluster + "/" + instance + "/" + session + ".git"
	u.RawQuery = ""
	return u.String(), nil
}

func runGitCommand(t *testing.T, dir string, args ...string) string {
	t.Helper()
	output, err := runGitCommandE(dir, args...)
	if err != nil {
		t.Fatal(err)
	}
	return output
}

func runGitCommandE(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s failed: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func makeDeterministicBytes(size int) []byte {
	out := make([]byte, size)
	rng := rand.New(rand.NewSource(42))
	_, _ = rng.Read(out)
	return out
}

func dialGatewayRPCClient(t *testing.T, serverURL, token, cluster, instance string) *websocket.Conn {
	t.Helper()
	clientWS := "ws" + strings.TrimPrefix(serverURL, "http") + "/v1/rpc?cluster=" + cluster + "&instance=" + instance
	conn, _, err := websocket.DefaultDialer.Dial(clientWS, http.Header{"Authorization": []string{"Bearer " + token}})
	if err != nil {
		t.Fatalf("dial client websocket: %v", err)
	}
	writeGatewayJSON(t, conn, map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "initialize"})
	readUntilID(t, conn, 1)
	return conn
}

func serveFakeAgent(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "tools/call":
			params, _ := msg["params"].(map[string]interface{})
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"method":  "notifications/message",
				"params":  map[string]interface{}{"data": "fake-agent-output\n"},
			})
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result": map[string]interface{}{
					"content": []map[string]interface{}{{"type": "text", "text": "ok " + params["name"].(string)}},
				},
			})
		}
	}
}

func serveFakeGitAgent(t *testing.T, conn *websocket.Conn, requests chan<- string) {
	t.Helper()
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "git/http":
			params, _ := msg["params"].(map[string]interface{})
			requests <- fmt.Sprintf("%s %s?%s", params["method"], params["path"], params["raw_query"])
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"method":  "git/response",
				"params": map[string]interface{}{
					"status":  200,
					"headers": map[string][]string{"Content-Type": {"application/x-git-upload-pack-advertisement"}},
				},
			})
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"method":  "git/body",
				"params":  map[string]interface{}{"data_b64": "Z2l0LW9r", "eof": true},
			})
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"ok": true},
			})
		}
	}
}

func serveStallingGitAgent(t *testing.T, conn *websocket.Conn, requests chan<- int) {
	t.Helper()
	var count int
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "git/http":
			count++
			requests <- 1
			if count == 1 {
				continue
			}
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"method":  "git/response",
				"params": map[string]interface{}{
					"status":  200,
					"headers": map[string][]string{"Content-Type": {"application/x-git-upload-pack-advertisement"}},
				},
			})
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"method":  "git/body",
				"params":  map[string]interface{}{"data_b64": "Z2l0LW9r", "eof": true},
			})
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"ok": true},
			})
		}
	}
}

func serveConcurrentBlockingFakeAgent(t *testing.T, conn *websocket.Conn, started chan<- int, release <-chan struct{}) {
	t.Helper()
	var writeMu sync.Mutex
	writeJSON := func(msg map[string]interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(msg)
	}
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "tools/call":
			reqID := msg["id"]
			started <- 1
			go func() {
				<-release
				writeJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      reqID,
					"result":  map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "ok"}}},
				})
			}()
		}
	}
}

func serveSessionAwareBlockingFakeAgent(t *testing.T, conn *websocket.Conn, started chan<- string, release <-chan struct{}) {
	t.Helper()
	var writeMu sync.Mutex
	writeJSON := func(msg map[string]interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(msg)
	}
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "tools/call":
			reqID := msg["id"]
			params, _ := msg["params"].(map[string]interface{})
			args, _ := params["arguments"].(map[string]interface{})
			sessionID, _ := args["session_id"].(string)
			started <- sessionID
			go func() {
				<-release
				writeJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "notifications/message",
					"params":  map[string]interface{}{"sessionID": sessionID, "data": "output " + sessionID + "\n"},
				})
				writeJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      reqID,
					"result":  map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "ok " + sessionID}}},
				})
			}()
		}
	}
}

func serveSessionOrphanNotificationFakeAgent(t *testing.T, conn *websocket.Conn, started chan<- string, release <-chan struct{}) {
	t.Helper()
	var writeMu sync.Mutex
	writeJSON := func(msg map[string]interface{}) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.WriteJSON(msg)
	}
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			writeJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "tools/call":
			reqID := msg["id"]
			params, _ := msg["params"].(map[string]interface{})
			args, _ := params["arguments"].(map[string]interface{})
			sessionID, _ := args["session_id"].(string)
			started <- sessionID
			go func() {
				<-release
				writeJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "notifications/message",
					"params":  map[string]interface{}{"data": "orphan output " + sessionID + "\n"},
				})
				time.Sleep(50 * time.Millisecond)
				writeJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      reqID,
					"result":  map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "ok " + sessionID}}},
				})
			}()
		}
	}
}

func serveTimeoutFakeAgent(t *testing.T, conn *websocket.Conn, cancelSeen chan<- struct{}) {
	t.Helper()
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "tools/cancel":
			select {
			case cancelSeen <- struct{}{}:
			default:
			}
		}
	}
}

func serveBlockingFakeAgent(t *testing.T, conn *websocket.Conn, release <-chan struct{}) {
	t.Helper()
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["method"] {
		case "initialize":
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
			})
		case "tools/call":
			<-release
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      msg["id"],
				"result":  map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "ok"}}},
			})
		}
	}
}

func writeGatewayJSON(t *testing.T, conn *websocket.Conn, msg map[string]interface{}) {
	t.Helper()
	if err := conn.WriteJSON(msg); err != nil {
		t.Fatalf("write json: %v", err)
	}
}

func readUntilID(t *testing.T, conn *websocket.Conn, id float64) map[string]interface{} {
	t.Helper()
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read json: %v", err)
		}
		if msgID, _ := msg["id"].(float64); msgID == id {
			return msg
		}
	}
}

func readGatewayNotification(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read notification: %v", err)
		}
		if method, _ := msg["method"].(string); method == "notifications/message" {
			return msg
		}
	}
	t.Fatal("timed out waiting for notification")
	return nil
}

func readGatewayMessage(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer conn.SetReadDeadline(time.Time{})
	var msg map[string]interface{}
	if err := conn.ReadJSON(&msg); err != nil {
		t.Fatalf("read gateway message: %v", err)
	}
	return msg
}

func notificationData(msg map[string]interface{}) string {
	params, _ := msg["params"].(map[string]interface{})
	data, _ := params["data"].(string)
	return data
}
