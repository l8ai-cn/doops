package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

// Low-level fixtures explicitly release a lease; production finalizers own it.
func (a *GatewayAgent) acquireForAction(ctx context.Context, action GatewayAction, resourceKey string, maxQueued int, wait time.Duration) error {
	_, err := a.acquireActionLease(ctx, action, resourceKey, maxQueued, wait)
	return err
}

func (a *GatewayAgent) releaseForAction(action GatewayAction, resourceKey string) {
	switch {
	case isExclusiveOperation(action, resourceKey):
		a.releaseExclusive(resourceKey)
	case isConcurrentQuery(action, resourceKey):
		a.releaseReadOnly()
	case resourceKey != "":
		a.releaseResource(resourceKey)
	case isConcurrentReadOnlyAction(action):
		a.releaseReadOnly()
	default:
		a.releaseExclusive("target:" + a.Cluster + "/" + a.Instance)
	}
}

func TestSessionLeaseCannotEnterDuringAgentUpgrade(t *testing.T) {
	agent := &GatewayAgent{}
	upgrade, err := agent.acquireActionLeaseForSession(context.Background(), ActionAgentUpgrade, "target:dev/node", "upgrade", 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer upgrade.Release()
	for _, request := range []struct {
		action   GatewayAction
		resource string
	}{
		{ActionExec, "target-mutation:dev/node"},
		{ActionWrite, "workspace:release"},
		{ActionInfo, "query:node"},
		{ActionRead, "workspace:release"},
	} {
		lease, err := agent.acquireActionLeaseForSession(context.Background(), request.action, request.resource, "release", 0, time.Second)
		if err == nil {
			lease.Release()
			t.Fatalf("%s entered during Agent upgrade", request.action)
		}
	}
	upgrade.Release()
	lease, err := agent.acquireActionLeaseForSession(context.Background(), ActionExec, "target-mutation:dev/node", "release", 0, time.Second)
	if err != nil {
		t.Fatalf("upgrade left a stale lock: %v", err)
	}
	lease.Release()
	if state := agent.runtimeState(); state.activeOps != 0 || state.busy {
		t.Fatalf("release did not restore idle state: %#v", state)
	}
}

func TestSessionConflictPolicyMatchesMutationAndWorkspaceLeases(t *testing.T) {
	holder := GatewayActiveOperation{Cluster: "dev", Instance: "node", Session: "release", Action: ActionExec, Resource: "target-mutation:dev/node"}
	for _, tc := range []struct {
		name     string
		action   GatewayAction
		resource string
		session  string
		conflict bool
	}{
		{"same session shell", ActionExec, holder.Resource, "release", false},
		{"same session workspace write", ActionWrite, "workspace:release", "release", true},
		{"different mutation", ActionExec, holder.Resource, "other", false},
		{"same workspace", ActionWrite, "workspace:release", "release", true},
		{"different workspace", ActionWrite, "workspace:other", "other", false},
		{"same session nginx", ActionExec, "nginx:dev/node", "release", true},
		{"different session nginx", ActionExec, "nginx:dev/node", "website", false},
		{"clean held workspace", ActionClean, "workspace:release", "maintenance", true},
		{"read-only query", ActionInfo, "query:node", "release", false},
		{"same workspace read", ActionRead, "workspace:release", "release", false},
		{"upgrade", ActionAgentUpgrade, "target:dev/node", "upgrade", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := GatewayActiveOperation{Cluster: "dev", Instance: "node", Session: tc.session, Action: tc.action, Resource: tc.resource}
			if got := operationsConflict(candidate, holder); got != tc.conflict {
				t.Fatalf("operation conflict = %t, want %t", got, tc.conflict)
			}
			service := &ClientService{activeOps: map[string]*gatewayActiveOperation{"holder": {GatewayActiveOperation: holder}}}
			service.activeOps["holder"].State = "cancel_requested"
			if got := service.hasCancellationBarrier(candidate); got != tc.conflict {
				t.Fatalf("cancellation conflict = %t, want %t", got, tc.conflict)
			}
		})
	}
}

