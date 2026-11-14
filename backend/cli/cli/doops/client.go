/**
 * client.go - MCP 客户端（持久 WebSocket 连接版）
 *
 * 核心改造：MCPClient 在首次调用时建立一条持久 WebSocket 连接，
 * 后续所有 Call() 和 CallAndCapture() 复用同一个连接。
 * 这解决了旧版每次命令都新建连接导致的：
 *   1. 重复 tool call 创建新连接
 *   2. 短连接受 LB idle timeout 截断
 *   3. 连接握手延迟累积
 */
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	websocketHandshakeAttempts   = 3
	websocketHandshakeRetryDelay = 50 * time.Millisecond
	executionCancellationTimeout = 30 * time.Second
)

var shellProgressHeartbeatPattern = regexp.MustCompile(`^\r\x1b\[K\[agent\] ⏳ 命令后台执行中\.\.\. \(耗时 [1-9][0-9]*s\)$`)

// MCPClient 是 doops CLI 的 MCP 客户端。
// 改造后持有一条持久 WebSocket 连接，支持多次 tool call 复用。
type MCPClient struct {
	Target       Server
	SessionStore *SessionStore
	SessionName  string
	Verbose      bool
	Token        string
	// UpgradeOperationID correlates upgrade calls and their initialize probes.
	UpgradeOperationID string

	// --- 持久连接状态 ---
	mu           sync.Mutex
	writes       chan struct{}
	connected    bool
	conn         *websocket.Conn // WebSocket 长连接
	reqCounter   int64           // 请求 ID 计数器（原子递增）
	capabilities map[string]interface{}
	// Enabled only by the COS snapshot client; every connection must advertise
	// the Gateway's executable-bit contract before snapshot calls can be sent.
	requireCOSSnapshotExecutable bool

	// --- WS 消息分发 ---
	// 所有 WS 消息通过 dispatchLoop 读取并按 request ID 分发
	dispatching bool
	pendingMu   sync.RWMutex
	pending     map[int64]chan wsEvent // reqID -> 等待响应的 channel

	CallTimeout time.Duration
}

// wsEvent 封装从 WebSocket 流中解析出的 JSON-RPC 消息
type wsEvent struct {
	Raw     string                 // 原始 JSON 字符串
	Parsed  map[string]interface{} // 解析后的 JSON 对象
	IsError bool                   // 是否为错误
}

type gatewayRPCError struct {
	Code    float64
	Message string
	Data    map[string]interface{}
}

func (e *gatewayRPCError) Error() string {
	message := fmt.Sprintf("remote error: %s", e.Message)
	report, ok := e.Data["structuredContent"].(map[string]interface{})
	if !ok || report["kind"] != "doops.verified-mutation" {
		return message
	}
	// This report is public Gateway evidence; other RPC data may contain secrets.
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Sprintf("%s\nencode doops.verified-mutation receipt: %v", message, err)
	}
	return message + "\n" + string(encoded)
}

func executionOutcomeUnknownError() error {
	return &gatewayRPCError{Code: -32008, Message: "execution_outcome_unknown"}
}

// NewMCPClient 创建 MCP 客户端实例（此时不建连）
func NewMCPClient(target Server, ss *SessionStore, sessionName string, verbose bool) *MCPClient {
	return &MCPClient{
		Target:       target,
		SessionStore: ss,
		SessionName:  sessionName,
		Verbose:      verbose,
		pending:      make(map[int64]chan wsEvent),
		writes:       make(chan struct{}, 1),
		CallTimeout:  defaultToolCallTimeout(),
	}
}

// log 打印调试日志（仅 verbose 模式）
func (c *MCPClient) log(step, msg string) {
	if c.Verbose {
		if c.UpgradeOperationID != "" {
			msg = redactUpgradeText(msg, c.Token)
		}
		fmt.Printf("\033[94m[%s]\033[0m %s\n", step, msg)
	}
}

// nextReqID 原子递增生成唯一请求 ID
func (c *MCPClient) nextReqID() int64 {
	return atomic.AddInt64(&c.reqCounter, 1)
}

// connect 建立与 agent 的持久 WebSocket 连接并完成 MCP initialize 握手。
func (c *MCPClient) connect() error {
	return c.connectWithVerification("", "")
}

