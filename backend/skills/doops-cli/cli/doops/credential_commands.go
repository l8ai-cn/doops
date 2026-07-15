package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const maxCredentialPayloadBytes = 1 << 20

type credentialResource struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	Type  string `json:"type"`
	State string `json:"state"`
}

type credentialVersion struct {
	ID            string `json:"id"`
	CredentialID  string `json:"credential_id"`
	PayloadDigest string `json:"payload_digest"`
	State         string `json:"state"`
}

type credentialGrant struct {
	ID           string   `json:"id"`
	CredentialID string   `json:"credential_id"`
	GranteeID    string   `json:"grantee_id"`
	Cluster      string   `json:"cluster"`
	Instance     string   `json:"instance"`
	Uses         []string `json:"uses"`
}

type credentialBundle struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Scope string `json:"scope"`
	State string `json:"state"`
}

func runCredentialCommand(args []string, servers []Server, configErr error, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("credential subcommand is required: create, list, put, grant, grant-revoke, verify, promote, revoke, bundle-create, bundle-get")
	}
	switch args[0] {
	case "create":
		return runCredentialCreate(args[1:], servers, configErr, stdout)
	case "list":
		return runCredentialList(args[1:], servers, configErr, stdout)
	case "put":
		return runCredentialPut(args[1:], servers, configErr, stdin, stdout)
	case "grant":
		return runCredentialGrant(args[1:], servers, configErr, stdout)
	case "grant-revoke":
		return runCredentialGrantRevoke(args[1:], servers, configErr, stdout)
	case "verify":
		return runCredentialVerify(args[1:], servers, configErr, stdout)
	case "promote":
		return runCredentialPromote(args[1:], servers, configErr, stdout)
	case "revoke":
		return runCredentialRevoke(args[1:], servers, configErr, stdout)
	case "bundle-create":
		return runCredentialBundleCreate(args[1:], servers, configErr, stdin, stdout)
	case "bundle-get":
		return runCredentialBundleGet(args[1:], servers, configErr, stdout)
	default:
		return fmt.Errorf("unsupported credential subcommand %q", args[0])
	}
}

func runCredentialGrantRevoke(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, gateway, id, grantID string
	flags := credentialFlagSet("credential grant-revoke")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	flags.StringVar(&id, "id", "", "Credential ID")
	flags.StringVar(&grantID, "grant-id", "", "Credential grant ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(id) == "" || strings.TrimSpace(grantID) == "" {
		return fmt.Errorf("--id and --grant-id are required")
	}
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	endpoint, err := credentialEndpoint(id, "grants")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"grant_id": strings.TrimSpace(grantID)})
	if err != nil {
		return err
	}
	if _, err := gatewayCredentialRequest(http.MethodDelete, gateway, token, endpoint, nil, body); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "credential_id=%s credential_grant_id=%s revoked=true\n",
		strings.TrimSpace(id), strings.TrimSpace(grantID))
	return err
}

func runCredentialBundleCreate(args []string, servers []Server, configErr error, stdin io.Reader, stdout io.Writer) error {
	var target, gateway string
	flags := credentialFlagSet("credential bundle-create")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("credential bundle-create accepts no positional arguments")
	}
	body, err := readCredentialPayload(stdin)
	if err != nil {
		return err
	}
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	raw, err := gatewayCredentialRequest(http.MethodPost, gateway, token, "/v1/credential-bundles", nil, body)
	if err != nil {
		return err
	}
	var bundle credentialBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return fmt.Errorf("decode credential bundle response: %w", err)
	}
	if strings.TrimSpace(bundle.ID) == "" {
		return fmt.Errorf("credential bundle response omitted id")
	}
	_, err = fmt.Fprintf(stdout, "credential_bundle_id=%s name=%s scope=%s state=%s\n",
		bundle.ID, bundle.Name, bundle.Scope, bundle.State)
	return err
}

