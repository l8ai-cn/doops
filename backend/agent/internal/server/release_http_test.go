package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReleaseHTTPRegistersTicketAndSupportsAuthorizedStatusLookup(t *testing.T) {
	store := newReleaseTestStore(t)
	hub := NewGatewayHub(store, GatewayHubOptions{})
	coordinator := newReleaseCoordinator(store, &fakeReleaseExecutor{}, time.Hour, time.Minute)
	hub.AttachReleaseCoordinator(coordinator)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()

	ownerToken := releaseHTTPUserToken(t, store, "owner", "dev", "node")
	observerToken := releaseHTTPUserToken(t, store, "observer", "dev", "node")
	otherToken := releaseHTTPUserToken(t, store, "other", "prod", "node")

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	body := releaseHTTPCreateBody("session-owner", "publish api", "api")
	body["scope"] = "client-controlled-scope"
	created := releaseHTTPPost(t, server.URL, ownerToken, body, http.StatusAccepted)
	if created.Number <= 0 || created.Status != ReleaseStatusQueued {
		t.Fatalf("unexpected created ticket: %#v", created)
	}
	if created.Scope != "deployment:dev/node/prod/default/api/api" {
		t.Fatalf("scope must be derived from the deployment identity: %q", created.Scope)
	}
	if created.SessionID != "session-owner" || created.Requirement != "publish api" {
		t.Fatalf("ticket lost the originating request: %#v", created)
	}

	stored := mustGetRelease(t, store, created.Number)
	if stored.Instruction != "reconcile the declared deployment plan" {
		t.Fatalf("private instruction was not persisted: %#v", stored)
	}

	got, raw := releaseHTTPGet(t, server.URL, observerToken, created.Number, http.StatusOK)
	if got.Number != created.Number || got.UserID == "" {
		t.Fatalf("authorized observer did not receive ticket: %#v", got)
	}
	if bytes.Contains(raw, []byte("reconcile the declared deployment plan")) ||
		bytes.Contains(raw, []byte(stored.TokenID)) {
		t.Fatalf("ticket response leaked private execution data: %s", raw)
	}

	releaseHTTPGet(t, server.URL, otherToken, created.Number, http.StatusNotFound)
	releaseHTTPGet(t, server.URL, "", created.Number, http.StatusUnauthorized)
}

func TestReleaseHTTPLatestQueuedRequestWinsPerDerivedScope(t *testing.T) {
	store := newReleaseTestStore(t)
	hub := NewGatewayHub(store, GatewayHubOptions{})
	coordinator := newReleaseCoordinator(store, &fakeReleaseExecutor{}, time.Hour, time.Minute)
	hub.AttachReleaseCoordinator(coordinator)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()
	token := releaseHTTPUserToken(t, store, "owner", "dev", "node")

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	first := releaseHTTPPost(t, server.URL, token, releaseHTTPCreateBody("session-1", "publish first", "api"), http.StatusAccepted)
	second := releaseHTTPPost(t, server.URL, token, releaseHTTPCreateBody("session-2", "publish latest", "api"), http.StatusAccepted)
	other := releaseHTTPPost(t, server.URL, token, releaseHTTPCreateBody("session-3", "publish worker", "worker"), http.StatusAccepted)

	first = mustGetRelease(t, store, first.Number)
	second = mustGetRelease(t, store, second.Number)
	other = mustGetRelease(t, store, other.Number)
	if first.Status != ReleaseStatusSuperseded || first.SupersededBy == nil || *first.SupersededBy != second.Number {
		t.Fatalf("older queued request was not superseded: %#v", first)
	}
	if second.Status != ReleaseStatusQueued || other.Status != ReleaseStatusQueued {
		t.Fatalf("latest same-scope and different-scope tickets must remain queued: second=%#v other=%#v", second, other)
	}
	if second.Scope == other.Scope {
		t.Fatalf("different deployment objects must not share a scope: second=%q other=%q", second.Scope, other.Scope)
	}
}

func TestReleaseHTTPRejectsInvalidOrUnauthorizedRequests(t *testing.T) {
	store := newReleaseTestStore(t)
	hub := NewGatewayHub(store, GatewayHubOptions{})
	coordinator := newReleaseCoordinator(store, &fakeReleaseExecutor{}, time.Hour, time.Minute)
	hub.AttachReleaseCoordinator(coordinator)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()
	authorized := releaseHTTPUserToken(t, store, "authorized", "dev", "node")
	unauthorized := releaseHTTPUserToken(t, store, "unauthorized", "prod", "node")

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	releaseHTTPPost(t, server.URL, unauthorized, releaseHTTPCreateBody("session", "publish", "api"), http.StatusForbidden)

	invalid := releaseHTTPCreateBody("session", "publish", "api")
	invalid["plan_digest"] = "not-a-digest"
	releaseHTTPPost(t, server.URL, authorized, invalid, http.StatusBadRequest)

	invalid = releaseHTTPCreateBody("session", "publish", "api")
	invalid["required_evidence"] = []string{}
	releaseHTTPPost(t, server.URL, authorized, invalid, http.StatusBadRequest)
}