func (c *MCPClient) connectWithVerification(expectedRelease, requiredProtocol string) (err error) {
	defer func() {
		if c.UpgradeOperationID != "" && err != nil {
			var rpcErr *gatewayRPCError
			if errors.As(err, &rpcErr) {
				data, _ := redactUpgradeValue(rpcErr.Data, c.Token).(map[string]interface{})
				err = fmt.Errorf("initialize failed: %w", &gatewayRPCError{
					Code: rpcErr.Code, Message: redactUpgradeText(rpcErr.Message, c.Token), Data: data,
				})
			} else {
				err = errors.New(redactUpgradeText(err.Error(), c.Token))
			}
		}
	}()
	c.mu.Lock()
	if c.connected {
		if err := c.cosSnapshotGatewayErrorLocked(); err != nil {
			c.closeConnectionLocked(c.conn)
			c.mu.Unlock()
			return err
		}
		c.mu.Unlock()
		if c.UpgradeOperationID != "" && (expectedRelease != "" || requiredProtocol != "") {
			return c.verifyConnectedUpgrade(expectedRelease, requiredProtocol)
		}
		return nil
	}
	defer c.mu.Unlock()

	wsURL, err := c.targetWebSocketURL()
	if err != nil {
		return err
	}

	c.log("CLIENT:WS_INIT", fmt.Sprintf("Connecting to %s", wsURL))

	header := http.Header{}
	if c.Token != "" {
		header.Set("Authorization", "Bearer "+c.Token)
	}

	var conn *websocket.Conn
	var resp *http.Response
	for attempt := 1; attempt <= websocketHandshakeAttempts; attempt++ {
		conn, resp, err = websocket.DefaultDialer.Dial(wsURL, header)
		if err == nil || resp != nil || !isTransientWebSocketHandshakeError(err) || attempt == websocketHandshakeAttempts {
			break
		}
		c.log("CLIENT:WS_RETRY", fmt.Sprintf(
			"WebSocket handshake ended before a response (attempt %d/%d): %v",
			attempt,
			websocketHandshakeAttempts,
			err,
		))
		time.Sleep(time.Duration(attempt) * websocketHandshakeRetryDelay)
	}
	if err != nil {
		if resp != nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			httpErr := decodeGatewayHTTPError("gateway /v1/rpc", resp, body)
			return fmt.Errorf("failed to connect to %s: %w", wsURL, httpErr)
		}
		return fmt.Errorf("failed to connect to gateway /v1/rpc at %s: %v", wsURL, err)
	}

	c.conn = conn

	// MCP Initialize 握手
	initID := c.nextReqID()
	initCh := c.registerPending(initID)
	defer c.unregisterPending(initID)

	initializeDeadline := time.Now().Add(5 * time.Second)
	if err := conn.SetWriteDeadline(initializeDeadline); err != nil {
		conn.Close()
		return fmt.Errorf("initialization failed: %v", err)
	}
	if err := conn.WriteJSON(map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "initialize",
		"id":      initID,
		"params":  c.initializeParams(expectedRelease, requiredProtocol),
	}); err != nil {
		c.conn.Close()
		return fmt.Errorf("initialization failed: %v", err)
	}

	// 启动后台事件分发循环
	go c.dispatchLoop(conn)
	c.dispatching = true

	// 启动客户端级 Ping 心跳，防范 NAT/LB 针对单向长连（如长时间无包）的静默超时。
	// 大多数云厂商 LB idle timeout 是 60~120s。
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			c.mu.Lock()
			active := c.connected && c.conn == conn
			c.mu.Unlock()
			if !active {
				return
			}
			err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			if err != nil {
				c.closeConnection(conn)
				return
			}
		}
	}()

	// 等待 initialize 响应（带超时）
	select {
	case evt := <-initCh:
		if err := validateInitializeResponse(initID, evt); err != nil {
			c.conn.Close()
			return err
		}
		c.capabilities = initializeCapabilities(evt)
	case <-time.After(time.Until(initializeDeadline)):
		c.conn.Close()
		return fmt.Errorf("initialize handshake timed out")
	}

	if err := c.cosSnapshotGatewayErrorLocked(); err != nil {
		c.closeConnectionLocked(conn)
		return err
	}
	c.connected = true
	c.log("CLIENT:CONNECTED", "Persistent WebSocket connection established")
	return nil
}

