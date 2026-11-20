package server

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/user/doops/agent/api"
)

func promptContextResponse(id interface{}, cause error, beforeAdmission bool) api.JSONRPCResponse {
	if cause == context.DeadlineExceeded {
		if beforeAdmission {
			return buildErrorResponse(id, -32603, doagentPromptAdmissionTimeout)
		}
		return buildErrorResponse(id, -32603, "operation_timeout")
	}
	if beforeAdmission {
		return buildErrorResponse(id, -32007, doagentPromptCancelledBeforeAdmission)
	}
	return buildErrorResponse(id, -32007, "operation canceled")
}

const (
	askToolFailed    = "doagent_tool_failed"
	askToolTimeout   = "doagent_tool_timeout"
	askToolCancelled = "doagent_tool_cancelled"
)

// The collector has received the matching turn_finished before this is called.
// Keep every failed call, including failures followed by a successful retry.
func (c *doagentToolTraceCollector) askFailureResponse(id interface{}) *api.JSONRPCResponse {
	var messages []string
	var failures []map[string]interface{}
	category := askToolFailed
	for _, trace := range c.orderedTraces() {
		if trace.Status != "failed" {
			var result struct {
				IsError bool `json:"isError"`
			}
			if trace.Status != "completed" || json.Unmarshal(trace.resultJSON, &result) != nil || !result.IsError {
				continue
			}
		}
		code := trace.ErrorCode
		if code == "" || len(code) > 64 || sanitizeCredentialCategory(code) != code {
			code = "tool_failed"
		}
		switch code {
		case "tool_cancelled", "cancelled":
			category = askToolCancelled
		case "tool_timeout", "timeout":
			if category != askToolCancelled {
				category = askToolTimeout
			}
		}
		name := sanitizeSecretText(trace.ToolName)
		description := name + ": " + code
		switch name {
		case "Bash":
			description = "doagent_native_command_failed: " + description
		case "mcp_dosql_read", "dosql_read":
			description = "dosql_query_failed: " + description
		}
		if trace.policyDenial != "" {
			description += ": " + trace.policyDenial
		}
		messages = append(messages, description)
		failure := map[string]interface{}{
			"toolName": name, "toolCallId": sanitizeSecretText(trace.ToolCallID), "errorCode": code,
		}
		if trace.ExitCode != nil {
			failure["exitCode"] = *trace.ExitCode
		}
		failures = append(failures, failure)
	}
	// A cancelled or expired turn is authoritative even when earlier tools failed.
	turn := c.turnResult
	switch {
	case turn["status"] == "cancelled":
		category = askToolCancelled
	case turn["stopReason"] == "llm_call_timeout" || turn["failureClass"] == "llm_call_timeout":
		category = askToolTimeout
	case len(failures) == 0:
		return nil
	}
	text := category
	if len(messages) > 0 {
		text += ": " + strings.Join(messages, "; ")
	}
	fields := map[string]interface{}{"errorCode": category}
	if len(failures) > 0 {
		fields["toolFailures"] = failures
	}
	response := buildStructuredToolErrorResponse(id, text, fields)
	return &response
}
