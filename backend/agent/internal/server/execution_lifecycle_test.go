package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/api"
)

type executionLifecycleFixture struct {
	path        string
	store       *GatewayStore
	hub         *GatewayHub
	server      *httptest.Server
	admin       CreatedToken
	opts        GatewayHubOptions
	httpTimeout time.Duration
}

func newExecutionLifecycleFixture(t *testing.T, opts GatewayHubOptions) *executionLifecycleFixture {
	t.Helper()
	f := &executionLifecycleFixture{path: filepath.Join(t.TempDir(), "gateway.db"), opts: opts}
	f.open(t)
	f.admin = f.adminUser(t, "lifecycle-admin")
	return f
}

func (f *executionLifecycleFixture) open(t *testing.T) {
	t.Helper()
	store, err := OpenGatewayStore(f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	f.store = store
	hub := NewGatewayHub(store, f.opts)
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	server := httptest.NewServer(mux)
	t.Cleanup(func() {
		server.Close()
		requireEventually(t, 3*time.Second, func() bool { return len(hub.listActiveOperations()) == 0 })
	})
	f.hub, f.server = hub, server
}

func (f *executionLifecycleFixture) user(t *testing.T, name string, grant ScopeGrant) CreatedToken {
	t.Helper()
	user, err := f.store.CreateUser(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.GrantUser(user.ID, grant); err != nil {
		t.Fatal(err)
	}
	token, err := f.store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: name})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *executionLifecycleFixture) adminUser(t *testing.T, name string) CreatedToken {
	t.Helper()
	token := f.user(t, name, ScopeGrant{Cluster: "*", Instance: "*"})
	auth, err := f.store.VerifyUserToken(token.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetUserAdmin(auth.UserID, true); err != nil {
		t.Fatal(err)
	}
	return token
}

func (f *executionLifecycleFixture) client(t *testing.T, token CreatedToken) *websocket.Conn {
	t.Helper()
	conn := dialGatewayRPCClient(t, f.server.URL, token.Plaintext, "dev", "local")
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func (f *executionLifecycleFixture) http(t *testing.T, token CreatedToken, method, path, body string, want int) []byte {
	t.Helper()
	req, err := http.NewRequest(method, f.server.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token.Plaintext)
	req.Header.Set("Content-Type", "application/json")
	timeout := f.httpTimeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != want {
		t.Fatalf("%s %s: HTTP %d, want %d: %s", method, path, resp.StatusCode, want, data)
	}
	return data
}

// cancel requests cancellation the way a client's tools/cancel does: it marks the
// operation cancel_requested and cancels its context, without claiming it stopped.
func (f *executionLifecycleFixture) cancel(t *testing.T, id string) {
	t.Helper()
	if !f.hub.cancelActiveOperation(id) {
		t.Fatalf("operation %s not found for cancellation", id)
	}
}

func (f *executionLifecycleFixture) operation(t *testing.T, session, state string) GatewayActiveOperation {
	t.Helper()
	var found GatewayActiveOperation
	requireEventually(t, 3*time.Second, func() bool {
		for _, op := range f.hub.listActiveOperations() {
			if op.Session == session && op.State == state {
				found = op
				return true
			}
		}
		return false
	})
	return found
}

func TestContextMutexAcquireHonorsCancellation(t *testing.T) {
	mutex := newContextMutex()
	if err := mutex.Acquire(context.Background()); err != nil {
		t.Fatalf("initial acquire: %v", err)
	}
	defer mutex.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := mutex.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire = %v, want context.Canceled", err)
	}
}

type executionLifecycleCall struct {
	id      int64
	name    string
	session string
}

type executionLifecycleAgent struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	calls   chan executionLifecycleCall
	cancels chan int64
}

func (f *executionLifecycleFixture) agent(t *testing.T, runtime string) *executionLifecycleAgent {
	t.Helper()
	initialize := gatewayTestInitializeResult(runtime)
	initialize["capabilities"].(map[string]interface{})["protocols"].(map[string]interface{})[executionLifecycleProtocol] = executionLifecycleCapability()
	return f.agentWithInitialize(t, initialize)
}