func (c *MCPClient) initializeParams(expectedRelease, requiredProtocol string) map[string]interface{} {
	params := map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"clientInfo":      map[string]string{"name": "doops-go-cli", "version": "2.0"},
	}
	if c.UpgradeOperationID != "" {
		params["upgrade_operation_id"] = c.UpgradeOperationID
		params["session_id"] = c.SessionName
		if expectedRelease != "" {
			params["expected_release_commit"] = expectedRelease
		}
		if requiredProtocol != "" {
			params["required_protocol"] = requiredProtocol
		}
	}
	return params
}

// A cached capability check cannot produce a Gateway verification receipt.
// Reused upgrade clients send a fresh initialize without replaying tool calls.
func (c *MCPClient) verifyConnectedUpgrade(expectedRelease, requiredProtocol string) error {
	id := c.nextReqID()
	conn, ch := c.registerCall(id)
	defer c.unregisterPending(id)
	deadline := time.Now().Add(5 * time.Second)
	if err := c.writeRequest(conn, map[string]interface{}{
		"jsonrpc": "2.0", "id": id, "method": "initialize",
		"params": c.initializeParams(expectedRelease, requiredProtocol),
	}, deadline); err != nil {
		return err
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case evt := <-ch:
		if err := validateInitializeResponse(id, evt); err != nil {
			return err
		}
		c.mu.Lock()
		c.capabilities = initializeCapabilities(evt)
		c.mu.Unlock()
		return nil
	case <-timer.C:
		return fmt.Errorf("initialize verification timed out")
	}
}

func isTransientWebSocketHandshakeError(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)
}

func (c *MCPClient) targetWebSocketURL() (string, error) {
	if strings.TrimSpace(c.Target.Gateway) == "" {
		return "", fmt.Errorf("target %q has no Gateway route", c.Target.Name)
	}

	rawGateway := strings.TrimSpace(c.Target.Gateway)
	if !strings.Contains(rawGateway, "://") {
		rawGateway = "wss://" + rawGateway
	}
	u, err := url.Parse(rawGateway)
	if err != nil {
		return "", fmt.Errorf("invalid gateway URL %q: %w", c.Target.Gateway, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported gateway scheme %q", u.Scheme)
	}
	if err := enforceSecureGatewayURL(c.Target.Gateway, u); err != nil {
		return "", err
	}
	u.Path = joinURLPath(u.Path, "v1", "rpc")
	q := u.Query()
	cluster := strings.TrimSpace(c.Target.Cluster)
	if cluster == "" {
		return "", fmt.Errorf("target %q has no Gateway cluster", c.Target.Name)
	}
	instance := strings.TrimSpace(c.Target.Instance)
	if instance == "" {
		return "", fmt.Errorf("target %q has no Gateway instance", c.Target.Name)
	}
	q.Set("cluster", cluster)
	q.Set("instance", instance)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// dispatchLoop 持续读取 WS 流并按 request ID 分发。
func (c *MCPClient) dispatchLoop(conn *websocket.Conn) {
	for {
		msgType, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.log("CLIENT:WS_ERROR", fmt.Sprintf("WS read error: %v", err))
			} else {
				c.log("CLIENT:WS_CLOSE", "WS connection closed cleanly by server")
			}

			c.closeConnection(conn)
			return
		}

		if msgType != websocket.TextMessage {
			continue // 忽略 ping/pong 等二进制帧（gorilla 内部会自动处理底层心跳帧）
		}

		var msg map[string]interface{}
		if err := json.Unmarshal(message, &msg); err != nil {
			continue
		}

		c.dispatchMessage(msg)
	}
}

func (c *MCPClient) dispatchMessage(msg map[string]interface{}) {
	evt := wsEvent{Parsed: msg}
	if raw, err := json.Marshal(msg); err == nil {
		evt.Raw = string(raw)
	}

	// 检查是否有 request ID 可以匹配
	if id, ok := msg["id"].(float64); ok {
		reqID := int64(id)
		c.pendingMu.RLock()
		ch, found := c.pending[reqID]
		c.pendingMu.RUnlock()
		if found {
			sendWSEvent(ch, evt)
			return
		}
	}

	if method, ok := msg["method"].(string); ok && method == "notifications/message" {
		requestID, ok := requestIDFromClientNotification(msg)
		if !ok {
			return
		}
		c.pendingMu.RLock()
		ch := c.pending[requestID]
		c.pendingMu.RUnlock()
		if ch != nil {
			sendWSEvent(ch, evt)
		}
	}
}

