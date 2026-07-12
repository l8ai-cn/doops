package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/user/doops/agent/api"
)

const (
	cicdReleaseRequestAPIVersion = "doops.sh/v3"
	cicdReleaseRequestKind       = "ReleaseRequest"
	cicdReleaseStatusAccepted    = "Accepted"
)

var cicdReleaseRevisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

type cicdReleaseRequest struct {
	APIVersion   string            `json:"apiVersion"`
	Kind         string            `json:"kind"`
	RepositoryID string            `json:"repositoryId"`
	Revision     string            `json:"revision"`
	WorkflowPath string            `json:"workflowPath"`
	Inputs       map[string]string `json:"inputs,omitempty"`
	DryRun       bool              `json:"dryRun"`
	AllowMutate  bool              `json:"allowMutate"`
}

type cicdReleaseSubmission struct {
	SessionID string
	Request   cicdReleaseRequest
}

type CICDReleaseResult struct {
	ReleaseID string `json:"releaseId"`
	Status    string `json:"status"`
}

type CICDReleaseSubmitter func(context.Context, cicdReleaseSubmission) (CICDReleaseResult, error)

func parseCICDReleaseSubmitParams(raw json.RawMessage) (cicdReleaseSubmission, error) {
	var params api.CICDReleaseSubmitParams
	if err := decodeStrictJSON(raw, &params); err != nil {
		return cicdReleaseSubmission{}, fmt.Errorf("invalid doops_cicd_submit params: %w", err)
	}
	if strings.TrimSpace(params.SessionID) == "" {
		return cicdReleaseSubmission{}, fmt.Errorf("session_id is required")
	}

	var request cicdReleaseRequest
	if err := decodeStrictJSON(params.Request, &request); err != nil {
		return cicdReleaseSubmission{}, fmt.Errorf("invalid ReleaseRequest: %w", err)
	}
	if err := validateCICDReleaseRequest(request); err != nil {
		return cicdReleaseSubmission{}, err
	}
	return cicdReleaseSubmission{
		SessionID: strings.TrimSpace(params.SessionID),
		Request:   request,
	}, nil
}

func validateCICDReleaseRequest(request cicdReleaseRequest) error {
	if request.APIVersion != cicdReleaseRequestAPIVersion || request.Kind != cicdReleaseRequestKind {
		return fmt.Errorf("request must be a doops.sh/v3 ReleaseRequest")
	}
	if strings.TrimSpace(request.RepositoryID) == "" {
		return fmt.Errorf("repositoryId is required")
	}
	if !cicdReleaseRevisionPattern.MatchString(strings.TrimSpace(request.Revision)) {
		return fmt.Errorf("revision must be an immutable 40-character Git commit")
	}
	workflow := strings.TrimSpace(request.WorkflowPath)
	if workflow == "" || path.IsAbs(workflow) || workflow != path.Clean(workflow) || workflow == "." || strings.HasPrefix(workflow, "../") {
		return fmt.Errorf("workflowPath must be a repository-relative path")
	}
	if !strings.HasPrefix(workflow, "deploy/workflows/") {
		return fmt.Errorf("workflowPath must be under deploy/workflows/")
	}
	if !request.DryRun && !request.AllowMutate {
		return fmt.Errorf("mutating release submission requires allowMutate=true")
	}
	return nil
}

func decodeStrictJSON(raw json.RawMessage, destination interface{}) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if decoder.More() {
		return fmt.Errorf("unexpected trailing JSON value")
	}
	return nil
}

func (h *GatewayHub) SetCICDReleaseSubmitter(submitter CICDReleaseSubmitter) {
	h.cicdSubmitMu.Lock()
	h.cicdSubmitter = submitter
	h.cicdSubmitMu.Unlock()
}

func (h *GatewayHub) submitCICDRelease(ctx context.Context, submission cicdReleaseSubmission) (CICDReleaseResult, error) {
	h.cicdSubmitMu.RLock()
	submitter := h.cicdSubmitter
	h.cicdSubmitMu.RUnlock()
	if submitter == nil {
		return CICDReleaseResult{}, errors.New("remote multi-Ops compiler is not configured")
	}
	result, err := submitter(ctx, submission)
	if err != nil {
		return CICDReleaseResult{}, err
	}
	if err := validateCICDReleaseResult(result); err != nil {
		return CICDReleaseResult{}, err
	}
	return result, nil
}

func validateCICDReleaseResult(result CICDReleaseResult) error {
	if strings.TrimSpace(result.ReleaseID) == "" || strings.TrimSpace(result.Status) == "" {
		return errors.New("remote multi-Ops compiler returned an incomplete release result")
	}
	if result.Status != cicdReleaseStatusAccepted {
		return fmt.Errorf("remote multi-Ops compiler returned non-accepted status %q", result.Status)
	}
	return nil
}
