package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/api"
	"github.com/user/doops/agent/pkg/release"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: checkWebSocketOrigin,
}

// checkWebSocketOrigin decides whether a cross-origin WebSocket upgrade is
// allowed. Non-browser clients (CLI/SDK) typically send no Origin header and
// are always allowed. Browser clients are validated against the request host
// and an optional allowlist (DOOPS_ALLOWED_WS_ORIGINS, comma-separated). A
// fully permissive mode (the old behaviour) can be re-enabled for development
// via DOOPS_ALLOW_ALL_WS_ORIGINS=1.
func checkWebSocketOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		// No Origin header: not a browser-enforced request (CLI/SDK/agent).
		return true
	}
	if strings.TrimSpace(os.Getenv("DOOPS_ALLOW_ALL_WS_ORIGINS")) == "1" {
		return true
	}
	originURL, err := url.Parse(origin)
	if err != nil {
		return false
	}
	originHost := strings.ToLower(originURL.Host)
	if originHost == "" {
		return false
	}
	// Same-host requests are always allowed.
	if strings.EqualFold(originHost, r.Host) || strings.EqualFold(originURL.Hostname(), hostOnly(r.Host)) {
		return true
	}
	for _, allowed := range strings.Split(os.Getenv("DOOPS_ALLOWED_WS_ORIGINS"), ",") {
		allowed = strings.ToLower(strings.TrimSpace(allowed))
		if allowed == "" {
			continue
		}
		if allowed == originHost || allowed == strings.ToLower(originURL.Hostname()) {
			return true
		}
	}
	return false
}

func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

const (
	agentWSReadTimeout            = 20 * time.Second
	agentWSPingInterval           = 5 * time.Second
	agentWSPingWriteTimeout       = 3 * time.Second
	agentWSDataWriteTimeout       = 60 * time.Second
	maxAgentPromptAttachments     = 4
	maxAgentPromptAttachmentBytes = 25 * 1024 * 1024
)

type websocketControlWriter interface {
	WriteControl(messageType int, data []byte, deadline time.Time) error
	Close() error
}

type websocketDataWriter interface {
	SetWriteDeadline(deadline time.Time) error
	NextWriter(messageType int) (io.WriteCloser, error)
	Close() error
}

type websocketBinaryWriter = websocketDataWriter

type websocketDataWriteState struct {
	sequence atomic.Uint64
}

func (s *websocketDataWriteState) begin() {
	s.sequence.Add(1)
}

func (s *websocketDataWriteState) end() {
	s.sequence.Add(1)
}

func (s *websocketDataWriteState) overlaps(start uint64) bool {
	end := s.sequence.Load()
	return start%2 == 1 || end%2 == 1 || start != end
}

func writeHeartbeatControl(conn websocketControlWriter, dataWriteState *websocketDataWriteState, messageType int, payload []byte, timeout time.Duration) bool {
	writeSequence := dataWriteState.sequence.Load()
	if err := conn.WriteControl(messageType, payload, time.Now().Add(timeout)); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) &&
			netErr.Timeout() &&
			netErr.Temporary() &&
			dataWriteState.overlaps(writeSequence) {
			return true
		}
		_ = conn.Close()
		return false
	}
	return true
}

func writeHeartbeatPing(conn websocketControlWriter, dataWriteState *websocketDataWriteState, payload []byte, timeout time.Duration) bool {
	return writeHeartbeatControl(conn, dataWriteState, websocket.PingMessage, payload, timeout)
}

// websocketWriteChunkBytes bounds how much of one message is written before
// the stall deadline is extended. A slow link keeps the tunnel as long as bytes
// keep moving; only a link that makes no progress for the timeout is closed.
const websocketWriteChunkBytes = 32 << 10

func writeWebSocketBinary(conn websocketBinaryWriter, writeMu *sync.Mutex, dataWriteState *websocketDataWriteState, payload []byte, timeout time.Duration) error {
	return writeWebSocketMessage(conn, writeMu, dataWriteState, websocket.BinaryMessage, payload, timeout)
}

func writeWebSocketJSON(conn websocketDataWriter, writeMu *sync.Mutex, dataWriteState *websocketDataWriteState, value interface{}, timeout time.Duration) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeWebSocketMessage(conn, writeMu, dataWriteState, websocket.TextMessage, payload, timeout)
}

// writeWebSocketMessage writes one message in chunks with a stall deadline:
// each chunk gets a fresh deadline, so total duration scales with link speed
// instead of failing every message that takes longer than timeout.
func writeWebSocketMessage(conn websocketDataWriter, writeMu *sync.Mutex, dataWriteState *websocketDataWriteState, messageType int, payload []byte, timeout time.Duration) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	dataWriteState.begin()
	defer dataWriteState.end()
	fail := func(err error) error {
		_ = conn.Close()
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
		return fail(err)
	}
	writer, err := conn.NextWriter(messageType)
	if err != nil {
		return fail(err)
	}
	for len(payload) > 0 {
		n := len(payload)
		if n > websocketWriteChunkBytes {
			n = websocketWriteChunkBytes
		}
		if _, err := writer.Write(payload[:n]); err != nil {
			return fail(err)
		}
		payload = payload[n:]
		if err := conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return fail(err)
		}
	}
	if err := writer.Close(); err != nil {
		return fail(err)
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		return fail(err)
	}
	return nil
}

func readWebSocketMessage(conn *websocket.Conn, onProgress func()) (int, []byte, error) {
	messageType, reader, err := conn.NextReader()
	if err != nil {
		return 0, nil, err
	}
	var message bytes.Buffer
	buffer := make([]byte, 32<<10)
	for {
		n, readErr := reader.Read(buffer)
		if n > 0 {
			_, _ = message.Write(buffer[:n])
			if onProgress != nil {
				onProgress()
			}
		}
		if readErr == io.EOF {
			return messageType, message.Bytes(), nil
		}
		if readErr != nil {
			return 0, nil, readErr
		}
	}
}

// ServeGatewayWebSocketConnWithReady serves the reverse tunnel after Gateway
// authorization.
func (gw *Gateway) ServeGatewayWebSocketConnWithReady(conn *websocket.Conn, remoteAddr string, onReady func()) {
	gw.serveWebSocketConn(conn, remoteAddr, onReady)
}