func sendWSEvent(ch chan wsEvent, evt wsEvent) {
	ch <- evt
}

func validateInitializeResponse(initID int64, evt wsEvent) error {
	if evt.IsError {
		return fmt.Errorf("initialize failed: %s", evt.Raw)
	}
	msg := evt.Parsed
	if msg == nil {
		return fmt.Errorf("initialize failed: empty response")
	}
	id, ok := msg["id"].(float64)
	if !ok || int64(id) != initID {
		return fmt.Errorf("initialize failed: unexpected response id")
	}
	if rpcErr, ok := msg["error"].(map[string]interface{}); ok {
		code, _ := rpcErr["code"].(float64)
		data, _ := rpcErr["data"].(map[string]interface{})
		return fmt.Errorf("initialize failed: %w", &gatewayRPCError{Code: code, Message: fmt.Sprint(rpcErr["message"]), Data: data})
	}
	result, ok := msg["result"].(map[string]interface{})
	if !ok {
		return fmt.Errorf("initialize failed: missing result")
	}
	if toolResultIsError(result) {
		return fmt.Errorf("initialize failed: %s", toolResultText(result, "verification failed"))
	}
	return nil
}

func initializeCapabilities(evt wsEvent) map[string]interface{} {
	result, _ := evt.Parsed["result"].(map[string]interface{})
	capabilities, _ := result["capabilities"].(map[string]interface{})
	return capabilities
}

func (c *MCPClient) RequireProtocol(protocol string) error {
	if err := validateRequiredProtocol(protocol); err != nil {
		return err
	}
	if err := c.connectWithVerification("", protocol); err != nil {
		return err
	}
	if c.capabilities == nil {
		return fmt.Errorf("initialize verification failed: missing capabilities")
	}
	protocols, validProtocols := c.capabilities["protocols"].(map[string]interface{})
	if _, present := c.capabilities["protocols"]; present && !validProtocols {
		return fmt.Errorf("initialize verification failed: malformed protocols")
	}
	if !protocolCapabilityCompatible(protocol, protocols[protocol]) {
		return &upgradeVerificationError{category: "upgrade_verification_protocol_missing", error: fmt.Errorf(
			"target Agent does not advertise protocol %q; upgrade the Agent before using this operation",
			protocol,
		)}
	}
	return nil
}

func (c *MCPClient) RequireAgentRelease(expected string) error {
	expected = strings.TrimSpace(expected)
	if expected == "" {
		return fmt.Errorf("expected DoOps release commit is empty")
	}
	if err := c.connectWithVerification(expected, ""); err != nil {
		return err
	}
	if c.capabilities == nil {
		return fmt.Errorf("initialize verification failed: missing capabilities")
	}
	actual, validCommit := c.capabilities["releaseCommit"].(string)
	if _, present := c.capabilities["releaseCommit"]; present && !validCommit {
		return fmt.Errorf("initialize verification failed: malformed release commit")
	}
	actual = strings.TrimSpace(actual)
	if actual == "" {
		return &upgradeVerificationError{category: "upgrade_verification_commit_mismatch", error: fmt.Errorf("target Agent did not advertise a DoOps release commit")}
	}
	if actual != expected {
		// The CLI and the Agent are released as a pair. The Agent reports the
		// commit it was built from, so a difference means this CLI and that
		// Agent are not the same release, not that the Agent is broken.
		return &upgradeVerificationError{category: "upgrade_verification_commit_mismatch", error: fmt.Errorf(
			"target Agent release commit mismatch: got %s, want %s; "+
				"the CLI and the Agent are released as a pair, so install the CLI "+
				"that matches the target Agent before upgrading it",
			actual, expected,
		)}
	}
	return nil
}

func validateRequiredProtocol(protocol string) error {
	if protocol != "structuredSSH" && protocol != "executionLifecycle" {
		return fmt.Errorf("unsupported protocol %q; supported protocols: structuredSSH, executionLifecycle", protocol)
	}
	return nil
}

