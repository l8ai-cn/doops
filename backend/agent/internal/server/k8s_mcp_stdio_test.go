package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestK8SMCPStdioExposesBoundedDomainTools(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n",
	)
	var output bytes.Buffer
	if err := RunK8SMCPStdio(input, &output); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}

	var response map[string]interface{}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP response: %v", err)
	}
	result, _ := response["result"].(map[string]interface{})
	tools, _ := result["tools"].([]interface{})
	if len(tools) != 4 {
		t.Fatalf("expected read, helm and web tools, got %#v", tools)
	}
	names := make([]string, 0, len(tools))
	for _, value := range tools {
		tool, _ := value.(map[string]interface{})
		name := tool["name"].(string)
		names = append(names, name)
		schema, _ := tool["inputSchema"].(map[string]interface{})
		required, _ := schema["required"].([]interface{})
		expectedRequired := "operation"
		if name == "helm" {
			expectedRequired = "operation,release,namespace"
		}
		if strings.Join(interfaceStrings(required), ",") != expectedRequired {
			t.Fatalf("unexpected required fields: %#v", required)
		}
		properties, _ := schema["properties"].(map[string]interface{})
		if _, exists := properties["confirm"]; exists {
			t.Fatalf("removed confirm field remains exposed by %s: %#v", name, properties)
		}
		if _, exists := properties["session_id"]; exists {
			t.Fatalf("runtime-bound session leaked into model-visible schema: %#v", properties)
		}
		if name == "k8s_read" {
			oneOf, _ := schema["oneOf"].([]interface{})
			if len(oneOf) != 6 { // get, top, describe, logs/rollout-status, events/helm-list, helm release reads
				t.Fatalf("k8s operation conditional schema missing: %#v", schema)
			}
			resource, _ := properties["resource"].(map[string]interface{})
			description, _ := resource["description"].(string)
			if !strings.Contains(description, "Required for logs") || !strings.Contains(description, "omit for namespace-wide events") || !strings.Contains(description, "kind/name") {
				t.Fatalf("k8s resource contract missing: %#v", resource)
			}
			kind, _ := properties["kind"].(map[string]interface{})
			if !strings.Contains(kind["description"].(string), "Required for get, top and describe") {
				t.Fatalf("k8s kind contract missing: %#v", kind)
			}
		}
		if name == "helm" {
			if _, exists := properties["values"]; exists {
				t.Fatalf("ambiguous legacy values field remains exposed: %#v", properties)
			}
			for _, requiredProperty := range []string{"asset_source", "value_files", "set_strings"} {
				if _, exists := properties[requiredProperty]; !exists {
					t.Fatalf("helm schema missing %q: %#v", requiredProperty, properties)
				}
			}
			if _, exists := properties["expected_image"]; exists {
				t.Fatalf("removed expected_image gate remains exposed: %#v", properties)
			}
			setStrings, _ := properties["set_strings"].(map[string]interface{})
			description := setStrings["description"].(string)
			if !strings.Contains(description, "chart-specific") ||
				!strings.Contains(description, "No credential payloads") ||
				strings.Contains(description, "release-YYYYMMDD") {
				t.Fatalf("helm set_strings scalar contract missing: %#v", setStrings)
			}
			assetSource, _ := properties["asset_source"].(map[string]interface{})
			if !strings.Contains(assetSource["description"].(string), "workspace") || strings.Contains(assetSource["description"].(string), "oci://") {
				t.Fatalf("helm asset source contract missing: %#v", assetSource)
			}
			valueFiles, _ := properties["value_files"].(map[string]interface{})
			if !strings.Contains(valueFiles["description"].(string), "ordered") ||
				!strings.Contains(valueFiles["description"].(string), "HTTP(S)") {
				t.Fatalf("helm values source contract missing: %#v", valueFiles)
			}
		}
	}
	if strings.Join(names, ",") != "k8s_read,helm,web_read,web" {
		t.Fatalf("unexpected tools: %#v", names)
	}
}