func (f *executionLifecycleFixture) agentWithInitialize(t *testing.T, initialize map[string]interface{}) *executionLifecycleAgent {
	t.Helper()
	previous := f.hub.getAgent("dev", "local")
	agent := &executionLifecycleAgent{
		conn:    dialFakeGatewayAgent(t, f.server.URL, "dev", "local"),
		calls:   make(chan executionLifecycleCall, 16),
		cancels: make(chan int64, 16),
	}
	done := make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() {
		close(stop)
		_ = agent.conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("controlled agent did not stop")
		}
	})
	go func() {
		defer close(done)
		for {
			var msg map[string]interface{}
			if err := agent.conn.ReadJSON(&msg); err != nil {
				return
			}
			switch msg["method"] {
			case "initialize":
				if err := agent.write(map[string]interface{}{"jsonrpc": "2.0", "id": msg["id"], "result": initialize}); err != nil {
					return
				}
			case "tools/call":
				id, _ := numericID(msg["id"])
				params, _ := msg["params"].(map[string]interface{})
				args, _ := params["arguments"].(map[string]interface{})
				name, _ := params["name"].(string)
				session, _ := args["session_id"].(string)
				select {
				case agent.calls <- executionLifecycleCall{id: id, name: name, session: session}:
				case <-stop:
					return
				}
			case "tools/cancel":
				params, _ := msg["params"].(map[string]interface{})
				id, _ := numericID(params["id"])
				if msg["id"] != fmt.Sprintf("cancel-%d", id) {
					return
				}
				if err := agent.write(map[string]interface{}{
					"jsonrpc": "2.0", "id": msg["id"], "result": map[string]interface{}{"canceled": true},
				}); err != nil {
					return
				}
				select {
				case agent.cancels <- id:
				case <-stop:
					return
				}
			}
		}
	}()
	requireEventually(t, 3*time.Second, func() bool {
		current := f.hub.getAgent("dev", "local")
		return current != nil && current != previous
	})
	return agent
}

func (a *executionLifecycleAgent) write(msg map[string]interface{}) error {
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	return a.conn.WriteJSON(msg)
}

func (a *executionLifecycleAgent) call(t *testing.T, name, session string) executionLifecycleCall {
	t.Helper()
	select {
	case call := <-a.calls:
		if call.name != name || call.session != session {
			t.Fatalf("dispatched %s/%s, want %s/%s", call.name, call.session, name, session)
		}
		return call
	case <-time.After(3 * time.Second):
		t.Fatalf("%s/%s did not reach the agent", name, session)
		return executionLifecycleCall{}
	}
}

func (a *executionLifecycleAgent) complete(t *testing.T, call executionLifecycleCall, failure string) {
	t.Helper()
	msg := map[string]interface{}{"jsonrpc": "2.0", "id": call.id}
	if failure == "" {
		msg["result"] = map[string]interface{}{"content": []map[string]interface{}{{"type": "text", "text": "completed"}}}
	} else {
		msg["error"] = map[string]interface{}{"code": -32800, "message": failure}
	}
	if err := a.write(msg); err != nil {
		t.Fatal(err)
	}
}

func lifecycleSend(t *testing.T, client *websocket.Conn, id int, tool, session string, args map[string]interface{}) {
	t.Helper()
	args["session_id"] = session
	writeGatewayJSON(t, client, map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]interface{}{"name": tool, "arguments": args},
	})
}

func lifecycleResponse(t *testing.T, client *websocket.Conn, id int, wantError string) map[string]interface{} {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	defer client.SetReadDeadline(time.Time{})
	msg := readUntilID(t, client, float64(id))
	if wantError == "" {
		if msg["error"] != nil || msg["result"] == nil {
			t.Fatalf("expected terminal success, got %#v", msg)
		}
	} else {
		rpcError, ok := msg["error"].(map[string]interface{})
		if !ok || !strings.Contains(fmt.Sprint(rpcError["message"]), wantError) {
			t.Fatalf("expected error %q, got %#v", wantError, msg)
		}
	}
	return msg
}

func TestExecutionLifecycleBackgroundCallsUseGatewayTerminalState(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "background-agent")
	client := f.client(t, f.admin)
	lifecycleSend(t, client, 2, "doops_bg", "background", map[string]interface{}{"command": "printf test"})
	agent.complete(t, agent.call(t, "doops_bg", "background"), "")
	lifecycleResponse(t, client, 2, "")
}