func protocolCapabilityCompatible(protocol string, value interface{}) bool {
	switch protocol {
	case "structuredSSH":
		capability, ok := value.(map[string]interface{})
		return ok &&
			len(capability) == 3 &&
			capability["schemaVersion"] == "doops.sh/structured-ssh/v1" &&
			capability["tool"] == "doops_ssh" &&
			capability["structuredArgv"] == true
	case "executionLifecycle":
		capability, ok := value.(map[string]interface{})
		return ok &&
			len(capability) == 4 &&
			capability["schemaVersion"] == "doops.sh/execution-lifecycle/v1" &&
			capability["terminalResponse"] == true &&
			capability["backgroundLifetime"] == true &&
			capability["promptCancellation"] == true
	default:
		return false
	}
}

// registerPending 为指定 reqID 注册一个事件接收 channel
func (c *MCPClient) registerPending(reqID int64) chan wsEvent {
	ch := make(chan wsEvent, 4096) // 流式输出可能包含批量 SSH/诊断结果
	c.pendingMu.Lock()
	c.pending[reqID] = ch
	c.pendingMu.Unlock()
	return ch
}

// unregisterPending 移除 reqID 的事件接收 channel
func (c *MCPClient) unregisterPending(reqID int64) {
	c.pendingMu.Lock()
	delete(c.pending, reqID)
	c.pendingMu.Unlock()
}

// Bind the pending response and its writer before Close can retire this connection.
func (c *MCPClient) registerCall(reqID int64) (*websocket.Conn, chan wsEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn, c.registerPending(reqID)
}

// writeRequest bounds both writer acquisition and socket backpressure. A failed
// write may have reached the peer; close that transport and never replay it.
func (c *MCPClient) writeRequest(conn *websocket.Conn, request interface{}, deadline time.Time) error {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case c.writes <- struct{}{}:
		defer func() { <-c.writes }()
	case <-timer.C:
		return fmt.Errorf("request not sent: %w", context.DeadlineExceeded)
	}
	c.mu.Lock()
	connected := c.connected && c.conn == conn
	protocolErr := c.cosSnapshotGatewayErrorLocked()
	c.mu.Unlock()
	if !connected || conn == nil {
		return errors.New("request not sent: connection is not available")
	}
	if protocolErr != nil {
		return protocolErr
	}
	if !time.Now().Before(deadline) {
		return fmt.Errorf("request not sent: %w", context.DeadlineExceeded)
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		c.closeConnection(conn)
		return fmt.Errorf("request not sent: %w", err)
	}
	if err := conn.WriteJSON(request); err != nil {
		c.closeConnection(conn)
		return fmt.Errorf("%w: WebSocket request write failed: %v", executionOutcomeUnknownError(), err)
	}
	return nil
}

func (c *MCPClient) requestCancellation(conn *websocket.Conn, reqID int64, deadline time.Time) error {
	return c.writeRequest(conn, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/cancel",
		"id":      fmt.Sprintf("cancel-%d", reqID),
		"params":  map[string]interface{}{"id": reqID},
	}, deadline)
}

func requestIDFromClientNotification(msg map[string]interface{}) (int64, bool) {
	params, _ := msg["params"].(map[string]interface{})
	if params == nil {
		return 0, false
	}
	requestID, ok := params["request_id"]
	if !ok {
		return 0, false
	}
	switch value := requestID.(type) {
	case float64:
		if value != math.Trunc(value) || value < math.MinInt64 || value > math.MaxInt64 {
			return 0, false
		}
		return int64(value), true
	case int:
		return int64(value), true
	case int64:
		return value, true
	default:
		return 0, false
	}
}

// Close 关闭持久连接，释放资源
func (c *MCPClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConnectionLocked(c.conn)
}

func (c *MCPClient) closeConnection(conn *websocket.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeConnectionLocked(conn)
}

func (c *MCPClient) closeConnectionLocked(conn *websocket.Conn) {
	if conn == nil || c.conn != conn {
		return
	}
	conn.Close()
	c.conn = nil
	c.connected = false
	c.dispatching = false
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	for _, ch := range c.pending {
		select {
		case ch <- wsEvent{IsError: true, Raw: "WS connection lost"}:
		default:
		}
	}
	c.pending = make(map[int64]chan wsEvent)
}

