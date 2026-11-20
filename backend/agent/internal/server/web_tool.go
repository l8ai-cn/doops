package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/user/doops/agent/api"
)

const webMCPToolName = "web"

var (
	webReadOperations     = []string{"render", "check", "config-get", "domain-list", "site-list"}
	webMutationOperations = []string{
		"deploy", "rollback", "config-set", "snippet-set", "install-nginx", "domain-set",
	}
)

func handleWebTool(ctx context.Context, raw json.RawMessage, push notificationSender) (string, error) {
	var params api.WebParams
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&params); err != nil {
		return "", fmt.Errorf("invalid web request: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return "", fmt.Errorf("invalid web request")
	}
	params.Operation = strings.ToLower(strings.TrimSpace(params.Operation))
	if err := validateSession(params.SessionID); err != nil {
		return "", err
	}
	if params.RuntimeMode != "" && params.RuntimeMode != "host" && params.RuntimeMode != "docker" {
		return "", errors.New("runtime_mode must be host or docker")
	}
	if params.Operation == "install-nginx" && params.RuntimeMode == "" {
		return "", errors.New("install-nginx requires an explicit runtime_mode")
	}
	if !containsWebString(webReadOperations, params.Operation) && !containsWebString(webMutationOperations, params.Operation) {
		return "", fmt.Errorf("unsupported web operation %q", params.Operation)
	}
	args, err := webCommandArgs(params)
	if err != nil {
		return "", err
	}
	script := strings.TrimSpace(os.Getenv("DOWEB_SCRIPT"))
	if script == "" {
		script = "/app/skills/doweb/scripts/doweb.mjs"
	}
	if !filepath.IsAbs(script) || strings.Contains(script, "..") {
		return "", errors.New("doweb script path is invalid")
	}
	node := strings.TrimSpace(os.Getenv("DOWEB_NODE"))
	if node == "" {
		node = "node"
	}
	command := exec.Command(node, append([]string{script}, args...)...)
	if params.Operation == "site-list" {
		// Published-site records belong to the target, even before this session has a workspace.
		command.Dir = filepath.Dir(script)
	} else {
		workspace, err := resolveWebSession(params.SessionID)
		if err != nil {
			return "", err
		}
		command.Dir = workspace
	}
	command, stdout, stderr, err := startRawCommand(ctx, command)
	if err != nil {
		return "", fmt.Errorf("start DoWeb executor: %w", err)
	}
	var result strings.Builder
	var resultMu sync.Mutex
	err = waitCommandStreams(ctx, command, stdout, stderr, func(event notificationEvent) {
		resultMu.Lock()
		result.WriteString(event.Data)
		resultMu.Unlock()
	}, false)
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, errExecutionOutcomeUnknown) {
			return "", errExecutionOutcomeUnknown
		}
		detail := strings.TrimSpace(result.String())
		if detail == "" {
			detail = err.Error()
		}
		var failure struct {
			ExecutionOutcome string `json:"executionOutcome"`
		}
		if json.Unmarshal([]byte(detail), &failure) == nil && failure.ExecutionOutcome == "unknown" {
			return "", fmt.Errorf("%w: %s", errExecutionOutcomeUnknown, sanitizeSecretText(detail))
		}
		return "", fmt.Errorf("doweb execution failed: %s", sanitizeSecretText(detail))
	}
	return strings.TrimSpace(result.String()), nil
}

