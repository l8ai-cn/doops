package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const executionCancellationTimeout = 30 * time.Second

var errExecutionOutcomeUnknown = errors.New("execution_outcome_unknown")

// requireExecutionLifecycle is intentionally narrower than the execution
// outcome guard. Foreground calls use terminal evidence and can run on legacy
// Agents; background calls require the Agent's lifetime/cancellation contract.
func (a *GatewayAgent) requireExecutionLifecycle(tool string) error {
	if tool == "doops_bg" && !a.ExecutionLifecycle {
		return fmt.Errorf("target Agent does not advertise protocol %q; upgrade the Agent before using background execution", executionLifecycleProtocol)
	}
	return nil
}

type executionOwnerKey struct{}

type executionOwner struct {
	client   *ClientService
	agent    *GatewayAgent
	resource string
	id       string
}

func (s *ClientService) withExecutionOwner(ctx context.Context, agent *GatewayAgent, resource, id string) context.Context {
	return context.WithValue(ctx, executionOwnerKey{}, executionOwner{s, agent, resource, id})
}

func (s *ClientService) inheritsExecutionOwner(ctx context.Context, agent *GatewayAgent, resource string) bool {
	owner, ok := ctx.Value(executionOwnerKey{}).(executionOwner)
	if !ok || owner.client != s || owner.agent != agent || owner.resource != resource || resource == "" {
		return false
	}
	s.activeOpsMu.Lock()
	defer s.activeOpsMu.Unlock()
	return s.activeOps[owner.id] != nil
}

func requiresExecutionOutcomeGuard(action GatewayAction, tool string, args json.RawMessage, resourceKey string) bool {
	if tool == "doops_agent_prompt" && extractStringArg(args, "operation") != "apply" {
		return false
	}
	if tool == "doops_agent_upgrade" && !upgradeAuditMutation(args) {
		return false
	}
	return !isConcurrentReadOnlyAction(action) && !isConcurrentQuery(action, resourceKey)
}

func interruptionNeedsReconciliation(tool string, args json.RawMessage) bool {
	switch tool {
	case "doops_agent_upgrade":
		return upgradeAuditMutation(args)
	case "doops_shell", "doops_ssh", "doops_bg",
		"doops_credential_materialize", "doops_credential_cleanup":
		return true
	case "doops_agent_prompt":
		return extractStringArg(args, "operation") == "apply"
	case "doops_k8s":
		return !gatewayK8SReadOnly(extractStringArg(args, "operation"))
	case "doops_web":
		return !containsWebString(webReadOperations, strings.ToLower(strings.TrimSpace(extractStringArg(args, "operation"))))
	default:
		return false
	}
}

// Local workspace mutations can finish safely after cancellation, but losing
// their terminal reply is not evidence that they stopped changing the workspace.
func missingTerminalNeedsReconciliation(tool string, args json.RawMessage) bool {
	switch tool {
	case "doops_file_write", "doops_clean_workspace",
		"doops_workspace_begin", "doops_workspace_chunk", "doops_workspace_commit":
		return true
	default:
		return interruptionNeedsReconciliation(tool, args)
	}
}

func executionResponseCanceled(response map[string]interface{}) bool {
	rpcError, _ := response["error"].(map[string]interface{})
	code, ok := numericID(rpcError["code"])
	return ok && code == -32007
}

// These categories are forwarded without losing their meaning. The
// cleanup_unknown category is deliberately absent: a queued request may still
// execute, so the Gateway must report execution_outcome_unknown for it.
var promptPreAdmissionCategories = []string{
	"doagent_prompt_busy",
	"doagent_prompt_admission_unsupported",
	"doagent_prompt_admission_unavailable",
	"doagent_prompt_admission_timeout",
	"doagent_prompt_cancelled_before_admission",
	"doagent_prompt_queued",
	"doagent_prompt_queue_timeout",
	"doagent_prompt_no_prompt",
	"doagent_prompt_rejected",
	"doagent_unavailable",
	"doagent_session_create_failed",
	"doagent_session_create_invalid",
	"doagent_permission_mode_configuration_failed",
	"doagent_model_configuration_failed",
	"doagent_mode_configuration_failed",
	"doagent_attachment_upload_failed",
	"doagent_sse_unavailable",
	"doagent_mutation_authority_unavailable",
}

