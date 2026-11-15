package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/pkg/release"
)

type temporaryHeartbeatError struct{}
type fatalHeartbeatTimeoutError struct{}

func newGatewayAuthorizedAgentServer(t *testing.T, gateway *Gateway) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		gateway.ServeGatewayWebSocketConnWithReady(conn, r.RemoteAddr, nil)
	}))
}

func (temporaryHeartbeatError) Error() string   { return "temporary heartbeat write timeout" }
func (temporaryHeartbeatError) Timeout() bool   { return true }
func (temporaryHeartbeatError) Temporary() bool { return true }

func (fatalHeartbeatTimeoutError) Error() string   { return "fatal heartbeat network write timeout" }
func (fatalHeartbeatTimeoutError) Timeout() bool   { return true }
func (fatalHeartbeatTimeoutError) Temporary() bool { return false }

type fakeWebSocketControlWriter struct {
	writeErr error
	closes   int
	onWrite  func()
}

func (f *fakeWebSocketControlWriter) WriteControl(int, []byte, time.Time) error {
	if f.onWrite != nil {
		f.onWrite()
	}
	return f.writeErr
}

func (f *fakeWebSocketControlWriter) Close() error {
	f.closes++
	return nil
}

func TestWriteHeartbeatPingClosesConnectionAfterUnexplainedWriteFailure(t *testing.T) {
	tests := []struct {
		name             string
		writeErr         error
		dataWriteActive  bool
		dataWriteOverlap bool
		wantKeep         bool
		wantCloses       int
	}{
		{
			name:       "temporary write timeout",
			writeErr:   temporaryHeartbeatError{},
			wantKeep:   false,
			wantCloses: 1,
		},
		{
			name:            "temporary write timeout during data write",
			writeErr:        temporaryHeartbeatError{},
			dataWriteActive: true,
			wantKeep:        true,
			wantCloses:      0,
		},
		{
			name:             "temporary write timeout overlapping completed data write",
			writeErr:         temporaryHeartbeatError{},
			dataWriteOverlap: true,
			wantKeep:         true,
			wantCloses:       0,
		},
		{
			name:             "fatal network timeout overlapping completed data write",
			writeErr:         fatalHeartbeatTimeoutError{},
			dataWriteOverlap: true,
			wantKeep:         false,
			wantCloses:       1,
		},
		{
			name:       "permanent write failure",
			writeErr:   fmt.Errorf("connection closed"),
			wantKeep:   false,
			wantCloses: 1,
		},
		{
			name:       "successful heartbeat",
			wantKeep:   true,
			wantCloses: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dataWriteState websocketDataWriteState
			if tt.dataWriteActive {
				dataWriteState.begin()
				defer dataWriteState.end()
			}
			conn := &fakeWebSocketControlWriter{writeErr: tt.writeErr}
			if tt.dataWriteOverlap {
				conn.onWrite = func() {
					dataWriteState.begin()
					dataWriteState.end()
				}
			}
			if got := writeHeartbeatPing(conn, &dataWriteState, nil, time.Second); got != tt.wantKeep {
				t.Fatalf("keep connection = %t, want %t", got, tt.wantKeep)
			}
			if conn.closes != tt.wantCloses {
				t.Fatalf("close calls = %d, want %d", conn.closes, tt.wantCloses)
			}
		})
	}
}

type fakeWebSocketDataWriter struct {
	deadlines []time.Time
	writeErr  error
	closes    int
}

func (f *fakeWebSocketDataWriter) SetWriteDeadline(deadline time.Time) error {
	f.deadlines = append(f.deadlines, deadline)
	return nil
}

func (f *fakeWebSocketDataWriter) NextWriter(int) (io.WriteCloser, error) {
	return fakeMessageWriter{f}, nil
}

type fakeMessageWriter struct{ f *fakeWebSocketDataWriter }

func (w fakeMessageWriter) Write(p []byte) (int, error) {
	if w.f.writeErr != nil {
		return 0, w.f.writeErr
	}
	return len(p), nil
}

func (w fakeMessageWriter) Close() error { return nil }

func (f *fakeWebSocketDataWriter) Close() error {
	f.closes++
	return nil
}

func TestWriteWebSocketJSONBoundsBlockedWrites(t *testing.T) {
	t.Run("successful write clears deadline", func(t *testing.T) {
		conn := &fakeWebSocketDataWriter{}
		var dataWriteState websocketDataWriteState
		if err := writeWebSocketJSON(conn, &sync.Mutex{}, &dataWriteState, map[string]string{"ok": "true"}, time.Second); err != nil {
			t.Fatalf("write JSON: %v", err)
		}
		last := len(conn.deadlines) - 1
		if last < 1 || conn.deadlines[0].IsZero() || !conn.deadlines[last].IsZero() {
			t.Fatalf("write deadlines = %v, want finite deadlines followed by zero", conn.deadlines)
		}
		if conn.closes != 0 {
			t.Fatalf("close calls = %d, want 0", conn.closes)
		}
		if dataWriteState.sequence.Load()%2 != 0 {
			t.Fatal("data write state remained active after success")
		}
	})

	t.Run("write failure closes connection", func(t *testing.T) {
		conn := &fakeWebSocketDataWriter{writeErr: fmt.Errorf("write timeout")}
		var dataWriteState websocketDataWriteState
		if err := writeWebSocketJSON(conn, &sync.Mutex{}, &dataWriteState, map[string]string{"ok": "false"}, time.Second); err == nil {
			t.Fatal("write JSON succeeded, want failure")
		}
		if len(conn.deadlines) != 1 || conn.deadlines[0].IsZero() {
			t.Fatalf("write deadlines = %v, want one finite deadline", conn.deadlines)
		}
		if conn.closes != 1 {
			t.Fatalf("close calls = %d, want 1", conn.closes)
		}
		if dataWriteState.sequence.Load()%2 != 0 {
			t.Fatal("data write state remained active after failure")
		}
	})
}

func TestReadWebSocketMessageReportsProgressBeforeMessageCompletes(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade websocket: %v", err)
			return
		}
		serverConn <- conn
	}))
	defer ts.Close()

	clientConn, _, err := websocket.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(ts.URL, "http"),
		nil,
	)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer clientConn.Close()
	receiver := <-serverConn
	defer receiver.Close()

	type readResult struct {
		messageType int
		body        []byte
		err         error
	}
	progress := make(chan struct{}, 1)
	result := make(chan readResult, 1)
	go func() {
		messageType, body, err := readWebSocketMessage(receiver, func() {
			select {
			case progress <- struct{}{}:
			default:
			}
		})
		result <- readResult{messageType: messageType, body: body, err: err}
	}()

	writer, err := clientConn.NextWriter(websocket.TextMessage)
	if err != nil {
		t.Fatalf("open message writer: %v", err)
	}
	body := bytes.Repeat([]byte("x"), 64<<10)
	if _, err := writer.Write(body); err != nil {
		t.Fatalf("write partial message: %v", err)
	}

	select {
	case <-progress:
	case <-time.After(time.Second):
		t.Fatal("reader did not report progress before the message completed")
	}
	select {
	case <-result:
		t.Fatal("reader completed before the message writer closed")
	default:
	}

	if err := writer.Close(); err != nil {
		t.Fatalf("close message writer: %v", err)
	}
	select {
	case got := <-result:
		if got.err != nil {
			t.Fatalf("read message: %v", got.err)
		}
		if got.messageType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", got.messageType)
		}
		if !bytes.Equal(got.body, body) {
			t.Fatalf("message body has %d bytes, want %d", len(got.body), len(body))
		}
	case <-time.After(time.Second):
		t.Fatal("reader did not complete after the message writer closed")
	}
}

func TestAgentReverseTunnelReadyOnlyAfterGatewayRegistration(t *testing.T) {
	gw := NewGateway()
	ready := make(chan struct{})
	var readyOnce sync.Once
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		gw.ServeGatewayWebSocketConnWithReady(conn, r.RemoteAddr, func() {
			readyOnce.Do(func() { close(ready) })
		})
	}))
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)
	select {
	case <-ready:
		t.Fatal("initialize must not mark reverse tunnel ready before registry acknowledgement")
	case <-time.After(50 * time.Millisecond):
	}
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "gateway/registered",
	}); err != nil {
		t.Fatalf("send gateway registration acknowledgement: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("gateway registration acknowledgement did not mark reverse tunnel ready")
	}
}

func TestAgentWebSocketFileReadWrite(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	callTool(t, conn, "doops_file_write", map[string]interface{}{
		"session_id": "files",
		"path":       "hello.txt",
		"content":    "hello gateway",
	})
	result := callTool(t, conn, "doops_file_read", map[string]interface{}{
		"session_id": "files",
		"path":       "hello.txt",
	})
	if result != "hello gateway" {
		t.Fatalf("read content mismatch: %q", result)
	}
	if _, err := os.Stat(filepath.Join(root, "files", "hello.txt")); err != nil {
		t.Fatalf("expected file in session workspace: %v", err)
	}
}