func TestExecutionLifecycleRPCReconnectFenceBlocksReplacementDispatch(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	oldAgent := f.agent(t, "reconnect-rpc-old")
	oldOperation := f.hub.client.registerActiveOperation(GatewayActiveOperation{
		Cluster: "dev", Instance: "local", Action: ActionExec,
		Session: "reconnect-rpc", Resource: "target-mutation:dev/local",
	}, func() {})
	f.hub.client.markOperationRunning(oldOperation)

	replacement := f.agent(t, "reconnect-rpc-new")
	client := f.client(t, f.admin)
	lifecycleSend(t, client, 2, "doops_file_write", "reconnect-rpc", map[string]interface{}{"path": "config.txt", "content": "x"})
	lifecycleResponse(t, client, 2, "active operation")
	select {
	case call := <-replacement.calls:
		t.Fatalf("replacement Agent received fenced workspace dispatch: %#v", call)
	default:
	}
	// Shell calls in the same session are not fenced by each other.
	lifecycleSend(t, client, 3, "doops_shell", "reconnect-rpc", map[string]interface{}{"command": "true"})
	replacement.complete(t, replacement.call(t, "doops_shell", "reconnect-rpc"), "")
	lifecycleResponse(t, client, 3, "")
	if oldAgent == replacement {
		t.Fatal("replacement fixture did not create a new Agent connection")
	}
	f.hub.client.finishActiveOperation(oldOperation)
}

func TestExecutionLifecycleBackgroundRequiresCapabilityBeforeDispatch(t *testing.T) {
	for name, capability := range invalidExecutionLifecycleCapabilities() {
		t.Run(name, func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			initialize := gatewayTestInitializeResult("legacy-background-" + name)
			if capability != nil {
				initialize["capabilities"].(map[string]interface{})["protocols"].(map[string]interface{})[executionLifecycleProtocol] = capability
			}
			agent := f.agentWithInitialize(t, initialize)
			client := f.client(t, f.admin)
			session := "legacy-background-" + name
			lifecycleSend(t, client, 2, "doops_bg", session, map[string]interface{}{"command": "printf test"})
			lifecycleResponse(t, client, 2, "does not advertise protocol")
			select {
			case call := <-agent.calls:
				t.Fatalf("background call reached legacy Agent: %#v", call)
			default:
			}
			if operations := f.hub.listActiveOperations(); len(operations) != 0 {
				t.Fatalf("rejected background call retained operations: %#v", operations)
			}
			events, err := f.store.ListAuditFiltered(AuditFilter{Session: session})
			if err != nil || len(events) != 1 || events[0].Status != "blocked" || events[0].EndedAt.IsZero() {
				t.Fatalf("rejected background audit = %#v, %v", events, err)
			}
			if events, err := f.store.UnresolvedExecutions("dev", "local"); err != nil || len(events) != 0 {
				t.Fatalf("rejected background call created execution blocker: %#v, %v", events, err)
			}
			lifecycleSend(t, client, 3, "doops_shell", "after-background-rejection-"+name, map[string]interface{}{"command": "printf test"})
			agent.complete(t, agent.call(t, "doops_shell", "after-background-rejection-"+name), "")
			lifecycleResponse(t, client, 3, "")
		})
	}
}