func TestWorkspaceReadsCanInspectAnExecutingSession(t *testing.T) {
	agent := &GatewayAgent{}
	writer, err := agent.acquireActionLeaseForSession(context.Background(), ActionExec, "target-mutation:dev/node", "release", 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Release()
	for index := 0; index < 2; index++ {
		reader, err := agent.acquireActionLeaseForSession(context.Background(), ActionRead, "workspace:release", "release", 0, time.Second)
		if err != nil {
			t.Fatalf("workspace read was blocked by session execution: %v", err)
		}
		defer reader.Release()
	}
	if write, err := agent.acquireActionLeaseForSession(context.Background(), ActionWrite, "workspace:release", "release", 0, time.Second); err == nil {
		write.Release()
		t.Fatal("workspace write entered an executing session")
	}
}

func TestActiveOperationConflictFencesSameSessionAcrossConnections(t *testing.T) {
	service := &ClientService{activeOps: map[string]*gatewayActiveOperation{}}
	oldAgent := &GatewayAgent{}
	newAgent := &GatewayAgent{}
	holder := GatewayActiveOperation{
		ID:       "old-operation",
		Cluster:  "dev",
		Instance: "node",
		Action:   ActionExec,
		Session:  "reconnect-session",
		Resource: "target-mutation:dev/node",
		State:    "running",
	}
	service.activeOps[holder.ID] = &gatewayActiveOperation{GatewayActiveOperation: holder, agent: oldAgent}

	workspace := func(session string) GatewayActiveOperation {
		return GatewayActiveOperation{
			Cluster: "dev", Instance: "node", Action: ActionWrite,
			Session: session, Resource: "workspace:" + session,
		}
	}
	if got := service.activeOperationConflict(workspace("reconnect-session"), newAgent); got != holder.ID {
		t.Fatalf("same-session workspace after reconnect was not fenced: got %q", got)
	}
	if got := service.activeOperationConflict(workspace("reconnect-session"), oldAgent); got != "" {
		t.Fatalf("same-connection operation was incorrectly fenced: %q", got)
	}
	if got := service.activeOperationConflict(workspace("other-session"), newAgent); got != "" {
		t.Fatalf("different session was incorrectly fenced: %q", got)
	}
	if got := service.activeOperationConflict(GatewayActiveOperation{
		Cluster: "dev", Instance: "node", Action: ActionExec,
		Session: "reconnect-session", Resource: "target-mutation:dev/node",
	}, newAgent); got != "" {
		t.Fatalf("same-session shell calls must not fence each other across connections: got %q", got)
	}
	service.activeOps[holder.ID].State = "finalizing"
	if got := service.activeOperationConflict(workspace("reconnect-session"), newAgent); got != "" {
		t.Fatalf("pending audit blocked a finished execution: got %q", got)
	}
}

func TestActiveOperationRegistrationBindsConnectionForQueueAndFence(t *testing.T) {
	service := &ClientService{activeOps: map[string]*gatewayActiveOperation{}}
	oldAgent := &GatewayAgent{}
	newAgent := &GatewayAgent{}
	candidate := GatewayActiveOperation{
		Cluster: "dev", Instance: "node", Action: ActionWrite,
		Session: "same-session", Resource: "workspace:same-session",
	}

	first, holderID := service.registerActiveOperationForAgent(candidate, func() {}, oldAgent)
	if holderID != "" {
		t.Fatalf("first operation unexpectedly conflicted: %q", holderID)
	}
	second, holderID := service.registerActiveOperationForAgent(candidate, func() {}, oldAgent)
	if holderID != "" {
		t.Fatalf("same-connection waiting operation was rejected: %q", holderID)
	}
	// Registrations in the same instant tie on StartedAt; pin the order so the
	// oldest-holder assertion does not depend on random operation IDs.
	service.activeOpsMu.Lock()
	service.activeOps[first].StartedAt = time.Unix(1, 0)
	service.activeOps[second].StartedAt = time.Unix(2, 0)
	service.activeOpsMu.Unlock()
	third, holderID := service.registerActiveOperationForAgent(candidate, func() {}, newAgent)
	if holderID != first {
		t.Fatalf("replacement connection holder = %q, want %q", holderID, first)
	}
	service.finishActiveOperation(third)
	service.finishActiveOperation(second)
	service.finishActiveOperation(first)
	if got := len(service.listActiveOperations()); got != 0 {
		t.Fatalf("operation registration test left active operations: %d", got)
	}
}

func TestActiveOperationConflictReportsOldestOwnerDeterministically(t *testing.T) {
	oldAgent, current := &GatewayAgent{}, &GatewayAgent{}
	candidate := GatewayActiveOperation{
		Cluster: "dev", Instance: "node", Action: ActionWrite,
		Session: "same-session", Resource: "workspace:same-session",
	}
	service := &ClientService{activeOps: map[string]*gatewayActiveOperation{}}
	for id, started := range map[string]int64{"a-new": 2, "b-old": 1, "c-old": 1, "finalized": 0, "current": 0} {
		holder := &gatewayActiveOperation{GatewayActiveOperation: candidate, agent: oldAgent}
		holder.StartedAt = time.Unix(started, 0)
		if id == "finalized" {
			holder.State = "finalizing"
		}
		if id == "current" {
			holder.agent = current
		}
		service.activeOps[id] = holder
	}
	for range 100 {
		if got := service.activeOperationConflict(candidate, current); got != "b-old" {
			t.Fatalf("oldest conflicting owner = %q, want b-old", got)
		}
		if got := service.activeOperationConflictLocked(candidate, current, "b-old"); got != "c-old" {
			t.Fatalf("oldest non-skipped owner = %q, want c-old", got)
		}
	}
}

func TestNginxLeaseProtectsItsResourceAndSessionWithoutTargetExclusion(t *testing.T) {
	agent := &GatewayAgent{}
	website, err := agent.acquireActionLeaseForSession(context.Background(), ActionExec, "nginx:dev/node", "website", 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer website.Release()
	other, err := agent.acquireActionLeaseForSession(context.Background(), ActionExec, "target-mutation:dev/node", "release", 0, time.Second)
	if err != nil {
		t.Fatalf("Nginx publish blocked an independent session: %v", err)
	}
	defer other.Release()
	for _, request := range []struct {
		resource string
		session  string
	}{
		{"nginx:dev/node", "second-website"},
		{"target-mutation:dev/node", "website"},
		{"workspace:website", "website"},
	} {
		if lease, err := agent.acquireActionLeaseForSession(context.Background(), ActionExec, request.resource, request.session, 0, time.Second); err == nil {
			lease.Release()
			t.Fatalf("conflicting lease was admitted: %#v", request)
		}
	}
	if state := agent.runtimeState(); state.busy || state.activeOps != 2 {
		t.Fatalf("Nginx lease became target-wide or double-counted: %#v", state)
	}
	website.Release()
	other.Release()
	if state := agent.runtimeState(); state.activeOps != 0 || len(state.resources) != 0 {
		t.Fatalf("release left stale leases: %#v", state)
	}
}

func TestSessionQueueAllowsOtherSessionAndReleasesAfterCancellation(t *testing.T) {
	agent := &GatewayAgent{closed: make(chan struct{})}
	// The session's workspace is the shared resource; a second writer queues behind it.
	holder, err := agent.acquireActionLeaseForSession(context.Background(), ActionWrite, "workspace:release", "release", 0, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		lease, err := agent.acquireActionLeaseForSession(ctx, ActionWrite, "workspace:release", "release", 1, time.Second)
		if lease != nil {
			lease.Release()
		}
		finished <- err
	}()
	requireEventually(t, time.Second, func() bool { return agent.runtimeState().queuedOps == 1 })
	other, err := agent.acquireActionLeaseForSession(context.Background(), ActionExec, "target-mutation:dev/node", "other", 0, time.Second)
	if err != nil {
		t.Fatalf("queued session blocked an independent session: %v", err)
	}
	other.Release()
	cancel()
	if err := <-finished; err != context.Canceled {
		t.Fatalf("queued cancellation = %v", err)
	}
	holder.Release()
	if state := agent.runtimeState(); state.activeOps != 0 || state.queuedOps != 0 || state.busy {
		t.Fatalf("cancellation left stale operation state: %#v", state)
	}
}

func TestSessionMutationRequiresExplicitSession(t *testing.T) {
	agent := &GatewayAgent{}
	if lease, err := agent.acquireActionLease(context.Background(), ActionExec, "target-mutation:dev/node", 0, time.Second); err == nil {
		lease.Release()
		t.Fatal("mutation without a session acquired a target-wide lock")
	}
	if agent.runtimeState().activeOps != 0 {
		t.Fatal("rejected request acquired a lease")
	}
}

func TestGatewaySessionExecAskQueue(t *testing.T) {
	const session = "shared-workspace"
	for _, order := range []struct{ name, holder, waiter string }{
		{"exec-first", "doops_shell", "doops_agent_prompt"},
		{"ask-first", "doops_agent_prompt", "doops_shell"},
	} {
		for _, outcome := range []string{"release", "cancel", "timeout", "disabled"} {
			t.Run(order.name+"/"+outcome, func(t *testing.T) {
				opts := GatewayHubOptions{MaxQueuedPerTarget: 1, TargetQueueTimeout: 10 * time.Second}
				if outcome == "disabled" {
					opts.MaxQueuedPerTarget = 0
				}
				if outcome == "timeout" {
					opts.TargetQueueTimeout = time.Second
				}
				f := newExecutionLifecycleFixture(t, opts)
				remote := f.agent(t, "session-queue")
				target := f.hub.getAgent("dev", "local")
				owner, waiter := f.client(t, f.admin), f.client(t, f.admin)
				args := map[string]map[string]interface{}{
					"doops_shell":        {"command": "pwd"},
					"doops_agent_prompt": {"operation": "ask", "instruction": "describe this workspace"},
				}
				lifecycleSend(t, owner, 2, order.holder, session, args[order.holder])
				held := remote.call(t, order.holder, session)
				lifecycleSend(t, waiter, 2, order.waiter, session, args[order.waiter])

				if outcome != "disabled" {
					requireEventually(t, 3*time.Second, func() bool {
						state := target.runtimeState()
						return state.activeOps == 1 && state.queuedOps == 1
					})
				}
				switch outcome {
				case "disabled":
					lifecycleResponse(t, waiter, 2, "target busy")
				case "timeout":
					// Withhold the original terminal until the Gateway's own timer fires.
					lifecycleResponse(t, waiter, 2, "target queue timeout")
				case "cancel":
					writeGatewayJSON(t, waiter, map[string]interface{}{
						"jsonrpc": "2.0", "id": "cancel", "method": "tools/cancel", "params": map[string]int{"id": 2},
					})
					if err := waiter.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
						t.Fatal(err)
					}
					// Collect both replies because their wire order is unspecified.
					replies := make(map[interface{}]map[string]interface{})
					for range 2 {
						var reply map[string]interface{}
						if err := waiter.ReadJSON(&reply); err != nil {
							t.Fatal(err)
						}
						replies[reply["id"]] = reply
					}
					gitCancellationTestAck(t, replies["cancel"], "cancel", true)
					gitCancellationTestTerminal(t, replies[float64(2)], 2, -32007, "operation canceled")
				case "release":
					lifecycleSend(t, owner, 3, order.waiter, session, args[order.waiter])
					lifecycleResponse(t, owner, 3, "target queue full")
				}
				if outcome != "release" {
					if state := target.runtimeState(); state.activeOps != 1 || state.queuedOps != 0 {
						t.Fatalf("waiter exit disturbed ownership: %#v", state)
					}
				}
				remote.complete(t, held, "")
				lifecycleResponse(t, owner, 2, "")
				if outcome == "release" {
					remote.complete(t, remote.call(t, order.waiter, session), "")
					lifecycleResponse(t, waiter, 2, "")
				}
				// A later request on the same agent connection exposes any stray dispatch.
				lifecycleSend(t, owner, 4, "doops_node_info", session, map[string]interface{}{})
				remote.complete(t, remote.call(t, "doops_node_info", session), "")
				lifecycleResponse(t, owner, 4, "")
				if state := target.runtimeState(); state.activeOps != 0 || state.queuedOps != 0 || len(state.resources) != 0 {
					t.Fatalf("queue did not drain: %#v", state)
				}
				select {
				case id := <-remote.cancels:
					t.Fatalf("Gateway sent cancel for agent request %d while canceling a waiter", id)
				default:
				}
			})
		}
	}
}

func TestGatewaySessionExecAskDuringUpgrade(t *testing.T) {
	f := newExecutionLifecycleFixture(t, GatewayHubOptions{MaxQueuedPerTarget: 1, TargetQueueTimeout: 10 * time.Second})
	remote := f.agent(t, "session-upgrade")
	owner, client := f.client(t, f.admin), f.client(t, f.admin)
	lifecycleSend(t, owner, 2, "doops_agent_upgrade", "upgrade", map[string]interface{}{
		"image": "registry.example/doops@sha256:" + strings.Repeat("a", 64), "mode": "k8s", "dry_run": true,
	})
	upgrade := remote.call(t, "doops_agent_upgrade", "upgrade")
	lifecycleSend(t, client, 2, "doops_agent_prompt", "workspace", map[string]interface{}{"instruction": "describe this workspace"})
	lifecycleResponse(t, client, 2, "target busy")
	lifecycleSend(t, client, 3, "doops_shell", "workspace", map[string]interface{}{"command": "pwd"})
	target := f.hub.getAgent("dev", "local")
	requireEventually(t, 3*time.Second, func() bool {
		state := target.runtimeState()
		return state.busy && state.activeOps == 1 && state.queuedOps == 1
	})
	remote.complete(t, upgrade, "")
	lifecycleResponse(t, owner, 2, "")
	remote.complete(t, remote.call(t, "doops_shell", "workspace"), "")
	lifecycleResponse(t, client, 3, "")
}
