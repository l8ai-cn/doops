package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/user/doops/agent/api"
)

const (
	k8sReadMCPToolName = "k8s_read"
	helmMCPToolName    = "helm"
	webReadMCPToolName = "web_read"
	mcpDomainDoops     = "doops"
	mcpDomainDoSQL     = "dosql"
)

var (
	k8sReadOperations = []string{
		"get", "describe", "logs", "top", "events", "rollout-status", "configmap-keys",
		"helm-list", "helm-history", "helm-status", "helm-values",
	}
	helmMutationOperations = []string{
		"upgrade-install", "rollback", "uninstall",
	}
)

type k8sMCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type k8sMCPToolCall struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// RunK8SMCPStdio exposes the existing structured Kubernetes executor to
// doagent without exposing shell or the Agent's network control plane.
func RunK8SMCPStdio(input io.Reader, output io.Writer) error {
	return RunK8SMCPStdioContext(context.Background(), input, output)
}

func RunK8SMCPStdioContext(ctx context.Context, input io.Reader, output io.Writer) error {
	return RunK8SMCPStdioDomainContext(ctx, input, output, mcpDomainDoops)
}

func RunK8SMCPStdioDomainContext(ctx context.Context, input io.Reader, output io.Writer, domain string) error {
	if domain != mcpDomainDoops && domain != mcpDomainDoSQL {
		return fmt.Errorf("unsupported MCP domain %q", domain)
	}
	return runK8SMCPStdioDomainContext(ctx, input, output, boundSessionFromWorkingDirectory, domain)
}

func runK8SMCPStdioContext(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	resolveSession func() (string, error),
) error {
	return runK8SMCPStdioDomainContext(ctx, input, output, resolveSession, mcpDomainDoops)
}