func TestExecutionLifecycleInternalCapabilityRejectionRetriesAuditWithoutOwnerGap(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agentWithInitialize(t, gatewayTestInitializeResult("legacy-internal-retry"))
	if _, err := f.store.db.Exec(`CREATE TRIGGER fail_internal_capability_audit
		BEFORE UPDATE ON audit_events
		WHEN OLD.session = 'legacy-internal-retry' AND NEW.status = 'blocked'
		BEGIN SELECT RAISE(FAIL, 'synthetic blocked audit outage'); END`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.store.db.Exec("DROP TRIGGER IF EXISTS fail_internal_capability_audit")
		requireEventually(t, 4*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	})

	_, err := f.hub.RunInternalToolCall(context.Background(), "dev", "local", "doops_bg", map[string]interface{}{
		"session_id": "legacy-internal-retry", "command": "printf test",
	})
	if err == nil || !strings.Contains(err.Error(), "execution_audit_completion_failed") {
		t.Fatalf("capability rejection did not surface audit failure: %v", err)
	}
	select {
	case call := <-agent.calls:
		t.Fatalf("rejected internal background reached legacy Agent: %#v", call)
	default:
	}
	requireEventually(t, time.Second, func() bool {
		operations := f.hub.listActiveOperations()
		return len(operations) == 1 && operations[0].State == "finalizing"
	})
	if _, err := f.store.db.Exec("DROP TRIGGER fail_internal_capability_audit"); err != nil {
		t.Fatal(err)
	}
	requireEventually(t, 4*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	events, err := f.store.ListAuditFiltered(AuditFilter{Session: "legacy-internal-retry"})
	if err != nil || len(events) != 1 || events[0].Status != "blocked" || events[0].EndedAt.IsZero() {
		t.Fatalf("capability rejection audit was not recovered: %#v, %v", events, err)
	}
}

func TestExecutionLifecycleForegroundCallsUseTerminalEvidenceWithoutCapability(t *testing.T) {
	for _, test := range []struct {
		name string
		tool string
		args map[string]interface{}
	}{
		{name: "shell", tool: "doops_shell", args: map[string]interface{}{"command": "printf test"}},
		{name: "prompt", tool: "doops_agent_prompt", args: map[string]interface{}{"instruction": "return []"}},
		{name: "apply prompt", tool: "doops_agent_prompt", args: map[string]interface{}{
			"operation": "apply", "instruction": "perform the verified synthetic mutation",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			initialize := gatewayTestInitializeResult("legacy-foreground-agent")
			agent := f.agentWithInitialize(t, initialize)
			client := f.client(t, f.admin)
			lifecycleSend(t, client, 2, test.tool, "legacy-foreground", test.args)
			agent.complete(t, agent.call(t, test.tool, "legacy-foreground"), "")
			lifecycleResponse(t, client, 2, "")
		})
	}
}

func TestExecutionLifecycleDoesNotGateForegroundTools(t *testing.T) {
	for _, tool := range []string{"doops_shell", "doops_ssh", "doops_agent_prompt", "doops_k8s"} {
		f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
		initialize := gatewayTestInitializeResult("foreground-agent")
		initialize["capabilities"].(map[string]interface{})["protocols"].(map[string]interface{})[structuredSSHProtocol] = structuredSSHCapability()
		agent := f.agentWithInitialize(t, initialize)
		client := f.client(t, f.admin)
		args := map[string]interface{}{}
		if tool == "doops_shell" {
			args["command"] = "printf test"
		} else if tool == "doops_ssh" {
			args["host"] = "root@example.com"
			args["command"] = []string{"true"}
		} else if tool == "doops_agent_prompt" {
			args["instruction"] = "return []"
		} else {
			args["operation"] = "get"
		}
		lifecycleSend(t, client, 2, tool, "unified-"+tool, args)
		agent.complete(t, agent.call(t, tool, "unified-"+tool), "")
		lifecycleResponse(t, client, 2, "")
	}
}

func TestExecutionLifecycleInternalBackgroundRequiresCapability(t *testing.T) {
	for name, capability := range invalidExecutionLifecycleCapabilities() {
		t.Run(name, func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			initialize := gatewayTestInitializeResult("legacy-internal-background")
			if capability != nil {
				initialize["capabilities"].(map[string]interface{})["protocols"].(map[string]interface{})[executionLifecycleProtocol] = capability
			}
			agent := f.agentWithInitialize(t, initialize)
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_, err := f.hub.RunInternalToolCall(ctx, "dev", "local", "doops_bg", map[string]interface{}{
				"session_id": "legacy-internal-background",
				"command":    "printf test",
			})
			if err == nil || !strings.Contains(err.Error(), "does not advertise protocol") {
				t.Fatalf("legacy internal background call was not rejected: %v", err)
			}
			select {
			case call := <-agent.calls:
				t.Fatalf("internal background call reached legacy Agent: %#v", call)
			default:
			}
			if operations := f.hub.listActiveOperations(); len(operations) != 0 {
				t.Fatalf("rejected internal background retained operations: %#v", operations)
			}
			events, err := f.store.ListAuditFiltered(AuditFilter{Session: "legacy-internal-background"})
			if err != nil || len(events) != 1 || events[0].Status != "blocked" || events[0].EndedAt.IsZero() {
				t.Fatalf("rejected internal background audit = %#v, %v", events, err)
			}
		})
	}
}

