package server

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestApplyRejectsRemovedStructuredK8sMutationEvidence(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	bindApplyCollector(collector, "apply-test")
	collector.calls["call-1"] = attestedMutationTrace(t, "mcp_doops_k8s", map[string]interface{}{
		"operation": "set-image",
		"resource":  "deployment/doops-agent",
		"container": "doops-agent",
		"image":     "repo/doops-agent@sha256:" + strings.Repeat("a", 64),
	})
	if err := collector.requireCompletedToolEvidence(); err == nil ||
		!strings.Contains(err.Error(), "mcp_doops_k8s=structured_mutation_tool_required") {
		t.Fatalf("removed structured k8s evidence accepted: %v", err)
	}
}

func TestApplyAcceptsStructuredHelmMutationEvidence(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	bindApplyCollector(collector, "apply-test")
	collector.calls["call-1"] = attestedMutationTrace(t, "mcp_doops_helm", map[string]interface{}{
		"operation": "upgrade-install",
		"release":   "doops-agent",
		"chart":     "deploy/helm/doops-agent",
		"namespace": "kz-ops",
	})
	if err := collector.requireCompletedToolEvidence(); err != nil {
		t.Fatalf("structured helm evidence rejected: %v", err)
	}
}

func TestApplyTurnFinishedReceiptRedactsCredentialLikeText(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	if err := collector.collect(map[string]interface{}{
		"sessionUpdate": "turn_finished",
		"turnId":        "turn-1",
		"status":        "interrupted",
		"stopReason":    "budget_exceeded",
		"failureClass":  "budget_exceeded",
		"failureReason": `password: synthetic-turn-secret`,
		"error":         `{"token":"synthetic-json-secret"}`,
	}); err != nil {
		t.Fatalf("collect turn terminal result: %v", err)
	}
	_, report, err := collector.verifiedMutationResult()
	if err != nil {
		t.Fatalf("build mutation report: %v", err)
	}
	turn, ok := report["turn"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing turn report: %#v", report)
	}
	encoded, _ := json.Marshal(turn)
	if strings.Contains(string(encoded), "synthetic-turn-secret") ||
		strings.Contains(string(encoded), "synthetic-json-secret") {
		t.Fatalf("turn report leaked credential-like text: %s", encoded)
	}
	if !strings.Contains(string(encoded), "REDACTED") {
		t.Fatalf("turn report did not show redaction: %s", encoded)
	}
}

func TestApplyCompletedMutationDoesNotHideUnresolvedTool(t *testing.T) {
	for _, status := range []string{"running", "failed", "completed"} {
		t.Run(status, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			collector.calls["verified"] = attestedMutationTrace(t, "mcp_doops_helm", map[string]interface{}{
				"operation": "upgrade-install", "release": "app",
				"chart": "deploy/helm/app", "namespace": "apps",
			})
			if collector.hasUnresolvedMutation() {
				t.Fatal("verified mutation should not require reconciliation")
			}
			collector.calls["unfinished"] = doagentToolTrace{ToolName: "mcp_doops_helm", Status: status}
			if !collector.hasUnresolvedMutation() {
				t.Fatal("one successful mutation hid a tool without verified completion")
			}
		})
	}
}

func TestApplyVerifiedToolFailureIsTerminalButNeverSuccessful(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			input := map[string]interface{}{
				"operation": "upgrade-install", "release": "app",
				"chart": "deploy/helm/app", "namespace": "apps",
			}
			trace := attestedMutationTrace(t, "mcp_doops_helm", input)
			trace.Status = status
			result := map[string]interface{}{
				"isError": true,
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "helm_verification_failed"}},
			}
			trace.resultJSON, _ = json.Marshal(result)
			trace.AttestedResultDigest, _ = digestRuntimeAttestationJSON(result)
			collector.calls["failed"] = trace
			collector.calls["success"] = attestedMutationTrace(t, "mcp_doops_helm", input)
			if collector.hasUnresolvedMutation() {
				t.Fatal("verified terminal failure became unknown")
			}
			if err := collector.requireCompletedToolEvidence(); err == nil || !strings.HasPrefix(err.Error(), "doagent_apply_tool_failed: doops_helm: helm_verification_failed") {
				t.Fatalf("successful tool hid a failed mutation: %v", err)
			}
			trace.AttestedResultDigest = "sha256:" + strings.Repeat("0", 64)
			collector.calls["failed"] = trace
			if !collector.hasUnresolvedMutation() {
				t.Fatal("unverified failure was accepted as terminal evidence")
			}
		})
	}
}

