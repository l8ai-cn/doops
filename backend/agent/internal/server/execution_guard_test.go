package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/api"
)

func TestExecutionGuardSessionMutationBlocksSameWorkspaceChanges(t *testing.T) {
	store := openExecutionAuditTestStore(t, t.TempDir()+"/gateway.db")
	hub := NewGatewayHub(store, GatewayHubOptions{})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	conn := dialFakeGatewayAgent(t, server.URL, "dev", "node")
	defer conn.Close()
	go serveFakeAgent(t, conn)
	waitForGatewayAgent(t, hub, "dev", "node")
	agent := hub.getAgent("dev", "node")
	mutation := "target-mutation:dev/node"
	lease, err := agent.acquireActionLeaseForSession(context.Background(), ActionExec, mutation, "release", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	for _, action := range []GatewayAction{ActionPush, ActionWrite, ActionClean} {
		if err := agent.acquireForAction(context.Background(), action, "workspace:release", 0, 0); err == nil {
			agent.releaseForAction(action, "workspace:release")
			t.Fatalf("%s acquired workspace while target mutation owned it", action)
		}
	}
	lease.Release()
	if err := agent.acquireForAction(context.Background(), ActionWrite, "workspace:release", 0, 0); err != nil {
		t.Fatalf("workspace remained blocked after mutation terminal release: %v", err)
	}
	agent.releaseForAction(ActionWrite, "workspace:release")
}

func TestExecutionGuardCleanLocksDeletedWorkspace(t *testing.T) {
	args := json.RawMessage(`{"session_id":"maintenance","workspace":"build"}`)
	resource := resourceKeyForTool(ActionClean, "doops_clean_workspace", args, "dev", "node")
	if resource != "workspace:build" {
		t.Fatalf("clean locked caller instead of deleted workspace: %q", resource)
	}
	agent := &GatewayAgent{
		closed:    make(chan struct{}),
		resources: make(map[string]*agentResourceSlot),
	}
	if err := agent.acquireForAction(context.Background(), ActionExec, "workspace:build", 0, 0); err != nil {
		t.Fatal(err)
	}
	defer agent.releaseForAction(ActionExec, "workspace:build")
	if err := agent.acquireForAction(context.Background(), ActionClean, resource, 0, 0); err == nil {
		agent.releaseForAction(ActionClean, resource)
		t.Fatal("maintenance clean acquired a running build workspace")
	}
}

func TestExecutionGuardReplacementCannotInheritLiveExecution(t *testing.T) {
	store := openExecutionAuditTestStore(t, t.TempDir()+"/gateway.db")
	hub := NewGatewayHub(store, GatewayHubOptions{})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()
	oldConn := dialFakeGatewayAgent(t, server.URL, "dev", "node")
	defer oldConn.Close()
	go serveFakeAgentWithRuntimeID(t, oldConn, "original")
	waitForGatewayAgent(t, hub, "dev", "node")
	oldAgent := hub.getAgent("dev", "node")
	id := startExecutionAuditTestEvent(t, store, AuditEvent{
		Cluster: "dev", Instance: "node", Action: ActionExec, Session: "original",
	})
	opID := hub.client.registerActiveOperation(GatewayActiveOperation{
		AuditID: id, Cluster: "dev", Instance: "node", Action: ActionExec,
		Resource: "workspace:original", Session: "original",
	}, func() {})
	defer hub.client.finishActiveOperation(opID)
	hub.client.markOperationRunning(opID)
	if err := hub.client.beginExecution(oldAgent, opID, id); err != nil {
		t.Fatal(err)
	}
	if events, err := hub.client.unresolvedExecutions("dev", "node"); err != nil || len(events) != 0 {
		t.Fatalf("live original owner incorrectly reported as unknown: %#v, %v", events, err)
	}
	newConn := dialFakeGatewayAgent(t, server.URL, "dev", "node")
	defer newConn.Close()
	go serveFakeAgentWithRuntimeID(t, newConn, "replacement")
	requireEventually(t, time.Second, func() bool { return hub.getAgent("dev", "node") != oldAgent })
	if events, err := hub.client.unresolvedExecutions("dev", "node"); err != nil || len(events) != 0 {
		t.Fatalf("replacement converted the still-owned execution to history: %#v, %v", events, err)
	}
	if err := hub.client.beginExecution(oldAgent, opID, id); !errors.Is(err, errGatewayAgentDisconnected) {
		t.Fatalf("stale Agent retained dispatch ownership: %v", err)
	}
	hub.client.finishActiveOperation(opID)
	if events, err := hub.client.unresolvedExecutions("dev", "node"); err != nil || len(events) != 1 || events[0].ID != id || events[0].Status != "outcome_unknown" {
		t.Fatalf("released stale execution was not reconciled as unknown: %#v, %v", events, err)
	}
	nextID := startExecutionAuditTestEvent(t, store, AuditEvent{
		Cluster: "dev", Instance: "node", Action: ActionExec, Session: "replacement",
	})
	nextOp := hub.client.registerActiveOperation(GatewayActiveOperation{
		AuditID: nextID, Cluster: "dev", Instance: "node", Action: ActionExec, Session: "replacement",
	}, func() {})
	defer hub.client.finishActiveOperation(nextOp)
	if err := hub.client.beginExecution(hub.getAgent("dev", "node"), nextOp, nextID); err != nil {
		t.Fatalf("historical unknown blocked replacement dispatch: %v", err)
	}
	if got := executionAuditTestEvent(t, store, id); got.Status != "outcome_unknown" || got.EndedAt.IsZero() {
		t.Fatalf("replacement dispatch cleared original uncertainty: %#v", got)
	}
}

func TestExecutionGuardSuccessfulContentCannotSignalUnknownOutcome(t *testing.T) {
	for _, result := range []map[string]interface{}{
		{"content": []map[string]interface{}{{"type": "text", "text": "execution_outcome_unknown"}}},
		{"isError": true, "content": []map[string]interface{}{{"type": "text", "text": "file mentions execution_outcome_unknown"}}},
	} {
		response := map[string]interface{}{"jsonrpc": "2.0", "id": 1, "result": result}
		if executionResponseUnknown(response) {
			t.Fatal("ordinary tool content became an execution protocol error")
		}
		queue := newGatewayMessageQueue(newGatewayMessageBudget(1024), 1024, 1, "tools/cancel")
		queue.observeTerminal(gatewayWSMessage{Parsed: response})
		select {
		case <-queue.terminal:
		default:
			t.Fatal("successful response was not observed as terminal")
		}
		if queue.terminalErr != nil {
			t.Fatalf("successful content created an uncertain terminal: %v", queue.terminalErr)
		}
		queue.close()
	}
	if !executionResponseUnknown(map[string]interface{}{
		"error": map[string]interface{}{"code": -32603, "message": "execution_outcome_unknown"},
	}) {
		t.Fatal("explicit protocol error failed to retain uncertainty")
	}
}

func TestExecutionGuardDoesNotInventLifecycleCapability(t *testing.T) {
	for _, agent := range []*GatewayAgent{nil, {}, {StructuredSSH: true}} {
		capabilities := clientCapabilities(agent)
		protocols, _ := capabilities["protocols"].(map[string]interface{})
		if _, exists := protocols["executionLifecycle"]; exists {
			t.Fatal("client exposed lifecycle capability not advertised by Agent")
		}
	}
}

func TestExecutionGuardHistoricalUnknownDoesNotBlockInternalCall(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "guarded-internal")
	id := startExecutionAuditTestEvent(t, f.store, AuditEvent{
		Cluster: "dev", Instance: "local", Action: ActionExec, Status: "outcome_unknown",
	})
	before := executionAuditTestEvent(t, f.store, id)
	done := make(chan error, 1)
	go func() {
		_, err := f.hub.RunInternalToolCall(context.Background(), "dev", "local", "doops_shell", map[string]interface{}{
			"session_id": "next-internal", "command": "true",
		})
		done <- err
	}()
	agent.complete(t, agent.call(t, "doops_shell", "next-internal"), "")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("historical unknown blocked internal execution: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("internal execution did not finish")
	}
	unresolved, err := f.store.UnresolvedExecutions("dev", "local")
	if err != nil || len(unresolved) != 1 || unresolved[0].ID != id {
		t.Fatalf("internal execution changed historical uncertainty: %#v, %v", unresolved, err)
	}
	if got := executionAuditTestEvent(t, f.store, id); !reflect.DeepEqual(got, before) {
		t.Fatalf("internal execution rewrote unknown audit: %#v", got)
	}
}