func webCommandArgs(params api.WebParams) ([]string, error) {
	args := []string{params.Operation}
	add := func(name, value string) {
		if strings.TrimSpace(value) != "" {
			args = append(args, "--"+name, value)
		}
	}
	addRepeated := func(name string, values []string) {
		for _, value := range values {
			add(name, value)
		}
	}
	add("site-id", params.SiteID)
	add("domain", params.Domain)
	add("domain-base", params.DomainBase)
	add("host-ip", params.HostIP)
	add("provider", params.Provider)
	if params.Wildcard != nil {
		add("wildcard", strconv.FormatBool(*params.Wildcard))
	}
	if params.Operation == "install-nginx" {
		args = append(args, "--local", "true")
		add("mode", defaultWebRuntimeMode(params.RuntimeMode))
		return args, nil
	}
	dist, err := resolveWebWorkspaceInput(params.SessionID, params.Dist, "dist")
	if err != nil {
		return nil, err
	}
	configFile, err := resolveWebWorkspaceInput(params.SessionID, params.ConfigFile, "config_file")
	if err != nil {
		return nil, err
	}
	snippetFile, err := resolveWebWorkspaceInput(params.SessionID, params.SnippetFile, "snippet_file")
	if err != nil {
		return nil, err
	}
	add("dist", dist)
	add("config-file", configFile)
	add("config-content", params.Config)
	add("snippet-file", snippetFile)
	add("snippet-content", params.Snippet)
	add("name", params.SnippetName)
	tlsCert, err := validateWebSystemPath(params.TLSCert, "tls_cert")
	if err != nil {
		return nil, err
	}
	tlsKey, err := validateWebSystemPath(params.TLSKey, "tls_key")
	if err != nil {
		return nil, err
	}
	add("tls-cert", tlsCert)
	add("tls-key", tlsKey)
	addRepeated("health-url", params.HealthURLs)
	addRepeated("proxy", params.Proxies)
	addRepeated("upstream", params.Upstreams)
	appendWebRuntimeFlags(&args, defaultWebRuntimeMode(params.RuntimeMode))
	return args, nil
}

func defaultWebRuntimeMode(value string) string {
	mode := strings.ToLower(strings.TrimSpace(value))
	if mode == "docker" {
		return "docker"
	}
	return "host"
}

func appendWebRuntimeFlags(args *[]string, mode string) {
	if strings.TrimSpace(os.Getenv("DOWEB_RUNTIME_CONFIG")) != "" {
		return
	}
	if mode == "docker" {
		*args = append(*args,
			"--base-dir", "/var/lib/doweb",
			"--nginx-available-dir", "/etc/doweb/nginx/sites-available",
			"--nginx-enabled-dir", "/etc/doweb/nginx/conf.d",
			"--nginx-snippet-dir", "/etc/doweb/nginx/snippets",
			"--nginx-bin", "/usr/local/bin/doweb-nginx",
			"--reload-cmd", "docker exec doweb-nginx nginx -s reload",
		)
		return
	}
	*args = append(*args,
		"--base-dir", "/var/lib/doweb",
		"--nginx-available-dir", "/etc/doweb/nginx/sites-available",
		"--nginx-enabled-dir", "/etc/nginx/conf.d",
		"--nginx-snippet-dir", "/etc/doweb/nginx/snippets",
		"--nginx-bin", "nginx",
		"--reload-cmd", "nginx -s reload",
	)
}

func validateWebSystemPath(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !filepath.IsAbs(value) || strings.Contains(value, "..") ||
		(!strings.HasPrefix(value, "/etc/") && !strings.HasPrefix(value, "/var/lib/doweb/")) {
		return "", fmt.Errorf("%s must be an absolute path under /etc or /var/lib/doweb", field)
	}
	return value, nil
}

func resolveWebWorkspaceInput(session, value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if filepath.IsAbs(value) {
		return "", fmt.Errorf("%s must be relative to the DoOps session workspace", field)
	}
	if value == "." || value == ".." || strings.HasSuffix(value, "/..") || strings.HasPrefix(value, "../") || strings.Contains(value, "/../") || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("%s escapes the DoOps session workspace", field)
	}
	root, err := resolveWebSession(session)
	if err != nil {
		return "", err
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, value))
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", field, err)
	}
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return "", fmt.Errorf("%s escapes the DoOps session workspace", field)
	}
	return path, nil
}

func resolveWebSession(session string) (string, error) {
	root, err := filepath.EvalSymlinks(workspaceRoot())
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	path := filepath.Join(root, session)
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("DoWeb requires an existing session workspace: %s", session)
	}
	return path, nil
}

func containsWebString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