func TestApplyInvalidHelmArgumentsRequireTrustedFailureEvidence(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*doagentToolTrace)
		reason string
	}{
		{"verified rejection", func(*doagentToolTrace) {}, ""},
		{"missing raw output", func(trace *doagentToolTrace) { trace.resultJSON = nil }, "raw_output_missing"},
		{"missing attestation", func(trace *doagentToolTrace) { trace.AttestationSchema = "" }, "attestation_schema_invalid"},
		{"wrong tool", func(trace *doagentToolTrace) { trace.AttestedTool = "web" }, "attested_tool_mismatch"},
		{"wrong evidence kind", func(trace *doagentToolTrace) { trace.EvidenceKind = "display-text" }, "evidence_kind_invalid"},
		{"wrong session", func(trace *doagentToolTrace) { trace.CanonicalScope["runtimeSessionId"] = "other" }, "session_binding_mismatch"},
		{"wrong scope", func(trace *doagentToolTrace) { trace.CanonicalScope["server"] = "other" }, "canonical_scope_mismatch"},
		{"forged scope digest", func(trace *doagentToolTrace) { trace.ScopeDigest = "" }, "scope_digest_mismatch"},
		{"forged input digest", func(trace *doagentToolTrace) { trace.InputDigest = "" }, "input_digest_mismatch"},
		{"forged result digest", func(trace *doagentToolTrace) { trace.AttestedResultDigest = "" }, "public_result_invalid"},
		{"missing authorization", func(trace *doagentToolTrace) { trace.MutationAuthorized = nil }, "mutation_not_authorized"},
		{"unauthorized", func(trace *doagentToolTrace) { trace.MutationAuthorized = boolPointer(false) }, "mutation_not_authorized"},
		{"wrong authority", func(trace *doagentToolTrace) { trace.MutationAuthorityDigest = "" }, "mutation_authority_binding_mismatch"},
		{"wrong execution mode", func(trace *doagentToolTrace) { trace.ExecutionMode = "dry-run" }, "execution_mode_invalid"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			trace := attestedMutationTrace(t, "mcp_doops_helm", map[string]interface{}{})
			trace.Status = "failed"
			result := map[string]interface{}{
				"isError": true,
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "helm tool requires a supported mutating operation"}},
			}
			trace.resultJSON, _ = json.Marshal(result)
			trace.AttestedResultDigest, _ = digestRuntimeAttestationJSON(result)
			test.mutate(&trace)
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			collector.calls["invalid-arguments"] = trace
			if got := collector.completed(); len(got) != 0 {
				t.Fatal("argument rejection counted as successful mutation")
			}
			want := ""
			if test.reason != "" {
				want = "doops_helm=" + test.reason
			}
			if got := collector.unresolvedMutationSummary(); got != want {
				t.Fatalf("unresolved mutation = %q, want %q", got, want)
			}
			if test.reason == "" {
				if err := collector.requireCompletedToolEvidence(); err == nil ||
					err.Error() != "doagent_apply_tool_failed: doops_helm: helm tool requires a supported mutating operation" {
					t.Fatalf("trusted argument rejection did not return its failure: %v", err)
				}
			}
			if test.reason == "raw_output_missing" {
				if err := collector.requireCompletedToolEvidence(); err == nil ||
					!strings.Contains(err.Error(), "raw_output_missing") ||
					strings.Contains(err.Error(), "doagent_apply_tool_failed") {
					t.Fatalf("missing raw output was promoted to a known failure: %v", err)
				}
			}
			collector.calls["success"] = attestedMutationTrace(t, "mcp_doops_helm", map[string]interface{}{
				"operation": "rollback",
			})
			if got := collector.unresolvedMutationSummary(); got != want {
				t.Fatalf("another success changed the rejected call's evidence: %q, want %q", got, want)
			}
		})
	}
}