func TestExecutionGuardDisconnectForwardsObservedTerminalError(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		accepted <- conn
	}))
	defer server.Close()
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	conn := <-accepted
	defer conn.Close()
	agent := newGatewayAgentSession("dev", "node", "", "test", conn, newGatewayMessageBudget(1<<20))
	go func() {
		var request map[string]interface{}
		if err := peer.ReadJSON(&request); err != nil {
			t.Error(err)
			return
		}
		id, _ := numericID(request["id"])
		agent.pendingMu.Lock()
		queue := agent.pending[id]
		agent.pendingMu.Unlock()
		queue.observeTerminal(gatewayWSMessage{Parsed: map[string]interface{}{
			"jsonrpc": "2.0", "id": request["id"],
			"error": map[string]interface{}{"code": -32603, "message": "synthetic terminal error"},
		}})
		close(agent.closed)
	}()
	want := errors.New("downstream failed")
	forwarded := false
	err = agent.relayToolCall(context.Background(), api.ToolCallParams{Name: "doops_file_read", Arguments: json.RawMessage(`{}`)}, time.Second,
		func(message gatewayWSMessage) error {
			forwarded = strings.Contains(fmt.Sprint(message.Parsed["error"]), "synthetic terminal error")
			return want
		})
	if !forwarded || !errors.Is(err, want) {
		t.Fatalf("observed terminal error became silent success: forwarded=%t err=%v", forwarded, err)
	}
}

