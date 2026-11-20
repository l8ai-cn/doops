package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func cancellationTestAgent(t *testing.T) (*Gateway, *websocket.Conn, <-chan map[string]interface{}) {
	t.Helper()
	gw := &Gateway{
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	server := newGatewayAuthorizedAgentServer(t, gw)
	t.Cleanup(server.Close)
	conn := dialAgentTestWS(t, server.URL)
	t.Cleanup(func() { _ = conn.Close() })
	messages := make(chan map[string]interface{}, 32)
	go func() {
		defer close(messages)
		for {
			var message map[string]interface{}
			if err := conn.ReadJSON(&message); err != nil {
				return
			}
			messages <- message
		}
	}()
	return gw, conn, messages
}

func sendCancellationTestTool(t *testing.T, conn *websocket.Conn, tool string, args map[string]interface{}) {
	t.Helper()
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": 42, "method": "tools/call",
		"params": map[string]interface{}{"name": tool, "arguments": args},
	}); err != nil {
		t.Fatal(err)
	}
}

func sendCancellationTestCancel(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": "cancel-42", "method": "tools/cancel",
		"params": map[string]interface{}{"id": 42},
	}); err != nil {
		t.Fatal(err)
	}
}

func awaitCancellationTestMessage(t *testing.T, messages <-chan map[string]interface{}, match func(map[string]interface{}) bool) map[string]interface{} {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case message, ok := <-messages:
			if !ok {
				t.Fatal("agent connection closed before expected message")
			}
			if match(message) {
				return message
			}
		case <-timer.C:
			t.Fatal("timed out waiting for agent message")
		}
	}
}

func assertCancellationTestPending(t *testing.T, messages <-chan map[string]interface{}) {
	t.Helper()
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case message, ok := <-messages:
			if !ok {
				t.Fatal("connection closed while operation should remain pending")
			}
			if message["id"] == float64(42) {
				t.Fatalf("original RPC became terminal before execution stopped: %#v", message)
			}
		case <-timer.C:
			return
		}
	}
}

func awaitCancellationTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for mock execution")
	}
}

func TestAgentCancellationUsesUnifiedTerminalContract(t *testing.T) {
	_, conn, messages := cancellationTestAgent(t)
	if err := conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": "initialize"}); err != nil {
		t.Fatal(err)
	}
	response := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(1) })
	result := response["result"].(map[string]interface{})
	protocols := result["capabilities"].(map[string]interface{})["protocols"].(map[string]interface{})
	if !executionLifecycleCapabilityCompatible(protocols[executionLifecycleProtocol]) {
		t.Fatalf("background lifetime and cancellation capability was not advertised: %#v", protocols)
	}
}

func TestAgentCancellationBackgroundOriginalRPCWaitsForExit(t *testing.T) {
	for _, exitCode := range []int{0, 7} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOOPS_WORKSPACE_ROOT", root)
			gw, conn, messages := cancellationTestAgent(t)
			release := filepath.Join(root, "release")
			sendCancellationTestTool(t, conn, "doops_bg", map[string]interface{}{
				"session_id": "bg-lifetime",
				"command":    fmt.Sprintf("while [ ! -e %s ]; do sleep 0.02; done; exit %d", shellQuote(release), exitCode),
			})
			submission := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool {
				return strings.Contains(fmt.Sprint(m["params"]), "Task submitted.")
			})
			if submission["method"] != "notifications/message" || !strings.Contains(fmt.Sprint(submission), "TaskID:") {
				t.Fatalf("submission must be progress, not terminal: %#v", submission)
			}
			assertCancellationTestPending(t, messages)
			gw.tasksMu.RLock()
			var task *bgTask
			for _, candidate := range gw.tasks {
				task = candidate
			}
			gw.tasksMu.RUnlock()
			if task == nil {
				t.Fatal("task was not registered")
			}
			status, err := gw.getTaskStatus(task.TaskID, 1)
			if err != nil || status.Status != "running" {
				t.Fatalf("pending background task status: %#v, %v", status, err)
			}
			if err := os.WriteFile(release, nil, 0600); err != nil {
				t.Fatal(err)
			}
			final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
			result := final["result"].(map[string]interface{})
			if (result["isError"] == true) != (exitCode != 0) {
				t.Fatalf("wrong terminal exit status: %#v", final)
			}
			select {
			case <-task.done:
			default:
				t.Fatal("terminal RPC preceded task completion")
			}
			if task.ExitCode != exitCode || task.Cmd.ProcessState == nil {
				t.Fatalf("process Wait not reflected in task: %#v", task)
			}
		})
	}
}

