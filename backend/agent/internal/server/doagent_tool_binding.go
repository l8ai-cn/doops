package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/user/doops/agent/internal/dispatcher"
)

type doagentToolTrace struct {
	ToolCallID                      string
	ToolName                        string
	Status                          string
	ErrorCode                       string
	ExitCode                        *int32
	policyDenial                    string
	AttestationSchema               string
	ContextSchema                   string
	OperationID                     string
	ContextDigest                   string
	PlanDigest                      string
	PlanBindingDigest               string
	ExecutionMode                   string
	MutationAuthorized              *bool
	MutationAuthorityDigest         string
	CapabilityKey                   string
	AttestedTool                    string
	EvidenceKind                    string
	EvidenceSubject                 string
	ObservedAt                      string
	CanonicalScope                  map[string]interface{}
	ScopeDigest                     string
	InputDigest                     string
	AttestedResultDigest            string
	PublicResultDigest              string
	inputJSON                       json.RawMessage
	resultJSON                      json.RawMessage
	sequence                        uint64
	terminalSequence                uint64
	expectedSessionID               string
	expectedMutationAuthorityDigest string
}

type doagentToolTraceCollector struct {
	mu                              sync.Mutex
	calls                           map[string]doagentToolTrace
	sequence                        uint64
	collectAgentReport              bool
	ordinaryAsk                     bool
	expectedSessionID               string
	expectedMutationAuthorityDigest string
	finalMessage                    string
	finalReport                     json.RawMessage
	turnResult                      map[string]interface{}
}

func newDoagentToolTraceCollector(collectAgentReport bool) *doagentToolTraceCollector {
	return &doagentToolTraceCollector{
		calls:              make(map[string]doagentToolTrace),
		collectAgentReport: collectAgentReport,
	}
}

func (c *doagentToolTraceCollector) bindSession(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expectedSessionID = strings.TrimSpace(sessionID)
}

func (c *doagentToolTraceCollector) bindMutationAuthority(digest string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.expectedMutationAuthorityDigest = strings.TrimSpace(digest)
}