func TestReleaseHTTPRejectsUnsafeSessionBeforeSupersedingQueuedTicket(t *testing.T) {
	store := newReleaseTestStore(t)
	hub := NewGatewayHub(store, GatewayHubOptions{})
	coordinator := newReleaseCoordinator(store, &fakeReleaseExecutor{}, time.Hour, time.Minute)
	hub.AttachReleaseCoordinator(coordinator)
	if err := coordinator.Start(); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	defer coordinator.Stop()
	token := releaseHTTPUserToken(t, store, "owner", "dev", "node")

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	queued := releaseHTTPPost(t, server.URL, token, releaseHTTPCreateBody("safe-session", "publish safe", "api"), http.StatusAccepted)
	unsafe := releaseHTTPCreateBody("../unsafe", "publish unsafe", "api")
	releaseHTTPPost(t, server.URL, token, unsafe, http.StatusBadRequest)

	got := mustGetRelease(t, store, queued.Number)
	if got.Status != ReleaseStatusQueued || got.SupersededBy != nil {
		t.Fatalf("unsafe session request must not supersede a valid queued release: %#v", got)
	}
}

func TestReleaseHTTPAuthenticatesBeforeExposingCoordinatorState(t *testing.T) {
	store := newReleaseTestStore(t)
	hub := NewGatewayHub(store, GatewayHubOptions{})
	hub.AttachReleaseCoordinator(newReleaseCoordinator(store, &fakeReleaseExecutor{}, time.Hour, time.Minute))

	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	releaseHTTPPost(t, server.URL, "", releaseHTTPCreateBody("session", "publish", "api"), http.StatusUnauthorized)
}

func releaseHTTPCreateBody(session, requirement, application string) map[string]interface{} {
	return map[string]interface{}{
		"session_id":        session,
		"requirement":       requirement,
		"cluster":           "dev",
		"instance":          "node",
		"application":       application,
		"environment":       "prod",
		"namespace":         "default",
		"release_name":      application,
		"plan_digest":       "sha256:" + strings.Repeat("a", 64),
		"source_revision":   strings.Repeat("b", 40),
		"workspace_commit":  strings.Repeat("c", 40),
		"execution_mode":    "dry-run",
		"instruction":       "reconcile the declared deployment plan",
		"required_evidence": []string{"source-identity", "runtime-state"},
	}
}

func releaseHTTPUserToken(t *testing.T, store *GatewayStore, name, cluster, instance string) string {
	t.Helper()
	user, err := store.CreateUser(name)
	if err != nil {
		t.Fatalf("create user %s: %v", name, err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{
		Cluster:  cluster,
		Instance: instance,
		Actions:  []GatewayAction{ActionReconcile},
	}); err != nil {
		t.Fatalf("grant user %s: %v", name, err)
	}
	token, err := store.CreateToken(CreateTokenRequest{
		Kind:   TokenKindUser,
		UserID: user.ID,
		Name:   name,
	})
	if err != nil {
		t.Fatalf("create token %s: %v", name, err)
	}
	return token.Plaintext
}

func releaseHTTPPost(t *testing.T, baseURL, token string, body map[string]interface{}, wantStatus int) ReleaseTicket {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("encode release request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/cicd/releases", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("create release request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post release: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read release response: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("unexpected POST status %d, want %d: %s", resp.StatusCode, wantStatus, data)
	}
	if wantStatus != http.StatusAccepted {
		return ReleaseTicket{}
	}
	var ticket ReleaseTicket
	if err := json.Unmarshal(data, &ticket); err != nil {
		t.Fatalf("decode release response: %v: %s", err, data)
	}
	return ticket
}

func releaseHTTPGet(t *testing.T, baseURL, token string, number int64, wantStatus int) (ReleaseTicket, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/cicd/releases/"+strconv.FormatInt(number, 10), nil)
	if err != nil {
		t.Fatalf("create release lookup: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("get release: %v", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read release lookup: %v", err)
	}
	if resp.StatusCode != wantStatus {
		t.Fatalf("unexpected GET status %d, want %d: %s", resp.StatusCode, wantStatus, data)
	}
	if wantStatus != http.StatusOK {
		return ReleaseTicket{}, data
	}
	var ticket ReleaseTicket
	if err := json.Unmarshal(data, &ticket); err != nil {
		t.Fatalf("decode release lookup: %v: %s", err, data)
	}
	return ticket, data
}
