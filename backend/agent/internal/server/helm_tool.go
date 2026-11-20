package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/user/doops/agent/api"
)

const (
	helmAssetSourceRemote    = "remote"
	helmAssetSourceWorkspace = "workspace"
)

type helmCommand struct {
	args       []string
	cwd        string
	release    string
	namespace  string
	mutationID string
	revision   int
}

type helmToolFailure struct {
	cause            error
	phase            string
	executionStarted bool
	outcomeUnknown   bool
}

func (e *helmToolFailure) Error() string { return e.cause.Error() }
func (e *helmToolFailure) Unwrap() error { return e.cause }

func handleHelmTool(ctx context.Context, raw json.RawMessage, push notificationSender) (string, error) {
	var params api.HelmParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return "", &helmToolFailure{cause: fmt.Errorf("invalid helm request: %w", err), phase: "validation"}
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", &helmToolFailure{cause: fmt.Errorf("invalid helm request"), phase: "validation"}
	}

	command, err := buildHelmCommand(params)
	if err != nil {
		return "", &helmToolFailure{cause: err, phase: "validation"}
	}
	// The call must end before doagent's MCP tool timeout; see helmToolDeadline.
	ctx, cancel := context.WithTimeout(ctx, helmToolDeadline)
	defer cancel()
	cleanup, err := prepareManagedHelmChart(ctx, params, &command)
	if err != nil {
		return "", &helmToolFailure{cause: err, phase: "preparation"}
	}
	defer cleanup()
	if err := ctx.Err(); err != nil {
		return "", &helmToolFailure{cause: err, phase: "preparation"}
	}
	verification, err := runHelmCommand(ctx, command, push)
	if err != nil {
		var failure *helmToolFailure
		if errors.As(err, &failure) {
			return "", err
		}
		return "", &helmToolFailure{cause: err, phase: "execution", executionStarted: true,
			outcomeUnknown: ctx.Err() != nil || errors.Is(err, errExecutionOutcomeUnknown)}
	}
	if verification == "" {
		return "Helm operation complete.", nil
	}
	return "Helm operation complete: " + verification, nil
}