func (gw *Gateway) serveWebSocketConn(conn *websocket.Conn, remoteAddr string, onReady func()) {
	defer conn.Close()
	conn.SetReadLimit(maxWebSocketMessageBytes())
	connectionCtx, cancelConnection := context.WithCancel(context.Background())

	// per-connection 写互斥锁（gorilla/websocket 不允许并发写）
	var connMu sync.Mutex
	var dataWriteState websocketDataWriteState

	conn.SetReadDeadline(time.Now().Add(agentWSReadTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(agentWSReadTimeout))
	})

	pingDone := make(chan struct{})
	defer close(pingDone)
	go func() {
		ticker := time.NewTicker(agentWSPingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-ticker.C:
				if !writeHeartbeatPing(conn, &dataWriteState, nil, agentWSPingWriteTimeout) {
					return
				}
			}
		}
	}()

	log.Printf("🔗 WS Client Connected: %s", remoteAddr)

	// per-connection 线程安全写函数（替代全局 gw.mu）
	writeJSON := func(v interface{}) {
		if err := writeWebSocketJSON(conn, &connMu, &dataWriteState, v, agentWSDataWriteTimeout); err != nil {
			log.Printf("WS WriteJSON error: %v", err)
		}
	}

	// Request bodies are handed to handlers through tunnelBody so a slow handler
	// can never block this read loop, which also answers Gateway pings.
	gitBodyMu := sync.Mutex{}
	gitBodyWriters := make(map[int64]*tunnelBody)
	activeGitMu := sync.Mutex{}
	activeGitCancels := make(map[int64]context.CancelFunc)
	proxyBodyMu := sync.Mutex{}
	proxyBodyWriters := make(map[int64]*tunnelBody)
	activeProxyMu := sync.Mutex{}
	activeProxyCancels := make(map[int64]context.CancelFunc)
	activeToolMu := sync.Mutex{}
	activeToolCalls := make(map[int64]activeToolCall)
	var executionWG sync.WaitGroup
	var readyOnce sync.Once
	closeGitBody := func(id int64) {
		gitBodyMu.Lock()
		body := gitBodyWriters[id]
		delete(gitBodyWriters, id)
		gitBodyMu.Unlock()
		if body != nil {
			body.Abort(nil)
		}
	}
	closeProxyBody := func(id int64) {
		proxyBodyMu.Lock()
		body := proxyBodyWriters[id]
		delete(proxyBodyWriters, id)
		proxyBodyMu.Unlock()
		if body != nil {
			body.Abort(nil)
		}
	}
	defer func() {
		_ = conn.Close()
		cancelConnection()
		gitBodyMu.Lock()
		writers := gitBodyWriters
		gitBodyWriters = make(map[int64]*tunnelBody)
		gitBodyMu.Unlock()
		for _, body := range writers {
			body.Abort(nil)
		}
		proxyBodyMu.Lock()
		proxyWriters := proxyBodyWriters
		proxyBodyWriters = make(map[int64]*tunnelBody)
		proxyBodyMu.Unlock()
		for _, body := range proxyWriters {
			body.Abort(nil)
		}
		executionWG.Wait()
	}()

	// 主读取循环
	for {
		messageType, message, err := readWebSocketMessage(conn, func() {
			_ = conn.SetReadDeadline(time.Now().Add(agentWSReadTimeout))
		})
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WS error: %v", err)
			}
			break
		}
		if messageType != websocket.TextMessage {
			continue
		}

		var req api.JSONRPCRequest
		if err := json.Unmarshal(message, &req); err != nil {
			// 如果连格式都不是 JSONRPC，忽略
			continue
		}

		if req.Method == "initialize" {
			// 处理初始化握手
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: map[string]interface{}{
					"protocolVersion": "2024-11-05",
					"serverInfo": map[string]string{
						"name":          "doops-agent",
						"version":       release.Version,
						"releaseCommit": release.Commit,
					},
					"runtimeId": agentProcessRuntimeID,
					"capabilities": map[string]interface{}{
						"tools": map[string]interface{}{},
						"protocols": map[string]interface{}{
							"credentialPlan":              credentialPlanCapability(),
							"structuredSSH":               structuredSSHCapability(),
							"executionLifecycle":          executionLifecycleCapability(),
							upgradeAuditProtocol:          upgradeAuditCapability(),
							registryAuthfileProtocol:      registryAuthfileCapability(),
							cosSnapshotExecutableProtocol: cosSnapshotExecutableCapability(),
						},
					},
				},
			})
			continue
		}
		if req.Method == "gateway/registered" {
			if onReady != nil {
				readyOnce.Do(onReady)
			}
			continue
		}

		if req.Method == "git/http" {
			id, ok := numericID(req.ID)
			if !ok {
				writeJSON(api.JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   &api.RPCError{Code: -32602, Message: "git/http requires numeric id"},
				})
				continue
			}
			var params gitHTTPRequestParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeJSON(api.JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   &api.RPCError{Code: -32602, Message: "Invalid git/http params"},
				})
				continue
			}
			body := newTunnelBody()
			gitBodyMu.Lock()
			gitBodyWriters[id] = body
			gitBodyMu.Unlock()
			ctx, cancel := context.WithCancel(connectionCtx)
			activeGitMu.Lock()
			activeGitCancels[id] = cancel
			activeGitMu.Unlock()
			executionWG.Add(1)
			go func() {
				defer executionWG.Done()
				defer func() {
					activeGitMu.Lock()
					delete(activeGitCancels, id)
					activeGitMu.Unlock()
					cancel()
				}()
				defer closeGitBody(id)
				gw.handleGitHTTPOverWS(ctx, req.ID, params, body.Reader(), writeJSON)
			}()
			continue
		}

		if req.Method == "proxy/http" {
			id, ok := numericID(req.ID)
			if !ok {
				writeJSON(api.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID,
					Error: &api.RPCError{Code: -32602, Message: "proxy/http requires numeric id"}})
				continue
			}
			var params proxyHTTPRequestParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeJSON(api.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID,
					Error: &api.RPCError{Code: -32602, Message: "Invalid proxy/http params"}})
				continue
			}
			body := newTunnelBody()
			proxyBodyMu.Lock()
			proxyBodyWriters[id] = body
			proxyBodyMu.Unlock()
			ctx, cancel := context.WithCancel(connectionCtx)
			activeProxyMu.Lock()
			activeProxyCancels[id] = cancel
			activeProxyMu.Unlock()
			executionWG.Add(1)
			go func() {
				defer executionWG.Done()
				defer func() {
					activeProxyMu.Lock()
					delete(activeProxyCancels, id)
					activeProxyMu.Unlock()
					cancel()
				}()
				defer closeProxyBody(id)
				gw.handleHTTPProxyOverWS(ctx, req.ID, params, body.Reader(), writeJSON)
			}()
			continue
		}

		if req.Method == "git/cancel" {
			var params struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.ID == 0 {
				if id, ok := numericID(req.ID); ok {
					params.ID = id
				}
			}
			activeGitMu.Lock()
			cancel := activeGitCancels[params.ID]
			activeGitMu.Unlock()
			if cancel != nil {
				cancel()
				closeGitBody(params.ID)
			}
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  map[string]interface{}{"canceled": cancel != nil},
			})
			continue
		}

		if req.Method == "proxy/cancel" {
			var params struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.ID == 0 {
				if id, ok := numericID(req.ID); ok {
					params.ID = id
				}
			}
			activeProxyMu.Lock()
			cancel := activeProxyCancels[params.ID]
			activeProxyMu.Unlock()
			if cancel != nil {
				cancel()
			}
			closeProxyBody(params.ID)
			writeJSON(api.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID,
				Result: map[string]interface{}{"canceled": cancel != nil}})
			continue
		}

		if req.Method == "git/body" {
			var frame struct {
				ID      int64  `json:"id"`
				DataB64 string `json:"data_b64,omitempty"`
				EOF     bool   `json:"eof,omitempty"`
			}
			if err := json.Unmarshal(req.Params, &frame); err != nil {
				continue
			}
			gitBodyMu.Lock()
			body := gitBodyWriters[frame.ID]
			gitBodyMu.Unlock()
			if body == nil {
				continue
			}
			if !pushTunnelBodyFrame(body, frame.DataB64, frame.EOF) {
				closeGitBody(frame.ID)
			}
			continue
		}

		if req.Method == "proxy/body" {
			var frame struct {
				ID      int64  `json:"id"`
				DataB64 string `json:"data_b64,omitempty"`
				EOF     bool   `json:"eof,omitempty"`
			}
			if err := json.Unmarshal(req.Params, &frame); err != nil {
				continue
			}
			proxyBodyMu.Lock()
			body := proxyBodyWriters[frame.ID]
			proxyBodyMu.Unlock()
			if body == nil {
				continue
			}
			if !pushTunnelBodyFrame(body, frame.DataB64, frame.EOF) {
				closeProxyBody(frame.ID)
			}
			continue
		}

		if req.Method == "tools/cancel" {
			var params struct {
				ID int64 `json:"id"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if params.ID == 0 {
				if id, ok := numericID(req.ID); ok {
					params.ID = id
				}
			}
			activeToolMu.Lock()
			call, ok := activeToolCalls[params.ID]
			activeToolMu.Unlock()
			if ok {
				call.cancel()
			}
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  map[string]interface{}{"canceled": ok},
			})
			continue
		}

		if req.Method == "tools/call" {
			var params api.ToolCallParams
			if err := json.Unmarshal(req.Params, &params); err != nil {
				writeJSON(api.JSONRPCResponse{
					JSONRPC: "2.0",
					ID:      req.ID,
					Error:   &api.RPCError{Code: -32602, Message: "Invalid Request"},
				})
				continue
			}

			// 异步处理实际的 tool 执行
			ctx, cancel := context.WithCancel(connectionCtx)
			if id, ok := numericID(req.ID); ok {
				activeToolMu.Lock()
				activeToolCalls[id] = activeToolCall{
					cancel: cancel,
				}
				activeToolMu.Unlock()
				executionWG.Add(1)
				go func() {
					defer executionWG.Done()
					defer func() {
						activeToolMu.Lock()
						delete(activeToolCalls, id)
						activeToolMu.Unlock()
						cancel()
					}()
					gw.handleToolCallOverWS(ctx, req.ID, params.Name, params.Arguments, writeJSON)
				}()
			} else {
				executionWG.Add(1)
				go func() {
					defer executionWG.Done()
					defer cancel()
					gw.handleToolCallOverWS(ctx, req.ID, params.Name, params.Arguments, writeJSON)
				}()
			}
		}
	}

	log.Printf("🔗 WS Client Disconnected: %s", remoteAddr)
}

var agentProcessRuntimeID = fmt.Sprintf("%s:%d:%d", processHostname(), os.Getpid(), time.Now().UnixNano())

func processHostname() string {
	hostname, err := os.Hostname()
	if err != nil || strings.TrimSpace(hostname) == "" {
		return "unknown"
	}
	return strings.TrimSpace(hostname)
}

// notificationSender 抽象了发送 notifications/message 的能力。
type notificationSender func(notificationEvent)

type notificationEvent struct {
	Kind   string
	Data   string
	Tool   string
	Status string
	Path   string
}

type activeToolCall struct {
	cancel context.CancelFunc
}

func rawNotification(text string) notificationEvent {
	return notificationEvent{Kind: "raw", Data: text}
}

func assistantDeltaNotification(text string) notificationEvent {
	return notificationEvent{Kind: "assistant_delta", Data: text}
}

func toolNotification(toolName, status string) notificationEvent {
	return notificationEvent{
		Kind:   "tool",
		Data:   fmt.Sprintf("[tool:%s] %s", toolName, status),
		Tool:   toolName,
		Status: status,
	}
}

// promptOutcomeUnknown keeps the execution_outcome_unknown contract and names the
// check that could not be proven, so callers know what to verify on the target.
func promptOutcomeUnknown(reason string) string {
	return errExecutionOutcomeUnknown.Error() + ": " + reason
}

func errorNotification(text string) notificationEvent {
	return notificationEvent{Kind: "error", Data: text}
}

// handleToolCallOverWS 处理具体的 MCP tool 调用（复用原有的处理逻辑，但直接向 WS 写入结果）
func (gw *Gateway) handleToolCallOverWS(ctx context.Context, reqID interface{}, toolName string, argBytes json.RawMessage, writeJSON func(v interface{})) {
	var sessionID string
	var argsMap map[string]interface{}
	json.Unmarshal(argBytes, &argsMap)
	if sid, ok := argsMap["session_id"].(string); ok {
		sessionID = sid
	} else {
		sessionID = "default" // fallback
	}
	if err := validateSession(sessionID); err != nil {
		writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
		return
	}

	// 统一流式推送方法
	pushProgress := func(evt notificationEvent) {
		if evt.Kind == "" {
			evt.Kind = "raw"
		}
		params := map[string]interface{}{
			"request_id": reqID,
			"sessionID":  sessionID,
			"data":       evt.Data,
			"kind":       evt.Kind,
		}
		if evt.Tool != "" {
			params["tool"] = evt.Tool
		}
		if evt.Status != "" {
			params["status"] = evt.Status
		}
		if evt.Path != "" {
			params["path"] = evt.Path
		}
		writeJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "notifications/message",
			"params":  params,
		})
	}

	// 工具分发
	switch toolName {
	case "doops_shell":
		var args api.ShellParams
		json.Unmarshal(argBytes, &args)
		if gw.Dispatcher != nil {
			if err := gw.Dispatcher.Check(args.Command); err != nil {
				writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
				return
			}
		}

		log.Printf("🖥️  WS Exec: [%s] %s", sessionID, args.Command)

		// 放弃过度设计的 PTY Sentinel 模式，换用标准 os/exec Streaming
		execCtx, cancelExec := context.WithTimeout(ctx, maxToolExecutionDuration())
		defer cancelExec()
		cmd, stdoutPipe, stderrPipe, err := executeRawCommand(execCtx, args.Command)
		if err != nil {
			resultText := "Error starting command: " + err.Error()
			writeJSON(buildToolErrorResponse(reqID, resultText))
			return
		}

		cmdErr := waitCommandStreams(execCtx, cmd, stdoutPipe, stderrPipe, pushProgress, true)
		if errors.Is(cmdErr, errExecutionOutcomeUnknown) {
			log.Printf("⚠️  WS outcome unknown: [%s] %v", sessionID, cmdErr)
			writeJSON(buildErrorResponse(reqID, -32603, cmdErr.Error()))
			return
		}
		if execCtx.Err() != nil {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
			return
		}
		writeSessionCommandAudit(sessionID, args.Command, cmdErr)

		if cmdErr != nil {
			finalText := fmt.Sprintf("Command failed with error: %v", cmdErr)
			writeJSON(buildToolErrorResponse(reqID, finalText))
		} else {
			writeJSON(buildSuccessResponse(reqID, "Operation complete."))
		}

	case "doops_ssh":
		var args api.SSHParams
		if err := json.Unmarshal(argBytes, &args); err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, "invalid SSH arguments"))
			return
		}
		log.Printf("🔐 WS SSH: [%s] host=%s options=%d remote_args=%d",
			sessionID, args.Host, len(args.Options), len(args.Command))

		execCtx, cancelExec := context.WithTimeout(ctx, maxToolExecutionDuration())
		defer cancelExec()
		cmd, stdoutPipe, stderrPipe, err := executeStructuredSSHCommand(execCtx, args)
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
			return
		}

		cmdErr := waitCommandStreams(execCtx, cmd, stdoutPipe, stderrPipe, pushProgress, true)
		if errors.Is(cmdErr, errExecutionOutcomeUnknown) {
			log.Printf("⚠️  WS outcome unknown: [%s] %v", sessionID, cmdErr)
			writeJSON(buildErrorResponse(reqID, -32603, cmdErr.Error()))
			return
		}
		if execCtx.Err() != nil {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
			return
		}
		writeSessionCommandAudit(sessionID, sshAuditSummary(args), cmdErr)
		if cmdErr != nil {
			if exitCode, ok := sshExitCode(cmdErr); ok {
				writeJSON(buildStructuredToolErrorResponse(
					reqID,
					fmt.Sprintf("SSH command failed: %v", cmdErr),
					map[string]interface{}{"sshExitCode": exitCode},
				))
			} else {
				writeJSON(buildToolErrorResponse(reqID, fmt.Sprintf("SSH command failed: %v", cmdErr)))
			}
		} else {
			writeJSON(buildSuccessResponse(reqID, "SSH command finished."))
		}

	case "doops_docker":
		var args api.DockerParams
		json.Unmarshal(argBytes, &args)

		if strings.ContainsAny(args.Command, ";|&$`()\r\n") {
			writeJSON(buildErrorResponse(reqID, -32602, "Rejected unsafe characters"))
			return
		}

		fullCmd := gw.containerRuntime + " " + args.Command
		log.Printf("🐳 WS Docker: [%s] %s", sessionID, fullCmd)

		execCtx, cancelExec := context.WithTimeout(ctx, maxToolExecutionDuration())
		defer cancelExec()
		cmd, stdoutPipe, stderrPipe, err := executeRawCommand(execCtx, fullCmd)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, "Error starting command: "+err.Error()))
			return
		}

		cmdErr := waitCommandStreams(execCtx, cmd, stdoutPipe, stderrPipe, pushProgress, true)
		if errors.Is(cmdErr, errExecutionOutcomeUnknown) {
			log.Printf("⚠️  WS outcome unknown: [%s] %v", sessionID, cmdErr)
			writeJSON(buildErrorResponse(reqID, -32603, cmdErr.Error()))
			return
		}
		if execCtx.Err() != nil {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
			return
		}

		if cmdErr != nil {
			finalText := fmt.Sprintf("Docker command failed: %v", cmdErr)
			writeJSON(buildToolErrorResponse(reqID, finalText))
		} else {
			writeJSON(buildSuccessResponse(reqID, "Docker command finished."))
		}

	case "doops_database_inventory":
		result, err := discoverDatabaseCandidates(ctx)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_node_info":
		var args api.NodeInfoParams
		json.Unmarshal(argBytes, &args)
		if args.Section != "" {
			if args.Section != "admission" {
				writeJSON(buildErrorResponse(reqID, -32602, "unsupported node info section"))
				return
			}
			result, err := readDoagentAdmission(ctx, configuredDoagentURL())
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
			} else {
				writeJSON(buildSuccessResponse(reqID, result))
			}
			return
		}

		cmdStr := strings.Join([]string{
			"echo '--- System Info ---'",
			"uname -a",
			"echo ''",
			"echo '--- Uptime ---'",
			"uptime",
			"echo ''",
			"echo '--- Memory ---'",
			"free -m 2>/dev/null || echo 'free command not found'",
			"echo ''",
			"echo '--- Disk ---'",
			"df -h /",
			"echo ''",
			"echo '--- Capabilities ---'",
			"if command -v kubectl >/dev/null 2>&1; then echo 'kubectl: OK '$(command -v kubectl); else echo 'kubectl: MISSING'; fi",
			"if command -v nerdctl >/dev/null 2>&1; then echo 'container-runtime: nerdctl '$(command -v nerdctl); elif command -v docker >/dev/null 2>&1; then echo 'container-runtime: docker '$(command -v docker); elif command -v podman >/dev/null 2>&1; then echo 'container-runtime: podman '$(command -v podman); else echo 'container-runtime: MISSING'; fi",
			"if command -v buildctl >/dev/null 2>&1; then echo 'buildctl: OK '$(command -v buildctl); else echo 'buildctl: MISSING'; fi",
			"if command -v skopeo >/dev/null 2>&1; then echo 'skopeo: OK '$(command -v skopeo); else echo 'skopeo: MISSING'; fi",
			"if [ -S /run/buildkit/buildkitd.sock ]; then echo 'buildkit-sock: OK /run/buildkit/buildkitd.sock'; else echo 'buildkit-sock: MISSING'; fi",
			"if [ -f /root/.kube/config ]; then echo 'kubeconfig: OK /root/.kube/config'; elif [ -f /etc/rancher/k3s/k3s.yaml ]; then echo 'kubeconfig: OK /etc/rancher/k3s/k3s.yaml'; elif [ -f /etc/kubernetes/admin.conf ]; then echo 'kubeconfig: OK /etc/kubernetes/admin.conf'; else echo 'kubeconfig: MISSING'; fi",
		}, "; ")
		execCtx, cancelExec := context.WithTimeout(ctx, maxToolExecutionDuration())
		defer cancelExec()
		cmd, stdoutPipe, stderrPipe, err := executeRawCommand(execCtx, cmdStr)
		if err != nil {
			resultText := "Error starting command: " + err.Error()
			writeJSON(buildSuccessResponse(reqID, resultText))
			return
		}

		cmdErr := waitCommandStreams(execCtx, cmd, stdoutPipe, stderrPipe, pushProgress, false)
		if errors.Is(cmdErr, errExecutionOutcomeUnknown) {
			writeJSON(buildErrorResponse(reqID, -32603, cmdErr.Error()))
			return
		}
		if execCtx.Err() != nil {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
			return
		}

		if cmdErr != nil {
			finalText := fmt.Sprintf("Command failed with error: %v", cmdErr)
			writeJSON(buildToolErrorResponse(reqID, finalText))
		} else {
			writeJSON(buildSuccessResponse(reqID, "Operation complete."))
		}

	case "doops_k8s":
		result, err := handleK8STool(ctx, argBytes, pushProgress)
		if errors.Is(err, errExecutionOutcomeUnknown) {
			writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
		} else if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_web":
		result, err := handleWebTool(ctx, argBytes, pushProgress)
		if errors.Is(err, errExecutionOutcomeUnknown) {
			writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
		} else if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_check_deployment":
		result, err := handleCheckDeployment(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_clean_workspace":
		result, err := handleCleanWorkspace(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_agent_upgrade":
		writeJSON(buildAgentUpgradeResponse(reqID, argBytes))

	case "doops_bg":
		var args api.BgExecParams
		json.Unmarshal(argBytes, &args)
		// 与 doops_shell 一致: 后台任务同样要经过危险命令过滤。
		if gw.Dispatcher != nil {
			if err := gw.Dispatcher.Check(args.Command); err != nil {
				writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
				return
			}
		}
		log.Printf("🚀 WS doops_bg: %s", args.Command)
		if ctx.Err() != nil {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
			return
		}
		task, err := gw.submitBgTask(sessionID, args.Command, args.LogPath)
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "Task failed: "+err.Error()))
			return
		}
		defer task.cancel()
		stopCancellation := context.AfterFunc(ctx, task.cancel)
		defer stopCancellation()
		pushProgress(rawNotification(fmt.Sprintf("Task submitted.\nTaskID: %s\nPID: %d\nLog: %s\n", task.TaskID, task.PID, task.LogPath)))
		logReader, logErr := os.Open(task.LogPath)
		if logErr != nil {
			task.cancel()
			<-task.done
		} else {
			defer logReader.Close()
			buffer := make([]byte, 32<<10)
			pendingText := make([]byte, 0, 32<<10)
			publishLog := func(data []byte, final bool) {
				pendingText = append(pendingText, data...)
				complete := len(pendingText)
				if !final {
					complete = 0
					for complete < len(pendingText) && utf8.FullRune(pendingText[complete:]) {
						_, size := utf8.DecodeRune(pendingText[complete:])
						complete += size
					}
				}
				if complete != 0 {
					pushProgress(rawNotification(string(pendingText[:complete])))
					pendingText = append(pendingText[:0], pendingText[complete:]...)
				}
			}
			readLog := func() error {
				for {
					n, err := logReader.Read(buffer)
					if n != 0 {
						publishLog(buffer[:n], false)
					}
					if err == io.EOF {
						return nil
					}
					if err != nil {
						return err
					}
				}
			}
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
		streamLogs:
			for {
				if logErr = readLog(); logErr != nil {
					task.cancel()
					<-task.done
					break
				}
				select {
				case <-ticker.C:
				case <-task.done:
					logErr = readLog()
					break streamLogs
				case <-ctx.Done():
					task.cancel()
					<-task.done
					logErr = readLog()
					break streamLogs
				}
			}
			publishLog(nil, true)
		}
		if errors.Is(task.err, errExecutionOutcomeUnknown) {
			writeJSON(buildErrorResponse(reqID, -32603, task.err.Error()))
		} else if ctx.Err() != nil {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
		} else if logErr != nil {
			writeJSON(buildToolErrorResponse(reqID, "background_log_unavailable"))
		} else if task.err != nil {
			writeJSON(buildToolErrorResponse(reqID, fmt.Sprintf("Task failed.\nTaskID: %s\nExitCode: %d", task.TaskID, task.ExitCode)))
		} else {
			writeJSON(buildSuccessResponse(reqID, fmt.Sprintf("Task complete.\nTaskID: %s\nExitCode: %d", task.TaskID, task.ExitCode)))
		}

	case "doops_task_status":
		var args api.TaskStatusParams
		json.Unmarshal(argBytes, &args)
		info, err := gw.getTaskStatus(args.TaskID, args.Lines)
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
		} else {
			resultText := fmt.Sprintf("TaskID: %s\nPID: %d\nStatus: %s\nExitCode: %d\nLog: %s\n---\n%s",
				info.TaskID, info.PID, info.Status, info.ExitCode, info.LogPath, info.LogTail)
			writeJSON(buildSuccessResponse(reqID, resultText))
		}

	case "doops_agent_prompt":
		var args api.AgentPromptParams
		json.Unmarshal(argBytes, &args)
		operation := strings.ToLower(strings.TrimSpace(args.Operation))
		if operation == "apply" {
			if strings.TrimSpace(args.Instruction) == "" {
				writeJSON(buildErrorResponse(reqID, -32602, "apply requires a non-empty instruction"))
				return
			}
		}
		if len(args.CredentialRefs) != 0 {
			writeJSON(buildErrorResponse(reqID, -32602, "credential_refs must be resolved by the gateway"))
			return
		}
		if len(args.CredentialBindings) != 0 {
			if operation != "" && operation != "ask" && operation != "apply" {
				writeJSON(buildErrorResponse(reqID, -32602, "credential bindings require an ask or apply operation"))
				return
			}
			if err := validatePromptCredentialAccess(operation, true); err != nil {
				writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
				return
			}
			bindingContext, err := credentialBindingContext(args.CredentialBindings)
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
				return
			}
			args.Instruction += "\n\nGateway-managed credential bindings (metadata only; never read Secret values). Use name or credential_id as credential_ref; grant_id and version_id are audit metadata:\n" + bindingContext
			if hasHelmCredentialBinding(args.CredentialBindings) {
				if operation != "apply" {
					writeJSON(buildErrorResponse(reqID, -32602, "Helm credential bindings require apply"))
					return
				}
				cleanup, err := bindHelmCredentialsForPrompt(sessionID, args.CredentialBindings)
				if err != nil {
					writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
					return
				}
				defer cleanup()
			}
			if hasDatabaseCredentialBinding(args.CredentialBindings) {
				cleanup, err := bindDoSQLCredentialsForPrompt(sessionID, args.CredentialBindings)
				if err != nil {
					writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
					return
				}
				defer cleanup()
				if strings.EqualFold(strings.TrimSpace(args.Skill), "dosql") &&
					strings.TrimSpace(args.SQLDatabase) != "" && !doSQLDirectRequested(args) {
					resolution, hint, resolveErr := resolveDoSQLPromptResolution(ctx, args.CredentialBindings,
						args.SQLDatabase, readDoSQLSecretData, discoverDatabaseCandidateList)
					if resolveErr == nil {
						args.Instruction += "\n\n" + hint
						if cleanupResolution, writeErr := writeDoSQLResolution(sessionID, resolution); writeErr == nil {
							defer cleanupResolution()
						}
					} else {
						args.Instruction += "\n\n[DoSQL server-side resolution failed] " +
							sanitizeSecretText(resolveErr.Error()) + "；不要猜测 credential/asset，直接报告该错误。"
					}
				}
			}
		}
		if doSQLDirectRequested(args) || doSQLCopyRequested(args) {
			handle := handleDirectDoSQLPrompt
			if doSQLCopyRequested(args) {
				handle = handleDoSQLCopy
			}
			result, err := handle(ctx, sessionID, args)
			if err != nil {
				if ctx.Err() != nil {
					writeJSON(buildErrorResponse(reqID, -32603, errExecutionOutcomeUnknown.Error()))
					return
				}
				writeJSON(buildToolErrorResponse(reqID, sanitizeSecretText(err.Error())))
				return
			}
			writeJSON(buildSuccessResponse(reqID, result))
			return
		}
		nativeMode, err := doagentModeForPrompt(operation, "")
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
			return
		}
		if args.ResponseFormat != "" && args.ResponseFormat != "json" {
			writeJSON(buildErrorResponse(reqID, -32602, `response_format must be "json" when specified`))
			return
		}
		if args.EventFormat != "" && args.EventFormat != "acp" {
			writeJSON(buildErrorResponse(reqID, -32602, `event_format must be "acp" when specified`))
			return
		}
		if operation == "apply" && len(args.Attachments) > 0 {
			writeJSON(buildErrorResponse(reqID, -32602, "deployment form does not accept conversation attachments"))
			return
		}
		if err := validateAgentPromptAttachments(args.Attachments); err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
			return
		}
		switch strings.ToLower(strings.TrimSpace(args.Mode)) {
		case "metadata":
			if args.ResponseFormat != "" {
				writeJSON(buildErrorResponse(reqID, -32602, "response_format is only supported for agent prompts"))
				return
			}
			result, err := gw.handleAgentMetadataWS(ctx, sessionID)
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
			} else {
				writeJSON(buildSuccessResponse(reqID, result))
			}
			return
		case "history":
			if args.ResponseFormat != "" {
				writeJSON(buildErrorResponse(reqID, -32602, "response_format is only supported for agent prompts"))
				return
			}
			result, err := gw.handleAgentHistoryWS(ctx, sessionID)
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
			} else {
				writeJSON(buildSuccessResponse(reqID, result))
			}
			return
		}
		gw.handleAgentPromptWS(
			ctx,
			reqID,
			sessionID,
			args.Instruction,
			args.Attachments,
			args.Model,
			args.ResponseFormat,
			agentPromptExecutionContext{
				NativeMode:          nativeMode,
				RequireToolEvidence: operation == "apply",
				StructuredEvents:    args.EventFormat == "acp",
				Skill:               args.Skill,
			},
			pushProgress,
			writeJSON,
		)

	case "doops_credential_import":
		result, err := handleCredentialSecretImportTool(ctx, argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_credential_materialize":
		result, err := handleCredentialMaterializeTool(ctx, argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_credential_cleanup":
		result, err := handleCredentialCleanupTool(ctx, argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_cos_sync":
		result, err := handleCOSSync(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_file_write":
		result, err := handleFileWrite(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_file_read":
		result, err := handleFileRead(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_workspace_read":
		result, err := handleWorkspaceRead(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_workspace_begin":
		result, err := handleWorkspaceBegin(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_workspace_chunk":
		result, err := handleWorkspaceChunk(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_workspace_commit":
		result, err := handleWorkspaceCommit(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_workspace_pull_begin":
		result, err := handleWorkspacePullBegin(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_workspace_pull_chunk":
		result, err := handleWorkspacePullChunk(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	default:
		writeJSON(buildErrorResponse(reqID, -32601, "Unknown tool over WS: "+toolName))
	}
}

func credentialBindingContext(bindings []api.AgentCredentialBinding) (string, error) {
	for _, binding := range bindings {
		if !validCredentialMetadataToken(binding.Name, 128) ||
			!validCredentialUse(CredentialUse(binding.Use)) ||
			!validKubernetesCredentialName(binding.Namespace) ||
			!validKubernetesCredentialName(binding.ResourceName) ||
			!validCredentialMetadataToken(binding.ResourceVersion, 256) ||
			credentialScalarContainsInlineMaterial(binding.ResourceVersion) {
			return "", errors.New("credential_binding_metadata_invalid")
		}
		switch binding.SecretType {
		case "", "Opaque", "kubernetes.io/dockerconfigjson", "kubernetes.io/tls":
		default:
			return "", errors.New("credential_binding_metadata_invalid")
		}
		if binding.Digest != "" && !validCredentialSHA256Digest(binding.Digest) {
			return "", errors.New("credential_binding_metadata_invalid")
		}
		if binding.Use == string(CredentialUseDatabase) {
			if len(binding.Keys) == 0 {
				return "", errors.New("credential_binding_metadata_invalid")
			}
		}
		if binding.Use == string(CredentialUseHelmPull) {
			if binding.ContextKind != "helm-native" || !validNativeHelmSocketPath(binding.ContextSocket) || binding.SecretType != "" {
				return "", errors.New("credential_binding_metadata_invalid")
			}
		}
		for _, key := range binding.Keys {
			if !validCredentialMetadataToken(key, 253) || credentialScalarContainsInlineMaterial(key) {
				return "", errors.New("credential_binding_metadata_invalid")
			}
		}
	}
	encoded, err := json.Marshal(bindings)
	if err != nil {
		return "", errors.New("credential_binding_metadata_invalid")
	}
	return string(encoded), nil
}

func hasHelmCredentialBinding(bindings []api.AgentCredentialBinding) bool {
	for _, binding := range bindings {
		if binding.Use == string(CredentialUseHelmPull) || binding.ContextKind == "helm-native" {
			return true
		}
	}
	return false
}

func hasDatabaseCredentialBinding(bindings []api.AgentCredentialBinding) bool {
	for _, binding := range bindings {
		if binding.Use == string(CredentialUseDatabase) {
			return true
		}
	}
	return false
}

func doagentModeForPrompt(operation, executionMode string) (string, error) {
	operation = strings.ToLower(strings.TrimSpace(operation))
	executionMode = strings.ToLower(strings.TrimSpace(executionMode))
	if executionMode != "" {
		return "", fmt.Errorf("execution_mode is not supported for ordinary agent prompts")
	}
	switch operation {
	case "", "ask":
		return "auto", nil
	case "apply":
		return "build", nil
	default:
		return "", fmt.Errorf("unsupported agent prompt operation: %s", operation)
	}
}

type agentPromptExecutionContext struct {
	NativeMode          string
	RequireToolEvidence bool
	StructuredEvents    bool
	Skill               string
}

// handleAgentPromptWS 封装 doops_agent_prompt 处理逻辑，通过 ACP HTTP API 调用本地 doagent 服务。
// doagentPromptSessionSequence makes every machine-result doagent session id
// unique to one prompt so machine-result tool traces cannot include an old turn.
var doagentPromptSessionSequence atomic.Uint64

func (gw *Gateway) handleAgentPromptWS(ctx context.Context, reqID interface{}, doopsSessionID string, instr string, attachments []api.AgentPromptAttachment, model string, responseFormat string, execution agentPromptExecutionContext, pushProgress notificationSender, writeJSON func(v interface{})) {
	ctx, cancelExecution := context.WithTimeout(ctx, 30*time.Minute)
	defer cancelExecution()
	log.Printf("🤖 WS Running doagent via ACP HTTP: session=%s structured=%t model=%s",
		doopsSessionID, responseFormat == "json", model)

	if strings.TrimSpace(instr) == "" && len(attachments) == 0 {
		// doagent decides an empty prompt before its admission path and creates
		// no turn; reject it here so the Gateway never opens a session or sends
		// a cancel for input it can already classify.
		writeJSON(buildErrorResponse(reqID, -32602, doagentPromptNoPrompt))
		return
	}

	var mutationContext string
	if execution.RequireToolEvidence {
		var err error
		mutationContext, err = mutationContextDigest(doopsSessionID, instr)
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, "doagent_apply_mutation_context_invalid"))
			return
		}
	}

	doagentURL := configuredDoagentURL()
	if err := ensureDoagentAvailable(doagentURL); err != nil {
		writeJSON(buildErrorResponse(reqID, -32603, "doagent_unavailable"))
		return
	}
	if err := requireDoagentPromptAdmission(ctx, doagentURL); err != nil {
		writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
		return
	}
	// Machine-result prompts use a fresh doagent session so their tool trace
	// catalog and turn_finished boundary cannot include a previous turn.
	var entry *sessionEntry
	if responseFormat != "json" {
		gw.sessionMapMu.RLock()
		entry = gw.sessionMap[doopsSessionID]
		gw.sessionMapMu.RUnlock()
	}

	var targetSessionID string
	if entry != nil {
		targetSessionID = entry.doagentSessionID
		gw.sessionMapMu.Lock()
		entry.lastUsed = time.Now()
		gw.sessionMapMu.Unlock()
	}
	// 首次会话：创建 doagent session 并注入系统提示词
	if targetSessionID == "" {
		requestedSessionID := doopsSessionID
		if responseFormat == "json" {
			requestedSessionID = fmt.Sprintf("%s-json-%d-%d", doopsSessionID, time.Now().UnixNano(), doagentPromptSessionSequence.Add(1))
		}
		var systemPrompt string
		sysPromptPaths := []string{"/app/skills/system_prompt.md", "/app/self-docs/agent/skills/system_prompt.md"}
		for _, sp := range sysPromptPaths {
			if data, err := os.ReadFile(sp); err == nil {
				systemPrompt = string(data)
				log.Printf("📋 首次会话，已注入系统提示词: %s (%d bytes)", sp, len(data))
				break
			}
		}

		createResp, err := doagentRPCContext(ctx, doagentURL, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "create-" + doopsSessionID,
			"method":  "session/new",
			"params": map[string]interface{}{
				"sessionId":      requestedSessionID,
				"systemPrompt":   systemPrompt,
				"cwd":            "/root/ws/" + doopsSessionID,
				"permissionMode": "auto",
			},
		})
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent_session_create_failed"))
			return
		}

		if result, ok := createResp["result"].(map[string]interface{}); ok {
			if sid, ok := result["sessionId"].(string); ok {
				targetSessionID = sid
			}
		}
		if targetSessionID == "" {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent_session_create_invalid"))
			return
		}

		if responseFormat != "json" {
			gw.sessionMapMu.Lock()
			gw.sessionMap[doopsSessionID] = &sessionEntry{
				doagentSessionID: targetSessionID,
				lastUsed:         time.Now(),
			}
			gw.sessionMapMu.Unlock()
		}
		log.Printf("🔗 Bound Doops Session %s -> doagent Session %s", doopsSessionID, targetSessionID)
	} else {
		// DoOps owns the per-prompt allowlist and never exposes an interactive
		// permission UI. Refresh reused sessions so older sessions created with
		// restricted mode cannot emit permission.updated for an allowed tool.
		if _, err := doagentRPCContext(ctx, doagentURL, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "refresh-permission-" + doopsSessionID,
			"method":  "session/new",
			"params": map[string]interface{}{
				"sessionId":      targetSessionID,
				"permissionMode": "auto",
			},
		}); err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent_permission_mode_configuration_failed"))
			return
		}
	}

	// 仅当调用方显式指定时覆盖模型，并在复用会话时同样生效。
	if model != "" {
		targetModel := model
		if !strings.Contains(targetModel, "/") {
			targetModel = "openai/" + targetModel
		}
		if _, err := doagentRPCContext(ctx, doagentURL, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "setmodel-" + doopsSessionID,
			"method":  "session/setModel",
			"params": map[string]interface{}{
				"sessionId": targetSessionID,
				"model":     targetModel,
			},
		}); err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent_model_configuration_failed"))
			return
		}
	}

	if _, err := doagentRPCContext(ctx, doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "setmode-" + doopsSessionID,
		"method":  "session/setMode",
		"params": map[string]interface{}{
			"sessionId": targetSessionID,
			"modeId":    execution.NativeMode,
		},
	}); err != nil {
		writeJSON(buildErrorResponse(reqID, -32603, "doagent_mode_configuration_failed"))
		return
	}
	log.Printf("🧭 Session %s using native doagent mode %s", targetSessionID, execution.NativeMode)

	var promptAttachments []map[string]interface{}
	if len(attachments) > 0 {
		var uploadErr error
		promptAttachments, uploadErr = gw.uploadAgentPromptAttachments(ctx, doagentURL, doopsSessionID, attachments)
		if uploadErr != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent_attachment_upload_failed"))
			return
		}
	}

	// 启动 SSE 事件订阅（在 prompt 之前连接，防止丢失事件）
	// Caller cancellation must not close the terminal-evidence subscription.
	sseCtx, sseCancel := context.WithCancel(context.WithoutCancel(ctx))

	sseDone := make(chan struct{})
	var sseErr error
	var terminalSeen atomic.Bool
	sseReady := make(chan error, 1)
	turnIDReady := make(chan string, 1)
	toolTraces := newDoagentToolTraceCollector(false)
	toolTraces.bindSession(doopsSessionID)
	go func() {
		defer close(sseDone)
		sseErr = subscribeDoagentSSEWithCollectorReady(
			sseCtx,
			doagentURL,
			targetSessionID,
			pushProgress,
			func(update map[string]interface{}) error {
				if update["sessionUpdate"] == "turn_finished" {
					switch update["status"] {
					case "completed", "cancelled", "failed", "interrupted":
						terminalSeen.Store(true)
					}
				}
				return toolTraces.collect(update)
			},
			turnIDReady,
			true,
			responseFormat != "json" && !execution.RequireToolEvidence,
			sseReady,
			execution.StructuredEvents,
		)
	}()
	defer func() {
		sseCancel()
		<-sseDone
	}()
	select {
	case err := <-sseReady:
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent_sse_unavailable"))
			return
		}
	case <-ctx.Done():
		writeJSON(buildErrorResponse(reqID, -32007, doagentPromptCancelledBeforeAdmission))
		return
	}

	// session/prompt 只返回本轮 turnId；终态仍以同 turn 的 SSE turn_finished 为准。
	promptValue := interface{}(instr)
	if len(promptAttachments) > 0 {
		content := make([]interface{}, 0, len(promptAttachments)+1)
		if instr != "" {
			content = append(content, map[string]interface{}{"type": "text", "text": instr})
		}
		for _, attachment := range promptAttachments {
			content = append(content, attachment)
		}
		promptValue = content
	}
	promptParams := map[string]interface{}{"sessionId": targetSessionID, "prompt": promptValue}
	promptParams["mode"] = "reject"
	if deadline, ok := ctx.Deadline(); ok {
		promptParams["wallClockTimeoutSecs"] = max(1, int64(time.Until(deadline)/time.Second))
	}
	allowedTools := doagentAllowedTools(execution.RequireToolEvidence, execution.Skill)
	promptParams["allowedTools"] = allowedTools
	promptParams["mutationAuthorized"] = execution.RequireToolEvidence
	if execution.RequireToolEvidence {
		// doagent owns bounded continuation within the same authorized turn.
		promptParams["completionReview"] = true
		authorityID, issuedAt, authorityToken, authorityDigest, authorityErr :=
			newMutationAuthorization(targetSessionID, allowedTools, mutationContext, time.Now())
		if authorityErr != nil {
			sseCancel()
			<-sseDone
			writeJSON(buildErrorResponse(reqID, -32603, "doagent_mutation_authority_unavailable"))
			return
		}
		promptParams["mutationAuthorizationId"] = authorityID
		promptParams["mutationAuthorizationIssuedAt"] = issuedAt
		promptParams["mutationAuthorizationToken"] = authorityToken
		promptParams["mutationContextDigest"] = mutationContext
		toolTraces.bindMutationAuthority(authorityDigest)
	}
	if responseFormat == "json" {
		promptParams["response_format"] = "json"
	}
	promptParams["responseMode"] = "stream"
	if ctx.Err() != nil {
		writeJSON(buildErrorResponse(reqID, -32007, doagentPromptCancelledBeforeAdmission))
		return
	}
	promptResponse, promptErr := doagentRPCContext(ctx, doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "prompt-" + doopsSessionID,
		"method":  "session/prompt",
		"params":  promptParams,
	})
	admission := classifyDoagentPromptAdmission(promptResponse)
	if promptErr != nil && admission.kind == promptAdmissionMalformed {
		// A transport failure can hide an accepted prompt; only the wire shape
		// without a result can be promoted to the transport category here.
		admission = promptAdmission{kind: promptAdmissionTransportFailed, detail: "rpc transport error"}
	}
	logDoagentPromptAdmission(doopsSessionID, targetSessionID, admission)

	switch admission.kind {
	case promptAdmissionAccepted:
		turnIDReady <- admission.turnID
		close(turnIDReady)
	case promptAdmissionNoPrompt:
		// No turn exists and doagent never touched the queue; a cancel would
		// disturb the session's real work.
		writeJSON(buildErrorResponse(reqID, -32602, doagentPromptNoPrompt))
		return
	case promptAdmissionQueuedNoTurn:
		settleQueuedPromptWS(ctx, reqID, doagentURL, targetSessionID, admission, writeJSON)
		return
	case promptAdmissionBusy:
		writeJSON(buildErrorResponse(reqID, -32603, doagentPromptBusy))
		return
	case promptAdmissionTimedOut:
		writeJSON(buildErrorResponse(reqID, -32603, doagentPromptAdmissionTimeout))
		return
	case promptAdmissionRejected:
		writeJSON(buildErrorResponse(reqID, -32003, "doagent_prompt_rejected"))
		return
	default:
		// A malformed or transport-failed answer can hide an accepted prompt.
		// Without its turn ID even a cancel ACK cannot prove which execution has
		// stopped, so keep the unknown-outcome protection.
		if responseFormat == "json" {
			// This session belongs exclusively to this prompt. Shared streaming
			// sessions may contain another turn whose cancellation we do not own.
			_ = cancelDoagentPrompt(ctx, doagentURL, targetSessionID, nil, &terminalSeen, doagentCancellationTimeout)
		}
		reason := "prompt_turn_id_missing"
		if admission.kind == promptAdmissionTransportFailed {
			reason = "prompt_transport_failed"
		}
		writeJSON(buildErrorResponse(reqID, -32603, promptOutcomeUnknown(reason)))
		return
	}

	// 等待本轮权威 turn_finished 终态或错误。对于 apply，已经完成并验签的
	// Helm 运行时结果是变更事实；最终模型轮失败不能把已完成的变更改判为失败。
	select {
	case <-ctx.Done():
		if err := cancelDoagentPrompt(ctx, doagentURL, targetSessionID, sseDone, &terminalSeen, doagentCancellationTimeout); err != nil {
			message := promptOutcomeUnknown("cancel_unconfirmed")
			if execution.RequireToolEvidence {
				writeJSON(buildDoagentApplyResponse(reqID, toolTraces, -32603, message))
			} else {
				writeJSON(buildErrorResponse(reqID, -32603, message))
			}
			return
		}
		if !execution.RequireToolEvidence {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
			return
		}
	case <-sseDone:
	}
	if !terminalSeen.Load() {
		_ = cancelDoagentPrompt(ctx, doagentURL, targetSessionID, sseDone, &terminalSeen, doagentCancellationTimeout)
		reason := "turn_finished_missing"
		if sseErr != nil {
			reason += " (" + sanitizeSecretText(sseErr.Error()) + ")"
		}
		if execution.RequireToolEvidence {
			writeJSON(buildDoagentApplyResponse(reqID, toolTraces, -32603, promptOutcomeUnknown(reason)))
		} else {
			writeJSON(buildErrorResponse(reqID, -32603, promptOutcomeUnknown(reason)))
		}
		return
	}
	if ctx.Err() != nil && !execution.RequireToolEvidence {
		_ = cancelDoagentPrompt(ctx, doagentURL, targetSessionID, sseDone, &terminalSeen, doagentCancellationTimeout)
		writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
		return
	}
	if execution.RequireToolEvidence {
		if unresolved := toolTraces.unresolvedMutationSummary(); unresolved != "" {
			writeJSON(buildDoagentApplyResponse(reqID, toolTraces, -32603, promptOutcomeUnknown("unverified_mutation "+unresolved)))
			return
		}
		if err := toolTraces.requireCompletedToolEvidence(); err != nil {
			if ctx.Err() != nil && strings.HasPrefix(err.Error(), "doagent_apply_tool_evidence_missing") {
				writeJSON(buildDoagentApplyResponse(reqID, toolTraces, -32007, "operation canceled"))
			} else {
				writeJSON(buildDoagentApplyResponse(reqID, toolTraces, 0, err.Error()))
			}
			return
		}
		if sseErr != nil {
			warning := "[warning] Tool outcomes were verified; doagent final turn ended with " + sanitizeSecretText(sseErr.Error())
			if execution.StructuredEvents {
				pushProgress(publicDoagentUINotification(map[string]interface{}{
					"sessionUpdate": "doops_notice", "content": map[string]interface{}{"type": "text", "text": warning},
				}))
			} else {
				pushProgress(rawNotification(warning + "\n"))
			}
		}
		writeJSON(buildDoagentApplyResponse(reqID, toolTraces, 0, ""))
		return
	}
	if sseErr != nil && !errors.Is(sseErr, context.Canceled) {
		log.Printf("doagent execution failed: session=%s category=doagent_execution_failed detail=%s",
			targetSessionID, sanitizeSecretText(sseErr.Error()))
		pushProgress(errorNotification("[error] " + sanitizeSecretText(sseErr.Error())))
		writeJSON(buildToolErrorResponse(reqID, "doagent_execution_failed"))
		return
	}
	if execution.Skill == "dosql" && toolTraces.hasFailedSQLRead() {
		writeJSON(buildToolErrorResponse(reqID, "dosql_query_failed"))
		return
	}
	if responseFormat == "json" {
		resultText, structuredContent, err := toolTraces.structuredResult(false)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, fmt.Sprintf("read final machine result: %v", err)))
			return
		}
		writeJSON(buildStructuredSuccessResponse(reqID, resultText, structuredContent))
		return
	}

	answer, err := toolTraces.finalAnswer()
	if err != nil {
		writeJSON(buildToolErrorResponse(reqID, err.Error()))
		return
	}
	writeJSON(buildSuccessResponse(reqID, answer))
}