func TestBackgroundRPCStreamsActualCommandOutput(t *testing.T) {
	for _, exitCode := range []int{0, 17} {
		t.Run(fmt.Sprint(exitCode), func(t *testing.T) {
			t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
			_, conn, messages := cancellationTestAgent(t)
			sendCancellationTestTool(t, conn, "doops_bg", map[string]interface{}{
				"session_id": "background-log", "command": fmt.Sprintf("printf 'BG_CANARY\\n'; sleep 0.3; exit %d", exitCode),
			})
			output := awaitCancellationTestMessage(t, messages, func(message map[string]interface{}) bool {
				return message["method"] == "notifications/message" && strings.Contains(fmt.Sprint(message["params"]), "BG_CANARY")
			})
			if output["result"] != nil {
				t.Fatal("command output was not streamed before the final result")
			}
			final := awaitCancellationTestMessage(t, messages, func(message map[string]interface{}) bool { return message["id"] == float64(42) })
			result, ok := final["result"].(map[string]interface{})
			if !ok || (result["isError"] == true) != (exitCode != 0) {
				t.Fatalf("background log streaming changed exit evidence: %#v", final)
			}
		})
	}
}

func TestBackgroundRPCPreservesSplitUTF8Output(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	_, conn, messages := cancellationTestAgent(t)
	sendCancellationTestTool(t, conn, "doops_bg", map[string]interface{}{
		"session_id": "background-utf8", "command": "printf '\\344'; sleep 0.3; printf '\\270\\255\\n'",
	})
	var output strings.Builder
	final := awaitCancellationTestMessage(t, messages, func(message map[string]interface{}) bool {
		if params, ok := message["params"].(map[string]interface{}); ok {
			if data, ok := params["data"].(string); ok {
				output.WriteString(data)
			}
		}
		return message["id"] == float64(42)
	})
	if final["error"] != nil || !strings.Contains(output.String(), "\u4e2d") || strings.Contains(output.String(), "\ufffd") {
		t.Fatalf("split UTF-8 was corrupted: final=%#v output=%q", final, output.String())
	}
}

func TestExecutionGuardShellScopeNeverDependsOnCommandKeywords(t *testing.T) {
	for _, tool := range []string{"doops_shell", "doops_bg"} {
		for _, command := range []string{
			"sleep 30",
			"./change-production",
			"python3 -c 'open(\"/etc/app.conf\", \"w\").write(\"changed\")'",
			"mongosh --eval 'db.items.drop()'",
			"curl -X POST https://service.example/actions",
			"h=helm; $h upgrade app chart",
		} {
			args, err := json.Marshal(map[string]string{"session_id": "isolated", "command": command})
			if err != nil {
				t.Fatal(err)
			}
			if got := resourceKeyForTool(ActionExec, tool, args, "dev", "node"); got != "target-mutation:dev/node" {
				t.Errorf("%s %q incorrectly isolated to %q", tool, command, got)
			}
		}
	}
}