func TestExecutionLifecycleInternalForegroundCallUsesTerminalEvidenceWithoutCapability(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	initialize := gatewayTestInitializeResult("legacy-internal-agent")
	agent := f.agentWithInitialize(t, initialize)

	result := make(chan struct {
		output string
		err    error
	}, 1)
	go func() {
		output, err := f.hub.RunInternalToolCall(context.Background(), "dev", "local", "doops_agent_prompt", map[string]interface{}{
			"session_id":  "legacy-internal",
			"instruction": "return []",
		})
		result <- struct {
			output string
			err    error
		}{output: output, err: err}
	}()
	agent.complete(t, agent.call(t, "doops_agent_prompt", "legacy-internal"), "")
	select {
	case got := <-result:
		if got.err != nil || got.output != "completed" {
			t.Fatalf("internal foreground call failed: output=%q err=%v", got.output, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("internal foreground call did not finish")
	}
}

func TestExecutionLifecycleWaitingBlocksOnlyTheContendedResource(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{
		MaxQueuedPerTarget: 1, TargetQueueTimeout: 5 * time.Second,
	})
	agent := f.agent(t, "quota-agent")
	waiter := f.user(t, "waiter", ScopeGrant{Cluster: "dev", Instance: "local"})
	holderClient, queuedClient, independentClient := f.client(t, f.admin), f.client(t, waiter), f.client(t, waiter)
	lifecycleSend(t, holderClient, 2, "doops_file_write", "occupied", map[string]interface{}{"path": "config.txt", "content": "first"})
	holderCall := agent.call(t, "doops_file_write", "occupied")
	holder := f.operation(t, "occupied", "running")
	lifecycleSend(t, queuedClient, 2, "doops_file_write", "occupied", map[string]interface{}{"path": "config.txt", "content": "second"})
	queued := f.operation(t, "occupied", "waiting")
	if len(queued.BlockedBy) != 1 || queued.BlockedBy[0] != holder.ID || queued.Resource != "workspace:occupied" {
		t.Fatalf("queued ownership is not visible: %#v", queued)
	}
	lifecycleSend(t, independentClient, 2, "doops_file_write", "independent", map[string]interface{}{"path": "config.txt", "content": "test"})
	independent := agent.call(t, "doops_file_write", "independent")
	agent.complete(t, independent, "")
	lifecycleResponse(t, independentClient, 2, "")
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 2 })
	agent.complete(t, holderCall, "")
	lifecycleResponse(t, holderClient, 2, "")
	queuedCall := agent.call(t, "doops_file_write", "occupied")
	agent.complete(t, queuedCall, "")
	lifecycleResponse(t, queuedClient, 2, "")
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
}

func TestExecutionLifecycleStructuredWorkspacesExcludeSameAndAllowDifferent(t *testing.T) {
	for _, contenderTool := range []string{"doops_file_read", "doops_file_write"} {
		t.Run(contenderTool, func(t *testing.T) {
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			agent := f.agent(t, "workspace-agent")
			holderClient, contender := f.client(t, f.admin), f.client(t, f.admin)
			lifecycleSend(t, holderClient, 2, "doops_file_write", "workspace-a", map[string]interface{}{"path": "config.txt", "content": "a"})
			holder := agent.call(t, "doops_file_write", "workspace-a")
			lifecycleSend(t, contender, 2, contenderTool, "workspace-a", map[string]interface{}{"path": "config.txt", "content": "b"})
			resourceCount := 2
			if contenderTool == "doops_file_read" {
				read := agent.call(t, contenderTool, "workspace-a")
				agent.complete(t, read, "")
				lifecycleResponse(t, contender, 2, "")
				resourceCount = 1
				requireEventually(t, 3*time.Second, func() bool { return f.hub.getAgent("dev", "local").snapshot().ActiveOps == 1 })
			} else {
				lifecycleResponse(t, contender, 2, "target busy")
			}
			lifecycleSend(t, contender, 3, contenderTool, "workspace-b", map[string]interface{}{"path": "config.txt", "content": "b"})
			parallel := agent.call(t, contenderTool, "workspace-b")
			if state := f.hub.getAgent("dev", "local").snapshot(); state.ActiveOps != 2 || len(state.Resources) != resourceCount {
				t.Fatalf("workspace mutation/shared-read ownership is incorrect: %#v", state)
			}
			agent.complete(t, parallel, "")
			lifecycleResponse(t, contender, 3, "")
			agent.complete(t, holder, "")
			lifecycleResponse(t, holderClient, 2, "")
		})
	}
}