// Apply facts come only from verified runtime receipts. Keep unknown outcomes
// as RPC errors for the Gateway's quarantine contract, with the history in data.
func buildDoagentApplyResponse(reqID interface{}, traces *doagentToolTraceCollector, code int, message string) api.JSONRPCResponse {
	text, report, err := traces.verifiedMutationResult()
	if err != nil {
		return buildErrorResponse(reqID, -32603, promptOutcomeUnknown("mutation_report_invalid"))
	}
	if code != 0 {
		report["outcome"] = "unknown"
		if code == -32007 {
			report["outcome"] = "cancelled"
		}
		response := buildErrorResponse(reqID, code, message)
		response.Error.Data = map[string]interface{}{"structuredContent": report}
		return response
	}
	if message != "" {
		return buildStructuredToolErrorResponse(reqID, message+"\n"+text, map[string]interface{}{
			"structuredContent": report,
		})
	}
	response := buildStructuredSuccessResponse(reqID, text, report)
	response.Result.(map[string]interface{})["isError"] = false
	return response
}

func validateAgentPromptAttachments(attachments []api.AgentPromptAttachment) error {
	if len(attachments) > maxAgentPromptAttachments {
		return fmt.Errorf("一次最多上传 %d 个附件", maxAgentPromptAttachments)
	}
	for _, attachment := range attachments {
		if strings.TrimSpace(attachment.Filename) == "" || strings.ContainsAny(attachment.Filename, `/\\`) || strings.ContainsAny(attachment.Filename, "\x00\r\n") {
			return fmt.Errorf("附件文件名无效")
		}
		if strings.TrimSpace(attachment.MediaType) == "" || strings.ContainsAny(attachment.MediaType, "\x00\r\n") {
			return fmt.Errorf("附件媒体类型无效")
		}
		decoded, err := base64.StdEncoding.DecodeString(attachment.Base64Data)
		if err != nil {
			return fmt.Errorf("附件编码无效")
		}
		if len(decoded) == 0 {
			return fmt.Errorf("附件不能为空")
		}
		if len(decoded) > maxAgentPromptAttachmentBytes {
			return fmt.Errorf("附件超过 25 MiB 限制")
		}
	}
	return nil
}

