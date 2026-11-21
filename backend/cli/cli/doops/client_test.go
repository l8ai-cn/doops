package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestCallRetriesWebSocketHandshakeEOFBeforeSendingToolCall(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if connections.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack first websocket handshake: %v", err)
			}
			_ = conn.Close()
			return
		}

		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade retry: %v", err)
		}
		defer conn.Close()
		for {
			var req map[string]interface{}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			switch req["method"] {
			case "initialize":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
				})
			case "tools/call":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result": map[string]interface{}{
						"content": []interface{}{
							map[string]interface{}{"type": "text", "text": "ok"},
						},
					},
				})
			}
		}
	}))
	defer ts.Close()

	client := newGatewayTestClient(t, ts.URL, "handshake-eof")
	defer client.Close()

	if err := client.Call("doops_shell", map[string]interface{}{"command": "hostname"}); err != nil {
		t.Fatalf("call should recover from pre-request handshake EOF: %v", err)
	}
	if got := connections.Load(); got != 2 {
		t.Fatalf("expected one handshake retry, got %d connections", got)
	}
}

func TestCallStopsAfterWebSocketHandshakeRetryLimit(t *testing.T) {
	var connections atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Fatalf("hijack websocket handshake: %v", err)
		}
		_ = conn.Close()
	}))
	defer ts.Close()

	client := newGatewayTestClient(t, ts.URL, "handshake-limit")
	defer client.Close()
	if err := client.Call("doops_shell", map[string]interface{}{"command": "hostname"}); err == nil {
		t.Fatal("expected repeated handshake EOF to fail")
	}
	if got := connections.Load(); got != websocketHandshakeAttempts {
		t.Fatalf("expected %d handshake attempts, got %d", websocketHandshakeAttempts, got)
	}
}