func buildHelmCommand(params api.HelmParams) (helmCommand, error) {
	params.SessionID = strings.TrimSpace(params.SessionID)
	params.Operation = strings.ToLower(strings.TrimSpace(params.Operation))
	params.Release = strings.TrimSpace(params.Release)
	params.AssetSource = strings.TrimSpace(params.AssetSource)
	params.Chart = strings.TrimSpace(params.Chart)
	params.Namespace = strings.TrimSpace(params.Namespace)
	params.Timeout = strings.TrimSpace(params.Timeout)
	if params.AssetSource == "" {
		params.AssetSource = helmAssetSourceRemote
	}
	if params.AssetSource != helmAssetSourceRemote && params.AssetSource != helmAssetSourceWorkspace {
		return helmCommand{}, fmt.Errorf("invalid helm asset_source %q", params.AssetSource)
	}

	if err := validateSession(params.SessionID); err != nil {
		return helmCommand{}, err
	}
	workspace := workspaceRoot()
	workspaceInfo, err := os.Stat(workspace)
	if err != nil || !workspaceInfo.IsDir() {
		return helmCommand{}, fmt.Errorf("agent runtime root is not available")
	}
	if !k8sNamePattern.MatchString(params.Release) {
		return helmCommand{}, fmt.Errorf("invalid helm release")
	}
	if !k8sNamePattern.MatchString(params.Namespace) {
		return helmCommand{}, fmt.Errorf("invalid kubernetes namespace")
	}
	if params.Timeout != "" {
		parsed, err := time.ParseDuration(params.Timeout)
		if err != nil || parsed <= 0 || parsed > maxHelmWaitTimeout {
			return helmCommand{}, fmt.Errorf("helm timeout must be a positive duration at most %s", maxHelmWaitTimeout)
		}
	}
	common := []string{"--namespace", params.Namespace, "--wait"}
	if params.Timeout != "" {
		common = append(common, "--timeout", params.Timeout)
	}

	switch params.Operation {
	case "upgrade-install":
		var chart string
		var values []string
		if params.AssetSource == helmAssetSourceWorkspace {
			workspace, chart, values, err = resolveWorkspaceHelmAssets(params)
		} else {
			chart, err = resolveRemoteHelmChartReference(params.Chart)
			if err == nil {
				for _, valuePath := range params.ValueFiles {
					resolved, valueErr := resolveRemoteHelmValueReference(valuePath)
					if valueErr != nil {
						return helmCommand{}, valueErr
					}
					values = append(values, resolved)
				}
			}
		}
		if err != nil {
			return helmCommand{}, err
		}
		mutationID := fmt.Sprintf("doops:%s:%d", params.SessionID, time.Now().UnixNano())
		args := []string{"upgrade", params.Release, chart, "--install", "--description", mutationID}
		// --atomic rolls a failed upgrade back automatically. It is the caller's
		// choice: by default a failed revision stays in place so the operator can
		// inspect it and decide whether and where to roll back.
		if params.Atomic {
			args = append(args, "--atomic")
		}
		// --take-ownership lets this release adopt objects that already exist
		// without Helm ownership metadata, such as an object applied by hand.
		// It is the caller's explicit choice, never a default: without it Helm
		// refuses to touch an object another owner may still manage.
		if params.TakeOwnership {
			args = append(args, "--take-ownership")
		}
		for _, valuePath := range values {
			args = append(args, "--values", valuePath)
		}
		keys := make([]string, 0, len(params.SetStrings))
		for key := range params.SetStrings {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			value := string(params.SetStrings[key])
			if helmSetStringContainsCredential(key, value) {
				return helmCommand{}, fmt.Errorf("helm set_strings contains forbidden credential material")
			}
			args = append(args, params.HelmSetFlag(key), key+"="+escapeHelmSetString(value))
		}
		return helmCommand{
			args: append(args, common...), cwd: workspace,
			release: params.Release, namespace: params.Namespace, mutationID: mutationID,
		}, nil
	case "rollback":
		if params.AssetSource == helmAssetSourceWorkspace {
			return helmCommand{}, fmt.Errorf("workspace asset_source is only supported for helm upgrade-install")
		}
		if params.Revision <= 0 {
			return helmCommand{}, fmt.Errorf("positive revision is required for helm rollback")
		}
		args := []string{"rollback", params.Release, fmt.Sprintf("%d", params.Revision)}
		return helmCommand{args: append(args, common...), cwd: workspace, release: params.Release, namespace: params.Namespace}, nil
	case "uninstall":
		if params.Revision != 0 {
			return helmCommand{}, fmt.Errorf("revision is not valid for helm uninstall")
		}
		args := []string{"uninstall", params.Release}
		return helmCommand{args: append(args, common...), cwd: workspace, release: params.Release, namespace: params.Namespace}, nil
	default:
		return helmCommand{}, fmt.Errorf("unsupported helm operation %q", params.Operation)
	}
}

func helmSetStringContainsCredential(key, value string) bool {
	if credentialScalarContainsInlineMaterial(value) {
		return true
	}
	if normalizeCredentialKey(key) == "data" {
		// Chart data is not necessarily a Secret payload. Inspect its contents
		// with the existing structured/text redactor before allowing the value.
		public, err := redactDoagentUIText(value)
		return err != nil || public != value
	}
	return credentialSecretLikeKey(key)
}

// annotateAnonymousHelmChartRejection names the cause when a repository turns
// away a download that carried no credential. The bare status code reads as a
// broken Chart URL, when what actually happened is that this repository needs a
// Helm credential bound to the target. The original code stays in the message
// so the repository's own answer is still visible.
func annotateAnonymousHelmChartRejection(err error, anonymous bool) error {
	if err == nil || !anonymous {
		return err
	}
	switch err.Error() {
	case "helm_artifact_http_401", "helm_artifact_http_403":
		return fmt.Errorf("helm_chart_requires_credential: %w", err)
	}
	return err
}