func (gw *Gateway) uploadAgentPromptAttachments(ctx context.Context, doagentURL, doopsSessionID string, attachments []api.AgentPromptAttachment) ([]map[string]interface{}, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	if filepath.Base(doopsSessionID) != doopsSessionID || strings.Contains(doopsSessionID, "..") {
		return nil, fmt.Errorf("invalid agent session id")
	}
	workspaceResp, err := doagentRPCContext(ctx, doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "workspace-" + doopsSessionID,
		"method":  "workspace/open",
		"params":  map[string]interface{}{"path": filepath.Join("/root/ws", doopsSessionID)},
	})
	if err != nil {
		return nil, fmt.Errorf("open doagent workspace: %w", err)
	}
	workspaceResult, ok := workspaceResp["result"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("doagent workspace response is invalid")
	}
	workspaceID, _ := workspaceResult["id"].(string)
	if strings.TrimSpace(workspaceID) == "" {
		return nil, fmt.Errorf("doagent workspace id is missing")
	}

	blocks := make([]map[string]interface{}, 0, len(attachments))
	for index, attachment := range attachments {
		response, uploadErr := doagentRPCContext(ctx, doagentURL, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      fmt.Sprintf("attachment-%s-%d", doopsSessionID, index),
			"method":  "attachment/upload",
			"params": map[string]interface{}{
				"workspaceId": workspaceID,
				"filename":    attachment.Filename,
				"mediaType":   attachment.MediaType,
				"base64Data":  attachment.Base64Data,
			},
		})
		if uploadErr != nil {
			return nil, fmt.Errorf("upload attachment %q: %w", attachment.Filename, uploadErr)
		}
		result, ok := response["result"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("attachment upload response is invalid")
		}
		attachmentID, _ := result["attachment_id"].(string)
		if strings.TrimSpace(attachmentID) == "" {
			return nil, fmt.Errorf("attachment upload id is missing")
		}
		blocks = append(blocks, map[string]interface{}{
			"type":          "attachment",
			"attachment_id": attachmentID,
			"workspaceId":   workspaceID,
		})
	}
	return blocks, nil
}