func TestKnowledgeBaseMarkdownRoundTripAndRejectsEscape(t *testing.T) {
	kb := filepath.Join(t.TempDir(), "kb")
	t.Setenv("DOOPS_KB_ROOT", kb)
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "outside.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatalf("write outside file: %v", err)
	}

	doc := filepath.Join(kb, "测试.md")
	if _, err := handleFileWrite(mustJSON(t, map[string]interface{}{
		"session_id": "files",
		"path":       doc,
		"content":    "hello 知识库",
	})); err != nil {
		t.Fatalf("write knowledge base document: %v", err)
	}
	got, err := handleFileRead(mustJSON(t, map[string]interface{}{
		"session_id": "files",
		"path":       doc,
	}))
	if err != nil {
		t.Fatalf("read knowledge base document: %v", err)
	}
	if got != "hello 知识库" {
		t.Fatalf("knowledge base content = %q", got)
	}
	if _, err := resolveWorkspaceFilePath("files", doc); err == nil {
		t.Fatal("session workspace resolver must still reject absolute knowledge base paths")
	}

	rejects := []string{
		outside,
		filepath.Join(kb, "..", "outside.md"),
		filepath.Join(kb, "nested", "note.md"),
		filepath.Join(kb, "note.txt"),
		filepath.Join(filepath.Dir(kb), "note.md"),
		filepath.Join(kb, "..md"),
	}
	for _, path := range rejects {
		if _, err := handleFileWrite(mustJSON(t, map[string]interface{}{
			"session_id": "files",
			"path":       path,
			"content":    "nope",
		})); err == nil {
			t.Fatalf("expected knowledge base write to reject %s", path)
		}
		if _, err := handleFileRead(mustJSON(t, map[string]interface{}{
			"session_id": "files",
			"path":       path,
		})); err == nil {
			t.Fatalf("expected knowledge base read to reject %s", path)
		}
	}

	link := filepath.Join(kb, "link.md")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := handleFileWrite(mustJSON(t, map[string]interface{}{
		"session_id": "files",
		"path":       link,
		"content":    "pwned",
	})); err == nil {
		t.Fatal("expected symlink knowledge base write to be rejected")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("read outside file: %v", err)
	}
	if string(data) != "secret" {
		t.Fatalf("symlink write escaped knowledge base: %q", data)
	}

	real := filepath.Join(t.TempDir(), "real-kb")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatalf("mkdir real kb: %v", err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked-kb")
	if err := os.Symlink(real, linkedRoot); err != nil {
		t.Fatalf("symlink knowledge root: %v", err)
	}
	t.Setenv("DOOPS_KB_ROOT", linkedRoot)
	if _, err := handleFileWrite(mustJSON(t, map[string]interface{}{
		"session_id": "files",
		"path":       filepath.Join(linkedRoot, "测试.md"),
		"content":    "nope",
	})); err == nil {
		t.Fatal("expected symlinked knowledge base root to be rejected")
	}
	if _, err := os.Stat(filepath.Join(real, "测试.md")); !os.IsNotExist(err) {
		t.Fatalf("write followed symlinked knowledge base root: %v", err)
	}
}

func TestAgentWebSocketFileToolsRejectWorkspaceEscape(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if _, err := handleFileWrite(mustJSON(t, map[string]interface{}{
		"session_id": "files",
		"path":       outside,
		"content":    "nope",
	})); err == nil {
		t.Fatal("expected absolute file write path to be rejected")
	}
	if _, err := handleFileRead(mustJSON(t, map[string]interface{}{
		"session_id": "files",
		"path":       "../outside.txt",
	})); err == nil {
		t.Fatal("expected relative escape read path to be rejected")
	}
}

func TestAgentWebSocketShellSnapshotNoOpDoesNotPanic(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	for _, command := range []string{
		": # kubectl apply -f -f x",
		"kubectl() { return 0; }; kubectl apply -f ",
		"printf 'independent command completed\\n'",
	} {
		result := callToolResult(t, conn, "doops_shell", map[string]interface{}{
			"session_id": "snapshot-noop", "command": command,
		})
		if result["error"] != nil || result["result"] == nil ||
			result["result"].(map[string]interface{})["isError"] == true {
			t.Fatalf("no-op or subsequent command failed: %q: %#v", command, result)
		}
	}
}

func TestAgentMetadataModeReturnsModelStatusWithoutPrompt(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	previousSettingsFile := doagentSettingsFile
	doagentSettingsFile = settingsPath
	t.Cleanup(func() { doagentSettingsFile = previousSettingsFile })
	if err := os.WriteFile(settingsPath, []byte(`{
  "model": "minimax/MiniMax-M3",
  "provider": {
    "minimax": {
      "options": {"apiKey": "real-key", "baseURL": "https://api.minimaxi.com/anthropic", "kind": "anthropic"},
      "models": {"MiniMax-M3": {"name": "MiniMax-M3"}}
    }
  }
}`), 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rpc" {
			http.NotFound(w, r)
			return
		}
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode rpc: %v", err)
		}
		if req["method"] != "initialize" {
			t.Fatalf("metadata mode should only initialize doagent, got method %v", req["method"])
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      req["id"],
			"result": map[string]interface{}{
				"_meta": map[string]interface{}{"model": "MiniMax-M3"},
			},
		})
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	got := callTool(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id": "metadata-smoke",
		"mode":       "metadata",
	})
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(got), &meta); err != nil {
		t.Fatalf("metadata should be JSON, got %q: %v", got, err)
	}
	if meta["status"] != "connected" || meta["provider"] != "minimax" || meta["model"] != "minimax/MiniMax-M3" {
		t.Fatalf("metadata mismatch: %+v", meta)
	}
}

func TestAgentMetadataAwaitingModelStillAdvertisesACP(t *testing.T) {
	settingsPath := filepath.Join(t.TempDir(), "settings.json")
	previousSettingsFile := doagentSettingsFile
	doagentSettingsFile = settingsPath
	t.Cleanup(func() { doagentSettingsFile = previousSettingsFile })
	if err := os.WriteFile(settingsPath, []byte(`{"mcp_servers":[],"permissions":{}}`), 0600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	t.Setenv("DO_AGENT_URL", "http://127.0.0.1:1")

	got, err := NewGateway().handleAgentMetadataWS(context.Background(), "awaiting-model")
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	var meta map[string]interface{}
	if err := json.Unmarshal([]byte(got), &meta); err != nil {
		t.Fatalf("metadata should be JSON, got %q: %v", got, err)
	}
	formats, _ := meta["event_formats"].([]interface{})
	if meta["status"] != "awaiting_model_provider" || meta["event_schema"] != "doops.sh/acp-events/v1" || len(formats) != 1 || formats[0] != "acp" {
		t.Fatalf("awaiting model must still advertise ACP: %+v", meta)
	}
	if meta["model"] != nil && meta["model"] != "" {
		t.Fatalf("awaiting model leaked a model: %+v", meta["model"])
	}
}

func TestAgentHistoryModeWithoutSessionReturnsEmptyHistory(t *testing.T) {
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("history mode without a bound session should not call doagent: %s", r.URL.Path)
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	got := callTool(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id": "empty-history",
		"mode":       "history",
	})
	var history map[string]interface{}
	if err := json.Unmarshal([]byte(got), &history); err != nil {
		t.Fatalf("history should be JSON, got %q: %v", got, err)
	}
	messages, _ := history["messages"].([]interface{})
	if history["source"] != "doagent" || history["sessionId"] != "empty-history" || len(messages) != 0 {
		t.Fatalf("history mismatch: %+v", history)
	}
}

func TestAgentPromptNotificationIncludesAssistantDeltaKind(t *testing.T) {
	prompted := make(chan struct{})
	receivedPrompt := make(chan string, 1)
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			var req map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode rpc: %v", err)
			}
			switch req["method"] {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  doagentAdmissionCapabilities(),
				})
			case "session/new":
				params, _ := req["params"].(map[string]interface{})
				if params["permissionMode"] != "auto" {
					t.Fatalf("doagent sessions must use native auto mode with DoOps tool boundaries: %#v", params)
				}
				if strings.Contains(fmt.Sprint(params["systemPrompt"]), "Current DoOps session_id") {
					t.Fatalf("runtime session leaked into model prompt: %#v", params)
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"sessionId": "doagent-kind"},
				})
			case "session/setMode":
				params, _ := req["params"].(map[string]interface{})
				if params["modeId"] != "auto" {
					t.Fatalf("generic prompt must use native auto mode: %#v", req)
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{},
				})
			case "session/prompt":
				params, _ := req["params"].(map[string]interface{})
				prompt, _ := params["prompt"].(string)
				allowed, _ := params["allowedTools"].([]interface{})
				if strings.Join(interfaceStringValues(allowed), ",") != "Read,glob,grep,Skill,Bash,mcp_doops_k8s_read,mcp_doops_web_read" {
					t.Fatalf("ordinary prompt must expose the Auto diagnostic tool surface: %#v", params)
				}
				if params["responseMode"] != "stream" {
					t.Fatalf("prompt must request SSE terminal delivery: %#v", params)
				}
				receivedPrompt <- prompt
				close(prompted)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result": map[string]interface{}{
						"status":   "accepted",
						"delivery": "sse",
						"turnId":   "turn-kind",
					},
				})
			default:
				t.Fatalf("unexpected rpc method: %v", req["method"])
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-prompted
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","turnId":"turn-kind","content":{"type":"text","text":"hello"}}}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message","turnId":"turn-kind","content":{"type":"text","text":"done"}}}}`)
			fmt.Fprintln(w)
			fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","turnId":"turn-kind","status":"completed","stopReason":"end_turn"}}}`)
			fmt.Fprintln(w)
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	rawArgs, _ := json.Marshal(map[string]interface{}{
		"session_id":  "kind-prompt",
		"instruction": "hello",
	})
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      42,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "doops_agent_prompt",
			"arguments": json.RawMessage(rawArgs),
		},
	}); err != nil {
		t.Fatalf("call prompt: %v", err)
	}

	msg := readNotification(t, conn)
	params, _ := msg["params"].(map[string]interface{})
	if params["kind"] != "assistant_delta" {
		t.Fatalf("expected assistant_delta notification kind, got %#v", params)
	}
	if params["data"] != "hello" {
		t.Fatalf("expected first assistant chunk, got %#v", params["data"])
	}

	for {
		var result map[string]interface{}
		if err := conn.ReadJSON(&result); err != nil {
			t.Fatalf("read prompt result: %v", err)
		}
		if _, ok := result["method"]; ok {
			continue
		}
		toolResult, _ := result["result"].(map[string]interface{})
		content, _ := toolResult["content"].([]interface{})
		if len(content) != 1 {
			t.Fatalf("expected text-only prompt result, got %#v", result)
		}
		if _, ok := toolResult["structuredContent"]; ok {
			t.Fatalf("doops_agent_prompt must not return structured content: %#v", toolResult)
		}
		break
	}

	if prompt := <-receivedPrompt; strings.Contains(prompt, "deploy.sh") {
		t.Fatalf("agent prompt must not inject a deploy.sh generation contract: %s", prompt)
	}
}