func runK8SMCPStdioDomainContext(
	ctx context.Context,
	input io.Reader,
	output io.Writer,
	resolveSession func() (string, error),
	domain string,
) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	encoder := json.NewEncoder(output)

	for scanner.Scan() {
		var request k8sMCPRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			if err := encoder.Encode(mcpErrorResponse(nil, -32700, "parse error")); err != nil {
				return err
			}
			continue
		}

		var response api.JSONRPCResponse
		switch request.Method {
		case "initialize":
			response = api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      request.ID,
				Result: map[string]interface{}{
					"protocolVersion": "2025-03-26",
					"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
					"serverInfo":      map[string]interface{}{"name": "doops-k8s", "version": "1"},
				},
			}
		case "notifications/initialized":
			continue
		case "tools/list":
			tools, err := mcpToolsForDomain(domain)
			if err != nil {
				response = mcpErrorResponse(request.ID, -32602, err.Error())
				break
			}
			response = api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      request.ID,
				Result: map[string]interface{}{
					"tools": tools,
				},
			}
		case "tools/call":
			sessionID, err := resolveSession()
			if err != nil {
				response = mcpErrorResponse(request.ID, -32000, err.Error())
				var call k8sMCPToolCall
				if json.Unmarshal(request.Params, &call) == nil && domain == mcpDomainDoops && call.Name == helmMCPToolName {
					response = helmMCPFailureResponse(request.ID, &helmToolFailure{cause: err, phase: "preparation"})
				}
				break
			}
			response = callK8SMCPToolDomain(ctx, request, sessionID, domain)
		default:
			response = mcpErrorResponse(request.ID, -32601, "method not found")
		}
		if err := encoder.Encode(response); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func mcpToolsForDomain(domain string) ([]interface{}, error) {
	switch domain {
	case mcpDomainDoops:
		return []interface{}{
			k8sMCPToolDefinition(k8sReadMCPToolName, k8sReadOperations, "Read Kubernetes state through the canonical doops_k8s contract. For operation=events, omit resource for namespace-wide events; an object filter must be kind/name. Never use Bash or kubectl as a fallback."),
			helmMCPToolDefinition(),
			webMCPToolDefinition(webReadMCPToolName, webReadOperations, "Read a target-local DoWeb site, Nginx or health state without mutation."),
			webMCPToolDefinition(webMCPToolName, webMutationOperations, "Mutate a target-local DoWeb site through SiteSpec, validation, health and rollback evidence. Requires confirm=true."),
		}, nil
	case mcpDomainDoSQL:
		return []interface{}{
			doSQLMCPToolDefinition(doSQLReadMCPToolName, doSQLReadOperations, "Run a Gateway-authorized, read-only DoSql query through the target database credential. Returns bounded execution evidence, never secret payloads."),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported MCP domain %q", domain)
	}
}

func callK8SMCPTool(
	ctx context.Context,
	request k8sMCPRequest,
	boundSessionID string,
) api.JSONRPCResponse {
	return callK8SMCPToolDomain(ctx, request, boundSessionID, mcpDomainDoops)
}

func callK8SMCPToolDomain(
	ctx context.Context,
	request k8sMCPRequest,
	boundSessionID string,
	domain string,
) api.JSONRPCResponse {
	var call k8sMCPToolCall
	if err := json.Unmarshal(request.Params, &call); err != nil {
		return mcpErrorResponse(request.ID, -32602, "invalid tools/call params")
	}
	if !mcpToolBelongsToDomain(call.Name, domain) {
		return mcpErrorResponse(request.ID, -32601, "unknown tool")
	}
	switch call.Name {
	case k8sReadMCPToolName:
		if !k8sMCPOperationAllowed(call.Arguments, k8sReadOperations) {
			return mcpErrorResponse(request.ID, -32602, "read tool requires a read-only operation")
		}
	case helmMCPToolName:
		if !k8sMCPOperationAllowed(call.Arguments, helmMutationOperations) {
			return helmMCPFailureResponse(request.ID, &helmToolFailure{
				cause: fmt.Errorf("helm tool requires a supported mutating operation"), phase: "validation",
			})
		}
	case webMCPToolName:
		if !k8sMCPOperationAllowed(call.Arguments, webMutationOperations) {
			return webMCPErrorResponse(request.ID, -32602, "web tool requires a supported mutation")
		}
	case webReadMCPToolName:
		if !k8sMCPOperationAllowed(call.Arguments, webReadOperations) {
			return webMCPErrorResponse(request.ID, -32602, "web_read tool requires a supported read operation")
		}
	case doSQLReadMCPToolName:
		if !k8sMCPOperationAllowed(call.Arguments, doSQLReadOperations) {
			return doSQLMCPErrorResponse(request.ID, -32602, "sql_read tool requires a supported read operation")
		}
	default:
		return mcpErrorResponse(request.ID, -32601, "unknown tool")
	}
	boundArguments, err := bindK8SMCPSession(call.Arguments, boundSessionID)
	if err != nil {
		if call.Name == helmMCPToolName {
			return helmMCPFailureResponse(request.ID, &helmToolFailure{cause: err, phase: "validation"})
		}
		return mcpErrorResponse(request.ID, -32602, err.Error())
	}
	// doagent stops and restarts this server when a call reaches its tool
	// timeout, and the restarted server is no longer bound to the session
	// workspace. So every call ends on its own first.
	ctx, cancel := context.WithTimeout(ctx, mcpToolCallDeadline)
	defer cancel()

	var progress strings.Builder
	push := func(event notificationEvent) {
		if text := strings.TrimSpace(event.Data); text != "" {
			if progress.Len() != 0 {
				progress.WriteByte('\n')
			}
			progress.WriteString(text)
		}
	}
	var result string
	if call.Name == helmMCPToolName {
		result, err = handleHelmTool(ctx, boundArguments, push)
		if err != nil {
			return helmMCPFailureResponse(request.ID, err)
		}
	} else if call.Name == webMCPToolName || call.Name == webReadMCPToolName {
		result, err = handleWebTool(ctx, boundArguments, push)
	} else if call.Name == doSQLReadMCPToolName {
		return callDoSQLTool(ctx, request, boundSessionID)
	} else {
		result, err = handleK8STool(ctx, boundArguments, push)
	}
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && readOnlyMCPTool(call.Name) {
			// A read that runs out of time changed nothing; say so plainly.
			return buildToolErrorResponse(request.ID, fmt.Sprintf("tool_timeout: %s did not finish within %s; nothing was changed", call.Name, mcpToolCallDeadline))
		}
		if ctx.Err() != nil || errors.Is(err, errExecutionOutcomeUnknown) ||
			errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return mcpErrorResponse(request.ID, -32008, errExecutionOutcomeUnknown.Error())
		}
		return buildToolErrorResponse(request.ID, sanitizeSecretText(err.Error()))
	}
	if progress.Len() != 0 {
		progress.WriteByte('\n')
	}
	progress.WriteString(result)
	response := api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      request.ID,
		Result: map[string]interface{}{
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": capToolOutput(progress.String())},
			},
			"isError": false,
		},
	}
	if call.Name == helmMCPToolName {
		fields := response.Result.(map[string]interface{})
		fields["structuredContent"] = map[string]interface{}{
			"executionStarted": true, "phase": "execution",
		}
	}
	return response
}