func TestCallDoesNotRetryHTTPHandshakeError(t *testing.T) {
	var connections atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"unauthorized","code":"gateway_auth_expired","auth_domain":"gateway","message":"Gateway user token has expired"}`))
	}))
	defer ts.Close()

	client := newGatewayTestClient(t, ts.URL, "handshake-http-error")
	defer client.Close()
	err := client.Call("doops_shell", map[string]interface{}{"command": "hostname"})
	if err == nil || !strings.Contains(err.Error(), "gateway_auth_expired") ||
		!strings.Contains(err.Error(), "auth_domain=gateway") {
		t.Fatalf("expected HTTP handshake error, got %v", err)
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("HTTP handshake errors must not be retried, got %d attempts", got)
	}
}

func TestCallDoesNotRetryAfterToolCallWasSent(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var connections atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade websocket: %v", err)
		}
		defer conn.Close()
		for {
			var req map[string]interface{}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			if req["method"] == "initialize" {
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
				})
				continue
			}
			return
		}
	}))
	defer ts.Close()

	client := newGatewayTestClient(t, ts.URL, "tool-call-disconnect")
	defer client.Close()
	if err := client.Call("doops_shell", map[string]interface{}{"command": "hostname"}); err == nil {
		t.Fatal("expected disconnect after tool submission to fail")
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("submitted tool calls must not be replayed, got %d connections", got)
	}
}

func newGatewayTestClient(t *testing.T, serverURL, session string) *MCPClient {
	t.Helper()
	return NewMCPClient(
		Server{
			Name:     "test-gateway",
			Gateway:  serverURL,
			Cluster:  "test",
			Instance: "node",
		},
		NewSessionStore(),
		session,
		false,
	)
}

func TestGatewayTargetUsesVersionedRPCEndpoint(t *testing.T) {
	client := NewMCPClient(Server{
		Name:     "local",
		Gateway:  "https://gateway.example.com",
		Cluster:  "dev",
		Instance: "local",
	}, NewSessionStore(), "smoke", false)

	got, err := client.targetWebSocketURL()
	if err != nil {
		t.Fatalf("build websocket URL: %v", err)
	}
	want := "wss://gateway.example.com/v1/rpc?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayTargetPreservesGatewayPathPrefix(t *testing.T) {
	client := NewMCPClient(Server{
		Name:     "local",
		Gateway:  "https://gateway.example.com/doops",
		Cluster:  "dev",
		Instance: "local",
	}, NewSessionStore(), "smoke", false)

	got, err := client.targetWebSocketURL()
	if err != nil {
		t.Fatalf("build websocket URL: %v", err)
	}
	want := "wss://gateway.example.com/doops/v1/rpc?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayTargetsURLPreservesGatewayPathPrefix(t *testing.T) {
	got, err := gatewayTargetsURL("https://gateway.example.com/doops")
	if err != nil {
		t.Fatalf("build targets URL: %v", err)
	}
	want := "https://gateway.example.com/doops/v1/targets"
	if got != want {
		t.Fatalf("targets URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayTargetRejectsInsecureHTTPGatewayByDefault(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "")
	client := NewMCPClient(Server{
		Name:     "prod",
		Gateway:  "http://203.0.113.10:42222",
		Cluster:  "prod",
		Instance: "master",
	}, NewSessionStore(), "smoke", false)

	if _, err := client.targetWebSocketURL(); err == nil {
		t.Fatal("expected insecure non-loopback gateway to be rejected by default")
	}
}

func TestGatewayTargetAllowsInsecureHTTPWithOptIn(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "1")
	client := NewMCPClient(Server{
		Name:     "prod",
		Gateway:  "http://203.0.113.10:42222",
		Cluster:  "prod",
		Instance: "master",
	}, NewSessionStore(), "smoke", false)

	got, err := client.targetWebSocketURL()
	if err != nil {
		t.Fatalf("opt-in should allow insecure gateway: %v", err)
	}
	want := "ws://203.0.113.10:42222/v1/rpc?cluster=prod&instance=master"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestGatewayTargetAllowsInsecureLocalhostForDevelopment(t *testing.T) {
	client := NewMCPClient(Server{
		Name:     "local",
		Gateway:  "http://127.0.0.1:42222",
		Cluster:  "dev",
		Instance: "local",
	}, NewSessionStore(), "smoke", false)

	got, err := client.targetWebSocketURL()
	if err != nil {
		t.Fatalf("local insecure gateway should be allowed: %v", err)
	}
	want := "ws://127.0.0.1:42222/v1/rpc?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestToolResultErrorTextIsDetectedBeforeCapture(t *testing.T) {
	result := map[string]interface{}{
		"isError": true,
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "chunk rejected"},
		},
	}
	if !toolResultIsError(result) {
		t.Fatalf("expected tool result to be marked as error")
	}
	if got := toolResultText(result, "fallback"); got != "chunk rejected" {
		t.Fatalf("unexpected tool error text: %q", got)
	}
}

func TestRPCOutputPreservesRawJSONAndChunkBoundaries(t *testing.T) {
	chunks := []string{
		"  {\"ok\":true}\n\n",
		"prefix {\"type\":\"step_finish\",\"part\":{\"reason\":\"stop\"}}\n",
		"{\"type\":\"tool_", "use\",\"business\":true}\n",
		"\r\x1b[K[agent] \u23f3 \u547d\u4ee4\u540e\u53f0\u6267\u884c\u4e2d... (\u8017\u65f6 5s)\n",
		"prefix \r\x1b[K[agent] \u23f3 \u547d\u4ee4\u540e\u53f0\u6267\u884c\u4e2d... (\u8017\u65f6 5s)",
		" \tno trailing newline",
	}
	want := strings.Join(chunks, "")
	upgrader := websocket.Upgrader{}
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			var req map[string]interface{}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			if req["method"] == "initialize" {
				_ = conn.WriteJSON(map[string]interface{}{"jsonrpc": "2.0", "id": req["id"], "result": agentInitializeResult()})
				continue
			}
			for _, chunk := range chunks {
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "method": "notifications/message",
					"params": map[string]interface{}{
						"request_id": req["id"], "sessionID": "raw-output", "kind": "raw", "data": chunk,
					},
				})
			}
			_ = conn.WriteJSON(map[string]interface{}{
				"jsonrpc": "2.0", "id": req["id"],
				"result": map[string]interface{}{"content": []interface{}{}},
			})
		}
	}))
	defer gateway.Close()

	client := newGatewayTestClient(t, gateway.URL, "raw-output")
	defer client.Close()
	got, err := client.CallAndCapture("doops_shell", map[string]interface{}{"command": "synthetic-output"})
	if err != nil || got != want {
		t.Fatalf("captured stream = %q, %v; want %q", got, err, want)
	}
	t.Setenv("DOOPS_CONFIG", filepath.Join(t.TempDir(), "config.json"))
	if err := saveServers([]Server{{Name: "test", Gateway: gateway.URL, Cluster: "test", Instance: "node"}}); err != nil {
		t.Fatal(err)
	}
	auth := &AuthStore{}
	auth.SetGateway(gateway.URL, "synthetic-token")
	if err := auth.Save(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, buildDoopsCLIBinary(t), "-session", "raw-output",
		"exec", "--target", "test", "--cmd", "synthetic-output").Output()
	targetLine, body, found := strings.Cut(string(output), "\n")
	if err != nil || !found || !strings.Contains(targetLine, "[TARGETING]") || body != want {
		t.Fatalf("CLI stream = %q, %v; want %q", output, err, want)
	}
}

func TestNotificationDispatchRoutesByRequestID(t *testing.T) {
	client := NewMCPClient(Server{Name: "local"}, NewSessionStore(), "session-a", false)
	chA := client.registerPending(1)
	chB := client.registerPending(2)
	defer client.unregisterPending(1)
	defer client.unregisterPending(2)

	client.dispatchMessage(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/message",
		"params": map[string]interface{}{
			"request_id": 2,
			"sessionID":  "session-a",
			"data":       "output from session-b\n",
		},
	})

	select {
	case evt := <-chB:
		if got := fmt.Sprint(evt.Parsed["method"]); got != "notifications/message" {
			t.Fatalf("unexpected event on session-b channel: %#v", evt.Parsed)
		}
	default:
		t.Fatal("session-b channel did not receive its notification")
	}

	select {
	case evt := <-chA:
		t.Fatalf("session-a channel received session-b notification: %#v", evt.Parsed)
	default:
	}
}

func TestNotificationDispatchDoesNotSilentlyDropWhenPendingBufferIsFull(t *testing.T) {
	client := NewMCPClient(Server{Name: "local"}, NewSessionStore(), "session-a", false)
	ch := client.registerPending(1)
	defer client.unregisterPending(1)

	for i := 0; i < cap(ch); i++ {
		ch <- wsEvent{Parsed: map[string]interface{}{"filled": i}}
	}

	done := make(chan struct{})
	go func() {
		client.dispatchMessage(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "notifications/message",
			"params": map[string]interface{}{
				"request_id": 1,
				"data":       "must not disappear\n",
			},
		})
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("dispatch returned while pending buffer was full")
	case <-time.After(20 * time.Millisecond):
	}
	<-ch
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatch did not resume after buffer space became available")
	}

	drained := make([]wsEvent, 0, cap(ch)+1)
	for {
		select {
		case evt := <-ch:
			drained = append(drained, evt)
		default:
			for _, evt := range drained {
				params, _ := evt.Parsed["params"].(map[string]interface{})
				if params != nil && params["data"] == "must not disappear\n" {
					return
				}
			}
			t.Fatalf("notification was silently dropped after draining %d buffered events", len(drained))
		}
	}
}

func TestConnectRejectsInitializeNotificationFalsePositive(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		var req map[string]interface{}
		if err := conn.ReadJSON(&req); err != nil {
			t.Fatalf("read init: %v", err)
		}
		_ = conn.WriteJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "notifications/message",
			"params":  map[string]interface{}{"request_id": req["id"], "data": "not init"},
		})
		time.Sleep(200 * time.Millisecond)
	}))
	defer ts.Close()
	client := newGatewayTestClient(t, ts.URL, "s")
	err := client.connect()
	if err == nil || !strings.Contains(err.Error(), "initialize failed") {
		t.Fatalf("expected initialize validation error, got %v", err)
	}
}

func TestCallAndCaptureTimesOutWithoutFinalResult(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	cancelSeen := make(chan struct{}, 1)
	var toolCalls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		for {
			var req map[string]interface{}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			if req["method"] == "initialize" {
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
				})
				continue
			}
			switch req["method"] {
			case "tools/call":
				toolCalls.Add(1)
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "method": "notifications/message",
					"params": map[string]interface{}{
						"request_id": req["id"], "sessionID": "s", "data": "partial output\n",
					},
				})
			case "tools/cancel":
				params, _ := req["params"].(map[string]interface{})
				if fmt.Sprint(params["id"]) == fmt.Sprint(float64(2)) {
					select {
					case cancelSeen <- struct{}{}:
					default:
					}
				}
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "id": req["id"],
					"result": map[string]interface{}{"canceled": true},
				})
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "id": params["id"],
					"error": map[string]interface{}{"code": -32008, "message": "execution_outcome_unknown"},
				})
			}
		}
	}))
	defer ts.Close()
	client := newGatewayTestClient(t, ts.URL, "s")
	client.CallTimeout = 30 * time.Millisecond
	got, err := client.CallAndCapture("doops_shell", map[string]interface{}{"command": "sleep"})
	if err == nil || err.Error() != "remote error: execution_outcome_unknown" {
		t.Fatalf("expected unknown execution outcome, got %v", err)
	}
	if got != "partial output\n" {
		t.Fatalf("partial output was lost: %q", got)
	}
	select {
	case <-cancelSeen:
	case <-time.After(time.Second):
		t.Fatal("timeout did not request cancellation")
	}
	if toolCalls.Load() != 1 {
		t.Fatalf("timed-out call was replayed: %d tool calls", toolCalls.Load())
	}
}

func TestCallAndCaptureFailurePreservesStreamedOutput(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		for {
			var req map[string]interface{}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			switch req["method"] {
			case "initialize":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "id": req["id"],
					"result": map[string]interface{}{"protocolVersion": "2024-11-05"},
				})
			case "tools/call":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "method": "notifications/message",
					"params": map[string]interface{}{
						"request_id": req["id"], "sessionID": "s", "data": "before failure\n",
					},
				})
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0", "id": req["id"],
					"error": map[string]interface{}{"code": -32603, "message": "synthetic failure"},
				})
			}
		}
	}))
	defer ts.Close()

	client := newGatewayTestClient(t, ts.URL, "s")
	got, err := client.CallAndCapture("doops_shell", map[string]interface{}{"command": "fail"})
	if err == nil || err.Error() != "remote error: synthetic failure" {
		t.Fatalf("expected remote failure, got %v", err)
	}
	if got != "before failure\n" {
		t.Fatalf("captured output was lost on failure: %q", got)
	}
}

func TestCallAndCaptureRejectsEmptyFinalResult(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		for {
			var req map[string]interface{}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			switch req["method"] {
			case "initialize":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
				})
			case "tools/call":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{},
				})
			}
		}
	}))
	defer ts.Close()
	client := newGatewayTestClient(t, ts.URL, "s")
	client.CallTimeout = time.Second
	_, err := client.CallAndCapture("doops_bg", map[string]interface{}{"command": "sleep 1"})
	if err == nil || !strings.Contains(err.Error(), "no result received") {
		t.Fatalf("expected empty final result to fail immediately, got %v", err)
	}
	stream := newGatewayTestClient(t, ts.URL, "s")
	stream.CallTimeout = time.Second
	if err := stream.Call("doops_bg", map[string]interface{}{"command": "sleep 1"}); err == nil || !strings.Contains(err.Error(), "outcome_unknown") {
		t.Fatalf("streaming call accepted an empty terminal response: %v", err)
	}
}

func TestCallStructuredReadsStructuredContentWithoutParsingText(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()

		for {
			var req map[string]interface{}
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			switch req["method"] {
			case "initialize":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
				})
			case "tools/call":
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result": map[string]interface{}{
						"content":           []map[string]interface{}{{"type": "text", "text": "human-readable only"}},
						"structuredContent": map[string]interface{}{"planDigest": "sha256:plan", "status": "Converged"},
					},
				})
			}
		}
	}))
	defer ts.Close()

	client := newGatewayTestClient(t, ts.URL, "structured")
	var result struct {
		PlanDigest string `json:"planDigest"`
		Status     string `json:"status"`
	}
	if err := client.CallStructured("example_structured_tool", map[string]interface{}{"plan": map[string]string{}}, &result); err != nil {
		t.Fatalf("call structured: %v", err)
	}
	if result.PlanDigest != "sha256:plan" || result.Status != "Converged" {
		t.Fatalf("unexpected structured result: %#v", result)
	}

	raw, err := json.Marshal(result)
	if err != nil || strings.Contains(string(raw), "human-readable") {
		t.Fatalf("structured result was contaminated by text: %s err=%v", raw, err)
	}
}