func (gw *Gateway) handleAgentMetadataWS(ctx context.Context, doopsSessionID string) (string, error) {
	doagentURL := configuredDoagentURL()
	status := inspectDoagentModelChannels()
	if (status.status == "unconfigured" || status.status == "awaiting_model_provider") && status.settingsFound {
		return marshalDoagentMetadataStatus(status)
	}
	if err := ensureDoagentAvailable(doagentURL); err != nil {
		status.status = "unavailable"
		status.channels = upsertModelChannel(status.channels, agentModelChannelStatus{
			Name:    "acp",
			Status:  "error",
			Message: "doagent_unavailable",
		})
		return marshalDoagentMetadataStatus(status)
	}

	resp, err := doagentRPC(doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "metadata-" + doopsSessionID,
		"method":  "initialize",
		"params":  map[string]interface{}{},
	})
	if err != nil {
		status.status = "unavailable"
		status.channels = upsertModelChannel(status.channels, agentModelChannelStatus{
			Name:    "acp",
			Status:  "error",
			Message: "doagent_initialize_failed",
		})
		return marshalDoagentMetadataStatus(status)
	}

	result, ok := resp["result"].(map[string]interface{})
	if !ok {
		status.status = "unavailable"
		status.channels = upsertModelChannel(status.channels, agentModelChannelStatus{
			Name:    "acp",
			Status:  "error",
			Message: "doagent initialize returned no result",
		})
		return marshalDoagentMetadataStatus(status)
	}
	meta, _ := result["_meta"].(map[string]interface{})
	model, _ := meta["model"].(string)
	tools, _ := meta["tools"].([]interface{})
	if strings.TrimSpace(model) != "" {
		status.model = normalizeProviderModel(status.provider, model)
	}
	status.status = "connected"
	status.channels = upsertModelChannel(status.channels, agentModelChannelStatus{
		Name:    "acp",
		Status:  "ok",
		Message: "doagent ACP initialize ok",
	})
	data, err := json.Marshal(map[string]interface{}{
		"source":         "doagent",
		"event_formats":  []string{"acp"},
		"event_schema":   "doops.sh/acp-events/v1",
		"status":         status.status,
		"provider":       status.provider,
		"model":          status.model,
		"models":         status.models,
		"tools":          tools,
		"channels":       status.channels,
		"channel_status": modelChannelsByName(status.channels),
		"metadata":       meta,
	})
	if err != nil {
		return "", fmt.Errorf("marshal doagent metadata: %w", err)
	}
	return string(data), nil
}

func (gw *Gateway) handleAgentHistoryWS(ctx context.Context, doopsSessionID string) (string, error) {
	_ = ctx
	gw.sessionMapMu.RLock()
	entry := gw.sessionMap[doopsSessionID]
	gw.sessionMapMu.RUnlock()
	if entry == nil || strings.TrimSpace(entry.doagentSessionID) == "" {
		return marshalDoagentHistory(doopsSessionID, nil)
	}

	doagentURL := configuredDoagentURL()
	if err := ensureDoagentAvailable(doagentURL); err != nil {
		return "", errors.New("doagent_unavailable")
	}
	resp, err := doagentRPC(doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "history-" + doopsSessionID,
		"method":  "session/messages",
		"params": map[string]interface{}{
			"sessionId": entry.doagentSessionID,
		},
	})
	if err != nil {
		return "", errors.New("doagent_history_unavailable")
	}
	return marshalDoagentHistory(doopsSessionID, resp["result"])
}