func TestK8SMCPStdioExposesIndependentDoSQLTools(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}` + "\n",
	)
	var output bytes.Buffer
	if err := RunK8SMCPStdioDomainContext(t.Context(), input, &output, mcpDomainDoSQL); err != nil {
		t.Fatalf("run DoSQL MCP server: %v", err)
	}
	var response map[string]interface{}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP response: %v", err)
	}
	result, _ := response["result"].(map[string]interface{})
	tools, _ := result["tools"].([]interface{})
	names := make([]string, 0, len(tools))
	for _, value := range tools {
		tool, _ := value.(map[string]interface{})
		names = append(names, tool["name"].(string))
	}
	if strings.Join(names, ",") != "read" {
		t.Fatalf("unexpected DoSQL tools: %#v", names)
	}
}

func TestK8SMCPStdioHelmToolDoesNotAddASecondConfirmationGate(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"helm","arguments":{"operation":"unsupported","release":"app","namespace":"apps"}}}` + "\n",
	)
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(
		t.Context(),
		input,
		&output,
		func() (string, error) { return "apply-test", nil },
	); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}
	if strings.Contains(output.String(), "confirm=true is required") {
		t.Fatalf("Helm still has a duplicate confirmation gate: %s", output.String())
	}
}

func TestK8SMCPStdioHelmInvalidArgumentsAreToolErrors(t *testing.T) {
	for _, arguments := range []string{
		`{}`,
		`{"operation":"unsupported"}`,
		`{"operation":42}`,
		`null`,
		`[]`,
		`{"operation":"rollback","release":"app","namespace":"apps","revision":0}`,
		`{"operation":"rollback","release":"app","namespace":"apps","revision":2,"unknown":true}`,
		`{"operation":"rollback","release":"app","namespace":"apps","revision":2,"timeout":"0s"}`,
		`{"operation":"rollback","session_id":"other"}`,
	} {
		t.Run(arguments, func(t *testing.T) {
			input := strings.NewReader(`{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":"helm","arguments":` + arguments + "}}\n")
			var output bytes.Buffer
			if err := runK8SMCPStdioContext(t.Context(), input, &output,
				func() (string, error) { return "validation-test", nil }); err != nil {
				t.Fatal(err)
			}
			response := decodeK8SMCPResponse(t, output.Bytes())
			if _, exists := response["error"]; exists {
				t.Fatalf("known Helm argument rejection used JSON-RPC error: %#v", response)
			}
			result, _ := response["result"].(map[string]interface{})
			if result["isError"] != true {
				t.Fatalf("invalid Helm arguments were not rejected: %#v", response)
			}
			facts, _ := result["structuredContent"].(map[string]interface{})
			if facts["executionStarted"] != false || facts["phase"] != "validation" {
				t.Fatalf("argument rejection lacks execution facts: %#v", result)
			}
			content, _ := result["content"].([]interface{})
			if len(content) != 1 || content[0].(map[string]interface{})["text"] == "" {
				t.Fatalf("argument rejection lost its reason: %#v", response)
			}
			text := content[0].(map[string]interface{})["text"].(string)
			if !strings.Contains(text, `"executionStarted":false`) || !strings.Contains(text, `"phase":"validation"`) {
				t.Fatalf("model cannot see safe correction facts: %q", text)
			}
		})
	}
}

func TestK8SMCPStdioRejectsRemovedK8sMutationTool(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"k8s","arguments":{"operation":"rollout-restart","resource":"deployment/app"}}}` + "\n",
	)
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(
		t.Context(),
		input,
		&output,
		func() (string, error) { return "apply-test", nil },
	); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}
	if !strings.Contains(output.String(), "unknown tool") {
		t.Fatalf("removed k8s mutation tool accepted: %s", output.String())
	}
}

func TestK8SMCPStdioReadToolRejectsMutation(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"k8s_read","arguments":{"operation":"rollout-restart","resource":"deployment/app"}}}` + "\n",
	)
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(
		t.Context(),
		input,
		&output,
		func() (string, error) { return "read-test", nil },
	); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}
	if !strings.Contains(output.String(), "read tool requires a read-only operation") {
		t.Fatalf("read tool accepted mutation: %s", output.String())
	}
}

