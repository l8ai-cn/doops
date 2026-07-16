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
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"github.com/user/doops/agent/api"
	"github.com/user/doops/agent/internal/dispatcher"
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
	agentWSReadTimeout      = 20 * time.Second
	agentWSPingInterval     = 5 * time.Second
	agentWSPingWriteTimeout = 3 * time.Second
)

type websocketControlWriter interface {
	WriteControl(messageType int, data []byte, deadline time.Time) error
	Close() error
}

func writeHeartbeatControl(conn websocketControlWriter, messageType int, payload []byte, timeout time.Duration) bool {
	if err := conn.WriteControl(messageType, payload, time.Now().Add(timeout)); err != nil {
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() && netErr.Temporary() {
			return true
		}
		_ = conn.Close()
		return false
	}
	return true
}

func writeHeartbeatPing(conn websocketControlWriter, payload []byte, timeout time.Duration) bool {
	return writeHeartbeatControl(conn, websocket.PingMessage, payload, timeout)
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

// HandleWebSocket 处理客户端发起的 WebSocket 升级请求。
// 承担原本的 HTTP+SSE 职责，现在是全双工单端点通信。
func (gw *Gateway) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 鉴权: 服务端设了 token 则客户端必须提供正确的 Key/Bearer token。
	// 使用常量时间比较，避免计时侧信道泄露 token。
	if gw.Token != "" && !secureTokenEqual(bearerToken(r), gw.Token) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WS Upgrade Failed: %v", err)
		return
	}

	gw.ServeWebSocketConn(conn, r.RemoteAddr)
}

// ServeWebSocketConn serves the doops JSON-RPC protocol over an already
// established WebSocket. It is used both by the normal inbound /ws endpoint
// and by reverse tunnel mode, where the agent dials a public gateway first.
func (gw *Gateway) ServeWebSocketConn(conn *websocket.Conn, remoteAddr string) {
	gw.ServeWebSocketConnWithReady(conn, remoteAddr, nil)
}

