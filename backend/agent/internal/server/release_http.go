package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

type createReleaseHTTPRequest struct {
	SessionID        string   `json:"session_id"`
	Requirement      string   `json:"requirement"`
	Cluster          string   `json:"cluster"`
	Instance         string   `json:"instance"`
	Application      string   `json:"application"`
	Environment      string   `json:"environment"`
	Namespace        string   `json:"namespace"`
	ReleaseName      string   `json:"release_name"`
	PlanDigest       string   `json:"plan_digest"`
	SourceRevision   string   `json:"source_revision"`
	WorkspaceCommit  string   `json:"workspace_commit"`
	ExecutionMode    string   `json:"execution_mode"`
	Instruction      string   `json:"instruction"`
	RequiredEvidence []string `json:"required_evidence"`
}

func (h *GatewayHub) HandleReleases(w http.ResponseWriter, r *http.Request) {
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/cicd/releases")
	if suffix == "" || suffix == "/" {
		h.handleReleaseCollection(w, r)
		return
	}
	if !strings.HasPrefix(suffix, "/") || strings.Contains(strings.TrimPrefix(suffix, "/"), "/") {
		http.NotFound(w, r)
		return
	}
	h.handleReleaseLookup(w, r, strings.TrimPrefix(suffix, "/"))
}

func (h *GatewayHub) handleReleaseCollection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	if h.releaseCoordinator == nil {
		http.Error(w, "CI/CD release coordinator is not running", http.StatusServiceUnavailable)
		return
	}
	if !h.releaseCoordinator.Accepting() {
		http.Error(w, "CI/CD release coordinator is halted", http.StatusServiceUnavailable)
		return
	}
	var req createReleaseHTTPRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid release request", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		http.Error(w, "release request must contain one JSON object", http.StatusBadRequest)
		return
	}
	createReq, err := normalizeCreateReleaseHTTPRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !h.store.UserCan(auth.UserID, createReq.Cluster, createReq.Instance, ActionReconcile) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	createReq.UserID = auth.UserID
	createReq.TokenID = auth.TokenID
	ticket, err := h.store.CreateReleaseTicket(createReq)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	h.releaseCoordinator.Wake()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(ticket)
}

func (h *GatewayHub) handleReleaseLookup(w http.ResponseWriter, r *http.Request, rawNumber string) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	auth, err := h.authenticateUser(r)
	if err != nil {
		h.writeUserAuthError(w, r)
		return
	}
	number, err := strconv.ParseInt(strings.TrimSpace(rawNumber), 10, 64)
	if err != nil || number <= 0 {
		http.Error(w, "invalid release number", http.StatusBadRequest)
		return
	}
	ticket, err := h.store.GetReleaseTicket(number)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "release ticket not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !h.store.UserCan(auth.UserID, ticket.Cluster, ticket.Instance, ActionReconcile) {
		http.Error(w, "release ticket not found", http.StatusNotFound)
		return
	}
	writeJSONHTTP(w, ticket)
}

func normalizeCreateReleaseHTTPRequest(req createReleaseHTTPRequest) (CreateReleaseTicketRequest, error) {
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Requirement = strings.TrimSpace(req.Requirement)
	req.Cluster = strings.TrimSpace(req.Cluster)
	req.Instance = strings.TrimSpace(req.Instance)
	req.Application = strings.TrimSpace(req.Application)
	req.Environment = strings.TrimSpace(req.Environment)
	req.Namespace = strings.TrimSpace(req.Namespace)
	req.ReleaseName = strings.TrimSpace(req.ReleaseName)
	req.PlanDigest = strings.TrimSpace(req.PlanDigest)
	req.SourceRevision = strings.ToLower(strings.TrimSpace(req.SourceRevision))
	req.WorkspaceCommit = strings.ToLower(strings.TrimSpace(req.WorkspaceCommit))
	req.ExecutionMode = strings.ToLower(strings.TrimSpace(req.ExecutionMode))
	req.Instruction = strings.TrimSpace(req.Instruction)
	req.RequiredEvidence = normalizeRequiredEvidence(req.RequiredEvidence)
	if err := validateSession(req.SessionID); err != nil {
		return CreateReleaseTicketRequest{}, err
	}

	for name, value := range map[string]string{
		"session_id":  req.SessionID,
		"requirement": req.Requirement,
		"cluster":     req.Cluster,
		"instance":    req.Instance,
		"application": req.Application,
		"environment": req.Environment,
		"plan_digest": req.PlanDigest,
		"instruction": req.Instruction,
	} {
		if value == "" {
			return CreateReleaseTicketRequest{}, fmt.Errorf("%s is required", name)
		}
	}
	if req.ReleaseName == "" {
		req.ReleaseName = req.Application
	}
	if !validAgentPlanDigest(req.PlanDigest) {
		return CreateReleaseTicketRequest{}, fmt.Errorf("plan_digest must be a sha256 digest")
	}
	if req.SourceRevision != "" && !validAgentSourceRevision(req.SourceRevision) {
		return CreateReleaseTicketRequest{}, fmt.Errorf("source_revision must be an immutable Git commit")
	}
	workspaceCommit, err := normalizeWorkspaceCommit(req.WorkspaceCommit)
	if err != nil {
		return CreateReleaseTicketRequest{}, fmt.Errorf("workspace_commit must be a Git object ID")
	}
	switch req.ExecutionMode {
	case "dry-run", "apply":
	default:
		return CreateReleaseTicketRequest{}, fmt.Errorf(`execution_mode must be "dry-run" or "apply"`)
	}
	if len(req.RequiredEvidence) == 0 {
		return CreateReleaseTicketRequest{}, fmt.Errorf("required_evidence must not be empty")
	}
	requiredEvidenceJSON, err := json.Marshal(req.RequiredEvidence)
	if err != nil {
		return CreateReleaseTicketRequest{}, fmt.Errorf("encode required_evidence: %w", err)
	}
	return CreateReleaseTicketRequest{
		SessionID:            req.SessionID,
		Requirement:          req.Requirement,
		Cluster:              req.Cluster,
		Instance:             req.Instance,
		Scope:                canonicalReleaseScope(req),
		Application:          req.Application,
		Environment:          req.Environment,
		Namespace:            req.Namespace,
		ReleaseName:          req.ReleaseName,
		PlanDigest:           req.PlanDigest,
		SourceRevision:       req.SourceRevision,
		WorkspaceCommit:      workspaceCommit,
		ExecutionMode:        req.ExecutionMode,
		Instruction:          req.Instruction,
		RequiredEvidenceJSON: string(requiredEvidenceJSON),
	}, nil
}

func canonicalReleaseScope(req createReleaseHTTPRequest) string {
	component := func(value string) string {
		value = strings.TrimSpace(value)
		if value == "" {
			return "_"
		}
		return url.PathEscape(value)
	}
	return "deployment:" + strings.Join([]string{
		component(req.Cluster),
		component(req.Instance),
		component(req.Environment),
		component(req.Namespace),
		component(req.Application),
		component(req.ReleaseName),
	}, "/")
}
