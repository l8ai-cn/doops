# Gateway Responsibility Split Design

## Goal

Separate gateway registration, user-facing calls, and per-agent connection handling so an
agent restart is an explicit lifecycle transition rather than a failed user RPC.

## Scope

- Keep the existing HTTP and WebSocket endpoints unchanged.
- Keep the existing user/agent token model unchanged.
- Extend SQLite with agent session generations and durable upgrade operations.
- Move ownership out of `GatewayHub` without adding a second network service.
- Make registration freshness observable to user-call and upgrade flows.

## Component Boundaries

### AgentRegistry

`AgentRegistry` owns the live `cluster/instance -> AgentSession` mapping and the persisted
agent status. It provides:

- register and replacement of an initialized, authenticated agent session;
- unregister only when the disconnected session is still current;
- heartbeat updates and online target snapshots;
- lookup and bounded wait for a replacement session;
- target status persistence with returned errors logged rather than discarded.

The registry owns no HTTP request handling, user authorization, audit records, or WebSocket
request forwarding.

### AgentSession

`AgentSession` owns one agent WebSocket connection. It provides:

- initialization, read loop, ping loop, and close notification;
- JSON-RPC and Git request relay;
- resource locks and per-session operation state;
- a target snapshot for the registry.

An `AgentSession` does not access `GatewayStore` and does not decide user permissions.

### AgentRegistrationHandler

`AgentRegistrationHandler` authenticates an agent connection, constructs an `AgentSession`,
completes initialization and a writable registration acknowledgement, then registers it with
`AgentRegistry` and waits for the session to close. A failed candidate acknowledgement cannot
replace or disconnect the existing healthy session. The handler has no user-token
authentication and does not dispatch user tool calls.

### ClientService

`ClientService` owns user-facing RPC, Git HTTP, and internal calls. It performs user
authentication, action authorization, audit, concurrency limits, and active-operation
tracking. It gets a live session only through `AgentRegistry`.

`GatewayHub` remains the composition root for route registration, admin handlers, and the
scheduler. It delegates instead of directly owning the agent map or session protocol.

## Upgrade Lifecycle

The current agent can apply a Kubernetes image change, but it cannot synchronously return the
rollout result after Kubernetes terminates its pod. Upgrade handling therefore has two phases:

1. The gateway records a durable, restart-recoverable upgrade operation before forwarding the
   workload image change.
2. The operation completes only when the registry observes a replacement session for the same
   `cluster/instance`, with a strictly newer server-side generation, a different process runtime
   identity, and at least one heartbeat.

If no replacement satisfies those conditions before the configured deadline, the operation
fails with an explicit re-registration timeout. A normal old-session disconnect during this
window is not itself an upgrade failure. Reconnecting the same process creates a new connection
generation but cannot satisfy the upgrade completion condition.

The Helm readiness probe represents reverse-tunnel initialization, not merely the local agent
TCP listener. The agent becomes ready only after it answers the gateway initialize request and
remains ready while that reverse-tunnel session is alive. Liveness remains process-local.

## Error Handling

- A normal target call with no registered session returns `target offline`.
- A target whose persisted status is stale is reported offline by the registry; persistence
  errors are logged and surfaced to operation telemetry.
- Agent replacement closes only the old session. A late close from the old session cannot
  mark the replacement offline.
- Upgrade timeout identifies the missing condition: no replacement process identity, no
  replacement registration, no heartbeat, or lost replacement session.

No fallback transport or alternate target path is introduced.

## Verification

1. Registration tests prove replacement and stale-disconnect behavior through `AgentRegistry`.
2. Session tests prove ping/pong updates and relay behavior without user authorization setup.
3. Client-service tests prove authorization, audit, and offline behavior using a registry
   interface.
4. An integration test starts an old fake agent, starts an upgrade operation, disconnects the
   old agent, registers a replacement process, and verifies the operation succeeds only after
   the new process heartbeat.
5. A second integration test omits replacement registration and verifies the exact timeout
   result.
6. A reconnect test proves that a new WebSocket generation from the same process identity cannot
   complete an upgrade.
7. Helm rendering and a live browser-independent gateway smoke test verify that a target is not
   considered ready until it has registered through the reverse tunnel.