func (gw *Gateway) ServeWebSocketConnWithReady(conn *websocket.Conn, remoteAddr string, onReady func()) {
	defer conn.Close()
	conn.SetReadLimit(maxWebSocketMessageBytes())

	// per-connection 写互斥锁（gorilla/websocket 不允许并发写）
	var connMu sync.Mutex

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
				if !writeHeartbeatPing(conn, nil, agentWSPingWriteTimeout) {
					return
				}
			}
		}
	}()

	log.Printf("🔗 WS Client Connected: %s", remoteAddr)

	// per-connection 线程安全写函数（替代全局 gw.mu）
	writeJSON := func(v interface{}) {
		connMu.Lock()
		defer connMu.Unlock()
		if err := conn.WriteJSON(v); err != nil {
			log.Printf("WS WriteJSON error: %v", err)
		}
	}

	gitBodyMu := sync.Mutex{}
	gitBodyWriters := make(map[int64]*io.PipeWriter)
	activeGitMu := sync.Mutex{}
	activeGitCancels := make(map[int64]context.CancelFunc)
	activeToolMu := sync.Mutex{}
	activeToolCancels := make(map[int64]context.CancelFunc)
	var readyOnce sync.Once
	closeGitBody := func(id int64) {
		gitBodyMu.Lock()
		pw := gitBodyWriters[id]
		delete(gitBodyWriters, id)
		gitBodyMu.Unlock()
		if pw != nil {
			_ = pw.Close()
		}
	}
	defer func() {
		activeGitMu.Lock()
		gitCancels := activeGitCancels
		activeGitCancels = make(map[int64]context.CancelFunc)
		activeGitMu.Unlock()
		for _, cancel := range gitCancels {
			cancel()
		}
		activeToolMu.Lock()
		cancels := activeToolCancels
		activeToolCancels = make(map[int64]context.CancelFunc)
		activeToolMu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		gitBodyMu.Lock()
		writers := gitBodyWriters
		gitBodyWriters = make(map[int64]*io.PipeWriter)
		gitBodyMu.Unlock()
		for _, pw := range writers {
			_ = pw.Close()
		}
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
						"name":    "doops-agent",
						"version": "2.0",
					},
					"runtimeId": agentProcessRuntimeID,
					"capabilities": map[string]interface{}{
						"tools": map[string]interface{}{},
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
			pr, pw := io.Pipe()
			gitBodyMu.Lock()
			gitBodyWriters[id] = pw
			gitBodyMu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			activeGitMu.Lock()
			activeGitCancels[id] = cancel
			activeGitMu.Unlock()
			go func() {
				defer func() {
					activeGitMu.Lock()
					delete(activeGitCancels, id)
					activeGitMu.Unlock()
					cancel()
				}()
				defer closeGitBody(id)
				gw.handleGitHTTPOverWS(ctx, req.ID, params, pr, writeJSON)
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
			pw := gitBodyWriters[frame.ID]
			gitBodyMu.Unlock()
			if pw == nil {
				continue
			}
			if frame.DataB64 != "" {
				data, err := base64.StdEncoding.DecodeString(frame.DataB64)
				if err != nil {
					_ = pw.CloseWithError(err)
					closeGitBody(frame.ID)
					continue
				}
				if _, err := pw.Write(data); err != nil {
					closeGitBody(frame.ID)
					continue
				}
			}
			if frame.EOF {
				closeGitBody(frame.ID)
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
			cancel := activeToolCancels[params.ID]
			activeToolMu.Unlock()
			if cancel != nil {
				cancel()
			}
			writeJSON(api.JSONRPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result:  map[string]interface{}{"canceled": cancel != nil},
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
			ctx, cancel := context.WithCancel(context.Background())
			if id, ok := numericID(req.ID); ok {
				activeToolMu.Lock()
				activeToolCancels[id] = cancel
				activeToolMu.Unlock()
				go func() {
					defer func() {
						activeToolMu.Lock()
						delete(activeToolCancels, id)
						activeToolMu.Unlock()
						cancel()
					}()
					gw.handleToolCallOverWS(ctx, req.ID, params.Name, params.Arguments, writeJSON)
				}()
			} else {
				go func() {
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

func rawNotification(text string) notificationEvent {
	return notificationEvent{Kind: "raw", Data: text}
}

func assistantDeltaNotification(text string) notificationEvent {
	return notificationEvent{Kind: "assistant_delta", Data: text}
}

func toolNotification(toolName, status string) notificationEvent {
	return notificationEvent{
		Kind:   "tool",
		Data:   fmt.Sprintf("[tool:%s]", toolName),
		Tool:   toolName,
		Status: status,
	}
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
			"sessionID": sessionID,
			"data":      evt.Data,
			"kind":      evt.Kind,
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
		if gw.Dispatcher != nil && gw.Dispatcher.Classify(args.Command) == dispatcher.PathBlocked {
			writeJSON(buildErrorResponse(reqID, -32602, "blocked dangerous command"))
			return
		}

		log.Printf("🖥️  WS Exec: [%s] %s", sessionID, args.Command)

		// 放弃过度设计的 PTY Sentinel 模式，换用标准 os/exec Streaming
		execCtx, cancelExec := context.WithTimeout(ctx, maxToolExecutionDuration())
		defer cancelExec()
		cmd, stdoutPipe, stderrPipe, err := executeRawCommand(execCtx, args.Command)
		if err != nil {
			resultText := "Error starting command: " + err.Error()
			writeJSON(buildSuccessResponse(reqID, resultText))
			return
		}

		// 用 WaitGroup 确保管道读取完毕后再调 cmd.Wait()，防止尾部数据丢失
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); streamReader(stdoutPipe, pushProgress) }()
		go func() { defer wg.Done(); streamReader(stderrPipe, pushProgress) }()

		// 启动心跳提示（防止长时间静默）
		doneCh := make(chan struct{})
		go slowProgressHeartbeat(doneCh, pushProgress)

		// 先等 reader 全部读完，再 Wait 进程退出
		wg.Wait()
		cmdErr := cmd.Wait()
		close(doneCh)
		if execCtx.Err() != nil {
			writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
			return
		}

		// [全局执行审计] 无论成功失败，记录所有的 exec 审计日志
		if sessionID != "" {
			logDir, err := workspacePath(sessionID)
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
				return
			}
			os.MkdirAll(logDir, 0755)
			logFile := filepath.Join(logDir, ".doops-audit-log")

			exitCode := 0
			if cmdErr != nil {
				if exiterr, ok := cmdErr.(*exec.ExitError); ok {
					exitCode = exiterr.ExitCode()
				} else {
					exitCode = 1
				}
			}

			entry := fmt.Sprintf("# [%s] exit=%d\n%s\n", time.Now().Format("2006-01-02 15:04:05"), exitCode, args.Command)
			if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
				f.WriteString(entry)
				f.Close()
				log.Printf("✅ Audit Log saved to %s", logFile)
			} else {
				log.Printf("❌ Failed to save Audit Log to %s: %v", logFile, err)
			}
		}

		// [配置快照] 检测到 kubectl apply -f 时，将目标配置文件内容追加到审计日志
		if cmdErr == nil && sessionID != "" && strings.Contains(args.Command, "kubectl apply -f") {
			logDir, err := workspacePath(sessionID)
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
				return
			}
			logFile := filepath.Join(logDir, ".doops-audit-log")
			// 提取 -f 后面的文件路径
			parts := strings.Split(args.Command, "-f ")
			if len(parts) > 1 {
				cfgPath := strings.Fields(parts[1])[0]
				// 仅快照位于会话工作区内的配置文件，拒绝绝对路径与 ".."，
				// 避免审计快照读取任意路径（如 /etc/shadow）。
				if resolved, err := resolveWorkspaceFilePath(sessionID, cfgPath); err == nil {
					if data, err := os.ReadFile(resolved); err == nil {
						snapshot := fmt.Sprintf("--- BEGIN %s ---\n%s\n--- END %s ---\n",
							filepath.Base(resolved), string(data), filepath.Base(resolved))
						if f, err := os.OpenFile(logFile, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644); err == nil {
							f.WriteString(snapshot)
							f.Close()
						}
					}
				}
			}
		}

		if cmdErr != nil {
			finalText := fmt.Sprintf("Command failed with error: %v", cmdErr)
			writeJSON(buildToolErrorResponse(reqID, finalText))
		} else {
			writeJSON(buildSuccessResponse(reqID, "Operation complete."))
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
			writeJSON(buildSuccessResponse(reqID, "Error starting command: "+err.Error()))
			return
		}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); streamReader(stdoutPipe, pushProgress) }()
		go func() { defer wg.Done(); streamReader(stderrPipe, pushProgress) }()

		doneCh := make(chan struct{})
		go slowProgressHeartbeat(doneCh, pushProgress)

		wg.Wait()
		cmdErr := cmd.Wait()
		close(doneCh)
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

	// 其他工具 (doops_kubectl, doops_node_info, doops_bg...) 可以采用完全相似的重构
	case "doops_node_info":
		var args api.NodeInfoParams
		json.Unmarshal(argBytes, &args)

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

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); streamReader(stdoutPipe, pushProgress) }()
		go func() { defer wg.Done(); streamReader(stderrPipe, pushProgress) }()

		wg.Wait()
		cmdErr := cmd.Wait()
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
		result, err := handleAgentUpgrade(argBytes)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, err.Error()))
		} else {
			writeJSON(buildSuccessResponse(reqID, result))
		}

	case "doops_bg":
		var args api.BgExecParams
		json.Unmarshal(argBytes, &args)
		// 与 doops_shell 一致: 后台任务同样要经过危险命令分类过滤。
		if gw.Dispatcher != nil && gw.Dispatcher.Classify(args.Command) == dispatcher.PathBlocked {
			writeJSON(buildErrorResponse(reqID, -32602, "blocked dangerous command"))
			return
		}
		log.Printf("🚀 WS doops_bg: %s", args.Command)
		task, err := gw.submitBgTask(sessionID, args.Command, args.LogPath)
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "Task failed: "+err.Error()))
		} else {
			resultText := fmt.Sprintf("Task submitted.\nTaskID: %s\nPID: %d\nLog: %s", task.TaskID, task.PID, task.LogPath)
			writeJSON(buildSuccessResponse(reqID, resultText))
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
		if err := validateWorkspaceCommitBinding(sessionID, args.WorkspaceCommit); err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
			return
		}
		operation := strings.ToLower(strings.TrimSpace(args.Operation))
		if operation == "apply" {
			if err := validateDoagentApplyInstruction(args.Instruction); err != nil {
				writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
				return
			}
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
		var admission *trustedReconciliationAdmission
		if args.ResponseFormat == "json" {
			var instruction struct {
				Task            string `json:"task"`
				WorkflowPath    string `json:"workflowPath"`
				WorkspaceCommit string `json:"workspaceCommit"`
			}
			if err := json.Unmarshal([]byte(args.Instruction), &instruction); err == nil &&
				instruction.Task == "execute-doops-cicd-workflow" &&
				strings.TrimSpace(instruction.WorkflowPath) != "" &&
				strings.TrimSpace(instruction.WorkspaceCommit) != "" {
				built, err := buildTrustedReconciliationAdmission(
					sessionID,
					args.Instruction,
					instruction.WorkspaceCommit,
				)
				if err != nil {
					writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
					return
				}
				admission = &built
			}
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
			args.Model,
			args.ResponseFormat,
			agentPromptExecutionContext{
				NativeMode: nativeMode,
				Admission:  admission,
			},
			pushProgress,
			writeJSON,
		)

	case "doops_credential_plan":
		result, err := handleCredentialPlanTool(argBytes)
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

	case "doops_git_clone":
		result, err := handleGitClone(argBytes)
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

func validateDoagentApplyInstruction(instruction string) error {
	var envelope struct {
		Task          string `json:"task"`
		Skill         string `json:"skill"`
		ExecutionMode string `json:"executionMode"`
	}
	if err := json.Unmarshal([]byte(instruction), &envelope); err != nil {
		return fmt.Errorf("apply requires a structured doops-cicd instruction")
	}
	if envelope.Task != "execute-doops-cicd-workflow" ||
		envelope.Skill != "$doops-cicd" ||
		envelope.ExecutionMode != "apply" {
		return fmt.Errorf("apply is only supported for an explicit doops-cicd apply instruction")
	}
	return nil
}

type agentPromptExecutionContext struct {
	NativeMode string
	Admission  *trustedReconciliationAdmission
}

// handleAgentPromptWS 封装 doops_agent_prompt 处理逻辑，通过 ACP HTTP API 调用本地 doagent 服务。
func (gw *Gateway) handleAgentPromptWS(ctx context.Context, reqID interface{}, doopsSessionID string, instr string, model string, responseFormat string, execution agentPromptExecutionContext, pushProgress notificationSender, writeJSON func(v interface{})) {
	log.Printf("🤖 WS Running doagent via ACP HTTP: %s [Model: %s]", instr, model)

	doagentURL := os.Getenv("DO_AGENT_URL")
	if doagentURL == "" {
		doagentURL = "http://127.0.0.1:9000"
	}
	if err := ensureDoagentAvailable(doagentURL); err != nil {
		writeJSON(buildErrorResponse(reqID, -32603, "doagent unavailable: "+err.Error()))
		return
	}

	var structuredResultPath string
	var runtimeToolCallsPath string
	if responseFormat == "json" {
		var err error
		structuredResultPath, err = prepareDoagentStructuredResult(doopsSessionID)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, fmt.Sprintf("prepare structured result artifact: %v", err)))
			return
		}
		runtimeToolCallsPath = filepath.Join(filepath.Dir(structuredResultPath), "runtime-tool-calls.json")
		if err := writeDoagentToolTraceCatalog(runtimeToolCallsPath, nil); err != nil {
			writeJSON(buildToolErrorResponse(reqID, fmt.Sprintf("prepare runtime tool call catalog: %v", err)))
			return
		}
		instr = appendDoagentStructuredResultContract(instr, structuredResultPath, runtimeToolCallsPath)
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
			requestedSessionID = fmt.Sprintf("%s-json-%d", doopsSessionID, time.Now().UnixNano())
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

		createResp, err := doagentRPC(doagentURL, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "create-" + doopsSessionID,
			"method":  "session/new",
			"params": map[string]interface{}{
				"sessionId":    requestedSessionID,
				"systemPrompt": systemPrompt,
				"cwd":          "/root/ws/" + doopsSessionID,
			},
		})
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent session/new failed: "+err.Error()))
			return
		}

		if result, ok := createResp["result"].(map[string]interface{}); ok {
			if sid, ok := result["sessionId"].(string); ok {
				targetSessionID = sid
			}
		}
		if targetSessionID == "" {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent session/new returned no sessionId"))
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
	}

	// 仅当调用方显式指定时覆盖模型，并在复用会话时同样生效。
	if model != "" {
		targetModel := model
		if !strings.Contains(targetModel, "/") {
			targetModel = "openai/" + targetModel
		}
		if _, err := doagentRPC(doagentURL, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "setmodel-" + doopsSessionID,
			"method":  "session/setModel",
			"params": map[string]interface{}{
				"sessionId": targetSessionID,
				"model":     targetModel,
			},
		}); err != nil {
			writeJSON(buildErrorResponse(reqID, -32603, "doagent session/setModel failed: "+err.Error()))
			return
		}
	}

	if _, err := doagentRPC(doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "setmode-" + doopsSessionID,
		"method":  "session/setMode",
		"params": map[string]interface{}{
			"sessionId": targetSessionID,
			"modeId":    execution.NativeMode,
		},
	}); err != nil {
		writeJSON(buildErrorResponse(reqID, -32603, "doagent session/setMode failed: "+err.Error()))
		return
	}
	log.Printf("🧭 Session %s using native doagent mode %s", targetSessionID, execution.NativeMode)

	// 启动 SSE 事件订阅（在 prompt 之前连接，防止丢失事件）
	sseCtx, sseCancel := context.WithTimeout(ctx, 30*time.Minute)
	defer sseCancel()

	sseDone := make(chan error, 1)
	sseReady := make(chan error, 1)
	toolTraces := newDoagentToolTraceCollector(runtimeToolCallsPath)
	go func() {
		sseDone <- subscribeDoagentSSEWithCollectorReady(
			sseCtx,
			doagentURL,
			targetSessionID,
			pushProgress,
			toolTraces.collect,
			sseReady,
		)
	}()
	if err := <-sseReady; err != nil {
		writeJSON(buildErrorResponse(reqID, -32603, "doagent SSE subscribe failed: "+err.Error()))
		return
	}

	// session/prompt 的同步响应只代表 admission；终态仍以 SSE turn_finished 为准。
	promptParams := buildDoagentPromptParams(
		targetSessionID,
		instr,
		execution.Admission,
		responseFormat == "json",
	)
	if _, err := doagentRPC(doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "prompt-" + doopsSessionID,
		"method":  "session/prompt",
		"params":  promptParams,
	}); err != nil {
		sseCancel()
		<-sseDone
		writeJSON(buildErrorResponse(reqID, -32603, "doagent session/prompt failed: "+err.Error()))
		return
	}

	// 等待权威 turn_finished 终态或错误。
	if err := <-sseDone; err != nil && err != context.Canceled {
		errMsg := fmt.Sprintf("doagent execution error: %v", err)
		log.Printf("⚠️ %s", errMsg)
		writeJSON(buildToolErrorResponse(reqID, errMsg))
		return
	}
	if ctx.Err() != nil {
		writeJSON(buildErrorResponse(reqID, -32007, "operation canceled"))
		return
	}
	if responseFormat == "json" {
		resultText, structuredContent, err := readDoagentStructuredResult(structuredResultPath)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, fmt.Sprintf("read structured result artifact: %v", err)))
			return
		}
		resultText, structuredContent, err = attestDeploymentRun(
			structuredContent,
			toolTraces.completed(),
		)
		if err != nil {
			writeJSON(buildToolErrorResponse(reqID, fmt.Sprintf("attest structured result artifact: %v", err)))
			return
		}
		writeJSON(buildStructuredSuccessResponse(reqID, resultText, structuredContent))
		return
	}

	writeJSON(buildSuccessResponse(reqID, "Operation complete."))
}

