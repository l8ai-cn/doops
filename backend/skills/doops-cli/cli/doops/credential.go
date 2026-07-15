package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type credentialPrepareRequest struct {
	Cluster         string `json:"cluster"`
	Instance        string `json:"instance"`
	SessionID       string `json:"session_id"`
	WorkflowPath    string `json:"workflow_path"`
	WorkspaceCommit string `json:"workspace_commit"`
	Mode            string `json:"mode"`
}

type credentialRun struct {
	APIVersion       string                              `json:"apiVersion"`
	Kind             string                              `json:"kind"`
	ID               string                              `json:"id"`
	Mode             string                              `json:"mode"`
	Cluster          string                              `json:"cluster"`
	Instance         string                              `json:"instance"`
	Materializations []credentialMaterializationMetadata `json:"materializations"`
}

type credentialMaterializationMetadata struct {
	CredentialID    string   `json:"credentialId"`
	VersionID       string   `json:"versionId"`
	ResourceName    string   `json:"resourceName,omitempty"`
	Namespace       string   `json:"namespace"`
	SecretType      string   `json:"secretType,omitempty"`
	Keys            []string `json:"keys,omitempty"`
	ResourceVersion string   `json:"resourceVersion,omitempty"`
	Digest          string   `json:"digest,omitempty"`
	Fingerprint     string   `json:"fingerprint,omitempty"`
	ExpiresAt       string   `json:"expiresAt,omitempty"`
	Status          string   `json:"status"`
	ErrorCategory   string   `json:"errorCategory,omitempty"`
}

func prepareWorkflowCredentials(
	ctx context.Context,
	server Server,
	token string,
	request credentialPrepareRequest,
) (credentialRun, error) {
	request.Cluster = strings.TrimSpace(request.Cluster)
	request.Instance = strings.TrimSpace(request.Instance)
	request.SessionID = strings.TrimSpace(request.SessionID)
	request.WorkflowPath = strings.TrimSpace(request.WorkflowPath)
	request.WorkspaceCommit = strings.TrimSpace(request.WorkspaceCommit)
	request.Mode = strings.TrimSpace(request.Mode)
	if request.Cluster == "" || request.Instance == "" || request.SessionID == "" ||
		request.WorkflowPath == "" || !validWorkspaceCommit(request.WorkspaceCommit) ||
		(request.Mode != "apply" && request.Mode != "dry-run") {
		return credentialRun{}, fmt.Errorf("invalid credential prepare request")
	}

	endpoint, err := gatewayURLWithPath(server.Gateway, "/v1/credentials/prepare", nil)
	if err != nil {
		return credentialRun{}, err
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return credentialRun{}, fmt.Errorf("encode credential prepare request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return credentialRun{}, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if token = strings.TrimSpace(token); token != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 11 * time.Minute}).Do(httpRequest)
	if err != nil {
		return credentialRun{}, fmt.Errorf("credential prepare request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
		return credentialRun{}, fmt.Errorf("credential prepare failed: HTTP %s", response.Status)
	}

	var run credentialRun
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(&run); err != nil {
		return credentialRun{}, fmt.Errorf("decode credential prepare response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return credentialRun{}, fmt.Errorf("credential prepare response must contain exactly one JSON object")
	}
	if run.APIVersion != workspaceManifestAPIVersion || run.Kind != "CredentialRun" ||
		strings.TrimSpace(run.ID) == "" || run.Mode != request.Mode ||
		run.Cluster != request.Cluster || run.Instance != request.Instance {
		return credentialRun{}, fmt.Errorf("invalid credential prepare response")
	}
	if run.Materializations == nil {
		run.Materializations = []credentialMaterializationMetadata{}
	}
	return run, nil
}