func TestExecutionGuardK8SScopeUsesNormalizedOperation(t *testing.T) {
	for _, operation := range []string{"scale", " SCALE ", "rollout-restart", "DELETE-POD", "future-operation", ""} {
		args, _ := json.Marshal(map[string]string{"operation": operation, "resource": "deployment/app"})
		if got := resourceKeyForTool(ActionExec, "doops_k8s", args, "dev", "node"); got != "target-mutation:dev/node" {
			t.Errorf("%q bypassed mutation ownership with %q", operation, got)
		}
	}
}

func TestExecutionGuardQueriesRemainAvailableDuringMutationAndUncertainty(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		t.Run(fmt.Sprint(uncertain), func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			agent := f.agent(t, "diagnostic-agent")
			holder, query := f.client(t, f.admin), f.client(t, f.admin)
			var mutation executionLifecycleCall
			if uncertain {
				startExecutionAuditTestEvent(t, f.store, AuditEvent{
					Cluster: "dev", Instance: "local", Action: ActionExec, Status: "outcome_unknown",
				})
			} else {
				lifecycleSend(t, holder, 2, "doops_shell", "writer", map[string]interface{}{"command": "./change-production"})
				mutation = agent.call(t, "doops_shell", "writer")
			}
			for id, tool := range []string{"doops_k8s", "doops_task_status"} {
				lifecycleSend(t, query, id+2, tool, "diagnostic", map[string]interface{}{
					"operation": "get", "kind": "pods", "task_id": "task-1",
				})
				call := agent.call(t, tool, "diagnostic")
				agent.complete(t, call, "")
				lifecycleResponse(t, query, id+2, "")
			}
			if !uncertain {
				agent.complete(t, mutation, "")
				lifecycleResponse(t, holder, 2, "")
			}
		})
	}
}

func TestExecutionGuardReadOnlyK8SDisconnectDoesNotPersistUnknown(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "read-only-k8s")
	client := f.client(t, f.admin)

	lifecycleSend(t, client, 2, "doops_k8s", "read-only-query", map[string]interface{}{
		"operation": "get",
		"kind":      "pods",
	})
	agent.call(t, "doops_k8s", "read-only-query")
	op := f.operation(t, "read-only-query", "running")
	if !strings.HasPrefix(op.Resource, "query:k8s:") {
		t.Fatalf("read-only K8s request acquired a mutation resource: %#v", op)
	}
	if events, err := f.hub.client.unresolvedExecutions("dev", "local"); err != nil || len(events) != 0 {
		t.Fatalf("active read-only request was exposed as an unresolved execution: %#v, %v", events, err)
	}

	if err := agent.conn.Close(); err != nil {
		t.Fatal(err)
	}
	response := readUntilID(t, client, 2)
	if rpcError, ok := response["error"].(map[string]interface{}); !ok ||
		strings.Contains(fmt.Sprint(rpcError["message"]), errExecutionOutcomeUnknown.Error()) {
		t.Fatalf("read-only disconnect became an unknown outcome: %#v", response)
	}
	requireEventually(t, 3*time.Second, func() bool {
		return len(f.hub.listActiveOperations()) == 0
	})
	unresolved, err := f.store.UnresolvedExecutions("dev", "local")
	if err != nil {
		t.Fatal(err)
	}
	if len(unresolved) != 0 {
		t.Fatalf("read-only disconnect persisted mutation blockers: %#v", unresolved)
	}
}