func (gw *Gateway) handleAgentMetadataWS(ctx context.Context, doopsSessionID string) (string, error) {
	doagentURL := os.Getenv("DO_AGENT_URL")
	if doagentURL == "" {
		doagentURL = "http://127.0.0.1:9000"
	}
	status := inspectDoagentModelChannels()
	if status.status == "unconfigured" && status.settingsFound {
		return marshalDoagentMetadataStatus(status)
	}
	if err := ensureDoagentAvailable(doagentURL); err != nil {
		status.status = "unavailable"
		status.channels = upsertModelChannel(status.channels, agentModelChannelStatus{
			Name:    "acp",
			Status:  "error",
			Message: err.Error(),
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
			Message: err.Error(),
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

	doagentURL := os.Getenv("DO_AGENT_URL")
	if doagentURL == "" {
		doagentURL = "http://127.0.0.1:9000"
	}
	if err := ensureDoagentAvailable(doagentURL); err != nil {
		return "", err
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
		return "", err
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
		"messages":  messages,
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

func doagentSettingsPath() string {
	if path := strings.TrimSpace(os.Getenv("DO_AGENT_SETTINGS")); path != "" {
		return path
	}
	return "/root/.agent/settings.json"
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
	if err := os.MkdirAll("/var/log", 0755); err != nil {
		return fmt.Errorf("create /var/log: %w", err)
	}
	logFile, err := os.OpenFile("/var/log/do-agent-acp.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open doagent log: %w", err)
	}
	defer logFile.Close()
	cmd := exec.Command(bin, "acp-http", "--port", port, "--cwd", "/root/ws")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
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
			return fmt.Errorf("do-agent exited before listening: %s", tailFile("/var/log/do-agent-acp.log", 2048))
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("do-agent did not listen on %s after startup: %s", baseURL, tailFile("/var/log/do-agent-acp.log", 2048))
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

func tailFile(path string, max int) string {
	data, err := os.ReadFile(path)
	if err != nil || max <= 0 {
		return ""
	}
	if len(data) <= max {
		return string(data)
	}
	return string(data[len(data)-max:])
}

// doagentRPC 向本地 doagent ACP HTTP 服务发送 JSON-RPC 请求并返回响应。
func doagentRPC(baseURL string, payload map[string]interface{}) (map[string]interface{}, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Post(baseURL+"/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("POST /rpc: %w", err)
	}
	defer resp.Body.Close()

	// 202 Accepted = 异步执行，无需等待完整 body
	if resp.StatusCode == http.StatusAccepted {
		return map[string]interface{}{"status": "accepted"}, nil
	}

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

	return result, nil
}

type doagentSSECollector func(update map[string]interface{}) (handled bool, completed bool, err error)

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

func prepareDoagentStructuredResult(sessionID string) (string, error) {
	workspace, err := workspacePath(sessionID)
	if err != nil {
		return "", err
	}
	if err := ensureDoagentResultDirectory(workspace); err != nil {
		return "", fmt.Errorf("prepare session workspace: %w", err)
	}
	resultDirectory := filepath.Join(workspace, ".doops")
	if err := ensureDoagentResultDirectory(resultDirectory); err != nil {
		return "", fmt.Errorf("prepare result directory: %w", err)
	}
	resultPath := filepath.Join(resultDirectory, "structured-result.json")
	if err := os.Remove(resultPath); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("remove stale result: %w", err)
	}
	return resultPath, nil
}

func ensureDoagentResultDirectory(path string) error {
	info, err := os.Lstat(path)
	switch {
	case os.IsNotExist(err):
		if err := os.Mkdir(path, 0o700); err != nil {
			return fmt.Errorf("create directory: %w", err)
		}
	case err != nil:
		return fmt.Errorf("inspect directory: %w", err)
	case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
		return fmt.Errorf("path must be a real directory")
	}
	return nil
}

func appendDoagentStructuredResultContract(instruction, resultPath, runtimeToolCallsPath string) string {
	contract := instruction + "\n\nMachine-readable result channel: before your final response, write the same final JSON object to " +
		resultPath + " using a temporary file in that directory followed by an atomic rename. " +
		"The file must contain exactly one JSON object with no Markdown or surrounding text. " +
		"Immediately before writing that object, read " + runtimeToolCallsPath + ". " +
		"For every evidence item, copy toolCallId exactly from a completed entry in that catalog and set module exactly to that entry's toolName. " +
		"Do not invent, abbreviate, alias, or predict runtime tool call identifiers. " +
		"The terminal response is not used as the machine result."
	return contract
}

func readDoagentStructuredResult(path string) (string, map[string]interface{}, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil, fmt.Errorf("structured result artifact was not created")
		}
		return "", nil, fmt.Errorf("inspect structured result artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("structured result artifact must be a regular file")
	}
	if info.Size() > maxFileReadBytes() {
		return "", nil, fmt.Errorf("structured result artifact size %d exceeds limit %d", info.Size(), maxFileReadBytes())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", nil, fmt.Errorf("read structured result artifact: %w", err)
	}
	text := string(data)
	object, err := decodeDoagentJSONObject(text)
	if err != nil {
		return "", nil, fmt.Errorf("structured result artifact must contain one JSON object: %w", err)
	}
	return text, object, nil
}

// subscribeDoagentSSE 订阅 doagent 的 SSE 事件流，将内容实时转发到 WebSocket 客户端。
// 仅当收到权威 turn_finished 或 error 事件时返回。
func subscribeDoagentSSE(ctx context.Context, baseURL string, sessionID string, pushProgress notificationSender) error {
	return subscribeDoagentSSEWithCollector(ctx, baseURL, sessionID, pushProgress, nil)
}

func subscribeDoagentSSEWithCollector(ctx context.Context, baseURL string, sessionID string, pushProgress notificationSender, collector doagentSSECollector) error {
	return subscribeDoagentSSEWithCollectorReady(ctx, baseURL, sessionID, pushProgress, collector, nil)
}

func subscribeDoagentSSEWithCollectorReady(
	ctx context.Context,
	baseURL string,
	sessionID string,
	pushProgress notificationSender,
	collector doagentSSECollector,
	ready chan<- error,
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
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var eventData strings.Builder
	receivedEvent := false
	idleTimeout := doagentSSEIdleTimeout()
	idleTimer := time.NewTimer(idleTimeout)
	idleDone := make(chan struct{})
	var idleMu sync.Mutex
	var idleErr error
	defer func() {
		close(idleDone)
		idleTimer.Stop()
	}()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleTimeout)
	}
	go func() {
		select {
		case <-idleTimer.C:
			recent := recentDoagentLogSummary()
			err := fmt.Errorf("doagent SSE idle timeout after %s", idleTimeout)
			if recent != "" {
				err = fmt.Errorf("%w: recent doagent log: %s", err, recent)
			}
			idleMu.Lock()
			idleErr = err
			idleMu.Unlock()
			cancel()
		case <-idleDone:
		}
	}()

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
			resetIdle()

			// doagent SSE 使用 JSON-RPC 2.0 通知格式：{"jsonrpc":"2.0","method":"...","params":{...}}
			var update map[string]interface{}
			if json.Unmarshal([]byte(data), &update) != nil {
				pushProgress(rawNotification(data))
				continue
			}

			method, _ := update["method"].(string)
			params, _ := update["params"].(map[string]interface{})

			switch method {
			case "session/update":
				// session/update 包含不同类型的更新
				sessionUpdate, _ := params["update"].(map[string]interface{})
				if collector != nil {
					handled, completed, err := collector(sessionUpdate)
					if err != nil {
						return err
					}
					if completed {
						return nil
					}
					if handled {
						continue
					}
				}
				updateType, _ := sessionUpdate["sessionUpdate"].(string)

				switch updateType {
				case "agent_message_chunk":
					// 流式文本块：content 是 map{"text":"...","type":"text"} 或直接字符串
					switch c := sessionUpdate["content"].(type) {
					case map[string]interface{}:
						if text, ok := c["text"].(string); ok && text != "" {
							pushProgress(assistantDeltaNotification(text))
						}
					case string:
						if c != "" {
							pushProgress(assistantDeltaNotification(c))
						}
					}
				case "tool_call_update":
					// 工具调用进度
					toolName, _ := sessionUpdate["toolName"].(string)
					status, _ := sessionUpdate["status"].(string)
					if toolName != "" && status == "in_progress" {
						pushProgress(toolNotification(toolName, status))
					}
				case "agent_message":
					// 最终文本先转发，但只有后续 turn_finished 才能结束本轮。
					switch c := sessionUpdate["content"].(type) {
					case map[string]interface{}:
						if text, ok := c["text"].(string); ok && text != "" {
							pushProgress(assistantDeltaNotification(text))
						}
					case []interface{}:
						for _, item := range c {
							if m, ok := item.(map[string]interface{}); ok {
								if inner, ok := m["content"].(map[string]interface{}); ok {
									if text, ok := inner["text"].(string); ok && text != "" {
										pushProgress(assistantDeltaNotification(text))
									}
								}
							}
						}
					case string:
						if c != "" {
							pushProgress(assistantDeltaNotification(c))
						}
					}
				case "completed", "usage_update":
					// 非权威状态或 token 统计，继续等待 turn_finished。
				case "turn_finished":
					if err := doagentTurnFinishedError(sessionUpdate); err != nil {
						pushProgress(errorNotification("[error] " + err.Error()))
						return err
					}
					return nil
				}

			case "permission.updated":
				details := make([]string, 0, 3)
				if perm, ok := params["permission"].(map[string]interface{}); ok {
					permID, _ := perm["id"].(string)
					if permID != "" {
						details = append(details, "id="+permID)
					}
					if title, _ := perm["title"].(string); title != "" {
						details = append(details, "title="+title)
					}
					if toolName, _ := perm["toolName"].(string); toolName != "" {
						details = append(details, "tool="+toolName)
					}
				}
				message := "doagent permission required"
				if len(details) > 0 {
					message += ": " + strings.Join(details, ", ")
				}
				pushProgress(errorNotification("[error] " + message))
				return errors.New(message)

			case "error":
				errMsg := "unknown error"
				if msg, ok := params["message"].(string); ok {
					errMsg = msg
				}
				pushProgress(errorNotification("[error] " + errMsg))
				return fmt.Errorf("doagent: %s", errMsg)

			default:
				// 其他事件（session/update chunks 等）静默处理
				if method != "" {
					log.Printf("🔔 SSE event: %s", method)
				} else {
					pushProgress(rawNotification(data))
				}
			}
			continue
		}
	}

	if err := scanner.Err(); err != nil {
		idleMu.Lock()
		timeoutErr := idleErr
		idleMu.Unlock()
		if timeoutErr != nil && errors.Is(err, context.Canceled) {
			return timeoutErr
		}
		return fmt.Errorf("SSE read: %w", err)
	}
	idleMu.Lock()
	timeoutErr := idleErr
	idleMu.Unlock()
	if timeoutErr != nil {
		return timeoutErr
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

func doagentSSEIdleTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("DOOPS_DOAGENT_SSE_IDLE_TIMEOUT"))
	if raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	// Idle = no SSE event from the agent-core for this long. The timer resets on
	// every streamed event, so this only fires during a genuine silent gap.
	// Agent-driven work (a reasoning step plus a long tool call: build, image
	// pull, kubectl wait) routinely stays silent well beyond 30s, so a short
	// default aborts healthy long-running stages (notably agent-driven CI/CD).
	// Default to a generous 5m; set DOOPS_DOAGENT_SSE_IDLE_TIMEOUT higher for
	// heavy build stages, or lower for snappy hang-detection in interactive use.
	return 5 * time.Minute
}