func TestApplyCorrectionRequiresLaterTrustedSuccessForSameHelmTarget(t *testing.T) {
	for _, test := range []struct {
		name    string
		mutate  func(map[string]interface{}, map[string]interface{}, map[string]interface{}, *doagentToolTrace)
		reverse bool
		wantOK  bool
	}{
		{name: "corrected parameter", wantOK: true},
		{name: "different release", mutate: func(_, next, _ map[string]interface{}, _ *doagentToolTrace) { next["release"] = "other" }},
		{name: "different namespace", mutate: func(_, next, _ map[string]interface{}, _ *doagentToolTrace) { next["namespace"] = "other" }},
		{name: "different operation", mutate: func(_, next, _ map[string]interface{}, _ *doagentToolTrace) { next["operation"] = "uninstall" }},
		{name: "missing target", mutate: func(first, _, _ map[string]interface{}, _ *doagentToolTrace) { delete(first, "release") }},
		{name: "executed failure", mutate: func(_, _, facts map[string]interface{}, _ *doagentToolTrace) { facts["executionStarted"] = true }},
		{name: "preparation corrected", wantOK: true, mutate: func(_, _, facts map[string]interface{}, _ *doagentToolTrace) { facts["phase"] = "preparation" }},
		{name: "execution phase cannot be corrected", mutate: func(_, _, facts map[string]interface{}, _ *doagentToolTrace) { facts["phase"] = "execution" }},
		{name: "missing execution flag", mutate: func(_, _, facts map[string]interface{}, _ *doagentToolTrace) { delete(facts, "executionStarted") }},
		{name: "unknown even with isError", mutate: func(_, _, facts map[string]interface{}, _ *doagentToolTrace) { facts["executionOutcome"] = "unknown" }},
		{name: "unverified success", mutate: func(_, _, _ map[string]interface{}, next *doagentToolTrace) {
			next.AttestedResultDigest = "sha256:forged"
		}},
		{name: "different authority", mutate: func(_, _, _ map[string]interface{}, next *doagentToolTrace) {
			next.MutationAuthorityDigest = "sha256:" + strings.Repeat("b", 64)
		}},
		{name: "different session", mutate: func(_, _, _ map[string]interface{}, next *doagentToolTrace) {
			next.CanonicalScope["runtimeSessionId"] = "other"
		}},
		{name: "different tool", mutate: func(_, _, _ map[string]interface{}, next *doagentToolTrace) { next.ToolName = "mcp_doops_web" }},
		{name: "success before failure", reverse: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstInput := map[string]interface{}{"operation": "rollback", "release": "app", "namespace": "apps", "revision": 0}
			nextInput := map[string]interface{}{"operation": "rollback", "release": "app", "namespace": "apps", "revision": 2}
			facts := map[string]interface{}{"phase": "validation", "executionStarted": false}
			next := attestedMutationTrace(t, "mcp_doops_helm", nextInput)
			if test.mutate != nil {
				test.mutate(firstInput, nextInput, facts, &next)
			}
			first := attestedMutationTrace(t, "mcp_doops_helm", firstInput)
			first.ToolCallID, first.Status = "z-first", "failed"
			firstResult := map[string]interface{}{
				"isError": true, "structuredContent": facts,
				"content": []interface{}{map[string]interface{}{"type": "text", "text": "positive revision is required"}},
			}
			first.resultJSON, _ = json.Marshal(firstResult)
			first.AttestedResultDigest, _ = digestRuntimeAttestationJSON(firstResult)
			next.ToolCallID = "a-later"
			next.inputJSON, _ = json.Marshal(nextInput)
			next.InputDigest, _ = digestRuntimeAttestationJSON(map[string]interface{}{
				"arguments": nextInput, "runtimeSessionId": "apply-test", "server": "doops", "tool": "helm",
			})
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			ordered := []doagentToolTrace{first, next}
			if test.reverse {
				ordered = []doagentToolTrace{next, first}
			}
			for _, trace := range ordered {
				if err := collector.collect(testRuntimeTraceUpdate(t, trace)); err != nil {
					t.Fatal(err)
				}
			}
			_, report, err := collector.verifiedMutationResult()
			if err != nil {
				t.Fatal(err)
			}
			if gotOK := report["outcome"] == "succeeded"; gotOK != test.wantOK {
				t.Fatalf("correction outcome = %#v", report)
			}
			if test.wantOK {
				if err := collector.requireCompletedToolEvidence(); err != nil {
					t.Fatal(err)
				}
			}
			history := report["mutations"].([]interface{})
			if history[0].(map[string]interface{})["tool_call_id"] != ordered[0].ToolCallID ||
				history[1].(map[string]interface{})["tool_call_id"] != ordered[1].ToolCallID {
				t.Fatalf("call order lost: %#v", history)
			}
			if test.name == "unknown even with isError" && !strings.Contains(collector.unresolvedMutationSummary(), "execution_outcome_unknown") {
				t.Fatalf("unknown became known failure: %#v", report)
			}
		})
	}
}