// promptPreAdmissionReply reports whether a prompt answer is a pre-admission
// refusal the Gateway must forward unchanged. remoteMutationFailed would
// otherwise rewrite every -32603 answer into execution_outcome_unknown, which is
// the misclassification these categories exist to remove.
func promptPreAdmissionReply(response map[string]interface{}) bool {
	if _, ok := response["result"].(map[string]interface{}); ok {
		return false
	}
	rpcError, ok := response["error"].(map[string]interface{})
	if !ok {
		return false
	}
	message, _ := rpcError["message"].(string)
	message = strings.TrimSpace(message)
	for _, category := range promptPreAdmissionCategories {
		if message == category {
			return true
		}
	}
	return false
}

func remoteMutationFailed(tool string, args json.RawMessage, response map[string]interface{}) bool {
	if tool != "doops_ssh" && tool != "doops_agent_prompt" && tool != "doops_k8s" && tool != "doops_web" &&
		tool != "doops_credential_materialize" && tool != "doops_credential_cleanup" {
		return false
	}
	if !interruptionNeedsReconciliation(tool, args) {
		return false
	}
	if promptPreAdmissionReply(response) {
		return false
	}
	if _, ok := response["result"].(map[string]interface{}); ok {
		// A structured result, including an isError tool result, proves that the
		// remote tool reached a terminal state. Only a missing terminal result
		// can leave a mutation outcome unknown.
		if tool == "doops_ssh" {
			result, _ := response["result"].(map[string]interface{})
			isError, _ := result["isError"].(bool)
			if !isError {
				return false
			}
			exitCode, ok := numericID(result["sshExitCode"])
			return ok && exitCode == 255
		}
		return false
	}
	rpcError, ok := response["error"].(map[string]interface{})
	if !ok {
		return false
	}
	code, _ := numericID(rpcError["code"])
	// Invalid parameters, unknown tools and denied admission never start work.
	return code != -32602 && code != -32601 && code != -32003
}

func executionResponseUnknown(response map[string]interface{}) bool {
	rpcError, ok := response["error"].(map[string]interface{})
	if !ok {
		return false
	}
	message, _ := rpcError["message"].(string)
	return message == errExecutionOutcomeUnknown.Error() ||
		strings.HasPrefix(message, errExecutionOutcomeUnknown.Error()+":")
}

type unknownMutationReceipt struct {
	error
	report map[string]interface{}
}

func (receipt *unknownMutationReceipt) Unwrap() error { return receipt.error }

// executionUnknownFromResponse keeps the reason the Agent gave for an unknown
// outcome, so the caller and the audit can tell which check could not be
// proven. Without it every unknown outcome reads the same.
func executionUnknownFromResponse(response map[string]interface{}) error {
	rpcError, _ := response["error"].(map[string]interface{})
	message, _ := rpcError["message"].(string)
	message = strings.TrimSpace(message)
	prefix := errExecutionOutcomeUnknown.Error()
	cause := errExecutionOutcomeUnknown
	switch {
	case strings.HasPrefix(message, prefix+":"):
		reason := strings.TrimSpace(strings.TrimPrefix(message, prefix+":"))
		if reason != "" {
			cause = fmt.Errorf("%w: %s", errExecutionOutcomeUnknown, sanitizeSecretText(reason))
		}
	case message != "" && message != prefix:
		cause = fmt.Errorf("%w: remote error: %s", errExecutionOutcomeUnknown, sanitizeSecretText(message))
	}
	data, _ := rpcError["data"].(map[string]interface{})
	report, _ := data["structuredContent"].(map[string]interface{})
	if report["kind"] == "doops.verified-mutation" {
		return &unknownMutationReceipt{cause, report}
	}
	return cause
}