func runCredentialBundleGet(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, gateway, name string
	flags := credentialFlagSet("credential bundle-get")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	flags.StringVar(&name, "name", "", "Credential bundle name")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(name) == "" {
		return fmt.Errorf("--name is required")
	}
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	raw, err := gatewayCredentialRequest(http.MethodGet, gateway, token, "/v1/credential-bundles",
		url.Values{"name": []string{strings.TrimSpace(name)}}, nil)
	if err != nil {
		return err
	}
	var bundle credentialBundle
	if err := json.Unmarshal(raw, &bundle); err != nil {
		return fmt.Errorf("decode credential bundle response: %w", err)
	}
	if strings.TrimSpace(bundle.ID) == "" {
		return fmt.Errorf("credential bundle response omitted id")
	}
	_, err = fmt.Fprintf(stdout, "credential_bundle_id=%s name=%s scope=%s state=%s\n",
		bundle.ID, bundle.Name, bundle.Scope, bundle.State)
	return err
}

func runCredentialVerify(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, id, versionID, sessionID, workflowPath, workspaceCommit string
	flags := credentialFlagSet("credential verify")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&id, "id", "", "Credential ID")
	flags.StringVar(&versionID, "version-id", "", "Staged credential version ID")
	flags.StringVar(&sessionID, "session", "", "Bound workspace session")
	flags.StringVar(&workflowPath, "workflow-path", "", "DeploymentTemplate path in the workspace")
	flags.StringVar(&workspaceCommit, "workspace-commit", "", "Immutable workspace commit")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(target) == "" || strings.TrimSpace(id) == "" ||
		strings.TrimSpace(versionID) == "" || strings.TrimSpace(sessionID) == "" ||
		strings.TrimSpace(workflowPath) == "" || strings.TrimSpace(workspaceCommit) == "" {
		return fmt.Errorf("--target, --id, --version-id, --session, --workflow-path and --workspace-commit are required")
	}
	if configErr != nil {
		return configErr
	}
	server := findServer(servers, strings.TrimSpace(target))
	if server == nil {
		return fmt.Errorf("target %q not found", strings.TrimSpace(target))
	}
	gateway, token, err := resolveCredentialGateway(target, "", servers, configErr)
	if err != nil {
		return err
	}
	endpoint, err := credentialEndpoint(id, "verify")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"version_id":       strings.TrimSpace(versionID),
		"cluster":          strings.TrimSpace(server.Cluster),
		"instance":         strings.TrimSpace(server.Instance),
		"session_id":       strings.TrimSpace(sessionID),
		"workflow_path":    strings.TrimSpace(workflowPath),
		"workspace_commit": strings.TrimSpace(workspaceCommit),
	})
	if err != nil {
		return err
	}
	if _, err := gatewayCredentialRequest(http.MethodPost, gateway, token, endpoint, nil, body); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "credential_id=%s version_id=%s verified=true\n",
		strings.TrimSpace(id), strings.TrimSpace(versionID))
	return err
}

func runCredentialCreate(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, gateway, name, scope, credentialType string
	flags := credentialFlagSet("credential create")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	flags.StringVar(&name, "name", "", "Credential name")
	flags.StringVar(&scope, "scope", "", "Credential scope: personal or platform")
	flags.StringVar(&credentialType, "type", "", "Credential type")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(name) == "" || strings.TrimSpace(scope) == "" || strings.TrimSpace(credentialType) == "" {
		return fmt.Errorf("--name, --scope and --type are required")
	}
	var err error
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"name":  strings.TrimSpace(name),
		"scope": strings.TrimSpace(scope),
		"type":  strings.TrimSpace(credentialType),
	})
	if err != nil {
		return err
	}
	raw, err := gatewayCredentialRequest(http.MethodPost, gateway, token, "/v1/credentials", nil, body)
	if err != nil {
		return err
	}
	var credential credentialResource
	if err := json.Unmarshal(raw, &credential); err != nil {
		return fmt.Errorf("decode credential create response: %w", err)
	}
	if strings.TrimSpace(credential.ID) == "" {
		return fmt.Errorf("credential create response omitted id")
	}
	_, err = fmt.Fprintf(stdout, "credential_id=%s\n", credential.ID)
	return err
}