func TestK8SMCPStdioK8SExecutionFailureIsToolError(t *testing.T) {
	bin := t.TempDir()
	writeK8SMCPExecutable(t, bin, "kubectl", `#!/bin/sh
printf 'password=stdio-k8s-canary\n' >&2
exit 7
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"k8s_read","arguments":{"operation":"get","kind":"pods"}}}` + "\n",
	)
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(
		t.Context(),
		input,
		&output,
		func() (string, error) { return "stdio-k8s", nil },
	); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}
	response := decodeK8SMCPResponse(t, output.Bytes())
	if _, exists := response["error"]; exists {
		t.Fatalf("execution failure used JSON-RPC error: %#v", response)
	}
	result, _ := response["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Fatalf("execution failure did not set result.isError: %#v", response)
	}
	content, _ := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if text != "kubectl get pods failed: exit status 7" {
		t.Fatalf("unexpected stable execution error: %q", text)
	}
	if strings.Contains(output.String(), "stdio-k8s-canary") {
		t.Fatalf("execution error leaked secret output: %s", output.String())
	}
}

func TestK8SMCPStdioHelmSessionPreparationFailureIsAttestable(t *testing.T) {
	input := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"helm","arguments":{"operation":"rollback","release":"app","namespace":"apps","revision":2}}}` + "\n")
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(t.Context(), input, &output, func() (string, error) {
		return "", errors.New("MCP working directory is not a bound DoOps session workspace")
	}); err != nil {
		t.Fatal(err)
	}
	response := decodeK8SMCPResponse(t, output.Bytes())
	result, _ := response["result"].(map[string]interface{})
	facts, _ := result["structuredContent"].(map[string]interface{})
	if response["error"] != nil || result["isError"] != true ||
		facts["phase"] != "preparation" || facts["executionStarted"] != false {
		t.Fatalf("session preparation failure lost its MCP receipt: %#v", response)
	}
}

func TestK8SMCPStdioHelmExecutionFailureIsToolError(t *testing.T) {
	bin := t.TempDir()
	writeK8SMCPExecutable(t, bin, "helm", `#!/bin/sh
printf 'token=stdio-helm-canary\n' >&2
exit 9
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())

	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"helm","arguments":{"operation":"rollback","release":"app","namespace":"apps","revision":2}}}` + "\n",
	)
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(
		t.Context(),
		input,
		&output,
		func() (string, error) { return "stdio-helm", nil },
	); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}
	response := decodeK8SMCPResponse(t, output.Bytes())
	if _, exists := response["error"]; exists {
		t.Fatalf("execution failure used JSON-RPC error: %#v", response)
	}
	result, _ := response["result"].(map[string]interface{})
	if result["isError"] != true {
		t.Fatalf("execution failure did not set result.isError: %#v", response)
	}
	content, _ := result["content"].([]interface{})
	text := content[0].(map[string]interface{})["text"].(string)
	if !strings.HasPrefix(text, "helm_mutation_failed: ") {
		t.Fatalf("unexpected stable execution error: %q", text)
	}
	facts, _ := result["structuredContent"].(map[string]interface{})
	if facts["executionStarted"] != true || facts["phase"] != "execution" {
		t.Fatalf("execution failure lost its execution facts: %#v", result)
	}
	if strings.Contains(output.String(), "stdio-helm-canary") {
		t.Fatalf("execution error leaked secret output: %s", output.String())
	}
}

func TestK8SMCPStdioReadInterruptionReturnsMachineCode(t *testing.T) {
	bin := t.TempDir()
	writeK8SMCPExecutable(t, bin, "kubectl", `#!/bin/sh
