package server

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestAgentWebSocketFileReadWrite(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", root)
	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
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

func TestAgentWebSocketCancelKillsShellCommand(t *testing.T) {
	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
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
	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
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
	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
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
	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
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
	gw := NewGateway("0")
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

func TestAgentWebSocketHandlesGitHTTPOverTunnel(t *testing.T) {
	gw := NewGateway("0")
	gw.gitHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery, "GET /git/release.git/info/refs?service=git-upload-pack"; got != want {
			t.Fatalf("unexpected git request: want %s, got %s", want, got)
		}
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("git-agent-ok"))
	})
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
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

func TestAgentWebSocketGitHTTPUsesLocalGitToken(t *testing.T) {
	gw := NewGateway("0")
	gw.Token = "agent-local-token"
	gw.gitHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || password != gw.Token {
			t.Fatalf("expected local agent git token, got authorization %q", r.Header.Get("Authorization"))
		}
		if got := r.Header.Get("X-Doops-Key"); got != "" {
			t.Fatalf("expected external gateway auth header to be stripped, got %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
	defer ts.Close()

	wsURL := "ws" + ts.URL[len("http"):]
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + gw.Token}})
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

	gw := NewGateway("0")
	ts := httptest.NewServer(http.HandlerFunc(gw.HandleWebSocket))
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
	if _, ok := resp["result"]; !ok {
		t.Fatalf("initialize failed: %#v", resp)
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
