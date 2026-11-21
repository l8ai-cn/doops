package server

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProxyPOSTUpstreamDisconnectPreservesUnknownWithoutBlockingNextRequest(t *testing.T) {
	firstReceived := make(chan struct{})
	var requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream request: %v", err)
			return
		}
		if requests.Add(1) == 1 {
			if r.Method != http.MethodPost || string(body) != "payload" {
				t.Errorf("upstream received %s %q, want POST payload", r.Method, body)
			}
			close(firstReceived)
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "hijacking is unavailable", http.StatusInternalServerError)
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("accepted"))
	}))
	defer upstream.Close()

	store, hub, gatewayServer, agentConn := newProxyOutcomeFixture(t, upstream.URL, "mutate.example.test")
	defer store.Close()
	defer gatewayServer.Close()
	defer agentConn.Close()

	first := httptest.NewRequest(http.MethodPost, gatewayServer.URL+"/mutate", strings.NewReader("payload"))
	first.Host = "mutate.example.test"
	firstRecorder := httptest.NewRecorder()
	hub.HandlePublicProxy(firstRecorder, first)

	select {
	case <-firstReceived:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the first POST")
	}
	if firstRecorder.Code != http.StatusBadGateway {
		t.Fatalf("first POST status = %d, body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}

	events, err := store.ListAuditFiltered(AuditFilter{
		Cluster: "dev", Instance: "local", Action: ActionProxy, Limit: 10,
	})
	if err != nil {
		t.Fatalf("list proxy audit: %v", err)
	}
	if len(events) != 1 || events[0].Status != "outcome_unknown" {
		t.Fatalf("upstream-disconnected POST audit = %#v, want one outcome_unknown event", events)
	}

	second := httptest.NewRequest(http.MethodPost, gatewayServer.URL+"/mutate", strings.NewReader("payload-2"))
	second.Host = "mutate.example.test"
	secondRecorder := httptest.NewRecorder()
	hub.HandlePublicProxy(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("historical unknown audit blocked second POST: status=%d body=%s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("new explicit POST request count=%d, want 2", got)
	}
}

func TestProxyUpstreamOutcomeClassification(t *testing.T) {
	for _, test := range []struct {
		name      string
		method    string
		mode      string
		status    int
		wantAudit string
		wantCalls int32
	}{
		{"get-disconnect", http.MethodGet, "disconnect", http.StatusBadGateway, "error", 1},
		{"post-admission-denied", http.MethodPost, "denied", http.StatusBadGateway, "error", 0},
		{"post-created", http.MethodPost, "response", http.StatusCreated, "success", 1},
		{"post-bad-request", http.MethodPost, "response", http.StatusBadRequest, "success", 1},
		{"post-server-error", http.MethodPost, "response", http.StatusServiceUnavailable, "success", 1},
		{"post-truncated-response", http.MethodPost, "truncated", http.StatusOK, "outcome_unknown", 1},
		{"get-truncated-response", http.MethodGet, "truncated", http.StatusOK, "error", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var requests atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.ReadAll(r.Body)
				requests.Add(1)
				if test.mode == "disconnect" || test.mode == "truncated" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Errorf("hijack upstream: %v", err)
						return
					}
					defer conn.Close()
					if test.mode == "truncated" {
						_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\npartial")
					}
					return
				}
				w.WriteHeader(test.status)
				_, _ = w.Write([]byte("response"))
			}))
			defer upstream.Close()
			upstreamURL := upstream.URL
			if test.mode == "denied" {
				upstreamURL = strings.Replace(upstreamURL, "127.0.0.1", "localhost", 1)
			}
			store, hub, gatewayServer, agentConn := newProxyOutcomeFixture(t, upstreamURL, "outcome.example.test")
			defer store.Close()
			defer gatewayServer.Close()
			defer agentConn.Close()

			req := httptest.NewRequest(test.method, gatewayServer.URL+"/operation", strings.NewReader("payload"))
			req.Host = "outcome.example.test"
			recorder := httptest.NewRecorder()
			var abort any
			func() {
				defer func() { abort = recover() }()
				hub.HandlePublicProxy(recorder, req)
			}()
			if test.mode == "truncated" {
				if abort != http.ErrAbortHandler {
					t.Fatalf("incomplete response did not abort: %v", abort)
				}
			} else if abort != nil {
				panic(abort)
			}
			if recorder.Code != test.status {
				t.Fatalf("HTTP status=%d, want %d: %s", recorder.Code, test.status, recorder.Body.String())
			}
			if test.mode == "response" && recorder.Body.String() != "response" {
				t.Fatalf("complete HTTP response changed: %q", recorder.Body.String())
			}
			if got := requests.Load(); got != test.wantCalls {
				t.Fatalf("upstream request count=%d, want %d", got, test.wantCalls)
			}
			events, err := store.ListAuditFiltered(AuditFilter{Action: ActionProxy, Limit: 10})
			if err != nil || len(events) != 1 || events[0].Status != test.wantAudit {
				t.Fatalf("proxy audit=%#v, error=%v, want %s", events, err, test.wantAudit)
			}
		})
	}
}