func TestAgentPromptRefreshesPermissionModeForReusedSession(t *testing.T) {
	var mu sync.Mutex
	permissionRefreshes := 0
	promptCount := 0
	prompted := make(chan string, 2)
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			var req map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode rpc: %v", err)
				return
			}
			id := req["id"]
			switch req["method"] {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": id,
					"result": doagentAdmissionCapabilities(),
				})
			case "session/new":
				params, _ := req["params"].(map[string]interface{})
				if params["permissionMode"] != "auto" {
					t.Fatalf("DoOps must always establish auto permission mode: %#v", params)
				}
				if _, hasCwd := params["cwd"]; !hasCwd {
					mu.Lock()
					permissionRefreshes++
					mu.Unlock()
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": id,
					"result": map[string]interface{}{"sessionId": "reused-agent-session"},
				})
			case "session/setMode":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": id, "result": map[string]interface{}{},
				})
			case "session/prompt":
				mu.Lock()
				promptCount++
				turnID := fmt.Sprintf("reused-turn-%d", promptCount)
				mu.Unlock()
				prompted <- turnID
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": id,
					"result": map[string]interface{}{"status": "accepted", "turnId": turnID},
				})
			default:
				t.Errorf("unexpected rpc method: %v", req["method"])
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			turnID := <-prompted
			fmt.Fprintf(w, "data: %s\n\n", fmt.Sprintf(`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","turnId":%q,"status":"completed","stopReason":"end_turn"}}}`, turnID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	args := map[string]interface{}{"session_id": "reused-permission", "instruction": "inspect"}
	for range 2 {
		result := callToolResult(t, conn, "doops_agent_prompt", args)
		if _, ok := result["error"]; ok {
			t.Fatalf("reused prompt failed: %#v", result)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if permissionRefreshes != 1 {
		t.Fatalf("expected exactly one permission refresh for the reused session, got %d", permissionRefreshes)
	}
}

func interfaceStringValues(values []interface{}) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		result = append(result, value.(string))
	}
	return result
}

func TestAgentInitializeDoesNotExposeDedicatedCICDReconciliation(t *testing.T) {
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()

	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	}); err != nil {
		t.Fatalf("send initialize: %v", err)
	}

	var response map[string]interface{}
	if err := conn.ReadJSON(&response); err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	result, _ := response["result"].(map[string]interface{})
	capabilities, _ := result["capabilities"].(map[string]interface{})
	for capability := range capabilities {
		if strings.Contains(strings.ToLower(capability), "deployment") {
			t.Fatalf("CI/CD must enter doagent through Ask, not a dedicated tool: %#v", capabilities)
		}
	}
}

// The prompt tells the model which tools to call. Every mcp_* tool it names must be
// one the Agent actually offers in some mode, or the model is sent to a tool that
// does not exist.
func TestAgentSystemPromptNamesOnlyOfferedTools(t *testing.T) {
	prompt, err := os.ReadFile(filepath.Join("..", "..", "skills", "system_prompt.md"))
	if err != nil {
		t.Fatalf("read system prompt: %v", err)
	}
	offered := map[string]bool{}
	for _, apply := range []bool{false, true} {
		for _, skill := range []string{"", "dosql", "doweb"} {
			for _, tool := range doagentAllowedTools(apply, skill) {
				offered[tool] = true
			}
		}
	}
	named := regexp.MustCompile(`mcp_[a-z0-9_]+`).FindAllString(string(prompt), -1)
	if len(named) == 0 {
		t.Fatal("system prompt names no tools")
	}
	for _, tool := range named {
		if !offered[tool] {
			t.Errorf("system prompt names %s, which the Agent never offers", tool)
		}
	}
}

func TestConfiguredDoagentURLUsesEntrypointPort(t *testing.T) {
	t.Setenv("DO_AGENT_URL", "")
	t.Setenv("DO_AGENT_PORT", "7749")
	if got := configuredDoagentURL(); got != "http://127.0.0.1:7749" {
		t.Fatalf("configured doagent URL = %q", got)
	}

	t.Setenv("DO_AGENT_URL", "http://127.0.0.1:9010")
	if got := configuredDoagentURL(); got != "http://127.0.0.1:9010" {
		t.Fatalf("explicit doagent URL = %q", got)
	}
}

func TestShellNotificationIncludesRawKind(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	rawArgs, _ := json.Marshal(map[string]interface{}{
		"session_id": "kind-shell",
		"command":    "printf 'raw-kind\\n'",
	})
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      43,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "doops_shell",
			"arguments": json.RawMessage(rawArgs),
		},
	}); err != nil {
		t.Fatalf("call shell: %v", err)
	}

	msg := readNotification(t, conn)
	params, _ := msg["params"].(map[string]interface{})
	if params["kind"] != "raw" {
		t.Fatalf("expected raw notification kind, got %#v", params)
	}
	if params["data"] != "raw-kind\n" {
		t.Fatalf("expected shell output, got %#v", params["data"])
	}

	for {
		var response map[string]interface{}
		if err := conn.ReadJSON(&response); err != nil {
			t.Fatalf("read shell response: %v", err)
		}
		if response["id"] != float64(43) {
			continue
		}
		if errObj, ok := response["error"]; ok {
			t.Fatalf("shell returned error: %#v", errObj)
		}
		break
	}
}

func TestSubscribeDoagentSSEReturnsErrorWhenStreamEndsWithoutFinalEvent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"partial"}}}}`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	var progress strings.Builder
	err := subscribeDoagentSSE(t.Context(), ts.URL, "sse-no-final", func(evt notificationEvent) {
		progress.WriteString(evt.Data)
	})
	if err == nil {
		t.Fatal("expected error when SSE ends before final event")
	}
	if !strings.Contains(err.Error(), "ended before final") {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := progress.String(); got != "" {
		t.Fatalf("progress mismatch: %q", got)
	}
}

func TestSubscribeDoagentSSEScopesUntaggedEventsToFreshMachineSession(t *testing.T) {
	for _, test := range []struct {
		name          string
		allowUntagged bool
		wantTypes     []string
	}{
		{
			name:          "fresh machine session accepts current untagged event",
			allowUntagged: true,
			wantTypes:     []string{"agent_message", "turn_finished"},
		},
		{
			name:          "reused session rejects untagged event",
			allowUntagged: false,
			wantTypes:     []string{"turn_finished"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/events" {
					t.Fatalf("unexpected path: %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message","replay":true,"content":{"type":"text","text":"stale"}}}}`)
				fmt.Fprintln(w)
				fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message","content":{"type":"text","text":"current"}}}}`)
				fmt.Fprintln(w)
				fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","turnId":"turn-current","status":"completed","stopReason":"end_turn"}}}`)
				fmt.Fprintln(w)
			}))
			defer ts.Close()

			turnIDReady := make(chan string, 1)
			turnIDReady <- "turn-current"
			close(turnIDReady)
			var gotTypes []string
			err := subscribeDoagentSSEWithCollectorReady(
				t.Context(),
				ts.URL,
				"scope-test",
				func(notificationEvent) {},
				func(update map[string]interface{}) error {
					gotTypes = append(gotTypes, fmt.Sprint(update["sessionUpdate"]))
					return nil
				},
				turnIDReady,
				test.allowUntagged,
				true,
				nil,
			)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			if !reflect.DeepEqual(gotTypes, test.wantTypes) {
				t.Fatalf("collected update types = %#v, want %#v", gotTypes, test.wantTypes)
			}
		})
	}
}

func TestSubscribeDoagentSSERejectsLiveUntaggedContentOnlyToolEvidence(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","replay":true,"toolCallId":"stale","toolName":"mcp_doops_helm","status":"completed","input":{"operation":"rollback"},"content":[{"type":"content","content":{"type":"text","text":"stale"}}]}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"current","toolName":"mcp_doops_helm","status":"in_progress","input":{"operation":"upgrade-install","release":"doops-agent","chart":"deploy/helm/doops-agent","namespace":"kz-ops"}}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call_update","toolCallId":"current","toolName":"mcp_doops_helm","status":"completed","content":[{"type":"content","content":{"type":"text","text":"Helm operation complete."}}]}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","turnId":"turn-current","status":"completed","stopReason":"end_turn"}}}`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	turnIDReady := make(chan string, 1)
	turnIDReady <- "turn-current"
	close(turnIDReady)
	collector := newDoagentToolTraceCollector(false)
	err := subscribeDoagentSSEWithCollectorReady(
		t.Context(),
		ts.URL,
		"apply-evidence",
		func(notificationEvent) {},
		collector.collect,
		turnIDReady,
		true,
		true,
		nil,
	)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := collector.requireCompletedToolEvidence(); err == nil ||
		!strings.Contains(err.Error(), "raw_output_missing") {
		t.Fatalf("content-only tool evidence accepted: %v", err)
	}
	if traces := collector.completed(); len(traces) != 0 {
		t.Fatalf("content-only traces accepted as completed evidence: %#v", traces)
	}
}

