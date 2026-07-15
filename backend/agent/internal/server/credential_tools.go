package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

type credentialPlanToolParams struct {
	SessionID       string `json:"session_id"`
	WorkflowPath    string `json:"workflow_path"`
	WorkspaceCommit string `json:"workspace_commit"`
}

func handleCredentialPlanTool(argBytes json.RawMessage) (string, error) {
	var args credentialPlanToolParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid credential plan params: %w", err)
	}
	args.SessionID = strings.TrimSpace(args.SessionID)
	args.WorkflowPath = strings.TrimSpace(args.WorkflowPath)
	args.WorkspaceCommit = strings.TrimSpace(args.WorkspaceCommit)
	if args.SessionID == "" || args.WorkflowPath == "" || args.WorkspaceCommit == "" {
		return "", fmt.Errorf("session_id, workflow_path, and workspace_commit are required")
	}
	if err := validateWorkspaceCommitBinding(args.SessionID, args.WorkspaceCommit); err != nil {
		return "", err
	}
	root, err := workspacePath(args.SessionID)
	if err != nil {
		return "", err
	}
	plan, err := parseCredentialPlan(root, args.WorkflowPath)
	if err != nil {
		return "", err
	}
	result, err := json.Marshal(plan)
	if err != nil {
		return "", fmt.Errorf("encode credential plan: %w", err)
	}
	return string(result), nil
}

func handleCredentialMaterializeTool(ctx context.Context, argBytes json.RawMessage) (string, error) {
	var args CredentialMaterializeRequest
	decoder := json.NewDecoder(bytes.NewReader(argBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", fmt.Errorf("invalid credential materialize params")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("invalid credential materialize params")
	}
	if err := validateSession(strings.TrimSpace(args.SessionID)); err != nil {
		return "", fmt.Errorf("invalid credential materialize session: %w", err)
	}
	result, err := materializeCredential(ctx, args)
	clear(args.Payload)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode credential materialization: %w", err)
	}
	return string(encoded), nil
}

func handleCredentialCleanupTool(ctx context.Context, argBytes json.RawMessage) (string, error) {
	var args CredentialCleanupRequest
	decoder := json.NewDecoder(bytes.NewReader(argBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&args); err != nil {
		return "", fmt.Errorf("invalid credential cleanup params")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("invalid credential cleanup params")
	}
	if err := validateSession(strings.TrimSpace(args.SessionID)); err != nil {
		return "", fmt.Errorf("invalid credential cleanup session: %w", err)
	}
	if err := cleanupCredential(ctx, args); err != nil {
		return "", err
	}
	return `{"status":"removed"}`, nil
}