func TestProxyDownstreamWriteFailureUsesCancelOutcome(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodGet} {
		for _, terminal := range []string{"disconnect", "cancel", "complete"} {
			t.Run(method+"/"+terminal, func(t *testing.T) {
				var requests atomic.Int32
				upstreamRelease := make(chan struct{})
				var releaseOnce sync.Once
				releaseUpstream := func() { releaseOnce.Do(func() { close(upstreamRelease) }) }
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					_, _ = io.ReadAll(r.Body)
					requests.Add(1)
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte("partial"))
					w.(http.Flusher).Flush()
					select {
					case <-upstreamRelease:
					case <-r.Context().Done():
					}
				}))
				defer upstream.Close()
				defer releaseUpstream()

				store, hub, gatewayServer, agentConn := newProxyOutcomeFixture(t, upstream.URL, "write-failure.example.test")
				defer store.Close()
				defer gatewayServer.Close()
				defer agentConn.Close()

				agent := hub.getAgent("dev", "local")
				request := httptest.NewRequest(method, "http://gateway.test/mutate", strings.NewReader("payload"))
				request.Host = "write-failure.example.test"
				resumeWrite := make(chan struct{})
				var resumeOnce sync.Once
				resume := func() { resumeOnce.Do(func() { close(resumeWrite) }) }
				defer resume()
				writer := &failingProxyResponseWriter{failed: make(chan struct{}), resume: resumeWrite}
				result := make(chan struct{})
				go func() {
					defer close(result)
					defer func() {
						if abort := recover(); abort != http.ErrAbortHandler {
							t.Errorf("expected HTTP abort after downstream write failure, got %v", abort)
						}
					}()
					hub.HandlePublicProxy(writer, request)
				}()

				select {
				case <-writer.failed:
				case <-time.After(time.Second):
					t.Fatal("downstream response writer was not exercised")
				}
				switch terminal {
				case "disconnect":
					_ = agent.conn.Close()
				case "complete":
					releaseUpstream()
					requireEventually(t, time.Second, func() bool {
						agent.pendingMu.Lock()
						defer agent.pendingMu.Unlock()
						for _, queue := range agent.pending {
							select {
							case <-queue.terminal:
								return queue.terminalErr == nil
							default:
							}
						}
						return false
					})
				}
				resume()

				select {
				case <-result:
				case <-time.After(2 * time.Second):
					t.Fatal("proxy did not finish after downstream write failure")
				}
				wantStatus, wantError := "error", "downstream response write failed"
				if method == http.MethodPost && terminal != "complete" {
					wantStatus, wantError = "outcome_unknown", errExecutionOutcomeUnknown.Error()
				}
				events, err := store.ListAuditFiltered(AuditFilter{Action: ActionProxy, Limit: 10})
				if err != nil || len(events) != 1 || events[0].Status != wantStatus || events[0].Error != wantError {
					t.Fatalf("downstream failure audit=%#v, error=%v, want %s: %s", events, err, wantStatus, wantError)
				}
				if terminal != "disconnect" {
					releaseUpstream()
					next := httptest.NewRequest(http.MethodPost, "http://gateway.test/next", nil)
					next.Host = request.Host
					recorder := httptest.NewRecorder()
					hub.HandlePublicProxy(recorder, next)
					wantHTTP, wantCalls := http.StatusOK, int32(2)
					if recorder.Code != wantHTTP || requests.Load() != wantCalls {
						t.Fatalf("next POST status=%d calls=%d, want %d calls=%d: %s",
							recorder.Code, requests.Load(), wantHTTP, wantCalls, recorder.Body.String())
					}
				}
			})
		}
	}
}

type failingProxyResponseWriter struct {
	header   http.Header
	failed   chan struct{}
	resume   <-chan struct{}
	failOnce sync.Once
}

func (w *failingProxyResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *failingProxyResponseWriter) WriteHeader(int) {}

func (w *failingProxyResponseWriter) Write([]byte) (int, error) {
	w.failOnce.Do(func() { close(w.failed) })
	<-w.resume
	return 0, errors.New("downstream response write failed")
}

func (w *failingProxyResponseWriter) Flush() {}

func newProxyOutcomeFixture(
	t *testing.T,
	upstreamURL string,
	host string,
) (*GatewayStore, *GatewayHub, *httptest.Server, io.Closer) {
	t.Helper()
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := store.UpsertProxyRoute(ProxyRouteInput{
		Host: host, Cluster: "dev", Instance: "local",
		Upstream: upstreamURL, Enabled: true,
	}); err != nil {
		store.Close()
		t.Fatalf("create proxy route: %v", err)
	}
	hub := NewGatewayHub(store, GatewayHubOptions{
		AgentLease: time.Second, TargetReconnectGrace: time.Second,
		OperationTimeout: time.Second, TargetQueueTimeout: time.Second,
	})
	mux := http.NewServeMux()
	hub.RegisterRoutes(mux)
	gatewayServer := httptest.NewServer(mux)
	agentGateway := NewGateway()
	agentGateway.SetProxyAllowlist([]string{"127.0.0.1"})
	agentConn := connectProxyTestAgent(t, gatewayServer.URL, agentGateway, "dev", "local")
	requireEventually(t, time.Second, func() bool { return hub.getAgent("dev", "local") != nil })
	return store, hub, gatewayServer, agentConn
}