func TestApplyConcurrentSuccessCannotCorrectEarlierArgumentFailure(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	bindApplyCollector(collector, "apply-test")
	input := map[string]interface{}{"operation": "rollback", "release": "app", "namespace": "apps"}
	first := attestedMutationTrace(t, "mcp_doops_helm", input)
	first.ToolCallID, first.Status = "first", "failed"
	result := helmMCPFailureResponse(1, &helmToolFailure{cause: fmt.Errorf("invalid revision"), phase: "validation"}).Result
	first.resultJSON, _ = json.Marshal(result)
	first.AttestedResultDigest, _ = digestRuntimeAttestationJSON(result)
	next := attestedMutationTrace(t, "mcp_doops_helm", input)
	next.ToolCallID = "next"
	for _, trace := range []doagentToolTrace{first, next} {
		if err := collector.collect(map[string]interface{}{
			"sessionUpdate": "tool_call", "toolCallId": trace.ToolCallID, "toolName": trace.ToolName,
			"status": "in_progress", "input": input,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, trace := range []doagentToolTrace{first, next} {
		if err := collector.collect(testRuntimeTraceUpdate(t, trace)); err != nil {
			t.Fatal(err)
		}
	}
	if err := collector.requireCompletedToolEvidence(); err == nil || !strings.Contains(err.Error(), "invalid revision") {
		t.Fatalf("overlapping invocation was mistaken for a correction: %v", err)
	}
}

func TestApplyExecutedHelmRecoveryRequiresSameIntent(t *testing.T) {
	for _, test := range []struct {
		name, operation string
		mutate          func(map[string]interface{}, map[string]interface{})
		untrusted       string
		concurrent      bool
		wantOK          bool
	}{
		{name: "upgrade values corrected", operation: "upgrade-install", wantOK: true},
		{name: "rollback timeout corrected", operation: "rollback", wantOK: true},
		{name: "uninstall after cause corrected", operation: "uninstall", wantOK: true},
		{name: "default remote source", operation: "upgrade-install", wantOK: true, mutate: func(_, next map[string]interface{}) { next["asset_source"] = "remote" }},
		{name: "different chart", operation: "upgrade-install", mutate: func(_, next map[string]interface{}) { next["chart"] = "https://charts.example/other.tgz" }},
		{name: "missing chart", operation: "upgrade-install", mutate: func(first, _ map[string]interface{}) { delete(first, "chart") }},
		{name: "different source", operation: "upgrade-install", mutate: func(_, next map[string]interface{}) { next["asset_source"] = "workspace" }},
		{name: "different revision", operation: "rollback", mutate: func(_, next map[string]interface{}) { next["revision"] = 3 }},
		{name: "distinct large revisions", operation: "rollback", mutate: func(first, next map[string]interface{}) {
			first["revision"], next["revision"] = json.Number("9007199254740992"), json.Number("9007199254740993")
		}},
		{name: "missing revision", operation: "rollback", mutate: func(first, _ map[string]interface{}) { delete(first, "revision") }},
		{name: "different release", operation: "upgrade-install", mutate: func(_, next map[string]interface{}) { next["release"] = "other" }},
		{name: "different namespace", operation: "upgrade-install", mutate: func(_, next map[string]interface{}) { next["namespace"] = "other" }},
		{name: "different operation", operation: "upgrade-install", mutate: func(_, next map[string]interface{}) { next["operation"] = "uninstall" }},
		{name: "concurrent success", operation: "upgrade-install", concurrent: true},
		{name: "missing raw receipt", operation: "upgrade-install", untrusted: "rawOutput"},
		{name: "missing attestation", operation: "upgrade-install", untrusted: "attestationSchemaVersion"},
		{name: "missing successful attestation", operation: "upgrade-install", untrusted: "success"},
		{name: "unknown", operation: "upgrade-install", untrusted: "unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			firstInput := map[string]interface{}{
				"operation": test.operation, "release": "app", "namespace": "apps",
				"chart": "https://charts.example/app.tgz", "revision": 2, "timeout": "1ms",
				"set_strings": map[string]interface{}{"service.port": "bad"},
			}
			nextInput := map[string]interface{}{
				"operation": test.operation, "release": "app", "namespace": "apps",
				"chart": "https://charts.example/app.tgz", "revision": 2, "timeout": "1m30s",
				"set_strings": map[string]interface{}{"service.port": "8080"},
			}
			if test.mutate != nil {
				test.mutate(firstInput, nextInput)
			}
			first := attestedMutationTrace(t, "mcp_doops_helm", firstInput)
			first.ToolCallID, first.Status = "z-failed", "failed"
			result := helmMCPFailureResponse(1, &helmToolFailure{
				cause: fmt.Errorf("synthetic Helm failure"), phase: "execution", executionStarted: true,
				outcomeUnknown: test.untrusted == "unknown",
			}).Result
			first.resultJSON, _ = json.Marshal(result)
			first.AttestedResultDigest, _ = digestRuntimeAttestationJSON(result)
			next := attestedMutationTrace(t, "mcp_doops_helm", nextInput)
			next.ToolCallID = "a-recovery"
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			for i, trace := range []doagentToolTrace{first, next} {
				if test.concurrent && i == 0 {
					for _, started := range []doagentToolTrace{first, next} {
						if err := collector.collect(map[string]interface{}{
							"sessionUpdate": "tool_call", "toolCallId": started.ToolCallID, "toolName": started.ToolName, "status": "in_progress",
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				update := testRuntimeTraceUpdate(t, trace)
				if i == 0 {
					delete(update, test.untrusted)
				} else if test.untrusted == "success" {
					delete(update, "attestationSchemaVersion")
				}
				if err := collector.collect(update); err != nil {
					t.Fatal(err)
				}
			}
			_, report, err := collector.verifiedMutationResult()
			if err != nil {
				t.Fatal(err)
			}
			if (report["outcome"] == "succeeded") != test.wantOK {
				t.Fatalf("wrong recovery outcome: %#v", report)
			}
			if !test.wantOK && test.untrusted == "" && report["outcome"] != "failed" {
				t.Fatalf("different intent lost its known failure: %#v", report)
			}
			if err := collector.requireCompletedToolEvidence(); (err == nil) != test.wantOK {
				t.Fatalf("terminal evidence disagrees with recovery: %v", err)
			}
			history := report["mutations"].([]interface{})
			failed := history[0].(map[string]interface{})
			if len(history) != 2 || failed["tool_call_id"] != first.ToolCallID ||
				history[1].(map[string]interface{})["tool_call_id"] != next.ToolCallID ||
				(failed["resolved_by"] == next.ToolCallID) != test.wantOK {
				t.Fatalf("recovery lost history or matched a different intent: %#v", history)
			}
		})
	}
}

func TestApplyVerifiedUnknownPrecedesIsError(t *testing.T) {
	for _, isError := range []bool{false, true} {
		t.Run(fmt.Sprint(isError), func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			input := map[string]interface{}{"operation": "rollback", "release": "app", "namespace": "apps"}
			trace := attestedMutationTrace(t, "mcp_doops_helm", input)
			result := map[string]interface{}{
				"isError": isError,
				"structuredContent": map[string]interface{}{
					"executionStarted": true, "phase": "execution", "executionOutcome": "unknown",
				},
			}
			trace.resultJSON, _ = json.Marshal(result)
			trace.AttestedResultDigest, _ = digestRuntimeAttestationJSON(result)
			if err := collector.collect(testRuntimeTraceUpdate(t, trace)); err != nil {
				t.Fatal(err)
			}
			next := attestedMutationTrace(t, "mcp_doops_helm", input)
			next.ToolCallID = "next"
			if err := collector.collect(testRuntimeTraceUpdate(t, next)); err != nil {
				t.Fatal(err)
			}
			if err := collector.requireCompletedToolEvidence(); err == nil || !strings.Contains(err.Error(), "execution_outcome_unknown") {
				t.Fatalf("later success hid unknown receipt: %v", err)
			}
			_, report, err := collector.verifiedMutationResult()
			if err != nil || report["outcome"] != "unknown" || report["verified"] != true {
				t.Fatalf("verified unknown lost: %#v, %v", report, err)
			}
		})
	}
}

func testRuntimeTraceUpdate(t *testing.T, trace doagentToolTrace) map[string]interface{} {
	t.Helper()
	input, err := decodeDoagentJSONValue(trace.inputJSON)
	if err != nil {
		t.Fatal(err)
	}
	result, err := decodeDoagentJSONValue(trace.resultJSON)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]interface{}{
		"sessionUpdate": "tool_call_update", "toolCallId": trace.ToolCallID,
		"toolName": trace.ToolName, "status": trace.Status, "input": input, "rawOutput": result,
		"attestationSchemaVersion": trace.AttestationSchema,
		"executionMode":            trace.ExecutionMode, "mutationAuthorized": true,
		"mutationAuthorityDigest": trace.MutationAuthorityDigest,
		"attestedTool":            trace.AttestedTool, "evidenceKind": trace.EvidenceKind,
		"evidenceSubject": trace.EvidenceSubject, "canonicalScope": trace.CanonicalScope,
		"scopeDigest": trace.ScopeDigest, "inputDigest": trace.InputDigest,
		"resultDigest": trace.AttestedResultDigest,
	}
}

func TestApplySuccessfulHelmResultStillRequiresValidArguments(t *testing.T) {
	for _, test := range []struct {
		name   string
		input  map[string]interface{}
		reason string
	}{
		{"missing operation", map[string]interface{}{}, "mutation_operation_invalid"},
		{"invalid operation", map[string]interface{}{"operation": "unsupported"}, "mutation_operation_invalid"},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			collector.calls["invalid-success"] = attestedMutationTrace(t, "mcp_doops_helm", test.input)
			if got := collector.unresolvedMutationSummary(); got != "doops_helm="+test.reason {
				t.Fatalf("invalid success evidence accepted: %q", got)
			}
			if len(collector.completed()) != 0 {
				t.Fatal("invalid success counted as completed mutation")
			}
		})
	}
}

func TestApplyRejectsUnrelatedCompletedToolEvidence(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	collector.calls["call-1"] = doagentToolTrace{
		ToolCallID: "call-1",
		ToolName:   "Read",
		Status:     "completed",
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		inputJSON:  json.RawMessage(`{"path":"README.md"}`),
		resultJSON: json.RawMessage(`{"content":"ok"}`),
	}
	err := collector.requireCompletedToolEvidence()
	if err == nil || !strings.Contains(err.Error(), "Read=structured_mutation_tool_required") {
		t.Fatalf("unrelated tool evidence accepted: %v", err)
	}
}

func TestApplyRejectsReadOnlyK8sEvidence(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	collector.calls["call-1"] = doagentToolTrace{
		ToolCallID: "call-1",
		ToolName:   "mcp_doops_k8s_read",
		Status:     "completed",
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		inputJSON:  json.RawMessage(`{"operation":"get","kind":"pods"}`),
		resultJSON: json.RawMessage(`{"content":[{"type":"text","text":"pods"}]}`),
	}
	err := collector.requireCompletedToolEvidence()
	if err == nil || !strings.Contains(err.Error(), "mcp_doops_k8s_read=structured_mutation_tool_required") {
		t.Fatalf("read-only k8s evidence accepted: %v", err)
	}
}

func TestApplyRejectsForgedOrMismatchedRuntimeAttestation(t *testing.T) {
	tests := []struct {
		name           string
		mutate         func(*doagentToolTrace)
		collectorBound string
		reason         string
	}{
		{
			name: "missing attestation",
			mutate: func(trace *doagentToolTrace) {
				trace.AttestationSchema = ""
			},
			collectorBound: "apply-test",
			reason:         "attestation_schema_invalid",
		},
		{
			name: "forged input digest",
			mutate: func(trace *doagentToolTrace) {
				trace.InputDigest = "sha256:" + strings.Repeat("0", 64)
			},
			collectorBound: "apply-test",
			reason:         "input_digest_mismatch",
		},
		{
			name:           "wrong session",
			mutate:         func(*doagentToolTrace) {},
			collectorBound: "other",
			reason:         "session_binding_mismatch",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, test.collectorBound)
			trace := attestedMutationTrace(t, "mcp_doops_helm", map[string]interface{}{
				"operation": "rollback",
				"release":   "app",
				"namespace": "apps",
				"revision":  2,
			})
			test.mutate(&trace)
			collector.calls["call-1"] = trace
			err := collector.requireCompletedToolEvidence()
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("error = %v, want %s", err, test.reason)
			}
		})
	}
}