func cancellationTestResistantCommand(pidFile string) string {
	child := fmt.Sprintf("trap '' TERM; echo $$ > %s; while :; do sleep 1; done", shellQuote(pidFile))
	return "bash -c " + shellQuote(child) + " >/dev/null 2>&1 & wait"
}

func awaitCancellationTestPID(t *testing.T, path string) (int, int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(path); err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err == nil {
				group, err := syscall.Getpgid(pid)
				if err == nil {
					t.Cleanup(func() { _ = syscall.Kill(-group, syscall.SIGKILL) })
					return pid, group
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not become ready")
	return 0, 0
}

func TestAgentCancellationJoinsResistantDescendants(t *testing.T) {
	for _, tool := range []string{"doops_shell", "doops_bg"} {
		t.Run(tool, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOOPS_WORKSPACE_ROOT", root)
			_, conn, messages := cancellationTestAgent(t)
			pidFile := filepath.Join(root, "child.pid")
			sendCancellationTestTool(t, conn, tool, map[string]interface{}{
				"session_id": "cancel-descendants", "command": cancellationTestResistantCommand(pidFile),
			})
			child, group := awaitCancellationTestPID(t, pidFile)
			sendCancellationTestCancel(t, conn)
			assertCancellationTestPending(t, messages)
			final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
			if !strings.Contains(fmt.Sprint(final["error"]), "operation canceled") {
				t.Fatalf("expected proved cancellation: %#v", final)
			}
			if err := syscall.Kill(-group, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("process group still exists after terminal reply: %v", err)
			}
			if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("descendant still exists after terminal reply: %v", err)
			}
		})
	}
}

func TestAgentCancellationWaitsForDescendantsAfterLeaderExit(t *testing.T) {
	for _, tool := range []string{"doops_shell", "doops_bg"} {
		t.Run(tool, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("DOOPS_WORKSPACE_ROOT", root)
			_, conn, messages := cancellationTestAgent(t)
			pidFile := filepath.Join(root, "child.pid")
			command := strings.TrimSuffix(cancellationTestResistantCommand(pidFile), " wait")
			sendCancellationTestTool(t, conn, tool, map[string]interface{}{
				"session_id": "leader-exit", "command": command,
			})
			_, group := awaitCancellationTestPID(t, pidFile)
			assertCancellationTestPending(t, messages)
			sendCancellationTestCancel(t, conn)
			final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
			if !strings.Contains(fmt.Sprint(final["error"]), "operation canceled") ||
				!errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
				t.Fatalf("leader exit left a running descendant: %#v", final)
			}
		})
	}
}

func TestAgentCancellationJoinsInFlightHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd, stdout, stderr, err := startRawCommand(ctx, exec.Command("sleep", "60"))
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	done := make(chan error, 1)
	go func() {
		done <- waitCommandStreams(ctx, cmd, stdout, stderr, func(notificationEvent) {
			close(entered)
			<-release
		}, true)
	}()
	select {
	case <-entered:
	case <-time.After(7 * time.Second):
		t.Fatal("heartbeat did not enter the streaming callback")
	}
	cancel()
	select {
	case err := <-done:
		t.Fatalf("execution returned while its heartbeat could still send: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	once.Do(func() { close(release) })
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation result changed after heartbeat join: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("execution did not finish after heartbeat callback returned")
	}
}

func TestAgentCancellationBackgroundDisconnectJoinsTask(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	gw, conn, messages := cancellationTestAgent(t)
	pidFile := filepath.Join(root, "child.pid")
	sendCancellationTestTool(t, conn, "doops_bg", map[string]interface{}{
		"session_id": "disconnect-bg", "command": cancellationTestResistantCommand(pidFile),
	})
	awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool {
		return strings.Contains(fmt.Sprint(m["params"]), "Task submitted.")
	})
	_, group := awaitCancellationTestPID(t, pidFile)
	gw.tasksMu.RLock()
	var task *bgTask
	for _, candidate := range gw.tasks {
		task = candidate
	}
	gw.tasksMu.RUnlock()
	_ = conn.Close()
	awaitCancellationTestSignal(t, task.done)
	if task.Cmd.ProcessState == nil || !errors.Is(syscall.Kill(-group, 0), syscall.ESRCH) {
		t.Fatal("background task survived its canceled connection")
	}
}