// Call 在持久连接上执行 MCP tool call，流式输出到 stdout。
func (c *MCPClient) Call(toolName string, arguments map[string]interface{}) error {
	// 确保连接已建立
	if err := c.connect(); err != nil {
		return err
	}

	// 注入 session_id
	if c.SessionName != "" {
		arguments["session_id"] = c.SessionName
	}

	// 分配请求 ID 并注册接收 channel
	reqID := c.nextReqID()
	conn, ch := c.registerCall(reqID)
	defer c.unregisterPending(reqID)

	// 发送 tool call
	c.log("CLIENT:SEND_TOOL_CALL", fmt.Sprintf("Invoking tool %s (reqID=%d)", toolName, reqID))
	// WebSocket 发送
	deadline := time.Now().Add(c.callTimeout())
	err := c.writeRequest(conn, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      reqID,
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": arguments,
		},
	}, deadline)
	if err != nil {
		return fmt.Errorf("tool call failed: %w", err)
	}

	// 监听事件：流式输出 + 最终结果
	streamedAny := false
	streamedAnswer := false // assistant text already streamed
	endsWithNewline := true
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	timedOut := false
	for {
		select {
		case evt := <-ch:
			if evt.IsError {
				if timedOut {
					return executionOutcomeUnknownError()
				}
				return fmt.Errorf("connection lost: %s", evt.Raw)
			}

			msg := evt.Parsed

			// Notification data is output, not a content-sniffed event protocol.
			if method, ok := msg["method"].(string); ok && method == "notifications/message" {
				if params, ok := msg["params"].(map[string]interface{}); ok {
					if chunk, ok := params["data"].(string); ok && chunk != "" {
						kind, _ := params["kind"].(string)
						if kind == "tool" {
							// Tool markers are progress, not the answer; keep them on their own line.
							if !endsWithNewline {
								fmt.Println()
							}
							fmt.Println(chunk)
							endsWithNewline = true
							continue
						}
						fmt.Print(chunk)
						endsWithNewline = strings.HasSuffix(chunk, "\n")
						streamedAny = true
						if kind == "assistant_delta" {
							streamedAnswer = true
						}
					}
				}
				continue
			}

			// Handle final result
			if id, ok := msg["id"].(float64); ok && int64(id) == reqID {
				if result, ok := msg["result"].(map[string]interface{}); ok {
					contentList, _ := result["content"].([]interface{})
					for _, item := range contentList {
						m := item.(map[string]interface{})
						if m["type"] == "text" {
							text := m["text"].(string)
							if printFinalResult(toolName, streamedAny, streamedAnswer) {
								if !endsWithNewline {
									fmt.Println()
								}
								fmt.Println(text)
								endsWithNewline = true
							} else if !endsWithNewline {
								fmt.Println()
								endsWithNewline = true
							}
						}
					}
					if isErr, ok := result["isError"]; ok && fmt.Sprintf("%v", isErr) == "true" {
						errMsg := "tool returned an error (see output above)"
						contentList, _ := result["content"].([]interface{})
						for _, item := range contentList {
							if m, ok := item.(map[string]interface{}); ok && m["type"] == "text" {
								if text, ok := m["text"].(string); ok && text != "" {
									errMsg = text
									break
								}
							}
						}
						return fmt.Errorf("%s", errMsg)
					}
				} else if rpcErr, ok := msg["error"].(map[string]interface{}); ok {
					code, _ := rpcErr["code"].(float64)
					data, _ := rpcErr["data"].(map[string]interface{})
					return &gatewayRPCError{Code: code, Message: fmt.Sprint(rpcErr["message"]), Data: data}
				}
				return nil // 收到最终结果，完成
			}
		case <-timeout.C:
			if timedOut {
				return executionOutcomeUnknownError()
			}
			timedOut = true
			cancelDeadline := time.Now().Add(executionCancellationTimeout)
			if err := c.requestCancellation(conn, reqID, cancelDeadline); err != nil {
				return executionOutcomeUnknownError()
			}
			timeout.Reset(time.Until(cancelDeadline))
		}
	}
}

// CallAndCapture 在持久连接上执行 MCP tool call，捕获结果文本而非打印。
func (c *MCPClient) CallAndCapture(toolName string, arguments map[string]interface{}) (string, error) {
	text, _, err := c.callAndCaptureResult(toolName, arguments)
	return text, err
}