// Keep known-tool failures in result so doagent can attest and emit rawOutput.
// Unknown execution outcomes remain explicit even though the MCP call returned.
func helmMCPFailureResponse(id interface{}, err error) api.JSONRPCResponse {
	fields := map[string]interface{}{"phase": "execution", "executionStarted": true}
	var failure *helmToolFailure
	if errors.As(err, &failure) {
		fields["phase"] = failure.phase
		fields["executionStarted"] = failure.executionStarted
		if failure.outcomeUnknown {
			fields["executionOutcome"] = "unknown"
		}
	}
	if errors.Is(err, errExecutionOutcomeUnknown) {
		fields["executionOutcome"] = "unknown"
	}
	// DoAgent gives the model MCP text content while preserving structuredContent
	// for attestation. Expose the same facts so a retry decision can use them too.
	facts, _ := json.Marshal(fields)
	text := capToolOutput(redactHelmDiagnostic(err.Error())) + "\n" + string(facts)
	return buildStructuredToolErrorResponse(id, text, map[string]interface{}{
		"structuredContent": fields,
	})
}

// maxToolOutputBytes bounds one tool result. doagent sends a tool result back
// inside a single SSE line, and a line beyond the Agent's read limit kills the
// whole event stream: the turn then ends as an unknown outcome even though the
// tool worked. Big output is cut here, with a marker, before it can reach the
// stream.
const maxToolOutputBytes = 256 * 1024

// capToolOutput keeps the head and the tail of a long tool result and marks
// what was cut, so the caller can tell a truncated result from a complete one.
func capToolOutput(text string) string {
	if len(text) <= maxToolOutputBytes {
		return text
	}
	head := maxToolOutputBytes / 2
	tail := maxToolOutputBytes - head
	kept := utf8Head(text, head)
	end := utf8Tail(text, tail)
	cut := len(text) - len(kept) - len(end)
	return kept +
		fmt.Sprintf("\n...[doops cut %d bytes of tool output; read the full state with doops exec]\n", cut) +
		end
}

// mcpToolCallDeadline ends every tool call before doagent's MCP tool timeout.
// Helm uses the tighter helmToolDeadline. A var so tests can shorten it.
var mcpToolCallDeadline = time.Duration(doopsMCPToolTimeoutSeconds)*time.Second - 30*time.Second

func readOnlyMCPTool(name string) bool {
	return name == k8sReadMCPToolName || name == webReadMCPToolName
}

func mcpToolBelongsToDomain(name, domain string) bool {
	if domain == mcpDomainDoSQL {
		return name == doSQLReadMCPToolName
	}
	return name == k8sReadMCPToolName || name == helmMCPToolName || name == webReadMCPToolName || name == webMCPToolName
}

func webMCPToolDefinition(name string, operations []string, description string) map[string]interface{} {
	return map[string]interface{}{
		"name":        name,
		"description": description,
		"inputSchema": map[string]interface{}{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"operation"},
			"properties": map[string]interface{}{
				"operation":    map[string]interface{}{"type": "string", "enum": operations},
				"confirm":      map[string]interface{}{"type": "boolean"},
				"runtime_mode": map[string]interface{}{"type": "string", "enum": []string{"host", "docker"}},
				"site_id":      map[string]interface{}{"type": "string"},
				"domain":       map[string]interface{}{"type": "string"},
				"domain_base":  map[string]interface{}{"type": "string"},
				"host_ip":      map[string]interface{}{"type": "string"},
				"provider":     map[string]interface{}{"type": "string"},
				"dist":         map[string]interface{}{"type": "string", "description": "Path relative to the bound DoOps session workspace."},
				"config_file":  map[string]interface{}{"type": "string", "description": "Candidate config path relative to the bound DoOps session workspace."},
				"config":       map[string]interface{}{"type": "string", "description": "Candidate Nginx config text for config-set; mutually exclusive with config_file."},
				"snippet_file": map[string]interface{}{"type": "string", "description": "Snippet path relative to the bound DoOps session workspace."},
				"snippet":      map[string]interface{}{"type": "string", "description": "Candidate snippet text for snippet-set; mutually exclusive with snippet_file."},
				"snippet_name": map[string]interface{}{"type": "string"},
				"tls_cert":     map[string]interface{}{"type": "string"},
				"tls_key":      map[string]interface{}{"type": "string"},
				"health_urls":  map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"proxies":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"upstreams":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}},
				"wildcard":     map[string]interface{}{"type": "boolean"},
			},
		},
	}
}