func TestApplyRejectsCompletedACPToolPublicContentWithoutRawOutput(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	if err := collector.collect(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"toolName":      "mcp_doops_helm",
		"status":        "in_progress",
		"input": map[string]interface{}{
			"operation": "upgrade-install",
			"release":   "doops-agent",
			"chart":     "deploy/helm/doops-agent",
			"namespace": "kz-ops",
		},
	}); err != nil {
		t.Fatalf("collect in-progress tool update: %v", err)
	}
	if err := collector.collect(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"toolName":      "mcp_doops_helm",
		"status":        "completed",
		"content": []interface{}{
			map[string]interface{}{
				"type": "content",
				"content": map[string]interface{}{
					"type": "text",
					"text": "Kubernetes operation complete.",
				},
			},
		},
	}); err != nil {
		t.Fatalf("collect completed tool update: %v", err)
	}
	if err := collector.requireCompletedToolEvidence(); err == nil ||
		!strings.Contains(err.Error(), "raw_output_missing") {
		t.Fatalf("content-only ACP tool evidence accepted: %v", err)
	}
}

func TestApplyAcceptsStandardACPToolCallRawInputWithRuntimeAttestation(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	bindApplyCollector(collector, "apply-test")
	input := map[string]interface{}{
		"operation": "upgrade-install",
		"release":   "doops-agent",
		"chart":     "deploy/helm/doops-agent",
		"namespace": "kz-ops",
	}
	trace := attestedMutationTrace(t, "mcp_doops_helm", input)
	var result interface{}
	if err := json.Unmarshal(trace.resultJSON, &result); err != nil {
		t.Fatalf("decode attested result: %v", err)
	}
	rawInput := map[string]interface{}{
		"toolName": "mcp_doops_helm",
		"args":     input,
	}
	inProgress := map[string]interface{}{
		"sessionUpdate": "tool_call",
		"toolCallId":    trace.ToolCallID,
		"toolName":      trace.ToolName,
		"status":        "in_progress",
		"rawInput":      rawInput,
	}
	if err := collector.collect(inProgress); err != nil {
		t.Fatalf("collect standard in-progress update: %v", err)
	}
	completed := map[string]interface{}{
		"sessionUpdate":            "tool_call_update",
		"toolCallId":               trace.ToolCallID,
		"toolName":                 trace.ToolName,
		"status":                   "completed",
		"rawInput":                 rawInput,
		"rawOutput":                result,
		"attestationSchemaVersion": trace.AttestationSchema,
		"executionMode":            trace.ExecutionMode,
		"mutationAuthorized":       true,
		"mutationAuthorityDigest":  trace.MutationAuthorityDigest,
		"attestedTool":             trace.AttestedTool,
		"evidenceKind":             trace.EvidenceKind,
		"evidenceSubject":          trace.EvidenceSubject,
		"canonicalScope":           trace.CanonicalScope,
		"scopeDigest":              trace.ScopeDigest,
		"inputDigest":              trace.InputDigest,
		"resultDigest":             trace.AttestedResultDigest,
	}
	if err := collector.collect(completed); err != nil {
		t.Fatalf("collect standard completed update: %v", err)
	}
	if err := collector.requireCompletedToolEvidence(); err != nil {
		t.Fatalf("standard ACP runtime evidence rejected: %v", err)
	}
}