func (q *gatewayMessageQueue) observeTerminal(msg gatewayWSMessage) {
	if _, notification := msg.Parsed["method"]; notification {
		return
	}
	if result, ok := msg.Parsed["result"].(map[string]interface{}); ok {
		// A cancel acknowledgement is not the original operation's terminal reply.
		if _, acknowledged := result["canceled"]; acknowledged {
			return
		}
	} else if _, failed := msg.Parsed["error"]; !failed {
		return
	}
	q.terminalOnce.Do(func() {
		q.terminalMessage = msg
		if executionResponseUnknown(msg.Parsed) {
			q.terminalErr = executionUnknownFromResponse(msg.Parsed)
		}
		close(q.terminal)
	})
}

func (s *ClientService) unresolvedExecutions(cluster, instance string) ([]AuditEvent, error) {
	s.registry.mu.RLock()
	defer s.registry.mu.RUnlock()
	return s.unresolvedExecutionsLocked(cluster, instance)
}

// The registry read lock prevents a replacement connection from inheriting
// ownership of an execution admitted on the old connection.
func (s *ClientService) unresolvedExecutionsLocked(cluster, instance string) ([]AuditEvent, error) {
	events, err := s.store.UnresolvedExecutions(cluster, instance)
	if err != nil {
		return nil, err
	}
	s.activeOpsMu.Lock()
	live := make(map[int64]bool, len(s.activeOps))
	for _, op := range s.activeOps {
		if op.AuditID != 0 {
			// The Gateway operation remains the owner across Agent connection
			// generations. A replacement connection must not convert an
			// operation that is still draining or finalizing into history.
			live[op.AuditID] = true
		}
	}
	s.activeOpsMu.Unlock()
	unresolved := events[:0]
	for _, event := range events {
		if event.Status == "executing" && live[event.ID] {
			continue
		}
		if event.Status == "executing" {
			if err := s.store.MarkExecutionUncertain(event.ID); err != nil {
				if !errors.Is(err, sql.ErrNoRows) {
					return nil, fmt.Errorf("persist execution outcome unknown for audit-%d: %w", event.ID, err)
				}
				var status string
				if readErr := s.store.db.QueryRow("SELECT status FROM audit_events WHERE id = ?", event.ID).Scan(&status); readErr != nil {
					return nil, fmt.Errorf("read concurrent execution outcome for audit-%d: %w", event.ID, readErr)
				}
				if status == "executing" {
					return nil, fmt.Errorf("persist execution outcome unknown for audit-%d: %w", event.ID, err)
				}
				if status != "outcome_unknown" {
					continue
				}
			}
			event.Status = "outcome_unknown"
		}
		unresolved = append(unresolved, event)
	}
	return unresolved, nil
}

func (s *ClientService) beginExecution(agent *GatewayAgent, operationID string, auditID int64) error {
	s.registry.mu.RLock()
	defer s.registry.mu.RUnlock()
	if s.registry.sessions[agent.Key] != agent {
		return errGatewayAgentDisconnected
	}
	select {
	case <-agent.closed:
		return errGatewayAgentDisconnected
	default:
	}
	s.activeOpsMu.Lock()
	defer s.activeOpsMu.Unlock()
	op := s.activeOps[operationID]
	if op == nil || op.State == "cancel_requested" {
		return context.Canceled
	}
	if holderID := s.activeOperationConflictLocked(op.GatewayActiveOperation, agent, operationID); holderID != "" {
		return fmt.Errorf("target busy: active operation %s", holderID)
	}
	for id, current := range s.activeOps {
		if id != operationID && current.State == "cancel_requested" && operationsConflict(op.GatewayActiveOperation, current.GatewayActiveOperation) {
			return fmt.Errorf("target busy: cancellation pending")
		}
	}
	if err := s.store.MarkExecutionDispatched(auditID); err != nil {
		return fmt.Errorf("execution_audit_unavailable: %w", err)
	}
	op.agent = agent
	return nil
}