func (c *MCPClient) callAndCaptureResult(toolName string, arguments map[string]interface{}) (output string, result map[string]interface{}, err error) {
	defer func() {
		if c.UpgradeOperationID == "" {
			return
		}
		output = redactUpgradeText(output, c.Token)
		result, _ = redactUpgradeValue(result, c.Token).(map[string]interface{})
		var rpcErr *gatewayRPCError
		if errors.As(err, &rpcErr) {
			data, _ := redactUpgradeValue(rpcErr.Data, c.Token).(map[string]interface{})
			err = &gatewayRPCError{Code: rpcErr.Code, Message: redactUpgradeText(rpcErr.Message, c.Token), Data: data}
		} else if err != nil {
			err = errors.New(redactUpgradeText(err.Error(), c.Token))
		}
	}()
	// 确保连接已建立
	if err := c.connect(); err != nil {
		return "", nil, err
	}

	// 注入 session_id
	if c.UpgradeOperationID != "" {
		arguments["session_id"] = c.SessionName
		arguments["upgrade_operation_id"] = c.UpgradeOperationID
	} else if sid := c.SessionStore.Get(c.Target.Name, c.SessionName); sid != "" {
		arguments["session_id"] = sid
	} else {
		arguments["session_id"] = c.SessionName
	}

	// 分配请求 ID 并注册接收 channel
	reqID := c.nextReqID()
	conn, ch := c.registerCall(reqID)
	defer c.unregisterPending(reqID)

	// 发送 tool call
	deadline := time.Now().Add(c.callTimeout())
	err = c.writeRequest(conn, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      reqID,
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": arguments,
		},
	}, deadline)
	if err != nil {
		return "", nil, fmt.Errorf("tool call failed: %w", err)
	}

	var capturedOutput strings.Builder

	// 等待最终结果
	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	timedOut := false
	for {
		select {
		case evt := <-ch:
			if evt.IsError {
				if timedOut {
					return capturedOutput.String(), nil, executionOutcomeUnknownError()
				}
				return capturedOutput.String(), nil, fmt.Errorf("connection lost: %s", evt.Raw)
			}

			msg := evt.Parsed

			// 累加流式输出
			if method, ok := msg["method"].(string); ok && method == "notifications/message" {
				if params, ok := msg["params"].(map[string]interface{}); ok {
					if chunk, ok := params["data"].(string); ok {
						// Shell output notifications include a trailing newline. The
						// Agent's standalone display heartbeat does not; exclude only
						// that complete control message before joining output chunks.
						if toolName == "doops_shell" && params["kind"] == "raw" && shellProgressHeartbeatPattern.MatchString(chunk) {
							continue
						}
						capturedOutput.WriteString(chunk)
					}
				}
				continue
			}

			if _, ok := msg["method"]; ok {
				continue
			}

			if id, ok := msg["id"].(float64); ok && int64(id) == reqID {
				if result, ok := msg["result"].(map[string]interface{}); ok {
					if text := toolResultText(result, ""); text != "" {
						capturedOutput.WriteString(text)
					}
					if toolResultIsError(result) {
						return capturedOutput.String(), result, fmt.Errorf("%s", toolResultText(result, "tool returned an error"))
					}
					if capturedOutput.Len() > 0 {
						return capturedOutput.String(), result, nil
					}
				} else if rpcErr, ok := msg["error"].(map[string]interface{}); ok {
					code, _ := rpcErr["code"].(float64)
					data, _ := rpcErr["data"].(map[string]interface{})
					rpcError := &gatewayRPCError{Code: code, Message: fmt.Sprint(rpcErr["message"]), Data: data}
					var result map[string]interface{}
					if code == -32603 && rpcError.Message == "audit_completion_failed" {
						result, _ = data["result"].(map[string]interface{})
						capturedOutput.WriteString(toolResultText(result, ""))
					}
					return capturedOutput.String(), result, rpcError
				}

				return capturedOutput.String(), nil, fmt.Errorf("no result received")
			}
		case <-timeout.C:
			if timedOut {
				return capturedOutput.String(), nil, executionOutcomeUnknownError()
			}
			timedOut = true
			cancelDeadline := time.Now().Add(executionCancellationTimeout)
			if err := c.requestCancellation(conn, reqID, cancelDeadline); err != nil {
				return capturedOutput.String(), nil, executionOutcomeUnknownError()
			}
			timeout.Reset(time.Until(cancelDeadline))
		}
	}
}