func TestAgentCancellationDisconnectJoinsTermResistantToolBeforeServeReturns(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	gw := NewGateway()
	serveDone := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		gw.ServeGatewayWebSocketConnWithReady(conn, r.RemoteAddr, nil)
		close(serveDone)
	}))
	t.Cleanup(server.Close)

	conn := dialAgentTestWS(t, server.URL)
	pidFile := filepath.Join(root, "child.pid")
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0", "id": "term-resistant", "method": "tools/call",
		"params": map[string]interface{}{
			"name": "doops_shell",
			"arguments": map[string]interface{}{
				"session_id": "disconnect-join",
				"command":    cancellationTestResistantCommand(pidFile),
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, group := awaitCancellationTestPID(t, pidFile)
	if err := conn.Close(); err != nil {
		t.Fatalf("close WebSocket: %v", err)
	}

	select {
	case <-serveDone:
	case <-time.After(5 * time.Second):
		t.Fatal("ServeGatewayWebSocketConnWithReady did not return after disconnect cleanup")
	}
	if err := syscall.Kill(-group, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("ServeGatewayWebSocketConnWithReady returned with the old process group alive: %v", err)
	}
}

func TestAgentRejectsBlockedShellAndBackgroundCommands(t *testing.T) {
	commands := map[string]string{
		"remove root":      "rm -rf /",
		"remove root glob": "rm -rf /*",
	}
	for _, tool := range []string{"doops_shell", "doops_bg"} {
		for name, command := range commands {
			t.Run(tool+"/"+name, func(t *testing.T) {
				_, conn, messages := cancellationTestAgent(t)
				sendCancellationTestTool(t, conn, tool, map[string]interface{}{
					"session_id": "blocked-command", "command": command,
				})
				final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
				if !strings.Contains(fmt.Sprint(final["error"]), "blocked_command(") {
					t.Fatalf("%s %q was not rejected by the Agent: %#v", tool, command, final)
				}
			})
		}
	}
}

func TestGatewayCleansUnknownBackgroundTasks(t *testing.T) {
	t.Setenv("DOOPS_COMPLETED_TASK_MAX_AGE", "1s")
	gw := NewGateway()
	gw.tasksMu.Lock()
	gw.tasks["unknown-task"] = &bgTask{
		TaskID:     "unknown-task",
		finishedAt: time.Now().Add(-2 * time.Second),
		err:        errExecutionOutcomeUnknown,
	}
	gw.cleanupCompletedTasksLocked(maxCompletedTaskAge())
	_, ok := gw.tasks["unknown-task"]
	gw.tasksMu.Unlock()
	if ok {
		t.Fatal("unknown background task was retained past its cleanup age")
	}
}

func TestGatewayAutomaticallyReapsExpiredBackgroundTasks(t *testing.T) {
	t.Setenv("DOOPS_COMPLETED_TASK_MAX_AGE", "100ms")
	gw := NewGateway()
	gw.tasksMu.Lock()
	gw.tasks["expired-task"] = &bgTask{
		TaskID:     "expired-task",
		finishedAt: time.Now().Add(-time.Second),
		err:        errExecutionOutcomeUnknown,
	}
	gw.tasksMu.Unlock()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gw.tasksMu.RLock()
		_, ok := gw.tasks["expired-task"]
		gw.tasksMu.RUnlock()
		if !ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expired background task was not automatically reaped")
}

type cancellationDoagentMock struct {
	prompted  chan struct{}
	canceled  chan struct{}
	closed    chan struct{}
	events    chan map[string]interface{}
	endEvents chan struct{}
}

func newCancellationDoagentMock(t *testing.T, promptFailure string, cancelStatus string) *cancellationDoagentMock {
	t.Helper()
	mock := &cancellationDoagentMock{
		prompted: make(chan struct{}), canceled: make(chan struct{}), closed: make(chan struct{}),
		events: make(chan map[string]interface{}, 8), endEvents: make(chan struct{}),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			defer close(mock.closed)
			if r.URL.Query().Get("sid") != "cancel-agent-session" {
				t.Errorf("wrong SSE session: %s", r.URL)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.(http.Flusher).Flush()
			for {
				select {
				case event := <-mock.events:
					data, _ := json.Marshal(event)
					_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
					w.(http.Flusher).Flush()
				case <-mock.endEvents:
					return
				case <-r.Context().Done():
					return
				}
			}
		}
		var request map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode mock RPC: %v", err)
			return
		}
		result := map[string]interface{}{}
		switch request["method"] {
		case "initialize":
			result = doagentAdmissionCapabilities()
		case "session/new":
			result["sessionId"] = "cancel-agent-session"
		case "session/setMode":
		case "session/prompt":
			close(mock.prompted)
			switch promptFailure {
			case "transport":
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","result":`)
				return
			case "admission_timeout":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": request["id"],
					"error": map[string]interface{}{"code": -32000, "message": "admission timed out",
						"data": map[string]interface{}{"failureClass": "admission_timeout"}},
				})
				return
			case "inflight":
				<-r.Context().Done()
				return
			case "missing_turn":
				result["status"] = "accepted"
			default:
				result["status"] = "accepted"
				result["turnId"] = "cancel-turn"
			}
		case "session/cancel":
			params := request["params"].(map[string]interface{})
			if params["sessionId"] != "cancel-agent-session" {
				t.Errorf("cancel targeted wrong session: %#v", params)
			}
			if r.Context().Err() != nil {
				t.Error("cleanup request inherited canceled caller")
			}
			close(mock.canceled)
			if cancelStatus == "rpc_error" {
				http.Error(w, "synthetic cancel transport failure", http.StatusServiceUnavailable)
				return
			}
			result["status"] = cancelStatus
			result["turnId"] = "cancel-turn"
		default:
			t.Errorf("unexpected mock RPC: %#v", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "id": request["id"], "result": result})
	}))
	t.Cleanup(server.Close)
	t.Setenv("DO_AGENT_URL", server.URL)
	return mock
}