func prepareManagedHelmChart(ctx context.Context, params api.HelmParams, command *helmCommand) (func(), error) {
	cleanup := func() {}
	if command.operation() != "upgrade" {
		return cleanup, nil
	}
	if strings.TrimSpace(params.AssetSource) == helmAssetSourceWorkspace {
		return cleanup, nil
	}
	params.SessionID = strings.TrimSpace(params.SessionID)
	params.Release = strings.TrimSpace(params.Release)
	params.Namespace = strings.TrimSpace(params.Namespace)
	params.Chart = strings.TrimSpace(params.Chart)
	auth, _, err := authorizedHelmCredential(ctx, params)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(params.Chart, "https://") {
		return nil, fmt.Errorf("helm_chart_requires_explicit_https_download_reference")
	}
	// A public Chart downloads anonymously when no bound credential matches
	// its origin, even if this target also uses private repositories.
	anonymous := auth == nil
	if anonymous {
		parsed, err := url.Parse(params.Chart)
		if err != nil {
			return nil, fmt.Errorf("helm chart reference invalid")
		}
		auth = &helmRepositoryAuth{URL: parsed.Scheme + "://" + parsed.Host}
	}
	chart, _, err := fetchManagedHelmChart(ctx, auth.URL, auth.Username, auth.Password, params.Chart)
	auth.Password = ""
	auth.Username = ""
	if err != nil {
		return nil, annotateAnonymousHelmChartRejection(err, anonymous)
	}
	directory, err := os.MkdirTemp("", "doops-helm-chart-*")
	if err != nil {
		return nil, fmt.Errorf("helm_chart_staging_failed")
	}
	cleanup = func() { _ = os.RemoveAll(directory) }
	path := filepath.Join(directory, "chart.tgz")
	if os.WriteFile(path, chart, 0600) != nil {
		cleanup()
		return nil, fmt.Errorf("helm_chart_staging_failed")
	}
	command.args[2] = path
	for index := 3; index < len(command.args); index++ {
		if command.args[index] != "--values" {
			continue
		}
		values, err := fetchHelmValues(ctx, command.args[index+1])
		if err != nil {
			cleanup()
			return nil, err
		}
		valuePath := filepath.Join(directory, fmt.Sprintf("values-%d.yaml", index))
		if err := os.WriteFile(valuePath, values, 0600); err != nil {
			cleanup()
			return nil, fmt.Errorf("helm_values_staging_failed")
		}
		command.args[index+1] = valuePath
		index++
	}
	return cleanup, nil
}

func fetchHelmValues(ctx context.Context, reference string) ([]byte, error) {
	endpoint, err := url.Parse(reference)
	if err != nil || !isRemoteHelmValueReference(reference) || credentialScalarContainsInlineMaterial(reference) {
		return nil, fmt.Errorf("helm_values_invalid")
	}
	if endpoint.Scheme == "http" && endpoint.Port() == "" {
		endpoint.Host += ":80"
	}
	client := credentialHTTPClient(endpoint)
	client.Transport.(*http.Transport).Proxy = nil
	defer client.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("helm_values_invalid")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("helm_values_unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("helm_values_http_%d", response.StatusCode)
	}
	values, err := io.ReadAll(io.LimitReader(response.Body, managedHelmYAMLLimit+1))
	if err != nil || len(values) > managedHelmYAMLLimit {
		return nil, fmt.Errorf("helm_values_invalid")
	}
	return values, nil
}