// CallStructured invokes an MCP tool whose contract returns structuredContent.
// Human-readable tool text and progress notifications are intentionally ignored.
func (c *MCPClient) CallStructured(toolName string, arguments map[string]interface{}, destination interface{}) error {
	if err := c.connect(); err != nil {
		return err
	}
	if sid := c.SessionStore.Get(c.Target.Name, c.SessionName); sid != "" {
		arguments["session_id"] = sid
	} else {
		arguments["session_id"] = c.SessionName
	}

	reqID := c.nextReqID()
	conn, ch := c.registerCall(reqID)
	defer c.unregisterPending(reqID)

	deadline := time.Now().Add(c.callTimeout())
	err := c.writeRequest(conn, map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "tools/call",
		"id":      reqID,
		"params": map[string]interface{}{
			"name":      toolName,
			"arguments": arguments,
		},
	}, deadline)
	if err != nil {
		return fmt.Errorf("tool call failed: %w", err)
	}

	timeout := time.NewTimer(time.Until(deadline))
	defer timeout.Stop()
	for {
		select {
		case evt := <-ch:
			if evt.IsError {
				return fmt.Errorf("connection lost: %s", evt.Raw)
			}
			msg := evt.Parsed
			if _, notification := msg["method"]; notification {
				continue
			}
			id, ok := msg["id"].(float64)
			if !ok || int64(id) != reqID {
				continue
			}
			if result, ok := msg["result"].(map[string]interface{}); ok {
				if toolResultIsError(result) {
					return fmt.Errorf("%s", toolResultText(result, "tool returned an error"))
				}
				content, found := result["structuredContent"]
				if !found {
					return fmt.Errorf("tool %s returned no structuredContent", toolName)
				}
				data, err := json.Marshal(content)
				if err != nil {
					return fmt.Errorf("encode structured tool result: %w", err)
				}
				if err := json.Unmarshal(data, destination); err != nil {
					return fmt.Errorf("decode structured tool result: %w", err)
				}
				return nil
			}
			if rpcErr, ok := msg["error"].(map[string]interface{}); ok {
				code, _ := rpcErr["code"].(float64)
				data, _ := rpcErr["data"].(map[string]interface{})
				return &gatewayRPCError{Code: code, Message: fmt.Sprint(rpcErr["message"]), Data: data}
			}
			return fmt.Errorf("tool %s returned no result", toolName)
		case <-timeout.C:
			return fmt.Errorf("tool call timed out after %s", c.callTimeout())
		}
	}
}

func (c *MCPClient) callTimeout() time.Duration {
	if c.CallTimeout > 0 {
		return c.CallTimeout
	}
	return defaultToolCallTimeout()
}

func defaultToolCallTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("DOOPS_CLI_CALL_TIMEOUT"))
	if raw == "" {
		return 30 * time.Minute
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	return 30 * time.Minute
}

func toolResultIsError(result map[string]interface{}) bool {
	isErr, ok := result["isError"]
	return ok && fmt.Sprintf("%v", isErr) == "true"
}

func toolResultText(result map[string]interface{}, fallback string) string {
	contentList, _ := result["content"].([]interface{})
	for _, item := range contentList {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if m["type"] == "text" {
			if text, ok := m["text"].(string); ok && text != "" {
				return text
			}
		}
	}
	return fallback
}

// ExecBg streams one doops_bg call until the Agent reports the process has stopped.
func (c *MCPClient) ExecBg(command string) error {
	if err := c.RequireProtocol("executionLifecycle"); err != nil {
		return err
	}
	return c.Call("doops_bg", map[string]interface{}{
		"command": command,
	})
}

// printFinalResult reports whether the final tool result text must be printed.
// ask streams its answer as assistant deltas; printing the final result again
// duplicated the whole answer. Other tools print the result only when nothing
// was streamed, except bg, whose result is a summary of its own.
func printFinalResult(toolName string, streamedAny, streamedAnswer bool) bool {
	switch toolName {
	case "doops_agent_prompt":
		return !streamedAnswer
	case "doops_bg":
		return true
	default:
		return !streamedAny
	}
}