func marshalDoagentHistory(doopsSessionID string, messages interface{}) (string, error) {
	if messages == nil {
		messages = []interface{}{}
	}
	data, err := json.Marshal(map[string]interface{}{
		"source":    "doagent",
		"sessionId": doopsSessionID,
		"messages":  sanitizeDoagentValue(messages),
	})
	if err != nil {
		return "", fmt.Errorf("marshal doagent history: %w", err)
	}
	return string(data), nil
}

type agentModelChannelStatus struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

type agentModelRuntimeStatus struct {
	status        string
	source        string
	provider      string
	model         string
	models        []string
	settingsFound bool
	channels      []agentModelChannelStatus
}

func inspectDoagentModelChannels() agentModelRuntimeStatus {
	path := doagentSettingsPath()
	status := agentModelRuntimeStatus{
		status: "unconfigured",
		source: "doagent-settings",
		channels: []agentModelChannelStatus{
			{Name: "settings", Status: "error", Message: "settings.json not found"},
			{Name: "api_key", Status: "error", Message: "API key is missing"},
			{Name: "acp", Status: "unknown", Message: "not checked yet"},
		},
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return status
	}
	status.settingsFound = true
	status.channels[0] = agentModelChannelStatus{Name: "settings", Status: "ok", Message: path}

	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		status.channels[0] = agentModelChannelStatus{Name: "settings", Status: "error", Message: "settings.json is not valid JSON"}
		return status
	}

	status.model, _ = settings["model"].(string)
	providers, _ := settings["provider"].(map[string]interface{})
	if len(providers) > 0 {
		status.provider = providerForSettingsModel(providers, status.model)
		if status.provider == "" {
			status.provider = firstSettingsProvider(providers)
		}
		status.models = modelsForSettingsProvider(providers, status.provider)
	}
	if len(status.models) == 0 && strings.TrimSpace(status.model) != "" {
		status.models = []string{status.model}
	}
	if strings.TrimSpace(status.model) == "" && len(providers) == 0 {
		// A fresh Agent has no model until the Gateway delivers its
		// modelProvider credential; that is a pending state, not a fault.
		status.status = "awaiting_model_provider"
		status.channels[1] = agentModelChannelStatus{
			Name: "api_key", Status: "pending",
			Message: "waiting for the Gateway to deliver a modelProvider credential bound to this target",
		}
		return status
	}
	if strings.TrimSpace(status.model) == "" || strings.TrimSpace(status.provider) == "" {
		status.channels[1] = agentModelChannelStatus{Name: "api_key", Status: "error", Message: "model or provider is missing"}
		return status
	}

	apiKey, baseURL := providerCredentials(providers, status.provider)
	switch {
	case strings.TrimSpace(apiKey) == "":
		status.channels[1] = agentModelChannelStatus{Name: "api_key", Status: "error", Message: "API key is empty"}
	case isPlaceholderAPIKey(apiKey):
		status.channels[1] = agentModelChannelStatus{Name: "api_key", Status: "error", Message: "API key is still a placeholder"}
	default:
		msg := "API key configured"
		if strings.TrimSpace(baseURL) != "" {
			msg = "API key configured; baseURL=" + strings.TrimSpace(baseURL)
		}
		status.channels[1] = agentModelChannelStatus{Name: "api_key", Status: "ok", Message: msg}
		status.status = "configured"
	}
	return status
}

func marshalDoagentMetadataStatus(status agentModelRuntimeStatus) (string, error) {
	data, err := json.Marshal(map[string]interface{}{
		"source":         status.source,
		"event_formats":  []string{"acp"},
		"event_schema":   "doops.sh/acp-events/v1",
		"status":         status.status,
		"provider":       status.provider,
		"model":          status.model,
		"models":         status.models,
		"channels":       status.channels,
		"channel_status": modelChannelsByName(status.channels),
		"metadata": map[string]interface{}{
			"model":    status.model,
			"provider": status.provider,
			"models":   status.models,
		},
	})
	if err != nil {
		return "", fmt.Errorf("marshal doagent metadata status: %w", err)
	}
	return string(data), nil
}

func modelChannelsByName(channels []agentModelChannelStatus) map[string]agentModelChannelStatus {
	out := make(map[string]agentModelChannelStatus, len(channels))
	for _, ch := range channels {
		if ch.Name != "" {
			out[ch.Name] = ch
		}
	}
	return out
}

func upsertModelChannel(channels []agentModelChannelStatus, next agentModelChannelStatus) []agentModelChannelStatus {
	for i := range channels {
		if channels[i].Name == next.Name {
			channels[i] = next
			return channels
		}
	}
	return append(channels, next)
}

func providerForSettingsModel(providers map[string]interface{}, model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	for name, raw := range providers {
		cfg, _ := raw.(map[string]interface{})
		models, _ := cfg["models"].(map[string]interface{})
		if _, ok := models[model]; ok {
			return name
		}
		if _, ok := models[providerModelKey(name, model)]; ok {
			return name
		}
	}
	if strings.Contains(model, "/") {
		name := strings.SplitN(model, "/", 2)[0]
		if _, ok := providers[name]; ok {
			return name
		}
	}
	return ""
}

func firstSettingsProvider(providers map[string]interface{}) string {
	for name := range providers {
		return name
	}
	return ""
}

func modelsForSettingsProvider(providers map[string]interface{}, provider string) []string {
	cfg, _ := providers[provider].(map[string]interface{})
	modelsMap, _ := cfg["models"].(map[string]interface{})
	models := make([]string, 0, len(modelsMap))
	for model := range modelsMap {
		if strings.Contains(model, "/") {
			models = append(models, model)
		} else {
			models = append(models, provider+"/"+model)
		}
	}
	return models
}

func providerCredentials(providers map[string]interface{}, provider string) (string, string) {
	cfg, _ := providers[provider].(map[string]interface{})
	options, _ := cfg["options"].(map[string]interface{})
	apiKey, _ := options["apiKey"].(string)
	baseURL, _ := options["baseURL"].(string)
	if apiKey == "" {
		apiKey, _ = options["api_key"].(string)
	}
	return apiKey, baseURL
}

func providerModelKey(provider, model string) string {
	if strings.HasPrefix(model, provider+"/") {
		return strings.TrimPrefix(model, provider+"/")
	}
	return provider + "/" + model
}

func normalizeProviderModel(provider, model string) string {
	model = strings.TrimSpace(model)
	if model == "" || strings.Contains(model, "/") || strings.TrimSpace(provider) == "" {
		return model
	}
	return strings.TrimSpace(provider) + "/" + model
}

func isPlaceholderAPIKey(apiKey string) bool {
	apiKey = strings.TrimSpace(strings.ToLower(apiKey))
	return apiKey == "" || strings.Contains(apiKey, "your_") || strings.Contains(apiKey, "replace") || strings.Contains(apiKey, "placeholder")
}

// doagentSettingsFile is the only settings file doagent reads. It is a variable
// solely so tests can point it at a fixture.
var doagentSettingsFile = "/root/.agent/settings.json"

func doagentSettingsPath() string {
	return doagentSettingsFile
}

func configuredDoagentURL() string {
	if baseURL := strings.TrimSpace(os.Getenv("DO_AGENT_URL")); baseURL != "" {
		return baseURL
	}
	port := strings.TrimSpace(os.Getenv("DO_AGENT_PORT"))
	if port == "" {
		port = "9000"
	}
	return "http://127.0.0.1:" + port
}

func ensureDoagentAvailable(baseURL string) error {
	if doagentTCPReady(baseURL, 500*time.Millisecond) {
		return nil
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("invalid DO_AGENT_URL %q: %w", baseURL, err)
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "" {
		return fmt.Errorf("%s is not reachable", baseURL)
	}
	port := parsed.Port()
	if port == "" {
		port = "9000"
	}
	bin, err := exec.LookPath("do-agent")
	if err != nil {
		bin = "/usr/local/bin/do-agent"
		if _, statErr := os.Stat(bin); statErr != nil {
			return fmt.Errorf("do-agent binary not found")
		}
	}
	if err := os.MkdirAll("/root/ws", 0755); err != nil {
		return fmt.Errorf("create /root/ws: %w", err)
	}
	cmd := exec.Command(bin, "acp-http", "--port", port, "--cwd", "/root/ws")
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start do-agent acp-http: %w", err)
	}
	log.Printf("🤖 started doagent ACP HTTP on demand: pid=%d port=%s", cmd.Process.Pid, port)
	for i := 0; i < 20; i++ {
		if doagentTCPReady(baseURL, 500*time.Millisecond) {
			return nil
		}
		if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
			return errors.New("do-agent exited before listening")
		}
		time.Sleep(250 * time.Millisecond)
	}
	return errors.New("do-agent did not listen after startup")
}

func doagentTCPReady(baseURL string, timeout time.Duration) bool {
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	port := parsed.Port()
	if host == "" {
		host = "127.0.0.1"
	}
	if port == "" {
		port = "9000"
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// doagentRPC 向本地 doagent ACP HTTP 服务发送 JSON-RPC 请求并返回响应。
func doagentRPC(baseURL string, payload map[string]interface{}) (map[string]interface{}, error) {
	return doagentRPCContext(context.Background(), baseURL, payload)
}

func doagentRPCContext(ctx context.Context, baseURL string, payload map[string]interface{}) (map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST /rpc: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal response (status=%d): %w", resp.StatusCode, err)
	}

	if rpcErr, ok := result["error"]; ok && rpcErr != nil {
		return result, fmt.Errorf("RPC error: %v", rpcErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("RPC HTTP status %d", resp.StatusCode)
	}

	return result, nil
}

const doagentCancellationTimeout = 10 * time.Second

func cancelDoagentPrompt(ctx context.Context, baseURL, sessionID string, done <-chan struct{}, terminalSeen *atomic.Bool, timeout time.Duration) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	// session/cancel acknowledges a request, not termination. Leave SSE open
	// until the matching turn_finished, even if the ACK says not_running.
	_, _ = doagentRPCContext(cleanupCtx, baseURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "cancel-" + sessionID,
		"method":  "session/cancel",
		"params":  map[string]interface{}{"sessionId": sessionID},
	})
	if done != nil {
		select {
		case <-done:
		case <-cleanupCtx.Done():
		}
	}
	if terminalSeen.Load() {
		return nil
	}
	return errExecutionOutcomeUnknown
}

type doagentSSECollector func(update map[string]interface{}) error

func doagentAgentMessageText(content interface{}) (string, error) {
	switch value := content.(type) {
	case map[string]interface{}:
		text, _ := value["text"].(string)
		if text == "" {
			return "", fmt.Errorf("agent_message has no text content")
		}
		return text, nil
	case []interface{}:
		var text strings.Builder
		for _, item := range value {
			message, _ := item.(map[string]interface{})
			inner, _ := message["content"].(map[string]interface{})
			part, _ := inner["text"].(string)
			if part == "" {
				return "", fmt.Errorf("agent_message has non-text content")
			}
			text.WriteString(part)
		}
		if text.Len() == 0 {
			return "", fmt.Errorf("agent_message has no text content")
		}
		return text.String(), nil
	case string:
		if value == "" {
			return "", fmt.Errorf("agent_message has no text content")
		}
		return value, nil
	default:
		return "", fmt.Errorf("agent_message has unsupported content")
	}
}

func decodeDoagentJSONObject(text string) (map[string]interface{}, error) {
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	var object map[string]interface{}
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}
	if object == nil {
		return nil, fmt.Errorf("expected JSON object")
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("expected exactly one JSON object")
		}
		return nil, err
	}
	return object, nil
}

// subscribeDoagentSSE 订阅 doagent 的 SSE 事件流，将内容实时转发到 WebSocket 客户端。
// 仅当收到权威 turn_finished 或 error 事件时返回。
func subscribeDoagentSSE(ctx context.Context, baseURL string, sessionID string, pushProgress notificationSender) error {
	return subscribeDoagentSSEWithCollector(ctx, baseURL, sessionID, pushProgress, nil)
}

func subscribeDoagentSSEWithCollector(ctx context.Context, baseURL string, sessionID string, pushProgress notificationSender, collector doagentSSECollector) error {
	return subscribeDoagentSSEWithCollectorReady(ctx, baseURL, sessionID, pushProgress, collector, nil, false, true, nil)
}

var doagentWaitingNoticeInterval = 20 * time.Second