func TestSubscribeDoagentSSEDoesNotForwardMalformedRawData(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: Authorization: Bearer synthetic-canary`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	var progress strings.Builder
	err := subscribeDoagentSSE(t.Context(), ts.URL, "malformed-sse", func(evt notificationEvent) {
		progress.WriteString(evt.Data)
	})
	if err == nil || !strings.Contains(err.Error(), "doagent_sse_protocol_invalid") {
		t.Fatalf("malformed SSE error = %v", err)
	}
	if strings.Contains(progress.String(), "synthetic-canary") ||
		!strings.Contains(progress.String(), "doagent_sse_protocol_invalid") {
		t.Fatalf("malformed SSE raw data leaked to progress: %q", progress.String())
	}
}

func TestSubscribeDoagentSSESilentStreamEndsOnlyOnTerminalSignal(t *testing.T) {
	const silence = 100 * time.Millisecond
	toolStart := `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"tool_call","toolCallId":"helm-1","toolName":"mcp_doops_helm","status":"in_progress"}}}` + "\n\n"
	finished := `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","status":"completed","stopReason":"end_turn"}}}` + "\n\n"

	for _, test := range []struct {
		name       string
		deadline   time.Duration // caller deadline; 0 means none
		closeAfter bool          // server drops the connection after the silence
		finish     bool          // server sends turn_finished after the silence
		wantErr    string        // empty means success
		minElapsed time.Duration
	}{
		{name: "silent tool then turn finished succeeds", finish: true, minElapsed: silence},
		{name: "silent tool then connection drop fails", closeAfter: true, wantErr: "ended before final", minElapsed: silence},
		{name: "silent tool past caller deadline is canceled", deadline: silence / 2, wantErr: "context deadline exceeded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/events" {
					t.Errorf("unexpected path: %s", r.URL.Path)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				fmt.Fprint(w, toolStart)
				flusher.Flush()
				select {
				case <-time.After(silence):
				case <-r.Context().Done():
					return
				}
				if test.finish {
					fmt.Fprint(w, finished)
					flusher.Flush()
				}
				if !test.closeAfter && !test.finish {
					<-r.Context().Done()
				}
			}))
			defer ts.Close()

			ctx := t.Context()
			if test.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, test.deadline)
				defer cancel()
			}
			started := time.Now()
			err := subscribeDoagentSSE(ctx, ts.URL, "sse-silent", func(notificationEvent) {})
			elapsed := time.Since(started)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("subscribe: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("error = %v, want containing %q", err, test.wantErr)
			}
			if elapsed < test.minElapsed {
				t.Fatalf("returned after %s, before the %s silence ended", elapsed, test.minElapsed)
			}
		})
	}
}

func TestSubscribeDoagentSSEReturnsTurnFinishedError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"failed-turn","update":{"sessionUpdate":"turn_finished","stopReason":"error","error":"Agent error: API error (404 Not Found): vip expired"}}}`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	started := time.Now()
	err := subscribeDoagentSSE(t.Context(), ts.URL, "failed-turn", func(notificationEvent) {})
	if err == nil {
		t.Fatal("expected failed turn to return an error")
	}
	if !strings.Contains(err.Error(), "vip expired") {
		t.Fatalf("turn failure must preserve the actual doagent error, got %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("terminal turn failure must return immediately, took %s", elapsed)
	}
}

func TestSubscribeDoagentSSEPreservesErrorEventMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"error","params":{"error":"error decoding response body"}}`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	var progress strings.Builder
	err := subscribeDoagentSSE(t.Context(), ts.URL, "error-event", func(evt notificationEvent) {
		progress.WriteString(evt.Data)
	})
	if err == nil || !strings.Contains(err.Error(), "error decoding response body") {
		t.Fatalf("error event must preserve the original message, got %v", err)
	}
	if progress.String() != "" {
		t.Fatalf("terminal error must be deferred to the caller, got progress %q", progress.String())
	}
}

func TestSubscribeDoagentSSEWaitsForAuthoritativeFailedTurn(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/events" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message","content":{"type":"text","text":"looks good"}}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"usage_update","inputTokens":10,"outputTokens":5}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","status":"interrupted","stopReason":"context_limit"}}}`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	var progress strings.Builder
	err := subscribeDoagentSSE(t.Context(), ts.URL, "interrupted-turn", func(evt notificationEvent) {
		progress.WriteString(evt.Data)
	})
	if err == nil || !strings.Contains(err.Error(), "context_limit") {
		t.Fatalf("authoritative failed turn must win over agent_message, got %v", err)
	}
	if !strings.Contains(progress.String(), "looks good") {
		t.Fatalf("agent message should remain visible as progress: %q", progress.String())
	}
}

func TestAgentPromptReturnsAdmissionErrorImmediately(t *testing.T) {
	prompted := make(chan struct{})
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			var req map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode rpc: %v", err)
				return
			}
			switch req["method"] {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": req["id"], "result": doagentAdmissionCapabilities(),
				})
			case "session/new":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{"sessionId": "admission-error"},
				})
			case "session/setMode":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"result":  map[string]interface{}{},
				})
			case "session/prompt":
				close(prompted)
				w.WriteHeader(http.StatusAccepted)
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      req["id"],
					"error":   map[string]interface{}{"code": -32003, "message": "queue full"},
				})
			default:
				t.Errorf("unexpected rpc method: %v", req["method"])
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			<-prompted
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	started := time.Now()
	result := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":  "admission-error",
		"instruction": "inspect",
	})
	if !strings.Contains(fmt.Sprint(result), "doagent_prompt_rejected") ||
		strings.Contains(fmt.Sprint(result), "queue full") {
		t.Fatalf("prompt admission error must return only a stable category: %#v", result)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("prompt admission error must not wait for SSE timeout, took %s", elapsed)
	}
}

func TestAgentApplyPreservesVerifiedHelmWhenTurnIsIncomplete(t *testing.T) {
	for _, terminal := range []struct {
		status, stopReason, failureClass string
	}{
		{"failed", "error", "agent_loop_error"},
		{"interrupted", "budget_exceeded", "budget_exceeded"},
		{"interrupted", "context_limit", "context_limit"},
		{"interrupted", "diminishing_returns", "diminishing_returns"},
		{"failed", "goal_evaluation_failed", "goal_evaluation_failed"},
		{"cancelled", "cancelled", ""},
		{"needs_input", "needs_input", ""},
	} {
		t.Run(terminal.stopReason, func(t *testing.T) {
			testAgentApplyTerminalHelmResult(t, false, false, map[string]interface{}{
				"status": terminal.status, "stopReason": terminal.stopReason,
				"failureClass":  terminal.failureClass,
				"failureReason": `{"password":"` + strings.Repeat("synthetic-turn-json-secret", 100) + `"}`,
				"error":         "token: " + strings.Repeat("synthetic-turn-yaml-secret", 100),
			})
		})
	}
}

func TestAgentApplySucceedsWhenTurnAndHelmComplete(t *testing.T) {
	testAgentApplyTerminalHelmResult(t, false, false, map[string]interface{}{
		"status": "completed", "stopReason": "end_turn",
	})
}

func TestAgentApplyKnownHelmFailureDoesNotBecomeUnknown(t *testing.T) {
	testAgentApplyTerminalHelmResult(t, true, false, nil)
}

func TestAgentApplyEmptyHelmArgumentsThenScopeFailureIsTerminal(t *testing.T) {
	testAgentApplyTerminalHelmResult(t, true, true, nil)
}

