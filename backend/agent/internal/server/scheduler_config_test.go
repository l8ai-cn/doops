package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestScanConfigStringReadsInstruction(t *testing.T) {
	got := scanConfigString(`{"instruction":"check disk pressure"}`, "instruction", "fallback")
	if got != "check disk pressure" {
		t.Fatalf("scanConfigString returned %q, want custom instruction", got)
	}
}

func TestHandleAdminJobsRejectsStringEncodedScanConfig(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("admin")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{
		Cluster:  "*",
		Instance: "*",
		Actions:  []GatewayAction{ActionAdmin},
	}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	token, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "admin"})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	hub := NewGatewayHub(store, GatewayHubOptions{})
	payload := map[string]interface{}{
		"name":             "bad scan config",
		"scan_mode":        "ask",
		"scan_config":      `{"instruction":"check disk pressure"}`,
		"repo_slug":        "org/repo",
		"dedup_window_sec": 3600,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token.Plaintext)
	w := httptest.NewRecorder()

	hub.HandleAdminJobs(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for string-encoded scan_config, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "scan_config must be a JSON object") {
		t.Fatalf("unexpected error body: %s", w.Body.String())
	}
}

func TestHandleAdminJobsStoresObjectScanConfig(t *testing.T) {
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser("admin")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{
		Cluster:  "*",
		Instance: "*",
		Actions:  []GatewayAction{ActionAdmin},
	}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	token, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "admin"})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	hub := NewGatewayHub(store, GatewayHubOptions{})
	payload := map[string]interface{}{
		"name":      "disk scan",
		"scan_mode": "ask",
		"scan_config": map[string]string{
			"instruction": "check disk pressure",
		},
		"repo_slug":        "org/repo",
		"dedup_window_sec": 3600,
	}
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token.Plaintext)
	w := httptest.NewRecorder()

	hub.HandleAdminJobs(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for object scan_config, got %d: %s", w.Code, w.Body.String())
	}
	jobs, err := store.ListSchedulerJobs()
	if err != nil {
		t.Fatalf("list jobs: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected one job, got %d", len(jobs))
	}
	if got := scanConfigString(jobs[0].ScanConfig, "instruction", "fallback"); got != "check disk pressure" {
		t.Fatalf("stored instruction = %q, want custom instruction", got)
	}
}