func boundSessionFromWorkingDirectory() (string, error) {
	root, err := filepath.EvalSymlinks(workspaceRoot())
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve MCP working directory: %w", err)
	}
	cwd, err = filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve MCP working directory: %w", err)
	}
	relative, err := filepath.Rel(root, cwd)
	if err != nil || relative == "." || filepath.Dir(relative) != "." {
		return "", fmt.Errorf("MCP working directory is not a bound DoOps session workspace")
	}
	if err := validateSession(relative); err != nil {
		return "", fmt.Errorf("MCP working directory has invalid session binding: %w", err)
	}
	return relative, nil
}

func bindK8SMCPSession(arguments json.RawMessage, sessionID string) (json.RawMessage, error) {
	if err := validateSession(sessionID); err != nil {
		return nil, err
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(arguments, &input); err != nil || input == nil {
		return nil, fmt.Errorf("tool arguments must be an object")
	}
	if _, exists := input["session_id"]; exists {
		return nil, fmt.Errorf("session_id is runtime-bound and must not be supplied")
	}
	input["session_id"], _ = json.Marshal(sessionID)
	bound, err := json.Marshal(input)
	if err != nil {
		return nil, fmt.Errorf("bind runtime session: %w", err)
	}
	return bound, nil
}

func k8sMCPOperationAllowed(arguments json.RawMessage, allowed []string) bool {
	var input struct {
		Operation string `json:"operation"`
	}
	if json.Unmarshal(arguments, &input) != nil {
		return false
	}
	normalizedOperation := strings.ToLower(strings.TrimSpace(input.Operation))
	for _, operation := range allowed {
		if normalizedOperation == operation {
			return true
		}
	}
	return false
}

func k8sMCPToolDefinition(name string, operations []string, description string) map[string]interface{} {
	inputSchema := map[string]interface{}{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"operation"},
		"properties": map[string]interface{}{
			"operation": map[string]interface{}{
				"type":        "string",
				"enum":        operations,
				"description": "Use get, describe, logs, top, events, rollout-status, configmap-keys, or the Helm reads helm-list, helm-history, helm-status, helm-values. get/top require kind; describe requires kind and name; logs/rollout-status require resource; helm-history/helm-status/helm-values require name (the release) and namespace. configmap-keys lists ConfigMap data key names without values (optional name, namespace, all_namespaces, selector). helm-values returns key paths only.",
			},
			"kind": map[string]interface{}{
				"type":        "string",
				"description": "Required for get, top and describe. Use the lowercase Kubernetes resource kind, for example nodes, pods or deployments.",
			},
			"name": map[string]interface{}{"type": "string"},
			"resource": map[string]interface{}{
				"type":        "string",
				"description": "Required for logs and rollout-status. Only for object-filtered events use kind/name; omit for namespace-wide events. Do not pass a namespace here.",
			},
			"namespace": map[string]interface{}{
				"type":        "string",
				"description": "Explicit Kubernetes namespace. Required for namespaced health reads; never place it in resource.",
			},
			"container": map[string]interface{}{"type": "string"},
			"output":    map[string]interface{}{"type": "string"},
			"timeout":   map[string]interface{}{"type": "string"},
			"tail":      map[string]interface{}{"type": "integer"},
			"selector": map[string]interface{}{
				"type":        "string",
				"description": "Label selector for get/top, for example app=api or tier in (web,api).",
			},
			"all_namespaces": map[string]interface{}{"type": "boolean", "description": "Read across all namespaces; do not combine with namespace."},
			"previous":       map[string]interface{}{"type": "boolean", "description": "logs: read the previous (crashed) container instance."},
			"since":          map[string]interface{}{"type": "string", "description": "logs: only newer than this duration, for example 10m or 2h."},
		},
		"oneOf": []interface{}{
			map[string]interface{}{
				"properties": map[string]interface{}{"operation": map[string]interface{}{"enum": []string{"get"}}},
				"required":   []string{"operation", "kind"},
			},
			map[string]interface{}{
				"properties": map[string]interface{}{"operation": map[string]interface{}{"enum": []string{"top"}}},
				"required":   []string{"operation", "kind"},
			},
			map[string]interface{}{
				"properties": map[string]interface{}{"operation": map[string]interface{}{"enum": []string{"describe"}}},
				"required":   []string{"operation", "kind", "name"},
			},
			map[string]interface{}{
				"properties": map[string]interface{}{"operation": map[string]interface{}{"enum": []string{"logs", "rollout-status"}}},
				"required":   []string{"operation", "resource"},
			},
			map[string]interface{}{
				"properties": map[string]interface{}{"operation": map[string]interface{}{"enum": []string{"events", "configmap-keys", "helm-list"}}},
				"required":   []string{"operation"},
			},
			map[string]interface{}{
				"properties": map[string]interface{}{"operation": map[string]interface{}{"enum": []string{"helm-history", "helm-status", "helm-values"}}},
				"required":   []string{"operation", "name", "namespace"},
			},
		},
	}
	return map[string]interface{}{
		"name":        name,
		"description": description,
		"inputSchema": inputSchema,
	}
}