func runCredentialList(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, gateway string
	flags := credentialFlagSet("credential list")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("credential list accepts no positional arguments")
	}
	var err error
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	raw, err := gatewayCredentialRequest(http.MethodGet, gateway, token, "/v1/credentials", nil, nil)
	if err != nil {
		return err
	}
	var response struct {
		Credentials []credentialResource `json:"credentials"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		return fmt.Errorf("decode credential list response: %w", err)
	}
	for _, credential := range response.Credentials {
		if _, err := fmt.Fprintf(stdout, "credential_id=%s name=%s scope=%s type=%s state=%s\n",
			credential.ID, credential.Name, credential.Scope, credential.Type, credential.State); err != nil {
			return err
		}
	}
	return nil
}

func runCredentialPut(args []string, servers []Server, configErr error, stdin io.Reader, stdout io.Writer) error {
	var target, gateway, id string
	flags := credentialFlagSet("credential put")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	flags.StringVar(&id, "id", "", "Credential ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(id) == "" {
		return fmt.Errorf("--id is required")
	}
	payload, err := readCredentialPayload(stdin)
	if err != nil {
		return err
	}
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	endpoint, err := credentialEndpoint(id, "payload")
	if err != nil {
		return err
	}
	raw, err := gatewayCredentialRequest(http.MethodPut, gateway, token, endpoint, nil, payload)
	if err != nil {
		return err
	}
	var version credentialVersion
	if err := json.Unmarshal(raw, &version); err != nil {
		return fmt.Errorf("decode credential put response: %w", err)
	}
	if strings.TrimSpace(version.ID) == "" || strings.TrimSpace(version.CredentialID) == "" {
		return fmt.Errorf("credential put response omitted version metadata")
	}
	_, err = fmt.Fprintf(stdout, "credential_id=%s version_id=%s state=%s payload_digest=%s\n",
		version.CredentialID, version.ID, version.State, version.PayloadDigest)
	return err
}

func runCredentialGrant(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, gateway, id, granteeID, cluster, instance, project, environment, template, namespace, uses string
	flags := credentialFlagSet("credential grant")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	flags.StringVar(&id, "id", "", "Credential ID")
	flags.StringVar(&granteeID, "grantee-id", "", "Gateway user ID receiving the grant")
	flags.StringVar(&cluster, "cluster", "", "Target cluster")
	flags.StringVar(&instance, "instance", "", "Target instance")
	flags.StringVar(&project, "project", "", "Workflow project")
	flags.StringVar(&environment, "environment", "", "Workflow environment")
	flags.StringVar(&template, "template", "", "Workflow template")
	flags.StringVar(&namespace, "namespace", "", "Kubernetes namespace")
	flags.StringVar(&uses, "uses", "", "Comma-separated credential uses")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(id) == "" || strings.TrimSpace(granteeID) == "" ||
		strings.TrimSpace(cluster) == "" || strings.TrimSpace(instance) == "" || strings.TrimSpace(project) == "" ||
		strings.TrimSpace(environment) == "" || strings.TrimSpace(template) == "" || strings.TrimSpace(namespace) == "" {
		return fmt.Errorf("--id, --grantee-id, --cluster, --instance, --project, --environment, --template and --namespace are required")
	}
	useList, err := parseCredentialUses(uses)
	if err != nil {
		return err
	}
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	endpoint, err := credentialEndpoint(id, "grants")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"grantee_id":  strings.TrimSpace(granteeID),
		"cluster":     strings.TrimSpace(cluster),
		"instance":    strings.TrimSpace(instance),
		"project":     strings.TrimSpace(project),
		"environment": strings.TrimSpace(environment),
		"template":    strings.TrimSpace(template),
		"namespace":   strings.TrimSpace(namespace),
		"uses":        useList,
	})
	if err != nil {
		return err
	}
	raw, err := gatewayCredentialRequest(http.MethodPost, gateway, token, endpoint, nil, body)
	if err != nil {
		return err
	}
	var grant credentialGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		return fmt.Errorf("decode credential grant response: %w", err)
	}
	if strings.TrimSpace(grant.ID) == "" {
		return fmt.Errorf("credential grant response omitted id")
	}
	_, err = fmt.Fprintf(stdout, "credential_grant_id=%s credential_id=%s grantee_id=%s\n", grant.ID, grant.CredentialID, grant.GranteeID)
	return err
}

func runCredentialPromote(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, gateway, id, versionID string
	flags := credentialFlagSet("credential promote")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	flags.StringVar(&id, "id", "", "Credential ID")
	flags.StringVar(&versionID, "version-id", "", "Credential version ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(id) == "" || strings.TrimSpace(versionID) == "" {
		return fmt.Errorf("--id and --version-id are required")
	}
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	endpoint, err := credentialEndpoint(id, "promote")
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{"version_id": strings.TrimSpace(versionID)})
	if err != nil {
		return err
	}
	if _, err := gatewayCredentialRequest(http.MethodPost, gateway, token, endpoint, nil, body); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "credential_id=%s version_id=%s promoted=true\n", strings.TrimSpace(id), strings.TrimSpace(versionID))
	return err
}

func runCredentialRevoke(args []string, servers []Server, configErr error, stdout io.Writer) error {
	var target, gateway, id string
	flags := credentialFlagSet("credential revoke")
	flags.StringVar(&target, "target", "", "Configured gateway target")
	flags.StringVar(&gateway, "gateway", "", "Gateway URL")
	flags.StringVar(&id, "id", "", "Credential ID")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || strings.TrimSpace(id) == "" {
		return fmt.Errorf("--id is required")
	}
	gateway, token, err := resolveCredentialGateway(target, gateway, servers, configErr)
	if err != nil {
		return err
	}
	endpoint, err := credentialEndpoint(id, "revoke")
	if err != nil {
		return err
	}
	if _, err := gatewayCredentialRequest(http.MethodPost, gateway, token, endpoint, nil, nil); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "credential_id=%s revoked=true\n", strings.TrimSpace(id))
	return err
}

func credentialFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func resolveCredentialGateway(target, gateway string, servers []Server, configErr error) (string, string, error) {
	target = strings.TrimSpace(target)
	gateway = strings.TrimSpace(gateway)
	token := strings.TrimSpace(os.Getenv("DOOPS_GATEWAY_TOKEN"))
	if target != "" {
		if configErr != nil {
			return "", "", configErr
		}
		server := findServer(servers, target)
		if server == nil {
			return "", "", fmt.Errorf("target %q not found", target)
		}
		if gateway == "" {
			gateway = strings.TrimSpace(server.Gateway)
		}
		if token == "" {
			token = ResolveToken(server.Name, server.Token)
		}
	}
	if gateway == "" {
		return "", "", fmt.Errorf("--gateway or --target is required")
	}
	return gateway, token, nil
}

func readCredentialPayload(stdin io.Reader) ([]byte, error) {
	payload, err := io.ReadAll(io.LimitReader(stdin, maxCredentialPayloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read credential payload from stdin: %w", err)
	}
	if len(payload) > maxCredentialPayloadBytes {
		return nil, fmt.Errorf("credential payload exceeds %d bytes", maxCredentialPayloadBytes)
	}
	if !json.Valid(payload) {
		return nil, fmt.Errorf("credential payload from stdin must be valid JSON")
	}
	return payload, nil
}

func parseCredentialUses(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	uses := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			return nil, fmt.Errorf("--uses must be a comma-separated non-empty list")
		}
		uses = append(uses, part)
	}
	if len(uses) == 0 {
		return nil, fmt.Errorf("--uses is required")
	}
	return uses, nil
}

func credentialEndpoint(id, operation string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" || strings.Contains(id, "/") {
		return "", fmt.Errorf("invalid credential id")
	}
	return "/v1/credentials/" + id + "/" + operation, nil
}

func gatewayCredentialRequest(method, gateway, token, endpoint string, query url.Values, body []byte) ([]byte, error) {
	requestURL, err := gatewayURLWithPath(gateway, endpoint, query)
	if err != nil {
		return nil, err
	}
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	request, err := http.NewRequest(method, requestURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if token = strings.TrimSpace(token); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := (&http.Client{Timeout: 11 * time.Minute}).Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxCredentialPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(responseBody) > maxCredentialPayloadBytes {
		return nil, fmt.Errorf("credential gateway response exceeds %d bytes", maxCredentialPayloadBytes)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("credential gateway request failed: HTTP %s", response.Status)
	}
	return responseBody, nil
}