sleep 10
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, code := range []string{"tool_cancelled", "tool_timeout"} {
		t.Run(code, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			if code == "tool_timeout" {
				ctx, cancel = context.WithTimeout(t.Context(), 0)
				defer cancel()
			}
			input := strings.NewReader(`{"jsonrpc":"2.0","id":8,"method":"tools/call","params":{"name":"k8s_read","arguments":{"operation":"get","kind":"pods"}}}` + "\n")
			var output bytes.Buffer
			if err := runK8SMCPStdioContext(ctx, input, &output, func() (string, error) { return "stdio-cancel", nil }); err != nil {
				t.Fatal(err)
			}
			result := requireK8SToolResult(t, decodeK8SMCPResponse(t, output.Bytes()))
			if result["errorCode"] != code || result["isError"] != true {
				t.Fatalf("expected terminal read %s: %#v", code, result)
			}
		})
	}
}

func TestK8SMCPStdioMalformedCallRemainsJSONRPCError(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":9,"method":"tools/call","params":"malformed"}` + "\n",
	)
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(
		t.Context(),
		input,
		&output,
		func() (string, error) { return "stdio-invalid", nil },
	); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}
	response := decodeK8SMCPResponse(t, output.Bytes())
	errorResponse, _ := response["error"].(map[string]interface{})
	if errorResponse["code"] != float64(-32602) ||
		!strings.Contains(errorResponse["message"].(string), "invalid tools/call params") {
		t.Fatalf("invalid arguments were not kept as JSON-RPC error: %#v", response)
	}
	if _, exists := response["result"]; exists {
		t.Fatalf("invalid arguments were converted to a tool result: %#v", response)
	}
}

func TestK8SMCPStdioRejectsModelSuppliedSession(t *testing.T) {
	input := strings.NewReader(
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"helm","arguments":{"session_id":"target-alias","operation":"rollback","release":"app","namespace":"apps","revision":2}}}` + "\n",
	)
	var output bytes.Buffer
	if err := runK8SMCPStdioContext(
		t.Context(),
		input,
		&output,
		func() (string, error) { return "release-session", nil },
	); err != nil {
		t.Fatalf("run MCP server: %v", err)
	}
	if !strings.Contains(output.String(), "session_id is runtime-bound and must not be supplied") {
		t.Fatalf("model-supplied session was not rejected: %s", output.String())
	}
}

func TestBindK8SMCPSessionUsesRuntimeBinding(t *testing.T) {
	bound, err := bindK8SMCPSession(
		json.RawMessage(`{"operation":"get","kind":"pods"}`),
		"release-session",
	)
	if err != nil {
		t.Fatalf("bind runtime session: %v", err)
	}
	var input map[string]interface{}
	if err := json.Unmarshal(bound, &input); err != nil {
		t.Fatal(err)
	}
	if input["session_id"] != "release-session" {
		t.Fatalf("runtime session was not injected: %#v", input)
	}
}

func TestK8SMCPOperationAllowedNormalizesModelCasing(t *testing.T) {
	for _, operation := range []string{"GET", " get ", "UPGRADE-INSTALL"} {
		arguments, err := json.Marshal(map[string]string{"operation": operation})
		if err != nil {
			t.Fatal(err)
		}
		if !k8sMCPOperationAllowed(
			arguments,
			append([]string{"get"}, helmMutationOperations...),
		) {
			t.Fatalf("operation %q was rejected before executor normalization", operation)
		}
	}
}

func interfaceStrings(values []interface{}) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.(string))
	}
	return result
}

func writeK8SMCPExecutable(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func decodeK8SMCPResponse(t *testing.T, raw []byte) map[string]interface{} {
	t.Helper()
	var response map[string]interface{}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatalf("decode MCP response: %v; raw=%s", err, raw)
	}
	return response
}
