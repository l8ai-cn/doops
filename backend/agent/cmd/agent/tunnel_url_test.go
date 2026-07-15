package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/internal/server"
)

func TestAgentDefaultsToHostNetworkCompatibleListener(t *testing.T) {
	if got, want := defaultAgentListenAddress, "0.0.0.0"; got != want {
		t.Fatalf("default listener mismatch: got %q want %q", got, want)
	}
}

func TestBuildGatewayAgentURLUsesVersionedEndpoint(t *testing.T) {
	got, err := buildGatewayAgentURL("https://gateway.example.com", "dev", "local")
	if err != nil {
		t.Fatalf("build url: %v", err)
	}
	want := "wss://gateway.example.com/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildGatewayAgentURLAllowsExplicitVersionedEndpoint(t *testing.T) {
	got, err := buildGatewayAgentURL("https://gateway.example.com/v1/agent/connect", "dev", "local")
	if err != nil {
		t.Fatalf("build url: %v", err)
	}
	want := "wss://gateway.example.com/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildGatewayAgentURLRejectsInsecureRemoteByDefault(t *testing.T) {
	// Ensure the default-deny path is exercised regardless of any ambient
	// DOOPS_ALLOW_INSECURE_GATEWAY value in the developer's shell.
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "")
	if _, err := buildGatewayAgentURL("http://203.0.113.10:42222", "dev", "local"); err == nil {
		t.Fatal("insecure (ws://) gateway URL to a non-loopback host must be rejected by default")
	}
}

func TestBuildGatewayAgentURLAllowsInsecureRemoteWithOptIn(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "1")
	got, err := buildGatewayAgentURL("http://203.0.113.10:42222", "dev", "local")
	if err != nil {
		t.Fatalf("insecure gateway URL should be allowed with opt-in: %v", err)
	}
	want := "ws://203.0.113.10:42222/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildGatewayAgentURLRejectsLegacyDirectAgentWSPath(t *testing.T) {
	t.Setenv("DOOPS_ALLOW_INSECURE_GATEWAY", "1")
	if _, err := buildGatewayAgentURL("http://203.0.113.10:42222/ws", "dev", "local"); err == nil {
		t.Fatal("legacy direct-agent /ws path must be rejected for gateway reverse tunnel")
	}
}

func TestBuildGatewayAgentURLDefaultsToSecureScheme(t *testing.T) {
	got, err := buildGatewayAgentURL("gateway.example.com", "dev", "local")
	if err != nil {
		t.Fatalf("bare host should default to wss: %v", err)
	}
	want := "wss://gateway.example.com/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestBuildGatewayAgentURLAllowsInsecureLocalhostForDevelopment(t *testing.T) {
	got, err := buildGatewayAgentURL("http://localhost:42222", "dev", "local")
	if err != nil {
		t.Fatalf("local insecure gateway should be allowed: %v", err)
	}
	want := "ws://localhost:42222/v1/agent/connect?cluster=dev&instance=local"
	if got != want {
		t.Fatalf("gateway URL mismatch\nwant: %s\n got: %s", want, got)
	}
}

func TestHealthLiveAlways200(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health/live", nil)
	rec := httptest.NewRecorder()
	handleHealthLive(rec, req)
	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("health live status mismatch: got %d want %d", got, want)
	}
}

func TestHealthReadyDependsOnReverseTunnelConnection(t *testing.T) {
	setReverseTunnelConnected(false)
	req := httptest.NewRequest(http.MethodGet, "/health/ready", nil)
	rec := httptest.NewRecorder()
	handleHealthReady(rec, req)
	if got, want := rec.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("health ready when disconnected: got %d want %d", got, want)
	}

	setReverseTunnelConnected(true)
	rec = httptest.NewRecorder()
	handleHealthReady(rec, req)
	if got, want := rec.Code, http.StatusOK; got != want {
		t.Fatalf("health ready when connected: got %d want %d", got, want)
	}

	setReverseTunnelConnected(false)
	rec = httptest.NewRecorder()
	handleHealthReady(rec, req)
	if got, want := rec.Code, http.StatusServiceUnavailable; got != want {
		t.Fatalf("health ready after disconnect: got %d want %d", got, want)
	}
}

func TestRunReverseTunnelUpdatesReadyStateOnConnectAndDisconnect(t *testing.T) {
	origDial := reverseTunnelDial
	origServeConn := reverseTunnelServeConn
	origContinue := reverseTunnelContinue
	t.Cleanup(func() {
		reverseTunnelDial = origDial
		reverseTunnelServeConn = origServeConn
		reverseTunnelContinue = origContinue
		setReverseTunnelConnected(false)
	})

	setReverseTunnelConnected(false)

	var loopCount int32
	reverseTunnelContinue = func() bool {
		return atomic.LoadInt32(&loopCount) < 2
	}
	reverseTunnelDial = func(rawURL string, header http.Header) (*websocket.Conn, *http.Response, error) {
		count := atomic.AddInt32(&loopCount, 1)
		if count == 1 {
			if len(rawURL) == 0 {
				t.Fatalf("invalid dial URL: %s", rawURL)
			}
			return nil, &http.Response{StatusCode: http.StatusSwitchingProtocols}, nil
		}
		return nil, &http.Response{StatusCode: http.StatusBadRequest}, errors.New("stopped")
	}

	connected := make(chan struct{}, 1)
	reverseTunnelServeConn = func(gw *server.Gateway, conn *websocket.Conn, name string, onReady func()) {
		_ = name
		_ = gw
		_ = conn
		onReady()
		if isReverseTunnelConnected() {
			connected <- struct{}{}
		}
	}

	done := make(chan struct{})
	go func() {
		runReverseTunnel(nil, "wss://gateway.example.com", "", "dev", "instance", 1*time.Millisecond)
		close(done)
	}()

	select {
	case <-connected:
	case <-time.After(2 * time.Second):
		t.Fatal("never observed ready state during tunnel serve")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runReverseTunnel did not finish in time")
	}
	if isReverseTunnelConnected() {
		t.Fatal("reverse tunnel should be not ready after serve returns")
	}
}