func subscribeDoagentSSEWithCollectorReady(
	ctx context.Context,
	baseURL string,
	sessionID string,
	pushProgress notificationSender,
	collector doagentSSECollector,
	targetTurnIDReady <-chan string,
	allowUntaggedCurrentTurn bool,
	forwardAgentMessages bool,
	ready chan<- error,
	structuredFormats ...bool,
) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/events?sid="+sessionID, nil)
	if err != nil {
		if ready != nil {
			ready <- err
		}
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		if ready != nil {
			ready <- err
		}
		return fmt.Errorf("SSE connect: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("SSE connect returned HTTP %d", resp.StatusCode)
		if ready != nil {
			ready <- err
		}
		return err
	}
	if ready != nil {
		ready <- nil
	}

	scanner := bufio.NewScanner(resp.Body)
	// One SSE event carries a whole tool result, so a line can be far larger
	// than a log line. A line beyond this limit used to kill the stream and
	// turn a finished turn into an unknown outcome.
	scanner.Buffer(make([]byte, 256*1024), maxDoagentSSELineBytes)

	var eventData strings.Builder
	var pendingAgentMessage strings.Builder
	targetTurnID := ""
	structuredEvents := len(structuredFormats) == 1 && structuredFormats[0]
	// Serialize the ticker with SSE delivery, including callers whose output
	// buffer is not concurrent. Notices describe silence, never tool progress.
	var progressMu sync.Mutex
	lastActivity := time.Now()
	lastProgress := lastActivity
	lastEvent := "SSE connected"
	sendProgress := pushProgress
	pushProgress = func(event notificationEvent) {
		progressMu.Lock()
		defer progressMu.Unlock()
		sendProgress(event)
		lastProgress = time.Now()
	}
	noticesDone, stopNotices := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(noticesDone)
		ticker := time.NewTicker(doagentWaitingNoticeInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-stopNotices:
				return
			case <-ticker.C:
				progressMu.Lock()
				silent := time.Since(lastProgress)
				if silent >= doagentWaitingNoticeInterval {
					text := fmt.Sprintf("[waiting] Still waiting for the terminal result; last event: %s (%s ago).\n",
						lastEvent, time.Since(lastActivity).Round(time.Second))
					if structuredEvents {
						sendProgress(publicDoagentUINotification(map[string]interface{}{
							"sessionUpdate": "doops_notice",
							"content":       map[string]interface{}{"type": "text", "text": text},
						}))
					} else {
						sendProgress(rawNotification(text))
					}
					lastProgress = time.Now()
				}
				progressMu.Unlock()
			}
		}
	}()
	defer func() {
		close(stopNotices)
		<-noticesDone
	}()
	toolNames := make(map[string]string)
	flushPendingAgentMessage := func() {
		if pendingAgentMessage.Len() == 0 {
			return
		}
		text := redactSecretText(pendingAgentMessage.String())
		if forwardAgentMessages && !structuredEvents {
			pushProgress(assistantDeltaNotification(text))
		}
		if structuredEvents {
			pushProgress(publicDoagentUINotification(map[string]interface{}{
				"sessionUpdate": "agent_message", "turnId": targetTurnID,
				"content": map[string]interface{}{"type": "text", "text": pendingAgentMessage.String()},
			}))
		}
		pendingAgentMessage.Reset()
	}
	// No idle timer: a running tool (Helm rollout wait, image pull, build) is
	// silent by design. The stream ends on turn_finished, a read error when the
	// connection drops, or the caller's execution deadline.
	receivedEvent := false

	for scanner.Scan() {
		line := scanner.Text()

		// SSE 格式: "event: type", "data: {json}", 空行分隔
		if strings.HasPrefix(line, "event: ") {
			continue // 事件类型从 data 的 type 字段判断
		}

		if strings.HasPrefix(line, "data: ") {
			eventData.WriteString(strings.TrimPrefix(line, "data: "))
			continue
		}

		// 空行 = 一个完整事件结束
		if line == "" && eventData.Len() > 0 {
			data := eventData.String()
			eventData.Reset()
			receivedEvent = true

			// doagent SSE 使用 JSON-RPC 2.0 通知格式：{"jsonrpc":"2.0","method":"...","params":{...}}
			update, err := decodeDoagentJSONObject(data)
			if err != nil {
				pushProgress(errorNotification("[error] doagent_sse_protocol_invalid"))
				return errors.New("doagent_sse_protocol_invalid")
			}

			method, _ := update["method"].(string)
			params, _ := update["params"].(map[string]interface{})

			switch method {
			case "session/update":
				if eventSessionID, _ := params["sessionId"].(string); eventSessionID != "" && eventSessionID != sessionID {
					continue
				}
				// session/update 包含不同类型的更新
				sessionUpdate, _ := params["update"].(map[string]interface{})
				updateType, _ := sessionUpdate["sessionUpdate"].(string)
				if targetTurnIDReady != nil {
					if targetTurnID == "" {
						select {
						case turnID, ok := <-targetTurnIDReady:
							if !ok || strings.TrimSpace(turnID) == "" {
								return fmt.Errorf("doagent prompt turn scope was not established")
							}
							targetTurnID = turnID
						case <-ctx.Done():
							return ctx.Err()
						}
					}
					eventTurnID, _ := sessionUpdate["turnId"].(string)
					if strings.TrimSpace(eventTurnID) == "" {
						if !allowUntaggedCurrentTurn ||
							sessionUpdate["replay"] == true ||
							updateType == "turn_finished" {
							continue
						}
					} else if eventTurnID != targetTurnID {
						continue
					}
				}
				if sessionUpdate["replay"] != true {
					progressMu.Lock()
					lastActivity, lastEvent = time.Now(), updateType
					if updateType == "tool_call" || updateType == "tool_call_update" {
						callID, _ := sessionUpdate["toolCallId"].(string)
						if name := doagentToolName(sessionUpdate); name != "" {
							toolNames[callID] = sanitizeSecretText(name)
						}
						status, _ := sessionUpdate["status"].(string)
						lastEvent = fmt.Sprintf("%s %s", toolNames[callID], sanitizeSecretText(status))
					}
					progressMu.Unlock()
				}
				if updateType == "agent_message_chunk" {
					text, err := doagentAgentMessageText(sessionUpdate["content"])
					if err != nil {
						return errors.New("doagent_sse_protocol_invalid")
					}
					if pendingAgentMessage.Len()+len(text) > 4<<20 {
						return errors.New("doagent_message_too_large")
					}
					pendingAgentMessage.WriteString(text)
					continue
				}
				if updateType == "agent_message" {
					// agent_message usually repeats the buffered chunks as the whole
					// reply. Forwarding both showed the same answer twice.
					final, err := doagentAgentMessageText(sessionUpdate["content"])
					if !structuredEvents && err == nil && pendingAgentMessage.Len() > 0 &&
						strings.HasPrefix(final, pendingAgentMessage.String()) {
						pendingAgentMessage.Reset()
					}
					flushPendingAgentMessage()
				}
				if updateType == "turn_finished" && pendingAgentMessage.Len() > 0 {
					// A reply streamed only as chunks has no agent_message event; the
					// collector still needs it as the turn's final answer.
					streamed := redactSecretText(pendingAgentMessage.String())
					flushPendingAgentMessage()
					if collector != nil {
						if err := collector(map[string]interface{}{
							"sessionUpdate": doagentStreamedReplyUpdate, "turnId": targetTurnID,
							"content": map[string]interface{}{"type": "text", "text": streamed},
						}); err != nil {
							return err
						}
					}
				}
				if updateType == "agent_progress" {
					text, err := doagentAgentMessageText(sessionUpdate["content"])
					if err != nil {
						return errors.New("doagent_sse_protocol_invalid")
					}
					public, err := redactDoagentUIText(text)
					if err != nil {
						public = "[progress text withheld: private payload could not be redacted; still waiting for the execution result]"
					}
					if sessionUpdate["replay"] != true {
						if structuredEvents {
							pushProgress(publicDoagentUINotification(map[string]interface{}{
								"sessionUpdate": "doops_notice", "turnId": targetTurnID,
								"content": map[string]interface{}{"type": "text", "text": public},
							}))
						} else {
							pushProgress(rawNotification("[progress] " + public + "\n"))
						}
					}
					// Progress is diagnostic UI text, not execution evidence.
					// Do not pass it to the receipt collector.
					continue
				}
				if collector != nil {
					if err := collector(sessionUpdate); err != nil {
						return err
					}
				}
				if structuredEvents {
					pushProgress(publicDoagentUINotification(sessionUpdate))
					if updateType == "turn_finished" {
						return doagentTurnFinishedError(sessionUpdate)
					}
					continue
				}
				sessionUpdate = sanitizeDoagentSessionUpdate(sessionUpdate)
				switch updateType {
				case "tool_call", "tool_call_update":
					callID, _ := sessionUpdate["toolCallId"].(string)
					toolName := toolNames[callID]
					status, _ := sessionUpdate["status"].(string)
					if toolName != "" && status != "" && sessionUpdate["replay"] != true {
						pushProgress(toolNotification(toolName, sanitizeSecretText(status)))
					}
				case "agent_message":
					text, err := doagentAgentMessageText(sessionUpdate["content"])
					if err != nil {
						return errors.New("doagent_sse_protocol_invalid")
					}
					if forwardAgentMessages {
						pushProgress(assistantDeltaNotification(text))
					}
				case "completed", "usage_update":
					// 非权威状态或 token 统计，继续等待 turn_finished。
				case "turn_finished":
					if err := doagentTurnFinishedError(sessionUpdate); err != nil {
						return err
					}
					return nil
				}

			case "permission.updated":
				message := "doagent permission required"
				pushProgress(errorNotification("[error] " + message))
				return errors.New(message)

			case "error":
				errMsg := "doagent_execution_failed"
				if params != nil {
					for _, key := range []string{"error", "failureReason", "message", "reason"} {
						if value, ok := params[key].(string); ok && strings.TrimSpace(value) != "" {
							errMsg = sanitizeSecretText(value)
							break
						}
					}
				}
				return fmt.Errorf("doagent execution failed: %s", errMsg)

			default:
				// 其他事件（session/update chunks 等）静默处理
				if method != "" {
					log.Printf("doagent SSE event ignored")
				} else {
					pushProgress(errorNotification("[error] doagent_sse_protocol_invalid"))
					return errors.New("doagent_sse_protocol_invalid")
				}
			}
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("SSE read: %w", err)
	}
	if receivedEvent {
		return fmt.Errorf("doagent SSE ended before final event")
	}
	return fmt.Errorf("doagent SSE ended without events")
}

func doagentTurnFinishedError(update map[string]interface{}) error {
	status := strings.TrimSpace(fmt.Sprint(update["status"]))
	stopReason := strings.TrimSpace(fmt.Sprint(update["stopReason"]))
	message := strings.TrimSpace(fmt.Sprint(update["failureReason"]))
	if message == "" || message == "<nil>" {
		message = strings.TrimSpace(fmt.Sprint(update["error"]))
	}
	if message != "" && message != "<nil>" {
		return fmt.Errorf("doagent turn failed: %s", sanitizeSecretText(message))
	}
	if status == "completed" {
		return nil
	}
	if status == "" || status == "<nil>" {
		status = "unknown"
	}
	if stopReason == "" || stopReason == "<nil>" {
		return fmt.Errorf("doagent turn %s", status)
	}
	return fmt.Errorf("doagent turn %s: %s", status, stopReason)
}

// maxDoagentSSELineBytes bounds one doagent SSE line. A line holds a whole
// tool result, so this must be well above maxToolOutputBytes.
const maxDoagentSSELineBytes = 8 * 1024 * 1024

// sanitizeSecretText redacts and keeps only the last 1200 bytes, cut on a rune
// boundary. That is right for error text but drops the start of query results.
func sanitizeSecretText(s string) string {
	return utf8Tail(redactSecretText(s), 1200)
}

// redactSecretText redacts without truncating, for data that must stay complete.
func redactSecretText(s string) string {
	if s == "" {
		return ""
	}
	replacers := []*regexp.Regexp{
		regexp.MustCompile(`(?is)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/@\s]+@`),
		regexp.MustCompile(`(?i)([?&](?:[A-Za-z0-9._-]*(?:token|password|secret|signature|credential|api[_-]?key)|sig|auth|authorization|cookie)=)[^&#\s]+`),
		regexp.MustCompile(`sk-[A-Za-z0-9_-]+`),
		regexp.MustCompile(`(?i)(api[_-]?key[=:])[^ ,}\n]+`),
		regexp.MustCompile(`(?i)(api[_-]?key )[A-Za-z0-9._~+/=-]{20,}`),
		regexp.MustCompile(`(?i)(Bearer )[A-Za-z0-9._-]+`),
		regexp.MustCompile(`(?i)(Basic )[A-Za-z0-9+/=._-]+`),
		regexp.MustCompile(`(?i)((?:authorization|cookie|password|token|secret)[_-]?[A-Za-z0-9_-]*[=:])[^ ,}\n]+`),
		// The space-separated form only counts as a credential when the value is
		// long enough to be one. "token too long" is prose, not a secret.
		regexp.MustCompile(`(?i)((?:authorization|cookie|password|token|secret)[_-]?[A-Za-z0-9_-]* )[A-Za-z0-9._~+/=-]{20,}`),
	}
	s = replacers[0].ReplaceAllString(s, "...REDACTED PRIVATE KEY...")
	for _, replacer := range replacers[1:] {
		s = replacer.ReplaceAllString(s, "${1}...REDACTED")
	}
	for _, pattern := range credentialInlineMaterialPatterns {
		s = pattern.ReplaceAllString(s, "...REDACTED")
	}
	return s
}

