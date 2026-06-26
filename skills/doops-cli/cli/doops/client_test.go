package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestGatewayTargetUsesVersionedRPCEndpoint(t *testing.T) {
	client := NewMCPClient(Server{
		Name:     "local",
		Gateway:  "https://gateway.example.com",
		Cluster:  "dev",
		Instance: "local",
	}, NewSessionStore(), "smoke", false)

	got, err := client.targetWebSocketURL("42222")
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

	got, err := client.targetWebSocketURL("42222")
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

	if _, err := client.targetWebSocketURL("42222"); err == nil {
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

	got, err := client.targetWebSocketURL("42222")
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

	got, err := client.targetWebSocketURL("42222")
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

func TestNotificationDispatchRoutesBySessionID(t *testing.T) {
	client := NewMCPClient(Server{Name: "local"}, NewSessionStore(), "session-a", false)
	chA := client.registerPending(1)
	chB := client.registerPending(2)
	defer client.unregisterPending(1)
	defer client.unregisterPending(2)
	client.registerPendingSession("session-a", chA)
	client.registerPendingSession("session-b", chB)
	defer client.unregisterPendingSession("session-a", chA)
	defer client.unregisterPendingSession("session-b", chB)

	client.dispatchMessage(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "notifications/message",
		"params": map[string]interface{}{
			"sessionID": "session-b",
			"data":      "output from session-b\n",
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
	client.registerPendingSession("session-a", ch)
	defer client.unregisterPendingSession("session-a", ch)

	for i := 0; i < cap(ch); i++ {
		ch <- wsEvent{Parsed: map[string]interface{}{"filled": i}}
	}

	done := make(chan struct{})
	go func() {
		client.dispatchMessage(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "notifications/message",
			"params": map[string]interface{}{
				"sessionID": "session-a",
				"data":      "must not disappear\n",
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
			"params":  map[string]interface{}{"data": "not init"},
		})
		time.Sleep(200 * time.Millisecond)
	}))
	defer ts.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	client := NewMCPClient(Server{Name: "direct", IP: host, Port: port}, NewSessionStore(), "s", false)
	err = client.connect()
	if err == nil || !strings.Contains(err.Error(), "initialize failed") {
		t.Fatalf("expected initialize validation error, got %v", err)
	}
}

func TestCallAndCaptureTimesOutWithoutFinalResult(t *testing.T) {
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
			if req["method"] == "initialize" {
				_ = conn.WriteJSON(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"protocolVersion": "2024-11-05"},
				})
			}
		}
	}))
	defer ts.Close()
	host, port, err := net.SplitHostPort(strings.TrimPrefix(ts.URL, "http://"))
	if err != nil {
		t.Fatalf("split test server address: %v", err)
	}
	client := NewMCPClient(Server{Name: "direct", IP: host, Port: port}, NewSessionStore(), "s", false)
	client.CallTimeout = 30 * time.Millisecond
	_, err = client.CallAndCapture("doops_shell", map[string]interface{}{"command": "sleep"})
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected call timeout, got %v", err)
	}
}