func TestExecutionGuardCanceledMutationRetainsUnknownWithoutBlockingNextCall(t *testing.T) {
	for _, tool := range []string{"doops_shell", "doops_ssh", "doops_bg", "doops_agent_prompt", "doops_k8s"} {
		t.Run(tool, func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			initialize := gatewayTestInitializeResult("interrupted-writer")
			protocols := initialize["capabilities"].(map[string]interface{})["protocols"].(map[string]interface{})
			protocols[structuredSSHProtocol] = structuredSSHCapability()
			protocols[executionLifecycleProtocol] = executionLifecycleCapability()
			agent := f.agentWithInitialize(t, initialize)
			client, probe := f.client(t, f.admin), f.client(t, f.admin)
			args := map[string]interface{}{"command": "./change-production"}
			switch tool {
			case "doops_ssh":
				args = map[string]interface{}{"host": "ops@example.com", "command": []string{"./change-production"}}
			case "doops_agent_prompt":
				args = map[string]interface{}{"operation": "apply", "instruction": "synthetic release"}
			case "doops_k8s":
				args = map[string]interface{}{"operation": "scale", "resource": "deployment/app", "replicas": 2}
			}
			lifecycleSend(t, client, 2, tool, "interrupted", args)
			call := agent.call(t, tool, "interrupted")
			op := f.operation(t, "interrupted", "running")
			f.cancel(t, op.ID)
			select {
			case <-agent.cancels:
			case <-time.After(3 * time.Second):
				t.Fatal("Agent did not receive the cancellation request")
			}
			// Same-session shell work runs concurrently even while a cancel is pending.
			lifecycleSend(t, probe, 2, "doops_shell", "interrupted", map[string]interface{}{"command": "./next-change"})
			agent.complete(t, agent.call(t, "doops_shell", "interrupted"), "")
			lifecycleResponse(t, probe, 2, "")
			// The session workspace stays fenced until the canceled call reaches a terminal state.
			lifecycleSend(t, probe, 5, "doops_file_write", "interrupted", map[string]interface{}{"path": "config.txt", "content": "x"})
			lifecycleResponse(t, probe, 5, "target busy")
			lifecycleSend(t, probe, 4, "doops_shell", "independent", map[string]interface{}{"command": "./independent-change"})
			agent.complete(t, agent.call(t, "doops_shell", "independent"), "")
			lifecycleResponse(t, probe, 4, "")
			if owned := f.operation(t, "interrupted", "cancel_requested"); owned.ID != op.ID || owned.AuditID != op.AuditID {
				t.Fatalf("cancel ACK released original ownership: %#v", owned)
			}
			agent.complete(t, call, "operation canceled")
			lifecycleResponse(t, client, 2, "execution_outcome_unknown")
			requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
			unresolved, err := f.store.UnresolvedExecutions("dev", "local")
			if err != nil || len(unresolved) != 1 || unresolved[0].ID != op.AuditID {
				t.Fatalf("local exit cleared remote uncertainty: %#v, %v", unresolved, err)
			}
			before := executionAuditTestEvent(t, f.store, op.AuditID)
			lifecycleSend(t, probe, 3, "doops_shell", "takeover", map[string]interface{}{"command": "./next-change"})
			agent.complete(t, agent.call(t, "doops_shell", "takeover"), "")
			lifecycleResponse(t, probe, 3, "")
			if got := executionAuditTestEvent(t, f.store, op.AuditID); !reflect.DeepEqual(got, before) {
				t.Fatalf("follow-up execution rewrote unknown audit: %#v", got)
			}
		})
	}
}

func TestExecutionGuardRemoteFailureNeedsResultEvidence(t *testing.T) {
	response := map[string]interface{}{
		"result": map[string]interface{}{"isError": true, "content": []interface{}{}},
	}
	for _, tool := range []string{"doops_ssh", "doops_agent_prompt", "doops_k8s"} {
		args := json.RawMessage(`{"operation":"apply"}`)
		if remoteMutationFailed(tool, args, response) {
			t.Fatalf("%s terminal tool failure was treated as unknown outcome", tool)
		}
		denied := map[string]interface{}{"error": map[string]interface{}{"code": float64(-32602), "message": "invalid arguments"}}
		if remoteMutationFailed(tool, args, denied) {
			t.Fatalf("%s pre-execution rejection was treated as unknown outcome", tool)
		}
	}
	if remoteMutationFailed("doops_k8s", json.RawMessage(`{"operation":"GET"}`), response) {
		t.Fatal("read-only Kubernetes failure was treated as mutation uncertainty")
	}
	transportFailure := map[string]interface{}{"error": map[string]interface{}{"code": float64(-32000), "message": "agent disconnected"}}
	if !remoteMutationFailed("doops_ssh", json.RawMessage(`{"command":["./change-production"]}`), transportFailure) {
		t.Fatal("transport failure for a mutation was not treated as unknown outcome")
	}
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "lost-response")
	client := f.client(t, f.admin)
	lifecycleSend(t, client, 2, "doops_k8s", "failed-write", map[string]interface{}{
		"operation": "scale", "resource": "deployment/app", "replicas": 2,
	})
	agent.complete(t, agent.call(t, "doops_k8s", "failed-write"), "connection closed after request")
	lifecycleResponse(t, client, 2, "execution_outcome_unknown")
	requireEventually(t, time.Second, func() bool {
		events, err := f.store.UnresolvedExecutions("dev", "local")
		return err == nil && len(events) == 1 && events[0].Status == "outcome_unknown"
	})
}