// sanitizeDoagentValue redacts and caps every string, for history and error data.
func sanitizeDoagentValue(value interface{}) interface{} {
	return mapDoagentValue(value, sanitizeSecretText)
}

// redactDoagentValue redacts every string without truncating it, for answers.
func redactDoagentValue(value interface{}) interface{} {
	return mapDoagentValue(value, redactSecretText)
}

func mapDoagentValue(value interface{}, text func(string) string) interface{} {
	switch typed := value.(type) {
	case string:
		return text(typed)
	case []interface{}:
		sanitized := make([]interface{}, len(typed))
		for index, item := range typed {
			sanitized[index] = mapDoagentValue(item, text)
		}
		return sanitized
	case map[string]interface{}:
		sanitized := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			if credentialSecretLikeKey(key) || strings.EqualFold(strings.TrimSpace(key), "passwd") {
				sanitized[key] = "...REDACTED"
			} else {
				sanitized[key] = mapDoagentValue(item, text)
			}
		}
		return sanitized
	default:
		return value
	}
}

func sanitizeDoagentSessionUpdate(update map[string]interface{}) map[string]interface{} {
	if update == nil {
		return nil
	}
	sanitized := make(map[string]interface{}, len(update))
	for key, value := range update {
		sanitized[key] = value
	}
	updateType, _ := update["sessionUpdate"].(string)
	switch updateType {
	case "agent_message_chunk", "agent_message":
		// Message text is the answer itself; redact it without the error-text cap.
		sanitized["content"] = redactDoagentValue(update["content"])
	case "error", "permission_request", "turn_finished":
		for _, key := range []string{"error", "failureReason", "message", "reason"} {
			if value, ok := update[key].(string); ok {
				sanitized[key] = sanitizeSecretText(value)
			}
		}
	}
	return sanitized
}

// -----------------------------------------------------------------------------
// 共享的底层基础设施流式处理器
// -----------------------------------------------------------------------------

// executeRawCommand 使用纯标准的 os/exec 启动进程（去 PTY 化），返回 cmd 对象、标准输出管、错误输出管
func executeRawCommand(ctx context.Context, command string) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	cmd, script, err := bashCommand(command)
	if err != nil {
		return nil, nil, nil, err
	}
	if script != nil {
		// The child holds its own copy of the descriptor after Start.
		defer script.Close()
	}
	return startRawCommand(ctx, cmd)
}

// inlineCommandLimit keeps `bash -c` for ordinary commands. Linux caps one
// argv entry at 128 KiB (MAX_ARG_STRLEN), so a longer command is written to an
// unlinked private file and run as `bash /dev/fd/3`; callers see no difference
// and nothing is left on disk.
const inlineCommandLimit = 64 << 10

func bashCommand(command string) (*exec.Cmd, *os.File, error) {
	if len(command) <= inlineCommandLimit {
		return exec.Command("bash", "-c", command), nil, nil
	}
	file, err := os.CreateTemp("", "doops-cmd-*.sh")
	if err != nil {
		return nil, nil, fmt.Errorf("prepare long command: %w", err)
	}
	_ = os.Remove(file.Name())
	if _, err := file.WriteString(command); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("prepare long command: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		_ = file.Close()
		return nil, nil, fmt.Errorf("prepare long command: %w", err)
	}
	cmd := exec.Command("bash", "/dev/fd/3")
	cmd.ExtraFiles = []*os.File{file}
	return cmd, file, nil
}

func startRawCommand(ctx context.Context, cmd *exec.Cmd) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = commandPipeGracePeriod

	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, nil, nil, errors.New("command output already set")
	}
	stdoutPipe, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, err
	}
	defer stdoutWriter.Close()
	stderrPipe, stderrWriter, err := os.Pipe()
	if err != nil {
		_ = stdoutPipe.Close()
		return nil, nil, nil, err
	}
	defer stderrWriter.Close()
	// Wait must not own the read ends; fast commands can exit before readers run.
	cmd.Stdout = stdoutWriter
	cmd.Stderr = stderrWriter

	if err := cmd.Start(); err != nil {
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
		return nil, nil, nil, err
	}
	return cmd, stdoutPipe, stderrPipe, nil
}

// Wait for both the command and its process group. Cancellation escalation is
// joined here, never left running after a terminal RPC response.
const postLeaderDescendantGracePeriod = 10 * time.Second
const commandPipeGracePeriod = time.Second

func waitProcessGroup(ctx context.Context, pid int, commandDone <-chan error) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	ctxDone := ctx.Done()
	var killTimer, cleanupTimer, postLeaderTimer *time.Timer
	var killDeadline, cleanupDeadline, postLeaderDeadline <-chan time.Time
	forcedCleanup := false
	defer func() {
		if killTimer != nil {
			killTimer.Stop()
		}
		if cleanupTimer != nil {
			cleanupTimer.Stop()
		}
		if postLeaderTimer != nil {
			postLeaderTimer.Stop()
		}
	}()
	var commandErr error
	for {
		if commandDone == nil {
			// Only reap adopted descendants after exec.Cmd.Wait owns the leader's status.
			for {
				var status syscall.WaitStatus
				reaped, _ := syscall.Wait4(-pid, &status, syscall.WNOHANG, nil)
				if reaped <= 0 {
					break
				}
			}
			err := syscall.Kill(-pid, 0)
			if errors.Is(err, syscall.ESRCH) {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if forcedCleanup {
					return fmt.Errorf("%w: detached process group was terminated", errExecutionOutcomeUnknown)
				}
				return commandErr
			}
			// A canceled group remains owned until absence is observed or the
			// cleanup deadline expires, including a temporarily denied status probe.
			if err != nil && !(ctx.Err() != nil && errors.Is(err, syscall.EPERM)) {
				return fmt.Errorf("%w: process group status: %v", errExecutionOutcomeUnknown, err)
			}
		}
		select {
		case commandErr = <-commandDone:
			commandDone = nil
			if ctx.Err() == nil {
				postLeaderTimer = time.NewTimer(postLeaderDescendantGracePeriod)
				postLeaderDeadline = postLeaderTimer.C
			}
		case <-ctxDone:
			ctxDone = nil
			if postLeaderTimer != nil {
				postLeaderTimer.Stop()
				postLeaderDeadline = nil
			}
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			killTimer = time.NewTimer(2 * time.Second)
			cleanupTimer = time.NewTimer(10 * time.Second)
			killDeadline = killTimer.C
			cleanupDeadline = cleanupTimer.C
		case <-killDeadline:
			killDeadline = nil
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		case <-cleanupDeadline:
			return fmt.Errorf("%w: process group did not terminate", errExecutionOutcomeUnknown)
		case <-postLeaderDeadline:
			postLeaderDeadline = nil
			forcedCleanup = true
			_ = syscall.Kill(-pid, syscall.SIGTERM)
			killTimer = time.NewTimer(2 * time.Second)
			cleanupTimer = time.NewTimer(10 * time.Second)
			killDeadline = killTimer.C
			cleanupDeadline = cleanupTimer.C
		case <-ticker.C:
		}
	}
}

// waitCommandStreams owns the read ends independently of Cmd.Wait. Bound the
// final drain because writers can escape the process-group watchdog.
func waitCommandStreams(ctx context.Context, cmd *exec.Cmd, stdout, stderr io.ReadCloser, push notificationSender, heartbeat bool) error {
	streamDone := make(chan error, 2)
	readStream := func(pipe io.ReadCloser) {
		scanner := bufio.NewScanner(pipe)
		defer func() {
			_ = pipe.Close()
			streamDone <- scanner.Err()
		}()
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		for scanner.Scan() {
			push(rawNotification(scanner.Text() + "\n"))
		}
	}
	go readStream(stdout)
	go readStream(stderr)

	var done, heartbeatDone chan struct{}
	if heartbeat {
		done = make(chan struct{})
		heartbeatDone = make(chan struct{})
		go func() {
			defer close(heartbeatDone)
			slowProgressHeartbeat(done, push)
		}()
	}
	commandDone := make(chan error, 1)
	go func() { commandDone <- cmd.Wait() }()
	cmdErr := waitProcessGroup(ctx, cmd.Process.Pid, commandDone)
	if errors.Is(cmdErr, errExecutionOutcomeUnknown) {
		_ = stdout.Close()
		_ = stderr.Close()
	}
	drainTimer := time.NewTimer(commandPipeGracePeriod)
	defer drainTimer.Stop()
	drainDeadline := drainTimer.C
	for remaining := 2; remaining > 0; {
		select {
		case readErr := <-streamDone:
			remaining--
			if readErr != nil && !errors.Is(cmdErr, errExecutionOutcomeUnknown) {
				cmdErr = fmt.Errorf("%w: command output read: %w", errExecutionOutcomeUnknown, readErr)
			}
		case <-drainDeadline:
			drainDeadline = nil
			_ = stdout.Close()
			_ = stderr.Close()
			cmdErr = fmt.Errorf("%w: command output drain timed out", errExecutionOutcomeUnknown)
		}
	}
	if done != nil {
		close(done)
		<-heartbeatDone
	}
	return cmdErr
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		return exitErr.ExitCode()
	}
	return 1
}

func sshExitCode(err error) (int, bool) {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return 0, false
	}
	code := exitErr.ExitCode()
	return code, code >= 0 && code <= 255
}

func writeSessionCommandAudit(sessionID, summary string, cmdErr error) {
	if sessionID == "" {
		return
	}
	entry := fmt.Sprintf("# [%s] exit=%d\n%s\n", time.Now().Format("2006-01-02 15:04:05"), commandExitCode(cmdErr), summary)
	if logFile, err := appendSessionAuditLog(sessionID, entry); err == nil {
		log.Printf("✅ Audit Log saved to %s", logFile)
	} else {
		log.Printf("❌ Failed to save Audit Log for session %s: %v", sessionID, err)
	}
}

// streamReader 逐行读取 pipe 内容并推送到 WS 客户端
func streamReader(pipe io.ReadCloser, pushProgress notificationSender) {
	scanner := bufio.NewScanner(pipe)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 允许长行
	for scanner.Scan() {
		line := scanner.Text() + "\n"
		pushProgress(rawNotification(line))
	}
}

// slowProgressHeartbeat 定期发送正在执行心跳标签
func slowProgressHeartbeat(doneCh chan struct{}, pushProgress notificationSender) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	elapsed := 0
	for {
		select {
		case <-doneCh:
			return
		case <-ticker.C:
			elapsed += 5
			pushProgress(rawNotification(fmt.Sprintf("\r\033[K[agent] ⏳ 命令后台执行中... (耗时 %ds)", elapsed)))
		}
	}
}

// buildSuccessResponse 构建标准的 MCP ToolCall 成功响应包
func buildSuccessResponse(reqID interface{}, text string) api.JSONRPCResponse {
	return api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      reqID,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": text,
				},
			},
		},
	}
}

func buildStructuredSuccessResponse(reqID interface{}, text string, structuredContent map[string]interface{}) api.JSONRPCResponse {
	return api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      reqID,
		Result: map[string]interface{}{
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": text,
				},
			},
			"structuredContent": structuredContent,
		},
	}
}

// buildErrorResponse 构建标准的 MCP ToolCall 异常响应包
func buildErrorResponse(reqID interface{}, code int, message string) api.JSONRPCResponse {
	return api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      reqID,
		Error:   &api.RPCError{Code: code, Message: message},
	}
}

// buildToolErrorResponse 构建工具执行逻辑失败的响应包（退出码非 0）
func buildToolErrorResponse(reqID interface{}, text string) api.JSONRPCResponse {
	return buildStructuredToolErrorResponse(reqID, text, nil)
}

func buildStructuredToolErrorResponse(reqID interface{}, text string, fields map[string]interface{}) api.JSONRPCResponse {
	result := map[string]interface{}{
		"isError": true,
		"content": []map[string]interface{}{
			{
				"type": "text",
				"text": text,
			},
		},
	}
	for key, value := range fields {
		result[key] = value
	}
	return api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      reqID,
		Result:  result,
	}
}