func helmMCPToolDefinition() map[string]interface{} {
	return map[string]interface{}{
		"name":        helmMCPToolName,
		"description": "Converge an explicitly named Helm release from remote assets or a runtime-bound session workspace. asset_source defaults to remote.",
		"inputSchema": map[string]interface{}{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"operation", "release", "namespace"},
			"properties": map[string]interface{}{
				"operation": map[string]interface{}{
					"type": "string",
					"enum": helmMutationOperations,
				},
				"release": map[string]interface{}{"type": "string"},
				"asset_source": map[string]interface{}{
					"type":        "string",
					"enum":        []string{helmAssetSourceRemote, helmAssetSourceWorkspace},
					"default":     helmAssetSourceRemote,
					"description": "remote uses an explicit HTTPS Chart download; workspace uses paths relative to the runtime-bound session workspace.",
				},
				"chart": map[string]interface{}{
					"type":        "string",
					"description": "remote: explicit HTTPS chart download; workspace: relative Chart directory containing Chart.yaml. Workspace paths must not be absolute, contain .., or escape the session through symlinks.",
				},
				"namespace": map[string]interface{}{"type": "string"},
				"value_files": map[string]interface{}{
					"type":        "array",
					"description": "Optional ordered YAML values overlays. remote: HTTP(S) URLs; workspace: regular files relative to the session, with no absolute paths, .., or symlink escape.",
					"items": map[string]interface{}{
						"type": "string",
					},
				},
				"set_strings": map[string]interface{}{
					"type":        "object",
					"description": "Optional non-sensitive chart-specific overrides. No credential payloads. Strings use Helm --set-string; numbers and booleans preserve their native Helm types through --set-json. Chart schema determines valid business keys, including image and CORS configuration.",
					"additionalProperties": map[string]interface{}{
						"type": []string{"string", "number", "boolean"},
					},
				},
				"revision": map[string]interface{}{"type": "integer", "minimum": 1},
				"timeout": map[string]interface{}{
					"type":        "string",
					"description": "Positive Helm --wait duration, e.g. 1m30s or 300s; at most 25m. Preparation, execution and verification share the enclosing tool deadline.",
				},
				"atomic": map[string]interface{}{
					"type":        "boolean",
					"default":     false,
					"description": "upgrade-install only. true adds helm --atomic: a failed upgrade is rolled back automatically. Default false keeps the failed revision for the operator to inspect and roll back explicitly.",
				},
				"take_ownership": map[string]interface{}{
					"type":        "boolean",
					"default":     false,
					"description": "upgrade-install only. true adds helm --take-ownership: the release adopts rendered objects that already exist without Helm ownership metadata (for example applied by hand). Set it only when the task says to bring those objects under this release.",
				},
			},
		},
	}
}

func mcpErrorResponse(id interface{}, code int, message string) api.JSONRPCResponse {
	return api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &api.RPCError{Code: code, Message: fmt.Sprintf("doops_k8s: %s", message)},
	}
}

func webMCPErrorResponse(id interface{}, code int, message string) api.JSONRPCResponse {
	return api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &api.RPCError{Code: code, Message: fmt.Sprintf("doops_web: %s", message)},
	}
}