func cancellationTestTurn(sessionID, turnID, kind, status string) map[string]interface{} {
	return map[string]interface{}{
		"jsonrpc": "2.0", "method": "session/update",
		"params": map[string]interface{}{
			"sessionId": sessionID,
			"update": map[string]interface{}{
				"sessionUpdate": kind, "turnId": turnID, "status": status, "stopReason": "cancelled",
			},
		},
	}
}

func awaitCancellationTestPromptAdmission(t *testing.T, mock *cancellationDoagentMock, messages <-chan map[string]interface{}) {
	t.Helper()
	awaitCancellationTestSignal(t, mock.prompted)
	event := cancellationTestTurn("cancel-agent-session", "cancel-turn", "tool_call_update", "in_progress")
	event["params"].(map[string]interface{})["update"].(map[string]interface{})["toolName"] = "Read"
	mock.events <- event
	awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool {
		params, _ := m["params"].(map[string]interface{})
		return m["method"] == "notifications/message" && params["kind"] == "tool"
	})
}

func TestAgentCancellationPromptWaitsForMatchingTurn(t *testing.T) {
	previousSecret := processMutationAuthoritySecret
	processMutationAuthoritySecret = strings.Repeat("a", 64)
	t.Cleanup(func() { processMutationAuthoritySecret = previousSecret })
	for _, operation := range []string{"", "apply"} {
		t.Run("operation-"+operation, func(t *testing.T) {
			mock := newCancellationDoagentMock(t, "", "cancelling")
			_, conn, messages := cancellationTestAgent(t)
			sendCancellationTestTool(t, conn, "doops_agent_prompt", map[string]interface{}{
				"session_id": "cancel-prompt", "instruction": "synthetic task", "operation": operation,
			})
			awaitCancellationTestPromptAdmission(t, mock, messages)
			sendCancellationTestCancel(t, conn)
			awaitCancellationTestSignal(t, mock.canceled)
			mock.events <- cancellationTestTurn("cancel-agent-session", "old-turn", "turn_finished", "cancelled")
			mock.events <- cancellationTestTurn("other-session", "cancel-turn", "turn_finished", "cancelled")
			mock.events <- cancellationTestTurn("cancel-agent-session", "cancel-turn", "completed", "cancelled")
			assertCancellationTestPending(t, messages)
			select {
			case <-mock.closed:
				t.Fatal("SSE closed on caller cancellation before terminal proof")
			default:
			}
			mock.events <- cancellationTestTurn("cancel-agent-session", "cancel-turn", "turn_finished", "cancelled")
			final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
			if !strings.Contains(fmt.Sprint(final["error"]), "operation canceled") ||
				strings.Contains(fmt.Sprint(final), "execution_outcome_unknown") {
				t.Fatalf("matching terminal did not establish cancellation: %#v", final)
			}
			awaitCancellationTestSignal(t, mock.closed)
		})
	}
}