func TestExecutionLifecycleTargetMutationAllowsConcurrentSessionsAndQueries(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "exclusive-agent")
	holderClient, other := f.client(t, f.admin), f.client(t, f.admin)
	lifecycleSend(t, holderClient, 2, "doops_shell", "mutation", map[string]interface{}{"command": "kubectl apply -f test.yaml"})
	mutation := agent.call(t, "doops_shell", "mutation")
	lifecycleSend(t, other, 2, "doops_shell", "other-mutation", map[string]interface{}{"command": "kubectl apply -f other.yaml"})
	otherMutation := agent.call(t, "doops_shell", "other-mutation")
	lifecycleSend(t, other, 3, "doops_node_info", "query", map[string]interface{}{})
	query := agent.call(t, "doops_node_info", "query")
	agent.complete(t, query, "")
	lifecycleResponse(t, other, 3, "")
	agent.complete(t, otherMutation, "")
	lifecycleResponse(t, other, 2, "")
	agent.complete(t, mutation, "")
	lifecycleResponse(t, holderClient, 2, "")
}

func TestExecutionLifecycleTargetMutationDoesNotQueueBehindDifferentSessionWorkspace(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{
		MaxQueuedPerTarget: 1,
		TargetQueueTimeout: 5 * time.Second,
	})
	agent := f.agent(t, "queued-mutation-agent")
	holderClient, mutationClient := f.client(t, f.admin), f.client(t, f.admin)

	lifecycleSend(t, holderClient, 2, "doops_file_write", "workspace-holder", map[string]interface{}{
		"path": "config.txt", "content": "held",
	})
	holder := agent.call(t, "doops_file_write", "workspace-holder")

	lifecycleSend(t, mutationClient, 2, "doops_shell", "queued-mutation", map[string]interface{}{
		"command": "kubectl apply -f release.yaml",
	})
	mutation := agent.call(t, "doops_shell", "queued-mutation")
	if state := f.hub.getAgent("dev", "local").snapshot(); state.ActiveOps != 2 {
		t.Fatalf("different session target mutation unexpectedly blocked by workspace: %#v", state)
	}

	agent.complete(t, mutation, "")
	lifecycleResponse(t, mutationClient, 2, "")
	agent.complete(t, holder, "")
	lifecycleResponse(t, holderClient, 2, "")
	requireEventually(t, 3*time.Second, func() bool {
		return len(f.hub.listActiveOperations()) == 0
	})
}

func TestExecutionLifecycleAgentUpgradeIsExclusive(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "upgrade-agent")
	holderClient, other := f.client(t, f.admin), f.client(t, f.admin)
	lifecycleSend(t, holderClient, 2, "doops_file_write", "workspace", map[string]interface{}{"path": "config.txt", "content": "test"})
	write := agent.call(t, "doops_file_write", "workspace")
	upgradeArgs := func() map[string]interface{} {
		return map[string]interface{}{"image": "registry.example/doops@sha256:" + strings.Repeat("a", 64), "mode": "k8s", "dry_run": true}
	}
	lifecycleSend(t, other, 4, "doops_agent_upgrade", "upgrade", upgradeArgs())
	lifecycleResponse(t, other, 4, "target busy")
	agent.complete(t, write, "")
	lifecycleResponse(t, holderClient, 2, "")
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	lifecycleSend(t, holderClient, 3, "doops_agent_upgrade", "upgrade", upgradeArgs())
	upgrade := agent.call(t, "doops_agent_upgrade", "upgrade")
	lifecycleSend(t, other, 5, "doops_node_info", "query", map[string]interface{}{})
	lifecycleResponse(t, other, 5, "target busy")
	lifecycleSend(t, other, 6, "doops_file_write", "independent", map[string]interface{}{"path": "config.txt", "content": "test"})
	lifecycleResponse(t, other, 6, "target busy")
	agent.complete(t, upgrade, "")
	lifecycleResponse(t, holderClient, 3, "")
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	lifecycleSend(t, other, 7, "doops_node_info", "query", map[string]interface{}{})
	agent.complete(t, agent.call(t, "doops_node_info", "query"), "")
	lifecycleResponse(t, other, 7, "")
}

