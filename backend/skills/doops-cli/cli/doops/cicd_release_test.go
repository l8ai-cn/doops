package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestGatewayCICDReleaseCreateReturnsTicketNumber(t *testing.T) {
	var got CICDReleaseCreateRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/cicd/releases" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer user-token" {
			t.Fatalf("missing gateway authorization: %#v", r.Header)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode create request: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(CICDReleaseTicket{
			Number:    73,
			SessionID: got.SessionID,
			Status:    "queued",
		})
	}))
	defer server.Close()

	request := CICDReleaseCreateRequest{
		SessionID:        "chat-73",
		Requirement:      "publish zhiyong",
		Cluster:          "doops-oilan",
		Instance:         "oilan-node",
		Application:      "zhiyong",
		Environment:      "production",
		PlanDigest:       "sha256:" + strings.Repeat("a", 64),
		SourceRevision:   strings.Repeat("b", 40),
		WorkspaceCommit:  strings.Repeat("c", 40),
		ExecutionMode:    "dry-run",
		Instruction:      "reconcile",
		RequiredEvidence: []string{"runtime-state"},
	}
	ticket, err := gatewayCICDReleaseCreate(
		Server{Gateway: server.URL, Token: "user-token"},
		"user-token",
		request,
	)
	if err != nil {
		t.Fatalf("create release ticket: %v", err)
	}
	if ticket.Number != 73 || ticket.Status != "queued" {
		t.Fatalf("unexpected ticket: %#v", ticket)
	}
	if got.SourceRevision == got.WorkspaceCommit {
		t.Fatalf("source and snapshot identities were collapsed: %#v", got)
	}
}

func TestGatewayCICDReleaseStatusUsesNumberedEndpoint(t *testing.T) {
	const number = int64(91)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/cicd/releases/"+strconv.FormatInt(number, 10) {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(CICDReleaseTicket{
			Number:      number,
			SessionID:   "chat-91",
			Requirement: "publish api",
			Status:      "completed",
		})
	}))
	defer server.Close()

	ticket, err := gatewayCICDReleaseStatus(Server{Gateway: server.URL}, "user-token", number)
	if err != nil {
		t.Fatalf("get release status: %v", err)
	}
	if ticket.Number != number || ticket.Status != "completed" || ticket.Requirement != "publish api" {
		t.Fatalf("unexpected ticket status: %#v", ticket)
	}
}

func TestGatewayCICDReleaseURLPreservesGatewayPrefix(t *testing.T) {
	got, err := gatewayURLWithPath("https://gateway.example/doops", "/v1/cicd/releases/17", nil)
	if err != nil {
		t.Fatalf("build release URL: %v", err)
	}
	if got != "https://gateway.example/doops/v1/cicd/releases/17" {
		t.Fatalf("unexpected release URL: %s", got)
	}
}