func testAgentApplyTerminalHelmResult(t *testing.T, toolFailed, invalidArgumentsFirst bool, terminal map[string]interface{}) {
	t.Helper()
	workspace := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", workspace)
	const (
		doopsSessionID = "apply-final-turn-error"
		agentSessionID = "apply-final-turn-error-agent"
		turnID         = "turn-apply-final-turn-error"
	)
	secret := strings.Repeat("b", 64)
	previousSecret := processMutationAuthoritySecret
	processMutationAuthoritySecret = secret
	t.Cleanup(func() {
		processMutationAuthoritySecret = previousSecret
	})

	promptParamsReady := make(chan map[string]interface{}, 1)
	doagent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rpc":
			var request map[string]interface{}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode rpc: %v", err)
				return
			}
			id := request["id"]
			switch request["method"] {
			case "initialize":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0", "id": id, "result": doagentAdmissionCapabilities(),
				})
			case "session/new":
				params, ok := request["params"].(map[string]interface{})
				if !ok {
					t.Fatalf("session/new params = %#v", request["params"])
				}
				if params["cwd"] != "/root/ws/"+doopsSessionID {
					t.Fatalf("apply session must use its bound workspace cwd, got %#v", params["cwd"])
				}
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]interface{}{"sessionId": agentSessionID},
				})
			case "session/setMode":
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      id,
					"result":  map[string]interface{}{},
				})
			case "session/prompt":
				params, ok := request["params"].(map[string]interface{})
				if !ok {
					t.Errorf("prompt params = %#v", request["params"])
					return
				}
				promptParamsReady <- params
				_ = json.NewEncoder(w).Encode(map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      id,
					"result": map[string]interface{}{
						"status": "accepted",
						"turnId": turnID,
					},
				})
			default:
				t.Errorf("unexpected rpc method: %v", request["method"])
			}
		case "/events":
			w.Header().Set("Content-Type", "text/event-stream")
			flusher, ok := w.(http.Flusher)
			if !ok {
				t.Fatal("SSE response does not support flushing")
			}
			flusher.Flush()
			promptParams := <-promptParamsReady
			authorityID, _ := promptParams["mutationAuthorizationId"].(string)
			issuedAt, _ := promptParams["mutationAuthorizationIssuedAt"].(float64)
			contextDigest, _ := promptParams["mutationContextDigest"].(string)
			_, authorityDigest, err := signMutationAuthorization(
				secret,
				agentSessionID,
				authorityID,
				int64(issuedAt),
				doagentAllowedTools(true, ""),
				contextDigest,
			)
			if err != nil {
				t.Errorf("sign mutation authority: %v", err)
				return
			}
			input := map[string]interface{}{
				"operation": "upgrade-install",
				"release":   "doops-agent",
				"chart":     "deploy/helm/doops-agent",
				"namespace": "kz-ops",
			}
			trace := attestedMutationTraceForSession(
				t,
				"mcp_doops_helm",
				input,
				doopsSessionID,
				authorityDigest,
			)
			toolStatus, turnStatus := "completed", "failed"
			stopReason, turnError := "error", "final model response failed after Helm completed"
			if toolFailed {
				toolStatus = "failed"
				failure := "helm_verification_failed"
				if invalidArgumentsFirst {
					failure = "helm_mutation_failed"
				}
				failedResult := map[string]interface{}{
					"isError": true,
					"content": []interface{}{map[string]interface{}{"type": "text", "text": failure}},
				}
				trace.resultJSON, _ = json.Marshal(failedResult)
				trace.AttestedResultDigest, _ = digestRuntimeAttestationJSON(failedResult)
			}
			var rawOutput interface{}
			if err := json.Unmarshal(trace.resultJSON, &rawOutput); err != nil {
				t.Fatalf("decode raw output: %v", err)
			}
			rawInput := map[string]interface{}{
				"toolName": trace.ToolName,
				"args":     input,
			}
			writeEvent := func(update map[string]interface{}) {
				data, err := json.Marshal(map[string]interface{}{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params":  map[string]interface{}{"update": update},
				})
				if err != nil {
					t.Fatalf("encode SSE event: %v", err)
				}
				fmt.Fprintf(w, "data: %s\n\n", data)
				flusher.Flush()
			}
			toolUpdate := map[string]interface{}{
				"sessionUpdate":            "tool_call_update",
				"turnId":                   turnID,
				"toolCallId":               trace.ToolCallID,
				"toolName":                 trace.ToolName,
				"status":                   toolStatus,
				"rawInput":                 rawInput,
				"rawOutput":                rawOutput,
				"attestationSchemaVersion": trace.AttestationSchema,
				"executionMode":            trace.ExecutionMode,
				"mutationAuthorized":       true,
				"mutationAuthorityDigest":  trace.MutationAuthorityDigest,
				"attestedTool":             trace.AttestedTool,
				"evidenceKind":             trace.EvidenceKind,
				"evidenceSubject":          trace.EvidenceSubject,
				"canonicalScope":           trace.CanonicalScope,
				"scopeDigest":              trace.ScopeDigest,
				"inputDigest":              trace.InputDigest,
				"resultDigest":             trace.AttestedResultDigest,
			}
			if invalidArgumentsFirst {
				rejected := callK8SMCPTool(t.Context(), k8sMCPRequest{
					ID: 10, Params: json.RawMessage(`{"name":"helm","arguments":{}}`),
				}, doopsSessionID)
				if rejected.Error != nil {
					t.Errorf("Helm argument rejection must be attestable: %#v", rejected.Error)
					return
				}
				emptyInput := map[string]interface{}{}
				rejectedTrace := attestedMutationTraceForSession(t, "mcp_doops_helm", emptyInput, doopsSessionID, authorityDigest)
				rejectedUpdate := make(map[string]interface{}, len(toolUpdate))
				for key, value := range toolUpdate {
					rejectedUpdate[key] = value
				}
				rejectedUpdate["toolCallId"] = "invalid-arguments"
				rejectedUpdate["rawInput"] = map[string]interface{}{"toolName": trace.ToolName, "args": emptyInput}
				rejectedUpdate["inputDigest"] = rejectedTrace.InputDigest
				rejectedUpdate["rawOutput"] = rejected.Result
				rejectedUpdate["resultDigest"], err = digestRuntimeAttestationJSON(rejected.Result)
				if err != nil {
					t.Errorf("digest rejected Helm result: %v", err)
					return
				}
				writeEvent(rejectedUpdate)
			}
			writeEvent(map[string]interface{}{
				"sessionUpdate": "tool_call",
				"turnId":        turnID,
				"toolCallId":    trace.ToolCallID,
				"toolName":      trace.ToolName,
				"status":        "in_progress",
				"rawInput":      rawInput,
			})
			writeEvent(toolUpdate)
			turnUpdate := map[string]interface{}{
				"sessionUpdate": "turn_finished",
				"turnId":        turnID,
				"status":        turnStatus,
				"stopReason":    stopReason,
				"error":         turnError,
			}
			if terminal != nil {
				delete(turnUpdate, "error")
				for key, value := range terminal {
					turnUpdate[key] = value
				}
			}
			writeEvent(turnUpdate)
		default:
			http.NotFound(w, r)
		}
	}))
	defer doagent.Close()
	t.Setenv("DO_AGENT_URL", doagent.URL)

	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()
	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_agent_prompt", map[string]interface{}{
		"session_id":      doopsSessionID,
		"instruction":     "upgrade doops-agent and a second release, then verify both",
		"operation":       "apply",
		"response_format": "json",
	})
	if strings.Contains(fmt.Sprint(result), "synthetic-turn-json-secret") ||
		strings.Contains(fmt.Sprint(result), "synthetic-turn-yaml-secret") {
		t.Fatalf("turn error leaked credential-like text: %#v", result)
	}
	if result["error"] != nil {
		t.Fatalf("verified Helm mutation was returned as RPC error: %#v", result)
	}
	response, ok := result["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("missing successful tool result: %#v", result)
	}
	if toolFailed {
		if response["isError"] != true || !strings.Contains(fmt.Sprint(response), "doagent_apply_tool_failed") {
			t.Fatalf("known Helm failure must remain a failed terminal result: %#v", response)
		}
		if strings.Contains(fmt.Sprint(result), "execution_outcome_unknown") {
			t.Fatalf("known Helm failure quarantined the target: %#v", result)
		}
		structured, _ := response["structuredContent"].(map[string]interface{})
		if structured["verified"] != true || structured["outcome"] != "failed" {
			t.Fatalf("known failure lost the verified receipt: %#v", response)
		}
		followup := callToolResult(t, conn, "doops_shell", map[string]interface{}{
			"session_id": doopsSessionID, "command": "printf followup-ok",
		})
		if followup["error"] != nil {
			t.Fatalf("follow-up command failed after terminal tool failure: %#v", followup)
		}
		return
	}
	if terminal != nil && terminal["status"] != "completed" {
		if response["isError"] != true || !strings.Contains(fmt.Sprint(response), "doagent_apply_incomplete") {
			t.Fatalf("partially completed request was reported as successful: %#v", response)
		}
		structured, _ := response["structuredContent"].(map[string]interface{})
		if structured["verified"] != true || structured["outcome"] != "failed" {
			t.Fatalf("incomplete turn must keep verified receipts without claiming success: %#v", structured)
		}
		turn, _ := structured["turn"].(map[string]interface{})
		if turn["status"] != terminal["status"] || turn["stopReason"] != terminal["stopReason"] {
			t.Fatalf("incomplete turn lost its terminal reason: %#v", turn)
		}
		if terminal["failureClass"] != "" && turn["failureClass"] != terminal["failureClass"] {
			t.Fatalf("incomplete turn lost its failure class: %#v", turn)
		}
		mutations, _ := structured["mutations"].([]interface{})
		if len(mutations) != 1 {
			t.Fatalf("completed mutation missing: %#v", mutations)
		}
		mutation, _ := mutations[0].(map[string]interface{})
		receipt, _ := mutation["result"].(map[string]interface{})
		if mutation["verified"] != true || mutation["status"] != "completed" || receipt["isError"] != false {
			t.Fatalf("successful Helm mutation was rewritten as failed: %#v", mutation)
		}
		if strings.Contains(fmt.Sprint(result), "execution_outcome_unknown") {
			t.Fatalf("known incomplete turn was misclassified as an unknown mutation: %#v", result)
		}
		return
	}
	if response["isError"] == true {
		t.Fatalf("verified Helm mutation was returned as tool error: %#v", response)
	}
	content, _ := response["content"].([]interface{})
	if len(content) == 0 || !strings.Contains(fmt.Sprint(content[0]), "Operation complete") {
		if !strings.Contains(fmt.Sprint(response["structuredContent"]), "doops.verified-mutation") {
			t.Fatalf("unexpected successful tool content: %#v", response)
		}
	}
	structured, _ := response["structuredContent"].(map[string]interface{})
	if structured["kind"] != "doops.verified-mutation" || structured["verified"] != true {
		t.Fatalf("verified mutation structured result missing: %#v", response)
	}
}

func TestAgentWebSocketCancelKillsShellCommand(t *testing.T) {
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	rawArgs, _ := json.Marshal(map[string]interface{}{
		"command":    "sleep 30",
		"session_id": "cancel-shell",
	})
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      float64(42),
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      "doops_shell",
			"arguments": json.RawMessage(rawArgs),
		},
	}); err != nil {
		t.Fatalf("send shell call: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      float64(43),
		"method":  "tools/cancel",
		"params":  map[string]interface{}{"id": float64(42)},
	}); err != nil {
		t.Fatalf("send shell cancel: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	var sawCancelAck bool
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read cancel response: %v", err)
		}
		if method, _ := msg["method"].(string); method != "" {
			continue
		}
		switch id, _ := msg["id"].(float64); id {
		case 43:
			result, _ := msg["result"].(map[string]interface{})
			if result["canceled"] != true {
				t.Fatalf("expected cancel ack, got %#v", msg)
			}
			sawCancelAck = true
		case 42:
			errObj, _ := msg["error"].(map[string]interface{})
			if !strings.Contains(fmt.Sprint(errObj["message"]), "operation canceled") {
				t.Fatalf("expected operation canceled, got %#v", msg)
			}
			if !sawCancelAck {
				t.Fatal("tool result arrived before cancel ack")
			}
			return
		}
	}
}