func recentDoagentLogSummary() string {
	return sanitizeSecretText(strings.TrimSpace(tailFile("/var/log/do-agent-acp.log", 2048)))
}

func sanitizeSecretText(s string) string {
	if s == "" {
		return ""
	}
	replacers := []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_-]+`),
		regexp.MustCompile(`(?i)(api[_-]?key[=: ]+)[^ ,}\n]+`),
		regexp.MustCompile(`(?i)(Bearer )[A-Za-z0-9._-]+`),
	}
	s = replacers[0].ReplaceAllString(s, "sk-...REDACTED")
	s = replacers[1].ReplaceAllString(s, "${1}...REDACTED")
	s = replacers[2].ReplaceAllString(s, "${1}...REDACTED")
	if len(s) > 1200 {
		return s[len(s)-1200:]
	}
	return s
}

// -----------------------------------------------------------------------------
// 共享的底层基础设施流式处理器
// -----------------------------------------------------------------------------

// executeRawCommand 使用纯标准的 os/exec 启动进程（去 PTY 化），返回 cmd 对象、标准输出管、错误输出管
func executeRawCommand(ctx context.Context, command string) (*exec.Cmd, io.ReadCloser, io.ReadCloser, error) {
	if ctx.Err() != nil {
		return nil, nil, nil, ctx.Err()
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	go func() {
		<-ctx.Done()
		if cmd.Process == nil {
			return
		}
		pgid := -cmd.Process.Pid
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		time.Sleep(2 * time.Second)
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}()
	return cmd, stdoutPipe, stderrPipe, nil
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
	return api.JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      reqID,
		Result: map[string]interface{}{
			"isError": true,
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": text,
				},
			},
		},
	}
}
