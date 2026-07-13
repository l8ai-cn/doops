package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type CICDReleaseCreateRequest struct {
	SessionID        string   `json:"session_id"`
	Requirement      string   `json:"requirement"`
	Cluster          string   `json:"cluster"`
	Instance         string   `json:"instance"`
	Application      string   `json:"application"`
	Environment      string   `json:"environment"`
	Namespace        string   `json:"namespace,omitempty"`
	ReleaseName      string   `json:"release_name,omitempty"`
	PlanDigest       string   `json:"plan_digest"`
	SourceRevision   string   `json:"source_revision,omitempty"`
	WorkspaceCommit  string   `json:"workspace_commit"`
	ExecutionMode    string   `json:"execution_mode"`
	Instruction      string   `json:"instruction"`
	RequiredEvidence []string `json:"required_evidence"`
}

type CICDReleaseEvent struct {
	ID            int64     `json:"id"`
	ReleaseNumber int64     `json:"release_number"`
	Type          string    `json:"type"`
	Status        string    `json:"status"`
	Stage         string    `json:"stage,omitempty"`
	Message       string    `json:"message,omitempty"`
	Data          string    `json:"data,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

type CICDReleaseTicket struct {
	Number          int64              `json:"number"`
	UserID          string             `json:"user_id,omitempty"`
	SessionID       string             `json:"session_id"`
	Requirement     string             `json:"requirement"`
	Cluster         string             `json:"cluster"`
	Instance        string             `json:"instance"`
	Scope           string             `json:"scope"`
	Application     string             `json:"application"`
	Environment     string             `json:"environment"`
	Namespace       string             `json:"namespace"`
	ReleaseName     string             `json:"release_name"`
	PlanDigest      string             `json:"plan_digest"`
	SourceRevision  string             `json:"source_revision,omitempty"`
	WorkspaceCommit string             `json:"workspace_commit"`
	ExecutionMode   string             `json:"execution_mode"`
	Status          string             `json:"status"`
	Stage           string             `json:"stage,omitempty"`
	SupersededBy    *int64             `json:"superseded_by,omitempty"`
	Result          string             `json:"result,omitempty"`
	Error           string             `json:"error,omitempty"`
	CreatedAt       time.Time          `json:"created_at"`
	UpdatedAt       time.Time          `json:"updated_at"`
	StartedAt       *time.Time         `json:"started_at,omitempty"`
	FinishedAt      *time.Time         `json:"finished_at,omitempty"`
	Events          []CICDReleaseEvent `json:"events,omitempty"`
}

func gatewayCICDReleaseCreate(server Server, token string, create CICDReleaseCreateRequest) (CICDReleaseTicket, error) {
	payload, err := json.Marshal(create)
	if err != nil {
		return CICDReleaseTicket{}, err
	}
	raw, err := gatewayCICDReleaseRequest(
		http.MethodPost,
		server.Gateway,
		token,
		"/v1/cicd/releases",
		payload,
	)
	if err != nil {
		return CICDReleaseTicket{}, err
	}
	var ticket CICDReleaseTicket
	if err := json.Unmarshal(raw, &ticket); err != nil {
		return CICDReleaseTicket{}, fmt.Errorf("decode release ticket: %w", err)
	}
	if ticket.Number <= 0 {
		return CICDReleaseTicket{}, fmt.Errorf("gateway returned an invalid release number")
	}
	return ticket, nil
}

func gatewayCICDReleaseStatus(server Server, token string, number int64) (CICDReleaseTicket, error) {
	if number <= 0 {
		return CICDReleaseTicket{}, fmt.Errorf("release number must be positive")
	}
	raw, err := gatewayCICDReleaseRequest(
		http.MethodGet,
		server.Gateway,
		token,
		"/v1/cicd/releases/"+strconv.FormatInt(number, 10),
		nil,
	)
	if err != nil {
		return CICDReleaseTicket{}, err
	}
	var ticket CICDReleaseTicket
	if err := json.Unmarshal(raw, &ticket); err != nil {
		return CICDReleaseTicket{}, fmt.Errorf("decode release ticket: %w", err)
	}
	return ticket, nil
}

func gatewayCICDReleaseRequest(method, gateway, token, endpoint string, body []byte) ([]byte, error) {
	requestURL, err := gatewayURLWithPath(gateway, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, requestURL, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gateway CI/CD request failed: HTTP %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

func runCICDStatusCommand(args []string, servers []Server, configErr error) error {
	var target string
	var numberRaw string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		numberRaw = args[0]
		args = args[1:]
	}
	flags := flag.NewFlagSet("cicd status", flag.ContinueOnError)
	flags.StringVar(&target, "target", "", "Configured gateway target")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if numberRaw == "" && len(flags.Args()) == 1 {
		numberRaw = flags.Args()[0]
	}
	if strings.TrimSpace(target) == "" || strings.TrimSpace(numberRaw) == "" {
		return fmt.Errorf("usage: doops cicd status <number> --target <gateway-target>")
	}
	if configErr != nil {
		return configErr
	}
	server := findServer(servers, target)
	if server == nil {
		return fmt.Errorf("target %q not found", target)
	}
	if strings.TrimSpace(server.Gateway) == "" {
		return fmt.Errorf("target %q must use a configured DoOps gateway", server.Name)
	}
	number, err := strconv.ParseInt(strings.TrimSpace(numberRaw), 10, 64)
	if err != nil || number <= 0 {
		return fmt.Errorf("release number must be a positive integer")
	}
	token := ResolveToken(server.Name, server.Token)
	ticket, err := gatewayCICDReleaseStatus(*server, token, number)
	if err != nil {
		return err
	}
	return writeCICDJSON(ticket)
}