func TestAgentWebSocketRejectsBlockedShellCommand(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_shell", map[string]interface{}{
		"session_id": "blocked-shell",
		"command":    "rm -rf /",
	})
	if result["error"] == nil {
		t.Fatalf("expected blocked command error, got %#v", result)
	}
}

func TestAgentWebSocketRejectsDockerNewlineInjection(t *testing.T) {
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	result := callToolResult(t, conn, "doops_docker", map[string]interface{}{
		"session_id": "docker",
		"command":    "ps\nid",
	})
	if result["error"] == nil {
		t.Fatalf("expected docker newline injection error, got %#v", result)
	}
}

func TestAgentWebSocketReadLimitRejectsOversizedFrame(t *testing.T) {
	t.Setenv("DOOPS_MAX_WS_MESSAGE_BYTES", "128")
	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	large := strings.Repeat("x", 512)
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      99,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name": "doops_shell",
			"arguments": map[string]interface{}{
				"session_id": "large",
				"command":    large,
			},
		},
	}); err != nil {
		t.Fatalf("write oversized frame: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	var msg map[string]interface{}
	if err := conn.ReadJSON(&msg); err == nil {
		t.Fatalf("expected oversized frame to close connection, got %#v", msg)
	}
}

func TestBackgroundTaskLogPathStaysInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	gw := NewGateway()
	if _, err := gw.submitBgTask("bg", "true", "/tmp/outside.log"); err == nil {
		t.Fatal("expected absolute background log path to be rejected")
	}
	task, err := gw.submitBgTask("bg", "printf ok", "logs/task.log")
	if err != nil {
		t.Fatalf("submit bg task: %v", err)
	}
	if !strings.HasPrefix(filepath.Clean(task.LogPath), filepath.Join(root, "bg")+string(os.PathSeparator)) {
		t.Fatalf("log path escaped workspace: %s", task.LogPath)
	}
}

func TestWSGitResponseWriterFlushesSmallWritesImmediately(t *testing.T) {
	var bodyFrames [][]byte
	writer := newWSGitResponseWriter(float64(42), func(value interface{}) {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		var message map[string]interface{}
		if err := json.Unmarshal(data, &message); err != nil {
			t.Fatalf("decode message: %v", err)
		}
		if method, _ := message["method"].(string); method != "git/body" {
			return
		}
		params, _ := message["params"].(map[string]interface{})
		dataB64, _ := params["data_b64"].(string)
		if dataB64 == "" {
			return
		}
		body, err := base64.StdEncoding.DecodeString(dataB64)
		if err != nil {
			t.Fatalf("decode body frame: %v", err)
		}
		bodyFrames = append(bodyFrames, body)
	})

	if _, err := writer.Write([]byte("progress")); err != nil {
		t.Fatalf("write small body: %v", err)
	}
	writer.Flush()
	if len(bodyFrames) != 1 {
		t.Fatalf("expected one immediate body frame, got %d", len(bodyFrames))
	}
	if got := string(bodyFrames[0]); got != "progress" {
		t.Fatalf("unexpected body frame: %q", got)
	}
}

func TestAgentWebSocketHandlesGitHTTPOverTunnel(t *testing.T) {
	gw := NewGateway()
	gw.gitHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery, "GET /git/release.git/info/refs?service=git-upload-pack"; got != want {
			t.Fatalf("unexpected git request: want %s, got %s", want, got)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("git-agent-ok"))
	})
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	id := float64(42)
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "git/http",
		"params": map[string]interface{}{
			"method":         "GET",
			"path":           "/git/release.git/info/refs",
			"raw_query":      "service=git-upload-pack",
			"content_length": int64(0),
		},
	}); err != nil {
		t.Fatalf("send git/http: %v", err)
	}
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "git/body",
		"params":  map[string]interface{}{"id": id, "eof": true},
	}); err != nil {
		t.Fatalf("send git/body eof: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	var body bytes.Buffer
	var status int
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read git response: %v", err)
		}
		if errObj, ok := msg["error"]; ok {
			t.Fatalf("git/http returned error: %#v", errObj)
		}
		if method, _ := msg["method"].(string); method != "" {
			params, _ := msg["params"].(map[string]interface{})
			switch method {
			case "git/response":
				status = int(params["status"].(float64))
			case "git/body":
				if dataB64, _ := params["data_b64"].(string); dataB64 != "" {
					data, err := base64.StdEncoding.DecodeString(dataB64)
					if err != nil {
						t.Fatalf("decode git body: %v", err)
					}
					body.Write(data)
				}
			}
			continue
		}
		if gotID, _ := msg["id"].(float64); gotID == id {
			break
		}
	}
	if status != http.StatusCreated {
		t.Fatalf("unexpected git status: %d", status)
	}
	if body.String() != "git-agent-ok" {
		t.Fatalf("unexpected git body: %q", body.String())
	}
}

func TestAgentWebSocketCancelsGitHTTPOverTunnel(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	gw := NewGateway()
	gw.gitHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	})
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	id := float64(44)
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "git/http",
		"params": map[string]interface{}{
			"method":         "GET",
			"path":           "/git/release.git/info/refs",
			"raw_query":      "service=git-upload-pack",
			"content_length": int64(0),
		},
	}); err != nil {
		t.Fatalf("send git/http: %v", err)
	}
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "git/body",
		"params":  map[string]interface{}{"id": id, "eof": true},
	}); err != nil {
		t.Fatalf("send git/body eof: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("git handler did not start")
	}

	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "git/cancel",
		"params":  map[string]interface{}{"id": id},
	}); err != nil {
		t.Fatalf("send git/cancel: %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("git handler context was not canceled")
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer conn.SetReadDeadline(time.Time{})
	for {
		var message map[string]interface{}
		if err := conn.ReadJSON(&message); err != nil {
			t.Fatalf("read git/cancel response: %v", err)
		}
		if gotID, _ := message["id"].(float64); gotID != id {
			continue
		}
		result, _ := message["result"].(map[string]interface{})
		if canceled, _ := result["canceled"].(bool); !canceled {
			t.Fatalf("unexpected git/cancel response: %#v", message)
		}
		return
	}
}

func TestAgentWebSocketGitHTTPStripsExternalAuthorization(t *testing.T) {
	gw := NewGateway()
	gw.gitHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected external authorization to be stripped, got %q", got)
		}
		if got := r.Header.Get("X-Doops-Key"); got != "" {
			t.Fatalf("expected external gateway auth header to be stripped, got %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	wsURL := "ws" + ts.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	id := float64(43)
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "git/http",
		"params": map[string]interface{}{
			"method":         "GET",
			"path":           "/git/release.git/info/refs",
			"raw_query":      "service=git-receive-pack",
			"content_length": int64(0),
			"header": map[string][]string{
				"Authorization": {"Bearer gateway-user-token"},
				"X-Doops-Key":   {"gateway-user-token"},
			},
		},
	}); err != nil {
		t.Fatalf("send git/http: %v", err)
	}
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "git/body",
		"params":  map[string]interface{}{"id": id, "eof": true},
	}); err != nil {
		t.Fatalf("send git/body eof: %v", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read git response: %v", err)
		}
		if errObj, ok := msg["error"]; ok {
			t.Fatalf("git/http returned error: %#v", errObj)
		}
		if gotID, _ := msg["id"].(float64); gotID == id {
			break
		}
	}
}

func TestFileReadRejectsLargeFiles(t *testing.T) {
	t.Setenv("DOOPS_MAX_FILE_READ_BYTES", "8")
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	path := filepath.Join(root, "large", "large.txt")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte("0123456789"), 0644); err != nil {
		t.Fatalf("write large fixture: %v", err)
	}

	_, err := handleFileRead(mustJSON(t, map[string]interface{}{"session_id": "large", "path": "large.txt"}))
	if err == nil || !strings.Contains(err.Error(), "viewing small text files only") {
		t.Fatalf("expected small-text-only error, got %v", err)
	}
}

func TestFileReadRejectsBinaryFiles(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	path := filepath.Join(root, "binary", "archive.tgz")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(path, []byte{0x1f, 0x8b, 0x08, 0x00, 0x61, 0x00}, 0644); err != nil {
		t.Fatalf("write binary fixture: %v", err)
	}

	_, err := handleFileRead(mustJSON(t, map[string]interface{}{"session_id": "binary", "path": "archive.tgz"}))
	if err == nil || !strings.Contains(err.Error(), "looks like binary data") {
		t.Fatalf("expected binary rejection, got %v", err)
	}
}