func TestCollectorAcceptsPartialACPToolUpdatesButNotAsApplyEvidence(t *testing.T) {
	tests := []struct {
		name   string
		update map[string]interface{}
		reason string
	}{
		{
			name: "empty input",
			update: map[string]interface{}{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    "call-1",
				"toolName":      "mcp_doops_helm",
				"status":        "in_progress",
				"input":         map[string]interface{}{},
			},
			reason: "status_not_completed",
		},
		{
			name: "empty public content",
			update: map[string]interface{}{
				"sessionUpdate": "tool_call_update",
				"toolCallId":    "call-1",
				"toolName":      "mcp_doops_helm",
				"status":        "completed",
				"content":       []interface{}{},
			},
			reason: "input_missing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			if err := collector.collect(tt.update); err != nil {
				t.Fatalf("partial update rejected: %v", err)
			}
			if err := collector.requireCompletedToolEvidence(); err == nil ||
				!strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("partial update accepted as apply evidence: %v", err)
			}
		})
	}
}

func TestApplyRejectsFailedToolTransitionToCompleted(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	collector.calls["call-1"] = doagentToolTrace{
		ToolCallID: "call-1",
		ToolName:   "mcp_doops_helm",
		Status:     "failed",
		inputJSON:  json.RawMessage(`{"operation":"upgrade-install"}`),
	}
	err := collector.collect(map[string]interface{}{
		"sessionUpdate": "tool_call_update",
		"toolCallId":    "call-1",
		"toolName":      "mcp_doops_helm",
		"status":        "completed",
		"content": []interface{}{
			map[string]interface{}{
				"type": "content",
				"content": map[string]interface{}{
					"type": "text",
					"text": "Kubernetes operation complete.",
				},
			},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("failed tool transition accepted: %v", err)
	}
}

func TestApplyToolEvidenceReportsRejectedTrace(t *testing.T) {
	collector := newDoagentToolTraceCollector(false)
	collector.calls["call-1"] = doagentToolTrace{
		ToolCallID: "call-1",
		ToolName:   "shell",
		Status:     "running",
	}

	err := collector.requireCompletedToolEvidence()
	if err == nil || !strings.Contains(err.Error(), "shell=status_not_completed") {
		t.Fatalf("rejected trace error = %v", err)
	}
}

func attestedMutationTrace(t *testing.T, runtimeTool string, input map[string]interface{}) doagentToolTrace {
	return attestedMutationTraceForSession(t, runtimeTool, input, "apply-test", testMutationAuthorityDigest)
}

func attestedMutationTraceForSession(t *testing.T, runtimeTool string, input map[string]interface{}, sessionID, authorityDigest string) doagentToolTrace {
	t.Helper()
	binding := runtimeToolBinding(modelVisibleDoagentToolName(runtimeTool))
	realTool := binding.Tool
	server := binding.Server
	if server == "" {
		// Keep malformed-tool fixtures constructible so the production validator
		// can exercise its unknown-binding rejection path.
		server = "doops"
		realTool = strings.TrimPrefix(modelVisibleDoagentToolName(runtimeTool), "doops_")
	}
	scope := map[string]interface{}{
		"runtimeSessionId": sessionID,
		"server":           server,
		"tool":             realTool,
	}
	result := map[string]interface{}{
		"content": []interface{}{map[string]interface{}{
			"type": "text",
			"text": "operation complete",
		}},
		"isError": false,
	}
	inputJSON, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	scopeDigest, err := digestRuntimeAttestationJSON(scope)
	if err != nil {
		t.Fatal(err)
	}
	inputDigest, err := digestRuntimeAttestationJSON(map[string]interface{}{
		"arguments":        input,
		"runtimeSessionId": sessionID,
		"server":           server,
		"tool":             realTool,
	})
	if err != nil {
		t.Fatal(err)
	}
	resultDigest, err := digestRuntimeAttestationJSON(result)
	if err != nil {
		t.Fatal(err)
	}
	return doagentToolTrace{
		ToolCallID:              "call-1",
		ToolName:                runtimeTool,
		Status:                  "completed",
		AttestationSchema:       "doops.tool-attestation/v2",
		ExecutionMode:           "apply",
		MutationAuthorized:      boolPointer(true),
		MutationAuthorityDigest: authorityDigest,
		AttestedTool:            realTool,
		EvidenceKind:            "runtime-tool-result",
		EvidenceSubject:         realTool,
		CanonicalScope:          scope,
		ScopeDigest:             scopeDigest,
		InputDigest:             inputDigest,
		AttestedResultDigest:    resultDigest,
		ObservedAt:              time.Now().UTC().Format(time.RFC3339Nano),
		inputJSON:               inputJSON,
		resultJSON:              resultJSON,
	}
}

const testMutationAuthorityDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func bindApplyCollector(collector *doagentToolTraceCollector, sessionID string) {
	collector.bindSession(sessionID)
	collector.bindMutationAuthority(testMutationAuthorityDigest)
}

func boolPointer(value bool) *bool {
	return &value
}

func TestSQLReadToolFailureCannotBecomeSuccessfulPrompt(t *testing.T) {
	for _, test := range []struct {
		name, status, result string
		failed               bool
	}{
		{"failed tool", "failed", `{}`, true},
		{"error result", "completed", `{"isError":true}`, true},
		{"verified query", "completed", `{"isError":false,"content":[{"type":"text","text":"{\"status\":\"verified\",\"output\":\"1\"}"}]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			collector.calls["sql"] = doagentToolTrace{
				ToolName: "mcp_dosql_read", Status: test.status, resultJSON: json.RawMessage(test.result),
			}
			if collector.hasFailedSQLRead() != test.failed {
				t.Fatalf("SQL tool failure state was lost: status=%s result=%s", test.status, test.result)
			}
		})
	}
}

// An unknown apply outcome must name which mutation could not be proven, so the
// caller knows what to check on the target instead of seeing a bare
// execution_outcome_unknown.
func TestApplyUnresolvedMutationSummaryNamesEachTool(t *testing.T) {
	input := map[string]interface{}{
		"operation": "upgrade-install", "release": "app",
		"chart": "deploy/helm/app", "namespace": "apps",
	}
	cases := []struct {
		name  string
		calls func(t *testing.T) map[string]doagentToolTrace
		want  string
	}{
		{"verified helm is resolved", func(t *testing.T) map[string]doagentToolTrace {
			return map[string]doagentToolTrace{"ok": attestedMutationTrace(t, "mcp_doops_helm", input)}
		}, ""},
		{"running helm is unresolved", func(t *testing.T) map[string]doagentToolTrace {
			return map[string]doagentToolTrace{"run": {ToolName: "mcp_doops_helm", Status: "running"}}
		}, "doops_helm=status_not_completed"},
		{"helm without raw output is unresolved", func(t *testing.T) map[string]doagentToolTrace {
			trace := attestedMutationTrace(t, "mcp_doops_helm", input)
			trace.resultJSON = nil
			return map[string]doagentToolTrace{"x": trace}
		}, "doops_helm=raw_output_missing"},
		{"shell kubectl is not a structured mutation", func(t *testing.T) map[string]doagentToolTrace {
			return map[string]doagentToolTrace{"sh": {ToolName: "shell", Status: "completed",
				inputJSON: json.RawMessage(`{"command":"kubectl rollout restart deployment/app"}`)}}
		}, ""},
		{"read tools are ignored", func(t *testing.T) map[string]doagentToolTrace {
			return map[string]doagentToolTrace{"r": {ToolName: "mcp_doops_k8s_read", Status: "running"}}
		}, ""},
		{"several reasons are sorted", func(t *testing.T) map[string]doagentToolTrace {
			return map[string]doagentToolTrace{
				"a": {ToolName: "mcp_doops_web", Status: "running"},
				"b": {ToolName: "mcp_doops_helm", Status: "running"},
			}
		}, "doops_helm=status_not_completed,doops_web=status_not_completed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			collector := newDoagentToolTraceCollector(false)
			bindApplyCollector(collector, "apply-test")
			for id, trace := range tc.calls(t) {
				collector.calls[id] = trace
			}
			if got := collector.unresolvedMutationSummary(); got != tc.want {
				t.Fatalf("unresolvedMutationSummary() = %q, want %q", got, tc.want)
			}
			if got := collector.hasUnresolvedMutation(); got != (tc.want != "") {
				t.Fatalf("hasUnresolvedMutation() = %t, want %t", got, tc.want != "")
			}
		})
	}
}

func TestPromptOutcomeUnknownKeepsContract(t *testing.T) {
	message := promptOutcomeUnknown("turn_finished_missing")
	response := map[string]interface{}{"error": map[string]interface{}{"message": message}}
	if !executionResponseUnknown(response) {
		t.Fatalf("%q is no longer recognised as execution_outcome_unknown", message)
	}
	if message != "execution_outcome_unknown: turn_finished_missing" {
		t.Fatalf("promptOutcomeUnknown = %q", message)
	}
}

// Shell, Bash and ExecuteCode may run kubectl or helm during an apply; they are
// neither forbidden nor counted as structured mutation evidence.
func TestApplyAllowsShellClusterCommands(t *testing.T) {
	helm := map[string]interface{}{
		"operation": "upgrade-install", "release": "app",
		"chart": "deploy/helm/app", "namespace": "apps",
	}
	for _, tc := range []struct {
		tool, field, command string
	}{
		{"shell", "command", "kubectl patch deployment app --patch '{}'"},
		{"doops_shell", "command", "kubectl rollout restart deployment/app"},
		{"mcp_doops_shell", "command", "kubectl apply -f app.yaml"},
		{"Bash", "command", "helm --namespace apps upgrade --install app deploy/helm/app"},
		{"bash", "command", "helm rollback app 3"},
		{"ExecuteCode", "code", "kubectl delete pod app-0"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			input, _ := json.Marshal(map[string]string{tc.field: tc.command})
			shell := doagentToolTrace{
				ToolCallID: "shell", ToolName: tc.tool, Status: "completed",
				ObservedAt: time.Now().UTC().Format(time.RFC3339Nano),
				inputJSON:  input, resultJSON: json.RawMessage(`{"exitCode":0}`),
			}

			alone := newDoagentToolTraceCollector(false)
			bindApplyCollector(alone, "apply-test")
			alone.calls["shell"] = shell
			if got := alone.unresolvedMutationSummary(); got != "" {
				t.Fatalf("shell command left unresolved: %q", got)
			}
			err := alone.requireCompletedToolEvidence()
			if err == nil || !strings.HasPrefix(err.Error(), "doagent_apply_tool_evidence_missing") ||
				strings.Contains(err.Error(), "forbidden") {
				t.Fatalf("shell-only apply error = %v, want evidence_missing without forbidden", err)
			}

			withHelm := newDoagentToolTraceCollector(false)
			bindApplyCollector(withHelm, "apply-test")
			withHelm.calls["shell"] = shell
			withHelm.calls["helm"] = attestedMutationTrace(t, "mcp_doops_helm", helm)
			if err := withHelm.requireCompletedToolEvidence(); err != nil {
				t.Fatalf("shell command blocked a verified helm apply: %v", err)
			}
		})
	}
}