func TestExecutionLifecycleCancelAckRetainsOwnershipUntilOriginalTerminal(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "cancel-agent")
	client, probe := f.client(t, f.admin), f.client(t, f.admin)
	lifecycleSend(t, client, 2, "doops_file_write", "cancel-owner", map[string]interface{}{"path": "config.txt", "content": "first"})
	call := agent.call(t, "doops_file_write", "cancel-owner")
	op := f.operation(t, "cancel-owner", "running")
	writeGatewayJSON(t, client, map[string]interface{}{
		"jsonrpc": "2.0", "id": 9, "method": "tools/cancel", "params": map[string]interface{}{"id": 2},
	})
	if accepted := lifecycleResponse(t, client, 9, ""); accepted["result"].(map[string]interface{})["canceled"] != true {
		t.Fatalf("own cancellation was not acknowledged: %#v", accepted)
	}
	select {
	case id := <-agent.cancels:
		if id != call.id {
			t.Fatalf("canceled %d, want original request %d", id, call.id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("agent never received cancellation")
	}
	// A subsequent reply on the same agent connection proves its ACK was consumed.
	lifecycleSend(t, probe, 2, "doops_node_info", "barrier", map[string]interface{}{})
	agent.complete(t, agent.call(t, "doops_node_info", "barrier"), "")
	lifecycleResponse(t, probe, 2, "")
	owned := f.operation(t, "cancel-owner", "running")
	if owned.ID != op.ID || owned.AuditID != op.AuditID {
		t.Fatalf("ACK replaced original ownership: %#v", owned)
	}
	events, err := f.store.UnresolvedExecutions("dev", "local")
	if err != nil || len(events) != 1 || events[0].ID != op.AuditID || events[0].Status != "executing" {
		t.Fatalf("ACK retired execution audit: %#v, %v", events, err)
	}
	lifecycleSend(t, probe, 3, "doops_file_write", "cancel-owner", map[string]interface{}{"path": "config.txt", "content": "blocked"})
	lifecycleResponse(t, probe, 3, "target busy")
	agent.complete(t, call, "operation canceled")
	lifecycleResponse(t, client, 2, "operation canceled")
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	lifecycleSend(t, probe, 4, "doops_file_write", "cancel-owner", map[string]interface{}{"path": "config.txt", "content": "released"})
	agent.complete(t, agent.call(t, "doops_file_write", "cancel-owner"), "")
	lifecycleResponse(t, probe, 4, "")
}

func TestExecutionLifecycleDisconnectPersistsAcrossReconnectAndRestart(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := f.agent(t, "lost-agent")
	client := f.client(t, f.admin)
	lifecycleSend(t, client, 2, "doops_shell", "lost-owner", map[string]interface{}{"command": "sleep 1"})
	agent.call(t, "doops_shell", "lost-owner")
	op := f.operation(t, "lost-owner", "running")
	// No terminal reply is sent; closing the connection avoids the 30-second grace.
	_ = agent.conn.Close()
	lifecycleResponse(t, client, 2, "execution_outcome_unknown")
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	unknownBefore := executionAuditTestEvent(t, f.store, op.AuditID)
	replacement := f.agent(t, "replacement-agent")
	reconnected := f.client(t, f.admin)
	lifecycleSend(t, reconnected, 2, "doops_file_write", "different-workspace", map[string]interface{}{"path": "config.txt", "content": "recovered"})
	replacement.complete(t, replacement.call(t, "doops_file_write", "different-workspace"), "")
	lifecycleResponse(t, reconnected, 2, "")
	lifecycleSend(t, reconnected, 3, "doops_node_info", "safe-query", map[string]interface{}{})
	replacement.complete(t, replacement.call(t, "doops_node_info", "safe-query"), "")
	lifecycleResponse(t, reconnected, 3, "")
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	_ = client.Close()
	_ = reconnected.Close()
	_ = replacement.conn.Close()
	requireEventually(t, 3*time.Second, func() bool { return f.hub.getAgent("dev", "local") == nil })
	f.server.Close()
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	restarted := f.agent(t, "restart-agent")
	afterRestart := f.client(t, f.admin)
	lifecycleSend(t, afterRestart, 2, "doops_shell", "new-workspace", map[string]interface{}{"command": "printf recovered"})
	restarted.complete(t, restarted.call(t, "doops_shell", "new-workspace"), "")
	lifecycleResponse(t, afterRestart, 2, "")
	events, err := f.store.UnresolvedExecutions("dev", "local")
	if err != nil || len(events) != 1 || events[0].ID != op.AuditID || events[0].Status != "outcome_unknown" || events[0].EndedAt.IsZero() {
		t.Fatalf("unknown outcome did not survive restart: %#v, %v", events, err)
	}
	if got := executionAuditTestEvent(t, f.store, op.AuditID); !reflect.DeepEqual(got, unknownBefore) {
		t.Fatalf("reconnect, restart or follow-up execution rewrote unknown audit: %#v", got)
	}
	// No Gateway route can declare an unknown outcome resolved.
	body := fmt.Sprintf(`{"id":"audit-%d","confirm_stopped":true}`, op.AuditID)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		f.http(t, f.admin, method, "/v1/admin/operations?cluster=dev&instance=local", body, http.StatusNotFound)
	}
	if got := executionAuditTestEvent(t, f.store, op.AuditID); !reflect.DeepEqual(got, unknownBefore) {
		t.Fatalf("removed operations route changed unknown audit: %#v", got)
	}
	lifecycleSend(t, afterRestart, 3, "doops_shell", "new-workspace", map[string]interface{}{"command": "printf next"})
	restarted.complete(t, restarted.call(t, "doops_shell", "new-workspace"), "")
	lifecycleResponse(t, afterRestart, 3, "")
	lifecycleSend(t, afterRestart, 4, "doops_node_info", "safe-query", map[string]interface{}{})
	restarted.complete(t, restarted.call(t, "doops_node_info", "safe-query"), "")
	lifecycleResponse(t, afterRestart, 4, "")
	resolutions, err := f.store.ListAuditFiltered(AuditFilter{Action: GatewayAction("execution:resolve"), Limit: 10})
	if err != nil || len(resolutions) != 0 {
		t.Fatalf("human declaration appended a resolution audit: %#v, %v", resolutions, err)
	}
}