func (c *doagentToolTraceCollector) collect(update map[string]interface{}) error {
	if update["replay"] == true {
		return nil
	}
	if update["sessionUpdate"] == "turn_finished" {
		terminal, err := doagentUIStrings(update, []string{"turnId", "status", "stopReason", "failureClass", "failureReason", "error"})
		if err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.turnResult = terminal
		return nil
	}
	if update["sessionUpdate"] == "agent_report" {
		if !c.collectAgentReport {
			return nil
		}
		report, ok := update["report"].(map[string]interface{})
		if !ok {
			return fmt.Errorf("agent report must be an object")
		}
		encoded, err := stableDoagentJSON(report)
		if err != nil {
			return fmt.Errorf("encode agent report: %w", err)
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if len(c.finalReport) != 0 {
			return fmt.Errorf("multiple agent reports were observed")
		}
		c.finalReport = encoded
		return nil
	}
	if update["sessionUpdate"] == "agent_message" {
		text, err := doagentAgentMessageText(update["content"])
		if err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.finalMessage != "" {
			return fmt.Errorf("multiple final agent messages were observed")
		}
		c.finalMessage = text
		return nil
	}
	if update["sessionUpdate"] == doagentStreamedReplyUpdate {
		text, err := doagentAgentMessageText(update["content"])
		if err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.finalMessage == "" {
			c.finalMessage = text
		}
		return nil
	}
	updateType, _ := update["sessionUpdate"].(string)
	if updateType != "tool_call" && updateType != "tool_call_update" {
		return nil
	}
	if c.ordinaryAsk && stringField(update, "turnId") == "" {
		return nil
	}
	toolCallID, _ := update["toolCallId"].(string)
	if strings.TrimSpace(toolCallID) == "" {
		return nil
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	trace, exists := c.calls[toolCallID]
	c.sequence++
	if !exists {
		trace.ToolCallID = toolCallID
		trace.sequence = c.sequence
	}
	if toolName := doagentToolName(update); toolName != "" {
		if trace.ToolName != "" && trace.ToolName != toolName {
			return fmt.Errorf("conflicting duplicate toolCallId %q tool name", toolCallID)
		}
		trace.ToolName = toolName
	}
	if input, ok, err := doagentToolInput(update); err != nil {
		return fmt.Errorf("read tool call %q input: %w", toolCallID, err)
	} else if ok {
		encoded, err := stableDoagentJSON(input)
		if err != nil {
			return fmt.Errorf("encode tool call %q input: %w", toolCallID, err)
		}
		if len(trace.inputJSON) != 0 && !bytes.Equal(trace.inputJSON, encoded) {
			return fmt.Errorf("conflicting duplicate toolCallId %q input", toolCallID)
		}
		trace.inputJSON = encoded
	}
	if status, _ := update["status"].(string); strings.TrimSpace(status) != "" {
		if (trace.Status == "completed" || trace.Status == "failed") && status != trace.Status {
			return fmt.Errorf("conflicting duplicate toolCallId %q status", toolCallID)
		}
		trace.Status = status
		if c.ordinaryAsk && status == "failed" && trace.ToolName == "Bash" && trace.policyDenial == "" {
			output, _ := doagentAgentMessageText(update["content"])
			if denial := dispatcher.ReportedDenial(output); denial != nil {
				trace.policyDenial = denial.Error()
			}
		}
	}
	if result, ok := update["rawOutput"]; ok {
		if err := mergeDoagentToolAttestationUpdate(update, result); err != nil {
			return fmt.Errorf("read tool call %q attestation: %w", toolCallID, err)
		}
		encoded, err := stableDoagentJSON(result)
		if err != nil {
			return fmt.Errorf("encode tool call %q result: %w", toolCallID, err)
		}
		if len(trace.resultJSON) != 0 && !bytes.Equal(trace.resultJSON, encoded) {
			return fmt.Errorf("conflicting duplicate toolCallId %q result", toolCallID)
		}
		trace.resultJSON = encoded
	}
	stringFields := []struct {
		source string
		target *string
	}{
		{"attestationSchemaVersion", &trace.AttestationSchema},
		{"contextSchemaVersion", &trace.ContextSchema},
		{"operationId", &trace.OperationID},
		{"contextDigest", &trace.ContextDigest},
		{"planDigest", &trace.PlanDigest},
		{"planBindingDigest", &trace.PlanBindingDigest},
		{"executionMode", &trace.ExecutionMode},
		{"mutationAuthorityDigest", &trace.MutationAuthorityDigest},
		{"capabilityKey", &trace.CapabilityKey},
		{"attestedTool", &trace.AttestedTool},
		{"evidenceKind", &trace.EvidenceKind},
		{"evidenceSubject", &trace.EvidenceSubject},
		{"scopeDigest", &trace.ScopeDigest},
		{"inputDigest", &trace.InputDigest},
		{"resultDigest", &trace.AttestedResultDigest},
		{"publicResultDigest", &trace.PublicResultDigest},
	}
	if c.ordinaryAsk && (trace.Status == "completed" || trace.Status == "failed") {
		if number, ok := update["exitCode"].(json.Number); ok {
			value, err := number.Int64()
			if err != nil || value != int64(int32(value)) {
				return fmt.Errorf("invalid tool exitCode")
			}
			exit := int32(value)
			if trace.ExitCode != nil && *trace.ExitCode != exit {
				return fmt.Errorf("conflicting tool exitCode")
			}
			trace.ExitCode = &exit
		}
		if code := stringField(update, "errorCode"); code != "" {
			if trace.ErrorCode != "" && trace.ErrorCode != code {
				return fmt.Errorf("conflicting tool errorCode")
			}
			trace.ErrorCode = code
		}
	}
	for _, field := range stringFields {
		value, exists := update[field.source]
		if !exists {
			continue
		}
		text, ok := value.(string)
		if !ok {
			return fmt.Errorf("%s must be a string", field.source)
		}
		if *field.target != "" && *field.target != text {
			return fmt.Errorf("conflicting %s", field.source)
		}
		*field.target = text
	}
	if (trace.Status == "completed" || trace.Status == "failed") && trace.ObservedAt == "" {
		trace.ObservedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if (trace.Status == "completed" || trace.Status == "failed") && trace.terminalSequence == 0 {
		trace.terminalSequence = c.sequence
	}
	if value, exists := update["mutationAuthorized"]; exists {
		authorized, ok := value.(bool)
		if !ok {
			return fmt.Errorf("mutationAuthorized must be a boolean")
		}
		if trace.MutationAuthorized != nil && *trace.MutationAuthorized != authorized {
			return fmt.Errorf("conflicting mutationAuthorized")
		}
		trace.MutationAuthorized = &authorized
	}
	if value, exists := update["canonicalScope"]; exists {
		scope, ok := value.(map[string]interface{})
		if !ok {
			return fmt.Errorf("canonicalScope must be an object")
		}
		if trace.CanonicalScope != nil && !reflect.DeepEqual(trace.CanonicalScope, scope) {
			return fmt.Errorf("conflicting canonicalScope")
		}
		trace.CanonicalScope = scope
	}
	c.calls[toolCallID] = trace
	return nil
}

func doagentToolName(update map[string]interface{}) string {
	for _, field := range []string{"toolName", "name"} {
		if value, _ := update[field].(string); strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	for _, field := range []string{"input", "args", "rawInput"} {
		value, ok := update[field]
		if !ok {
			continue
		}
		_, toolName := normalizeDoagentToolInput(value)
		if toolName != "" {
			return toolName
		}
	}
	return ""
}

func doagentToolInput(update map[string]interface{}) (interface{}, bool, error) {
	var input interface{}
	found := false
	for _, field := range []string{"input", "args", "rawInput"} {
		value, ok := update[field]
		if !ok {
			continue
		}
		normalized, _ := normalizeDoagentToolInput(value)
		if found {
			current, err := stableDoagentJSON(normalized)
			if err != nil {
				return nil, false, err
			}
			previous, err := stableDoagentJSON(input)
			if err != nil {
				return nil, false, err
			}
			if !bytes.Equal(previous, current) {
				return nil, false, fmt.Errorf("conflicting input fields")
			}
			continue
		}
		input = normalized
		found = true
	}
	return input, found, nil
}

func normalizeDoagentToolInput(value interface{}) (interface{}, string) {
	object, ok := value.(map[string]interface{})
	if !ok {
		return value, ""
	}
	toolName, _ := object["toolName"].(string)
	if strings.TrimSpace(toolName) == "" {
		toolName, _ = object["name"].(string)
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		return value, ""
	}
	for _, field := range []string{"args", "arguments", "input"} {
		if args, exists := object[field]; exists {
			return args, toolName
		}
	}
	return value, toolName
}

func mergeDoagentToolAttestationUpdate(update map[string]interface{}, result interface{}) error {
	var attestations []map[string]interface{}
	if direct, exists := update["attestation"]; exists {
		attestation, ok := direct.(map[string]interface{})
		if !ok {
			return fmt.Errorf("attestation must be an object")
		}
		attestations = append(attestations, attestation)
	}
	if resultObject, ok := result.(map[string]interface{}); ok {
		if meta, ok := resultObject["_meta"].(map[string]interface{}); ok {
			if attestation, ok := meta["doops.sh/tool-attestation"].(map[string]interface{}); ok {
				attestations = append(attestations, attestation)
			}
		}
	}
	fields := map[string]string{
		"schemaVersion":           "attestationSchemaVersion",
		"contextSchemaVersion":    "contextSchemaVersion",
		"operationId":             "operationId",
		"contextDigest":           "contextDigest",
		"planDigest":              "planDigest",
		"planBindingDigest":       "planBindingDigest",
		"executionMode":           "executionMode",
		"mutationAuthorized":      "mutationAuthorized",
		"mutationAuthorityDigest": "mutationAuthorityDigest",
		"capabilityKey":           "capabilityKey",
		"tool":                    "attestedTool",
		"evidenceKind":            "evidenceKind",
		"evidenceSubject":         "evidenceSubject",
		"canonicalScope":          "canonicalScope",
		"scopeDigest":             "scopeDigest",
		"inputDigest":             "inputDigest",
		"resultDigest":            "resultDigest",
		"publicResultDigest":      "publicResultDigest",
	}
	for _, attestation := range attestations {
		for source, target := range fields {
			value, exists := attestation[source]
			if !exists {
				continue
			}
			if current, exists := update[target]; exists && !reflect.DeepEqual(current, value) {
				return fmt.Errorf("conflicting %s", target)
			}
			update[target] = value
		}
	}
	return nil
}

func (c *doagentToolTraceCollector) structuredResult(requireAgentReport bool) (string, map[string]interface{}, error) {
	c.mu.Lock()
	text := c.finalMessage
	report := append(json.RawMessage(nil), c.finalReport...)
	c.mu.Unlock()
	if requireAgentReport {
		if len(report) == 0 {
			return "", nil, fmt.Errorf("structured CI/CD report was not emitted")
		}
		var object map[string]interface{}
		if err := json.Unmarshal(report, &object); err != nil {
			return "", nil, fmt.Errorf("decode structured CI/CD report: %w", err)
		}
		return string(report), object, nil
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, fmt.Errorf("final machine result was not emitted")
	}
	object, err := decodeDoagentJSONObject(text)
	if err != nil {
		return "", nil, fmt.Errorf("final machine result must contain one JSON object: %w", err)
	}
	return text, object, nil
}

func (c *doagentToolTraceCollector) verifiedMutationResult() (string, map[string]interface{}, error) {
	traces := c.orderedTraces()
	mutations := make([]interface{}, 0, len(traces))
	verified, outcome := true, "succeeded"
	for i, trace := range traces {
		tool := modelVisibleDoagentToolName(trace.ToolName)
		if tool != "doops_helm" && tool != "doops_web" {
			continue
		}
		entry := map[string]interface{}{
			"tool":         tool,
			"tool_call_id": trace.ToolCallID,
			"status":       trace.Status,
		}
		reason := trace.terminalMutationReason()
		switch reason {
		case "", "tool_result_failed", "execution_outcome_unknown":
			result, err := trace.verifiedResult()
			if err != nil {
				return "", nil, fmt.Errorf("decode verified mutation result: %w", err)
			}
			entry["verified"], entry["result"] = true, result
			if reason == "execution_outcome_unknown" {
				outcome = "unknown"
			} else if reason == "tool_result_failed" {
				if resolvedBy := correctedHelmCall(traces, i); resolvedBy != "" {
					entry["resolved_by"] = resolvedBy
				} else if outcome != "unknown" {
					outcome = "failed"
				}
			}
		default:
			entry["verified"], entry["reason"] = false, reason
			verified, outcome = false, "unknown"
		}
		mutations = append(mutations, entry)
	}
	if len(mutations) == 0 {
		verified, outcome = false, "failed"
	}
	report := map[string]interface{}{
		"kind":      "doops.verified-mutation",
		"verified":  verified,
		"outcome":   outcome,
		"mutations": mutations,
	}
	c.mu.Lock()
	summary := c.finalMessage
	turn := c.turnResult
	c.mu.Unlock()
	if len(turn) != 0 {
		report["turn"] = turn
		if outcome != "unknown" && doagentTurnFinishedError(turn) != nil {
			report["outcome"] = "failed"
		}
	}
	if strings.TrimSpace(summary) != "" {
		report["summary"] = redactSecretText(summary)
	}
	encoded, err := stableDoagentJSON(report)
	if err != nil {
		return "", nil, fmt.Errorf("encode verified mutation result: %w", err)
	}
	return string(encoded), report, nil
}

func (c *doagentToolTraceCollector) orderedTraces() []doagentToolTrace {
	c.mu.Lock()
	defer c.mu.Unlock()

	traces := make([]doagentToolTrace, 0, len(c.calls))
	for id, trace := range c.calls {
		trace.ToolCallID = id
		trace.expectedSessionID = c.expectedSessionID
		trace.expectedMutationAuthorityDigest = c.expectedMutationAuthorityDigest
		traces = append(traces, trace)
	}
	sort.Slice(traces, func(i, j int) bool {
		if traces[i].sequence == traces[j].sequence {
			return traces[i].ToolCallID < traces[j].ToolCallID
		}
		return traces[i].sequence < traces[j].sequence
	})
	return traces
}

func (c *doagentToolTraceCollector) completed() []doagentToolTrace {
	var traces []doagentToolTrace
	for _, trace := range c.orderedTraces() {
		if trace.eligible() {
			traces = append(traces, trace)
		}
	}
	return traces
}

func (c *doagentToolTraceCollector) rejectionSummary() string {
	c.mu.Lock()
	defer c.mu.Unlock()

	rejections := make([]string, 0, len(c.calls))
	for _, trace := range c.calls {
		trace.expectedSessionID = c.expectedSessionID
		trace.expectedMutationAuthorityDigest = c.expectedMutationAuthorityDigest
		if reason := trace.ineligibilityReason(); reason != "" {
			tool := modelVisibleDoagentToolName(trace.ToolName)
			if tool == "" {
				tool = "unknown"
			}
			rejections = append(rejections, tool+"="+reason)
		}
	}
	sort.Strings(rejections)
	return strings.Join(rejections, ",")
}

func (c *doagentToolTraceCollector) requireCompletedToolEvidence() error {
	if unresolved := c.unresolvedMutationSummary(); unresolved != "" {
		return fmt.Errorf("doagent_apply_tool_evidence_missing: %s", unresolved)
	}
	traces := c.orderedTraces()
	for i, trace := range traces {
		if trace.terminalMutationReason() == "tool_result_failed" {
			if correctedHelmCall(traces, i) != "" {
				continue
			}
			return fmt.Errorf("doagent_apply_tool_failed: %s: %s",
				modelVisibleDoagentToolName(trace.ToolName), trace.failedResultText())
		}
	}
	if len(c.completed()) != 0 {
		return nil
	}
	if rejected := c.rejectionSummary(); rejected != "" {
		return fmt.Errorf("doagent_apply_tool_evidence_missing: %s", rejected)
	}
	return fmt.Errorf("doagent_apply_tool_evidence_missing")
}

// A later serial success can recover a known failure of the same Helm intent.
// Unexecuted validation/preparation errors may also correct invalid parameters.
func correctedHelmCall(traces []doagentToolTrace, index int) string {
	failed := traces[index]
	if modelVisibleDoagentToolName(failed.ToolName) != "doops_helm" ||
		failed.terminalSequence == 0 || failed.terminalMutationReason() != "tool_result_failed" {
		return ""
	}
	result, _ := failed.verifiedResult()
	facts, _ := result["structuredContent"].(map[string]interface{})
	executed := facts["executionStarted"] == true && facts["phase"] == "execution"
	unexecuted := facts["executionStarted"] == false &&
		(facts["phase"] == "validation" || facts["phase"] == "preparation")
	if !executed && !unexecuted {
		return ""
	}
	value, err := decodeDoagentJSONValue(failed.inputJSON)
	input, ok := value.(map[string]interface{})
	if err != nil || !ok {
		return ""
	}
	release := strings.TrimSpace(stringField(input, "release"))
	namespace := strings.TrimSpace(stringField(input, "namespace"))
	operation := strings.ToLower(strings.TrimSpace(stringField(input, "operation")))
	if release == "" || namespace == "" || (executed && !stringSliceContains(helmMutationOperations, operation)) {
		return ""
	}
	for _, next := range traces[index+1:] {
		if next.sequence <= failed.terminalSequence ||
			modelVisibleDoagentToolName(next.ToolName) != "doops_helm" || !next.eligible() {
			continue
		}
		nextValue, err := decodeDoagentJSONValue(next.inputJSON)
		nextInput, ok := nextValue.(map[string]interface{})
		if err != nil || !ok {
			continue
		}
		nextOperation := strings.ToLower(strings.TrimSpace(stringField(nextInput, "operation")))
		if stringSliceContains(helmMutationOperations, operation) && operation != nextOperation {
			continue
		}
		if executed {
			switch operation {
			case "upgrade-install":
				source, nextSource := strings.TrimSpace(stringField(input, "asset_source")), strings.TrimSpace(stringField(nextInput, "asset_source"))
				if source == "" {
					source = helmAssetSourceRemote
				}
				if nextSource == "" {
					nextSource = helmAssetSourceRemote
				}
				chart := strings.TrimSpace(stringField(input, "chart"))
				nextChart := strings.TrimSpace(stringField(nextInput, "chart"))
				if chart == "" || nextChart == "" || chart != nextChart || source != nextSource {
					continue
				}
			case "rollback":
				revision, ok := input["revision"].(json.Number)
				number, err := revision.Int64()
				if !ok || err != nil || number <= 0 || revision != nextInput["revision"] {
					continue
				}
			}
		}
		if release == strings.TrimSpace(stringField(nextInput, "release")) &&
			namespace == strings.TrimSpace(stringField(nextInput, "namespace")) {
			return next.ToolCallID
		}
	}
	return ""
}

func (c *doagentToolTraceCollector) hasUnresolvedMutation() bool {
	return c.unresolvedMutationSummary() != ""
}

// unresolvedMutationSummary lists tool=reason for every mutation whose outcome
// cannot be proven from its trace, or "" when every mutation is resolved.
func (c *doagentToolTraceCollector) unresolvedMutationSummary() string {
	traces := c.orderedTraces()
	var unresolved []string
	for _, trace := range traces {
		tool := modelVisibleDoagentToolName(trace.ToolName)
		if tool == "" {
			tool = "unknown"
		}
		if tool != "doops_helm" && tool != "doops_web" {
			continue
		}
		trace.expectedSessionID = c.expectedSessionID
		trace.expectedMutationAuthorityDigest = c.expectedMutationAuthorityDigest
		if reason := trace.terminalMutationReason(); reason != "" && reason != "tool_result_failed" {
			unresolved = append(unresolved, tool+"="+reason)
		}
	}
	sort.Strings(unresolved)
	return strings.Join(unresolved, ",")
}

func (trace doagentToolTrace) eligible() bool {
	return trace.ineligibilityReason() == ""
}

func (trace doagentToolTrace) ineligibilityReason() string {
	return trace.terminalMutationReason()
}

// A verified failed tool result is terminal evidence, not successful deployment.
func (trace doagentToolTrace) terminalMutationReason() string {
	switch {
	case trace.Status != "completed" && trace.Status != "failed":
		return "status_not_completed"
	case trace.ToolName == "":
		return "tool_name_missing"
	case len(trace.inputJSON) == 0:
		return "input_missing"
	case len(trace.resultJSON) == 0:
		return "raw_output_missing"
	case !validDoagentObservedAt(trace.ObservedAt):
		return "observed_at_invalid"
	}
	tool := modelVisibleDoagentToolName(trace.ToolName)
	if tool != "doops_helm" && tool != "doops_web" {
		return "structured_mutation_tool_required"
	}
	if reason := trace.runtimeMutationAttestationReason(); reason != "" {
		return reason
	}
	return ""
}

func (trace doagentToolTrace) runtimeMutationAttestationReason() string {
	tool := modelVisibleDoagentToolName(trace.ToolName)
	binding := runtimeToolBinding(tool)
	if binding.Server == "" {
		return "structured_mutation_tool_required"
	}
	realTool := binding.Tool
	if trace.AttestationSchema != "doops.tool-attestation/v2" {
		return "attestation_schema_invalid"
	}
	if trace.AttestedTool != realTool || trace.EvidenceSubject != realTool {
		return "attested_tool_mismatch"
	}
	if trace.EvidenceKind != "runtime-tool-result" {
		return "evidence_kind_invalid"
	}
	if trace.expectedSessionID == "" {
		return "session_binding_mismatch"
	}
	if stringField(trace.CanonicalScope, "runtimeSessionId") != trace.expectedSessionID {
		return "session_binding_mismatch"
	}
	expectedScope := map[string]interface{}{
		"runtimeSessionId": trace.expectedSessionID,
		"server":           binding.Server,
		"tool":             realTool,
	}
	if !reflect.DeepEqual(trace.CanonicalScope, expectedScope) {
		return "canonical_scope_mismatch"
	}
	expectedScopeDigest, err := digestRuntimeAttestationJSON(expectedScope)
	if err != nil || trace.ScopeDigest != expectedScopeDigest {
		return "scope_digest_mismatch"
	}
	input, err := decodeDoagentJSONValue(trace.inputJSON)
	if err != nil {
		return "input_invalid"
	}
	inputObject, ok := input.(map[string]interface{})
	if !ok {
		return "input_invalid"
	}
	expectedInputDigest, err := digestRuntimeAttestationJSON(map[string]interface{}{
		"arguments":        inputObject,
		"runtimeSessionId": trace.expectedSessionID,
		"server":           binding.Server,
		"tool":             realTool,
	})
	if err != nil || trace.InputDigest != expectedInputDigest {
		return "input_digest_mismatch"
	}
	if trace.MutationAuthorized == nil || !*trace.MutationAuthorized {
		return "mutation_not_authorized"
	}
	if trace.expectedMutationAuthorityDigest == "" ||
		trace.MutationAuthorityDigest != trace.expectedMutationAuthorityDigest {
		return "mutation_authority_binding_mismatch"
	}
	if trace.ExecutionMode != "apply" {
		return "execution_mode_invalid"
	}
	result, err := trace.verifiedResult()
	if err != nil {
		return "public_result_invalid"
	}
	facts, _ := result["structuredContent"].(map[string]interface{})
	if facts["executionOutcome"] == "unknown" {
		return "execution_outcome_unknown"
	}
	isError, ok := result["isError"].(bool)
	if !ok {
		return "tool_result_invalid"
	}
	if isError {
		return "tool_result_failed"
	}
	// An attested rejection can have invalid arguments; only success proves a mutation.
	operation := strings.ToLower(strings.TrimSpace(stringField(inputObject, "operation")))
	allowedOperations := helmMutationOperations
	if tool == "doops_web" {
		allowedOperations = webMutationOperations
	}
	if !stringSliceContains(allowedOperations, operation) {
		return "mutation_operation_invalid"
	}
	if trace.Status != "completed" {
		return "status_result_mismatch"
	}
	return ""
}

func stringSliceContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func validDoagentObservedAt(value string) bool {
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func (trace doagentToolTrace) verifiedResult() (map[string]interface{}, error) {
	result, err := publicDoagentToolResult(trace)
	if err != nil {
		return nil, err
	}
	resultObject, ok := result.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("runtime tool result must be an object")
	}
	expectedResultDigest, err := digestRuntimeAttestationJSON(resultObject)
	if err != nil {
		return nil, fmt.Errorf("digest runtime tool result: %w", err)
	}
	if expectedResultDigest != trace.effectiveResultDigest() {
		return nil, fmt.Errorf("runtime tool result digest mismatch")
	}
	return resultObject, nil
}

func (trace doagentToolTrace) effectiveResultDigest() string {
	if trace.PublicResultDigest != "" {
		return trace.PublicResultDigest
	}
	return trace.AttestedResultDigest
}

func rejectSensitiveToolResult(value interface{}) error {
	switch typed := value.(type) {
	case map[string]interface{}:
		for key, item := range typed {
			if credentialSecretLikeKey(key) || strings.EqualFold(strings.TrimSpace(key), "passwd") {
				return fmt.Errorf("runtime tool result contains forbidden sensitive field %q", key)
			}
			if err := rejectSensitiveToolResult(item); err != nil {
				return err
			}
		}
	case []interface{}:
		for _, item := range typed {
			if err := rejectSensitiveToolResult(item); err != nil {
				return err
			}
		}
	case string:
		if credentialScalarContainsInlineMaterial(typed) {
			return fmt.Errorf("runtime tool result contains forbidden sensitive scalar")
		}
	}
	return nil
}

func publicDoagentToolResult(trace doagentToolTrace) (interface{}, error) {
	result, err := decodeDoagentJSONValue(trace.resultJSON)
	if err != nil {
		return nil, err
	}
	if resultObject, ok := result.(map[string]interface{}); ok {
		if metadata, exists := resultObject["_meta"]; exists {
			attestationOnly := false
			if meta, ok := metadata.(map[string]interface{}); ok && len(meta) == 1 {
				_, attestationOnly = meta["doops.sh/tool-attestation"].(map[string]interface{})
			}
			if !attestationOnly {
				if err := rejectSensitiveToolResult(metadata); err != nil {
					return nil, err
				}
			}
			delete(resultObject, "_meta")
		}
	}
	if err := rejectSensitiveToolResult(result); err != nil {
		return nil, err
	}
	return result, nil
}

func modelVisibleDoagentToolName(runtimeName string) string {
	switch runtimeName {
	case "mcp_doops_helm":
		return "doops_helm"
	case "mcp_doops_web":
		return "doops_web"
	}
	return runtimeName
}

type doagentRuntimeToolBinding struct {
	Server string
	Tool   string
}

func runtimeToolBinding(modelTool string) doagentRuntimeToolBinding {
	switch modelTool {
	case "doops_helm":
		return doagentRuntimeToolBinding{Server: "doops", Tool: "helm"}
	case "doops_web":
		return doagentRuntimeToolBinding{Server: "doops", Tool: "web"}
	default:
		return doagentRuntimeToolBinding{}
	}
}

func stableDoagentJSON(value interface{}) (json.RawMessage, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoded, err := decodeDoagentJSONValue(data)
	if err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

func decodeDoagentJSONValue(data []byte) (interface{}, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing interface{}
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("expected exactly one JSON value")
		}
		return nil, err
	}
	return value, nil
}

func stringField(object map[string]interface{}, field string) string {
	value, _ := object[field].(string)
	return value
}

// failedResultText is the tool's own error, so a failed apply names the cause
// instead of only "tool failed". It is redacted and capped like other reasons.
func (trace doagentToolTrace) failedResultText() string {
	result, err := trace.verifiedResult()
	if err != nil {
		return "tool error text unavailable"
	}
	content, _ := result["content"].([]interface{})
	for _, item := range content {
		part, _ := item.(map[string]interface{})
		if text, _ := part["text"].(string); strings.TrimSpace(text) != "" {
			text = redactSecretText(strings.TrimSpace(text))
			if len([]rune(text)) > 400 {
				text = string([]rune(text)[:400]) + "..."
			}
			return text
		}
	}
	return "tool returned no error text"
}