func resolveWorkspaceHelmAssets(params api.HelmParams) (string, string, []string, error) {
	workspace, sessionRoot, err := resolveHelmSessionWorkspace(params.SessionID)
	if err != nil {
		return "", "", nil, err
	}

	chart, err := cleanWorkspaceRelativePath(params.Chart)
	if err != nil {
		return "", "", nil, fmt.Errorf("invalid workspace chart path: %w", err)
	}
	_, resolvedChart, err := resolveWorkspaceAsset(sessionRoot, chart, false)
	if err != nil {
		return "", "", nil, err
	}
	chartInfo, err := os.Stat(resolvedChart)
	if err != nil || !chartInfo.IsDir() {
		return "", "", nil, fmt.Errorf("workspace chart %q must be a directory", chart)
	}
	chartMetadata := filepath.ToSlash(filepath.Join(filepath.FromSlash(chart), "Chart.yaml"))
	if _, _, err := resolveWorkspaceAsset(sessionRoot, chartMetadata, true); err != nil {
		return "", "", nil, fmt.Errorf("workspace chart %q is invalid: %w", chart, err)
	}

	var values []string
	for _, valuePath := range params.ValueFiles {
		value, _, err := resolveWorkspaceAsset(sessionRoot, valuePath, true)
		if err != nil {
			return "", "", nil, err
		}
		values = append(values, value)
	}

	return workspace, chart, values, nil
}

func resolveHelmSessionWorkspace(sessionID string) (string, string, error) {
	workspace, err := workspacePath(sessionID)
	if err != nil {
		return "", "", err
	}
	root, err := filepath.EvalSymlinks(workspaceRoot())
	if err != nil {
		return "", "", fmt.Errorf("resolve agent workspace root: %w", err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil || !rootInfo.IsDir() {
		return "", "", fmt.Errorf("agent runtime root is not available")
	}
	resolved, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		return "", "", fmt.Errorf("resolve session workspace: %w", err)
	}
	relative, err := filepath.Rel(root, resolved)
	if err != nil || relative == "." || filepath.Dir(relative) != "." {
		return "", "", fmt.Errorf("session workspace is not bound below the agent workspace root")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", "", fmt.Errorf("session workspace is not a directory")
	}
	return workspace, resolved, nil
}

func cleanWorkspaceRelativePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || filepath.IsAbs(value) || strings.ContainsAny(value, `\`) {
		return "", fmt.Errorf("path must be relative to the session workspace")
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." {
			return "", fmt.Errorf("path traversal is not allowed")
		}
	}
	cleaned := filepath.Clean(filepath.FromSlash(value))
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path must stay inside the session workspace")
	}
	return filepath.ToSlash(cleaned), nil
}

func resolveWorkspaceAsset(sessionRoot, relative string, requireRegular bool) (string, string, error) {
	cleaned, err := cleanWorkspaceRelativePath(relative)
	if err != nil {
		return "", "", err
	}
	candidate := filepath.Join(sessionRoot, filepath.FromSlash(cleaned))
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", fmt.Errorf("workspace asset %q does not exist", cleaned)
		}
		return "", "", fmt.Errorf("resolve workspace asset %q: %w", cleaned, err)
	}
	if !pathWithin(sessionRoot, resolved) {
		return "", "", fmt.Errorf("workspace asset %q escapes the session workspace", cleaned)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", "", fmt.Errorf("stat workspace asset %q: %w", cleaned, err)
	}
	if requireRegular && !info.Mode().IsRegular() {
		return "", "", fmt.Errorf("workspace asset %q must be a regular file", cleaned)
	}
	return cleaned, resolved, nil
}

func pathWithin(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func resolveRemoteHelmChartReference(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !isRemoteHelmChartReference(value) {
		return "", fmt.Errorf("helm chart must be an explicit https:// reference")
	}
	return value, nil
}

func resolveRemoteHelmValueReference(value string) (string, error) {
	value = strings.TrimSpace(value)
	if !isRemoteHelmValueReference(value) {
		return "", fmt.Errorf("helm value files must be explicit http(s):// references")
	}
	return value, nil
}

func isRemoteHelmChartReference(value string) bool {
	return isRemoteHelmReference(value, "https")
}

func isRemoteHelmValueReference(value string) bool {
	return isRemoteHelmReference(value, "http", "https")
}

func isRemoteHelmReference(value string, schemes ...string) bool {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	for _, scheme := range schemes {
		if strings.EqualFold(parsed.Scheme, scheme) {
			return true
		}
	}
	return false
}

func escapeHelmSetString(value string) string {
	replacer := strings.NewReplacer(`\`, `\\`, ",", `\,`, "=", `\=`)
	return replacer.Replace(value)
}