func TestWorkspacePullBundleBeginAndChunk(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	session := "pull-smoke"
	ws := filepath.Join(root, session)
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ws, "course.tgz"), []byte("course bundle content"), 0644); err != nil {
		t.Fatalf("write workspace file: %v", err)
	}

	beginJSON, err := handleWorkspacePullBegin(mustJSON(t, map[string]interface{}{"session_id": session}))
	if err != nil {
		t.Fatalf("pull begin: %v", err)
	}
	var begin workspacePullBeginResult
	if err := json.Unmarshal([]byte(beginJSON), &begin); err != nil {
		t.Fatalf("decode pull begin: %v", err)
	}
	if begin.BundleID == "" || begin.Size <= 0 || begin.SHA256 == "" {
		t.Fatalf("incomplete bundle metadata: %+v", begin)
	}

	chunkJSON, err := handleWorkspacePullChunk(mustJSON(t, map[string]interface{}{
		"bundle_id": begin.BundleID,
		"offset":    0,
		"limit":     32,
	}))
	if err != nil {
		t.Fatalf("pull chunk: %v", err)
	}
	var chunk workspacePullChunkResult
	if err := json.Unmarshal([]byte(chunkJSON), &chunk); err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	if chunk.BundleID != begin.BundleID || chunk.NextOffset <= 0 || chunk.DataB64 == "" {
		t.Fatalf("bad chunk metadata: %+v", chunk)
	}
	if _, err := base64.StdEncoding.DecodeString(chunk.DataB64); err != nil {
		t.Fatalf("decode chunk data: %v", err)
	}
}

func TestAgentWebSocketWorkspaceChunkUpload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)

	gw := NewGateway()
	ts := newGatewayAuthorizedAgentServer(t, gw)
	defer ts.Close()

	conn := dialAgentTestWS(t, ts.URL)
	defer conn.Close()
	initializeAgentTestWS(t, conn)

	archive := tinyTarGz(t, "hello.txt", "hello chunked push\n")
	commit := sha256Hex(archive)
	begin := callTool(t, conn, "doops_workspace_begin", map[string]interface{}{
		"session_id": "smoke",
		"commit":     commit,
	})
	chunk := base64.StdEncoding.EncodeToString(archive)
	callTool(t, conn, "doops_workspace_chunk", map[string]interface{}{
		"upload_id": begin,
		"seq":       0,
		"data_b64":  chunk,
	})
	callTool(t, conn, "doops_workspace_commit", map[string]interface{}{
		"upload_id":  begin,
		"session_id": "smoke",
		"commit":     commit,
	})

	got, err := os.ReadFile(filepath.Join(root, "smoke", "hello.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "hello chunked push\n" {
		t.Fatalf("extracted content mismatch: %q", got)
	}
	ready, err := os.ReadFile(filepath.Join(root, "smoke", ".doops-ready"))
	if err != nil {
		t.Fatalf("read ready file: %v", err)
	}
	if string(bytes.TrimSpace(ready)) != commit {
		t.Fatalf("ready commit mismatch: %q", ready)
	}
}

func TestWorkspaceUploadRejectsOutOfOrderChunks(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)

	archive := tinyTarGz(t, "hello.txt", "ordered\n")
	commit := sha256Hex(archive)
	uploadID, err := handleWorkspaceBegin(mustJSON(t, map[string]interface{}{
		"session_id": "ordered",
		"commit":     commit,
	}))
	if err != nil {
		t.Fatalf("begin upload: %v", err)
	}

	_, err = handleWorkspaceChunk(mustJSON(t, map[string]interface{}{
		"upload_id": uploadID,
		"seq":       1,
		"data_b64":  base64.StdEncoding.EncodeToString(archive),
	}))
	if err == nil || !strings.Contains(err.Error(), "unexpected chunk sequence") {
		t.Fatalf("expected out-of-order chunk error, got %v", err)
	}
}

func TestWorkspaceCommitRejectsSHA256MismatchAndCleansUpload(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)

	archive := tinyTarGz(t, "hello.txt", "checksum\n")
	wrongCommit := strings.Repeat("0", 64)
	uploadID, err := handleWorkspaceBegin(mustJSON(t, map[string]interface{}{
		"session_id": "checksum",
		"commit":     wrongCommit,
	}))
	if err != nil {
		t.Fatalf("begin upload: %v", err)
	}
	if _, err := handleWorkspaceChunk(mustJSON(t, map[string]interface{}{
		"upload_id": uploadID,
		"seq":       0,
		"data_b64":  base64.StdEncoding.EncodeToString(archive),
	})); err != nil {
		t.Fatalf("chunk upload: %v", err)
	}

	_, err = handleWorkspaceCommit(mustJSON(t, map[string]interface{}{
		"upload_id":  uploadID,
		"session_id": "checksum",
		"commit":     wrongCommit,
	}))
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got %v", err)
	}
	if _, statErr := os.Stat(uploadArchivePath(uploadID)); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed archive to be cleaned, stat err=%v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "checksum")); !os.IsNotExist(statErr) {
		t.Fatalf("expected destination not to be created on checksum failure, stat err=%v", statErr)
	}
}

func TestWorkspaceUploadRejectsSizeLimit(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("DOOPS_MAX_WORKSPACE_UPLOAD_BYTES", "8")

	uploadID, err := handleWorkspaceBegin(mustJSON(t, map[string]interface{}{
		"session_id": "too-big",
		"commit":     strings.Repeat("1", 64),
	}))
	if err != nil {
		t.Fatalf("begin upload: %v", err)
	}
	_, err = handleWorkspaceChunk(mustJSON(t, map[string]interface{}{
		"upload_id": uploadID,
		"seq":       0,
		"data_b64":  base64.StdEncoding.EncodeToString([]byte("123456789")),
	}))
	if err == nil || !strings.Contains(err.Error(), "upload exceeds") {
		t.Fatalf("expected upload limit error, got %v", err)
	}
}

func TestWorkspaceCommitRejectsExpandedSizeLimit(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
	t.Setenv("DOOPS_MAX_WORKSPACE_UPLOAD_BYTES", "512")

	archive := tinyTarGz(t, "large.txt", strings.Repeat("A", 4096))
	commit := sha256Hex(archive)
	uploadID, err := handleWorkspaceBegin(mustJSON(t, map[string]interface{}{
		"session_id": "expanded",
		"commit":     commit,
	}))
	if err != nil {
		t.Fatalf("begin upload: %v", err)
	}
	if _, err := handleWorkspaceChunk(mustJSON(t, map[string]interface{}{
		"upload_id": uploadID,
		"seq":       0,
		"data_b64":  base64.StdEncoding.EncodeToString(archive),
	})); err != nil {
		t.Fatalf("chunk upload: %v", err)
	}

	_, err = handleWorkspaceCommit(mustJSON(t, map[string]interface{}{
		"upload_id":  uploadID,
		"session_id": "expanded",
		"commit":     commit,
	}))
	if err == nil || !strings.Contains(err.Error(), "extracted workspace exceeds") {
		t.Fatalf("expected expanded size limit error, got %v", err)
	}
	if _, statErr := os.Stat(uploadArchivePath(uploadID)); !os.IsNotExist(statErr) {
		t.Fatalf("expected failed archive to be cleaned, stat err=%v", statErr)
	}
}

func TestCleanupExpiredWorkspaceUploads(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	uploadID := "upl_" + strings.Repeat("a", 32)
	if err := os.MkdirAll(uploadDir(), 0700); err != nil {
		t.Fatalf("mkdir upload dir: %v", err)
	}
	if err := os.WriteFile(uploadArchivePath(uploadID), []byte("stale"), 0600); err != nil {
		t.Fatalf("write stale archive: %v", err)
	}
	if err := saveUploadMeta(uploadID, workspaceUploadMeta{
		SessionID: "stale",
		Commit:    strings.Repeat("2", 64),
		NextSeq:   0,
		Size:      0,
		CreatedAt: time.Now().UTC().Add(-48 * time.Hour),
	}); err != nil {
		t.Fatalf("write stale meta: %v", err)
	}

	if err := cleanupExpiredUploads(24 * time.Hour); err != nil {
		t.Fatalf("cleanup expired uploads: %v", err)
	}
	if _, err := os.Stat(uploadArchivePath(uploadID)); !os.IsNotExist(err) {
		t.Fatalf("expected stale archive removed, stat err=%v", err)
	}
	if _, err := os.Stat(uploadMetaPath(uploadID)); !os.IsNotExist(err) {
		t.Fatalf("expected stale metadata removed, stat err=%v", err)
	}
}

func dialAgentTestWS(t *testing.T, serverURL string) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + serverURL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial ws: %v", err)
	}
	return conn
}

func initializeAgentTestWS(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
	}); err != nil {
		t.Fatalf("send initialize: %v", err)
	}
	var resp map[string]interface{}
	if err := conn.ReadJSON(&resp); err != nil {
		t.Fatalf("read initialize: %v", err)
	}
	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		t.Fatalf("initialize failed: %#v", resp)
	}
	if strings.TrimSpace(fmt.Sprint(result["runtimeId"])) == "" {
		t.Fatalf("initialize result must identify the agent process: %#v", resp)
	}
	serverInfo, _ := result["serverInfo"].(map[string]interface{})
	releaseCommit, _ := serverInfo["releaseCommit"].(string)
	version, _ := serverInfo["version"].(string)
	if version != release.Version || releaseCommit != release.Commit {
		t.Fatalf("initialize result must expose release version=%q and commit=%q: %#v", release.Version, release.Commit, resp)
	}
	capabilities, _ := result["capabilities"].(map[string]interface{})
	protocols, _ := capabilities["protocols"].(map[string]interface{})
	if !credentialPlanCapabilityCompatible(protocols["credentialPlan"]) {
		t.Fatalf("initialize result must advertise credential plan v2: %#v", resp)
	}
	if !structuredSSHCapabilityCompatible(protocols[structuredSSHProtocol]) {
		t.Fatalf("initialize result must advertise structured SSH: %#v", resp)
	}
}