func TestAgentCancellationPromptWithoutTerminalIsUnknown(t *testing.T) {
	for _, cancelStatus := range []string{"cancelling", "not_running", "rpc_error"} {
		t.Run(cancelStatus, func(t *testing.T) {
			mock := newCancellationDoagentMock(t, "", cancelStatus)
			_, conn, messages := cancellationTestAgent(t)
			sendCancellationTestTool(t, conn, "doops_agent_prompt", map[string]interface{}{
				"session_id": "unknown-prompt", "instruction": "synthetic task",
			})
			awaitCancellationTestPromptAdmission(t, mock, messages)
			sendCancellationTestCancel(t, conn)
			awaitCancellationTestSignal(t, mock.canceled)
			close(mock.endEvents)
			final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
			if !strings.Contains(fmt.Sprint(final["error"]), "execution_outcome_unknown") ||
				strings.Contains(fmt.Sprint(final), "operation canceled") {
				t.Fatalf("unproved stop must remain unknown: %#v", final)
			}
		})
	}
}

func TestAgentCancellationUnexpectedSSEDisconnectCancelsPrompt(t *testing.T) {
	mock := newCancellationDoagentMock(t, "", "cancelling")
	_, conn, messages := cancellationTestAgent(t)
	sendCancellationTestTool(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id": "lost-sse", "instruction": "synthetic task",
	})
	awaitCancellationTestPromptAdmission(t, mock, messages)
	close(mock.endEvents)
	awaitCancellationTestSignal(t, mock.canceled)
	final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
	if !strings.Contains(fmt.Sprint(final["error"]), "execution_outcome_unknown") {
		t.Fatalf("SSE disconnect was mistaken for completion: %#v", final)
	}
}

func TestAgentCancellationPromptDisconnectKeepsSSEForCleanup(t *testing.T) {
	mock := newCancellationDoagentMock(t, "", "cancelling")
	_, conn, messages := cancellationTestAgent(t)
	sendCancellationTestTool(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id": "disconnect-prompt", "instruction": "synthetic task",
	})
	awaitCancellationTestPromptAdmission(t, mock, messages)
	_ = conn.Close()
	awaitCancellationTestSignal(t, mock.canceled)
	select {
	case <-mock.closed:
		t.Fatal("agent abandoned SSE proof when its gateway disconnected")
	case <-time.After(100 * time.Millisecond):
	}
	mock.events <- cancellationTestTurn("cancel-agent-session", "cancel-turn", "turn_finished", "cancelled")
	awaitCancellationTestSignal(t, mock.closed)
}

func TestAgentCancellationLostAdmissionIsUnknown(t *testing.T) {
	for _, failure := range []string{"transport", "admission_timeout", "missing_turn", "inflight"} {
		for _, format := range []string{"", "json"} {
			t.Run(failure+"/response_format="+format, func(t *testing.T) {
				mock := newCancellationDoagentMock(t, failure, "not_running")
				_, conn, messages := cancellationTestAgent(t)
				sendCancellationTestTool(t, conn, "doops_agent_prompt", map[string]interface{}{
					"session_id": "lost-admission", "instruction": "synthetic task",
					"response_format": format,
				})
				awaitCancellationTestSignal(t, mock.prompted)
				if failure == "inflight" {
					sendCancellationTestCancel(t, conn)
				}
				if format == "json" {
					awaitCancellationTestSignal(t, mock.canceled)
				}
				final := awaitCancellationTestMessage(t, messages, func(m map[string]interface{}) bool { return m["id"] == float64(42) })
				if !strings.Contains(fmt.Sprint(final["error"]), "execution_outcome_unknown") {
					t.Fatalf("lost admission incorrectly treated as stopped: %#v", final)
				}
				if format == "" {
					select {
					case <-mock.canceled:
						t.Fatal("lost admission canceled a potentially shared session")
					default:
					}
				}
			})
		}
	}
}

func TestAgentCancellationCleanupDeadlineIsDetachedAndBounded(t *testing.T) {
	for _, stalledRPC := range []bool{false, true} {
		t.Run(fmt.Sprint(stalledRPC), func(t *testing.T) {
			received := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(received)
				if stalledRPC {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				_, _ = fmt.Fprint(w, `{"jsonrpc":"2.0","result":{"status":"cancelling","turnId":"turn"}}`)
			}))
			defer server.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			var terminal atomic.Bool
			started := time.Now()
			err := cancelDoagentPrompt(ctx, server.URL, "bounded", make(chan struct{}), &terminal, 100*time.Millisecond)
			if !errors.Is(err, errExecutionOutcomeUnknown) {
				t.Fatalf("missing terminal was not unknown: %v", err)
			}
			select {
			case <-received:
			default:
				t.Fatal("cancel RPC was suppressed by the canceled caller")
			}
			if elapsed := time.Since(started); elapsed < 80*time.Millisecond || elapsed > time.Second {
				t.Fatalf("cleanup did not honor its independent bound: %s", elapsed)
			}
		})
	}
}