func TestExecutionLifecycleReplacementDoesNotInheritUnknownOutcome(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	old := f.agent(t, "old-generation")
	oldGeneration := f.hub.getAgent("dev", "local").Generation
	auth, err := f.store.VerifyUserToken(f.admin.Plaintext)
	if err != nil {
		t.Fatal(err)
	}
	responseReached := make(chan []byte, 1)
	releaseResponse := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseResponse) })
	forward := func(raw []byte) error {
		responseReached <- append([]byte(nil), raw...)
		<-releaseResponse
		return nil
	}
	done := make(chan error, 1)
	// Stall delivery of the old call's failure so the response path is still
	// observable after registry replacement. The persisted unknown outcome must
	// not block admission on the replacement Agent after resource cleanup.
	go func() {
		done <- f.hub.client.handleGatewayToolCall(context.Background(), auth, "dev", "local", api.JSONRPCRequest{
			JSONRPC: "2.0", ID: 2, Method: "tools/call",
			Params: json.RawMessage(`{"name":"doops_shell","arguments":{"session_id":"old-owner","command":"sleep 1"}}`),
		}, forward, func(value interface{}) error {
			raw, err := json.Marshal(value)
			if err != nil {
				return err
			}
			return forward(raw)
		})
	}()
	old.call(t, "doops_shell", "old-owner")
	owner := f.operation(t, "old-owner", "running")
	replacement := f.agent(t, "new-generation")
	if generation := f.hub.getAgent("dev", "local").Generation; generation <= oldGeneration {
		t.Fatalf("agent generation did not advance: %d -> %d", oldGeneration, generation)
	}
	select {
	case raw := <-responseReached:
		if !strings.Contains(string(raw), "execution_outcome_unknown") {
			t.Fatalf("old generation produced a false terminal outcome: %s", raw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("replacement did not interrupt the old transport")
	}
	events, err := f.store.UnresolvedExecutions("dev", "local")
	if err != nil || len(events) != 1 || events[0].ID != owner.AuditID || events[0].Status != "outcome_unknown" {
		t.Fatalf("replacement must retain the durable unknown outcome: %#v, %v", events, err)
	}
	before := executionAuditTestEvent(t, f.store, owner.AuditID)
	client := f.client(t, f.admin)
	lifecycleSend(t, client, 2, "doops_file_write", "new-owner", map[string]interface{}{"path": "config.txt", "content": "recovered"})
	replacement.complete(t, replacement.call(t, "doops_file_write", "new-owner"), "")
	lifecycleResponse(t, client, 2, "")
	releaseOnce.Do(func() { close(releaseResponse) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("old operation failed to leave after response was released")
	}
	requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
	lifecycleSend(t, client, 3, "doops_file_write", "new-owner", map[string]interface{}{"path": "config.txt", "content": "next"})
	replacement.complete(t, replacement.call(t, "doops_file_write", "new-owner"), "")
	lifecycleResponse(t, client, 3, "")
	if got := executionAuditTestEvent(t, f.store, owner.AuditID); !reflect.DeepEqual(got, before) {
		t.Fatalf("replacement execution rewrote old generation uncertainty: %#v", got)
	}
}