func callTool(t *testing.T, conn *websocket.Conn, tool string, args map[string]interface{}) string {
	t.Helper()
	msg := callToolResult(t, conn, tool, args)
	if errObj, ok := msg["error"]; ok {
		t.Fatalf("%s returned error: %#v", tool, errObj)
	}
	result, _ := msg["result"].(map[string]interface{})
	if isErr, ok := result["isError"]; ok && isErr == true {
		t.Fatalf("%s returned tool error: %#v", tool, result)
	}
	content, _ := result["content"].([]interface{})
	if len(content) == 0 {
		return ""
	}
	item, _ := content[0].(map[string]interface{})
	text, _ := item["text"].(string)
	return text
}

func callToolResult(t *testing.T, conn *websocket.Conn, tool string, args map[string]interface{}) map[string]interface{} {
	t.Helper()
	rawArgs, _ := json.Marshal(args)
	id := float64(len(tool) + len(rawArgs))
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params": map[string]interface{}{
			"name":      tool,
			"arguments": json.RawMessage(rawArgs),
		},
	}); err != nil {
		t.Fatalf("call %s: %v", tool, err)
	}
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read %s response: %v", tool, err)
		}
		if _, ok := msg["method"]; ok {
			continue
		}
		return msg
	}
}

func readNotification(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	for {
		var msg map[string]interface{}
		if err := conn.ReadJSON(&msg); err != nil {
			t.Fatalf("read notification: %v", err)
		}
		if method, _ := msg["method"].(string); method == "notifications/message" {
			return msg
		}
	}
}

func tinyTarGz(t *testing.T, name, content string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	data := []byte(content)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0644, Size: int64(len(data))}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(data); err != nil {
		t.Fatalf("write tar data: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return buf.Bytes()
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func mustJSON(t *testing.T, v interface{}) json.RawMessage {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal json: %v", err)
	}
	return data
}

func TestSanitizeSecretTextRedactsCredentialFields(t *testing.T) {
	raw := "authorization=Bearer synthetic cookie=session-canary dbPassword=database-canary githubToken=token-canary " +
		"https://user:url-canary@example.test/repo.git?access_token=query-canary Basic YmFzaWMtY2FuYXJ5 " +
		"https://storage.example.test/object?X-Goog-Signature=signature-canary&X-Amz-Security-Token=security-token-canary&githubToken=github-query-canary " +
		"ghp_" + strings.Repeat("a", 40) + " github_pat_" + strings.Repeat("b", 82) + " AKIA" + strings.Repeat("A", 16) + " " +
		"-----BEGIN PRIVATE KEY-----\npem-canary\n-----END PRIVATE KEY-----"
	sanitized := sanitizeSecretText(raw)
	for _, secret := range []string{
		"synthetic", "session-canary", "database-canary", "token-canary",
		"url-canary", "query-canary", "signature-canary", "security-token-canary",
		"github-query-canary", "YmFzaWMtY2FuYXJ5", "pem-canary",
		"ghp_", "github_pat_", "AKIA",
	} {
		if strings.Contains(sanitized, secret) {
			t.Fatalf("sanitized text retained %q: %s", secret, sanitized)
		}
	}
}

func TestMarshalDoagentHistorySanitizesNestedMessages(t *testing.T) {
	history, err := marshalDoagentHistory("history-session", []interface{}{
		map[string]interface{}{
			"role":  "assistant",
			"token": "opaque-history-canary",
			"content": []interface{}{
				map[string]interface{}{"text": "https://storage.example.test/object?signature=history-canary"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	if strings.Contains(history, "history-canary") ||
		strings.Contains(history, "opaque-history-canary") ||
		!strings.Contains(history, "REDACTED") {
		t.Fatalf("history was not sanitized: %s", history)
	}
}

func TestSubscribeDoagentSSESanitizesAgentMessagesBeforeCollectorAndProgress(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"https://storage.example.test/object?X-Amz-Signature="}}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"stream-canary"}}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","status":"completed","stopReason":"end_turn"}}}`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	var progress strings.Builder
	var collected string
	err := subscribeDoagentSSEWithCollector(
		t.Context(),
		ts.URL,
		"sse-sanitize",
		func(evt notificationEvent) { progress.WriteString(evt.Data) },
		func(update map[string]interface{}) error {
			data, _ := json.Marshal(update)
			collected += string(data)
			return nil
		},
	)
	if err != nil {
		t.Fatalf("subscribe sanitized SSE: %v", err)
	}
	if strings.Contains(progress.String(), "stream-canary") ||
		strings.Contains(collected, "stream-canary") ||
		!strings.Contains(progress.String(), "REDACTED") {
		t.Fatalf("raw agent message escaped sanitization: progress=%q collector=%q", progress.String(), collected)
	}
}

func TestSubscribeDoagentSSEDefersStructuredAgentMessage(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message","content":{"type":"text","text":"{\"kind\":\"DeploymentRun\",\"subject\":\"https://storage.example.test/object?X-Amz-Signature=stream-canary\"}"}}}}`)
		fmt.Fprintln(w)
		fmt.Fprintln(w, `data: {"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"turn_finished","status":"completed","stopReason":"end_turn"}}}`)
		fmt.Fprintln(w)
	}))
	defer ts.Close()

	var progress strings.Builder
	var collected []string
	err := subscribeDoagentSSEWithCollectorReady(
		t.Context(),
		ts.URL,
		"structured-no-forward",
		func(evt notificationEvent) { progress.WriteString(evt.Data) },
		func(update map[string]interface{}) error {
			collected = append(collected, fmt.Sprint(update["sessionUpdate"]))
			return nil
		},
		nil,
		false,
		false,
		nil,
	)
	if err != nil {
		t.Fatalf("subscribe structured SSE: %v", err)
	}
	if strings.Contains(progress.String(), "DeploymentRun") ||
		strings.Contains(progress.String(), "stream-canary") {
		t.Fatalf("unvalidated structured result was forwarded: %s", progress.String())
	}
	if !reflect.DeepEqual(collected, []string{"agent_message", "turn_finished"}) {
		t.Fatalf("structured updates were not collected: %#v", collected)
	}
}

func TestWaitProcessGroupBoundsDetachedDescendants(t *testing.T) {
	cmd := exec.Command("bash", "-c", "sleep 60 & exit 0")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	commandDone := make(chan error, 1)
	go func() { commandDone <- cmd.Wait() }()

	started := time.Now()
	err := waitProcessGroup(context.Background(), cmd.Process.Pid, commandDone)
	if !strings.Contains(err.Error(), errExecutionOutcomeUnknown.Error()) {
		t.Fatalf("detached descendants did not reach bounded unknown outcome: %v", err)
	}
	if elapsed := time.Since(started); elapsed < postLeaderDescendantGracePeriod || elapsed > postLeaderDescendantGracePeriod+3*time.Second {
		t.Fatalf("detached descendant wait was not bounded: %s", elapsed)
	}
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skipf("ps is unavailable: %v", err)
	}
	processes, err := exec.Command("ps", "-e", "-o", "pid=,pgid=,state=,args=").CombinedOutput()
	if err != nil && len(processes) == 0 {
		t.Fatalf("inspect detached process group: %v", err)
	}
	for _, line := range strings.Split(string(processes), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || fields[1] != fmt.Sprint(cmd.Process.Pid) || strings.Contains(fields[2], "Z") {
			continue
		}
		t.Fatalf("detached process group survived cleanup: %q", line)
	}
}

func TestWaitCommandStreamsPreservesExitCode(t *testing.T) {
	cmd, stdout, stderr, err := executeRawCommand(context.Background(), "exit 42")
	if err != nil {
		t.Fatal(err)
	}
	err = waitCommandStreams(context.Background(), cmd, stdout, stderr, func(notificationEvent) {}, false)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 42 {
		t.Fatalf("waitCommandStreams error = %v, want exit 42", err)
	}
}

func TestWaitCommandStreamsBoundsDescendantHoldingOutputPipe(t *testing.T) {
	cmd, stdout, stderr, err := executeRawCommand(context.Background(), "sleep 60 & exit 0")
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)

	started := time.Now()
	err = waitCommandStreams(context.Background(), cmd, stdout, stderr, func(notificationEvent) {}, false)
	if err == nil || !strings.Contains(err.Error(), errExecutionOutcomeUnknown.Error()) {
		t.Fatalf("descendant holding output pipe did not reach bounded unknown outcome: %v", err)
	}
	if elapsed := time.Since(started); elapsed < postLeaderDescendantGracePeriod ||
		elapsed > postLeaderDescendantGracePeriod+3*time.Second {
		t.Fatalf("descendant output pipe wait was not bounded: %s", elapsed)
	}
}

// A message that takes longer than one write timeout on a slow link must still
// be delivered as long as every chunk makes progress within the timeout.
func TestWriteWebSocketMessageSurvivesSlowButProgressingLink(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
	}))
	defer server.Close()
	client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	peer := <-serverConn
	defer peer.Close()

	payload := strings.Repeat("x", 6*websocketWriteChunkBytes)
	received := make(chan int, 1)
	go func() {
		_, reader, err := peer.NextReader()
		if err != nil {
			received <- -1
			return
		}
		total := 0
		buf := make([]byte, 4<<10)
		for {
			n, err := reader.Read(buf)
			total += n
			time.Sleep(25 * time.Millisecond) // ~160 KiB/s reader
			if err != nil {
				break
			}
		}
		received <- total
	}()
	var state websocketDataWriteState
	// The whole message takes well over 200ms; each chunk finishes in time.
	if err := writeWebSocketMessage(client, &sync.Mutex{}, &state, websocket.TextMessage, []byte(payload), 200*time.Millisecond); err != nil {
		t.Fatalf("slow but progressing write failed: %v", err)
	}
	if got := <-received; got != len(payload) {
		t.Fatalf("received %d bytes, want %d", got, len(payload))
	}
}
