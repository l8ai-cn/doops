package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	defer conn.Close()
	conn.SetReadLimit(maxWebSocketMessageBytes())

	// per-connection 写互斥锁（gorilla/websocket 不允许并发写）
	var connMu sync.Mutex

	// 自动配置 ping/pong 心跳，防止中间节点 (Nginx/SLB) 断开空闲连接。
	// 反向隧道模式下，ping 失败必须主动关闭连接，确保 gateway 重启或网络
	// 黑洞后 agent 退出本轮 ServeWebSocketConn 并进入外层重连循环。
	conn.SetReadDeadline(time.Now().Add(agentWSReadTimeout))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(agentWSReadTimeout))
		return nil
	})

	// Ping 心跳协程使用 done 信道显式退出（避免最长一个 ping 周期的 goroutine 泄漏）
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
				deadline := time.Now().Add(agentWSPingWriteTimeout)
				connMu.Lock()
				err := conn.WriteControl(websocket.PingMessage, nil, deadline)
				connMu.Unlock()
				if err != nil {
					_ = conn.Close()
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
	activeToolMu := sync.Mutex{}
	activeToolCancels := make(map[int64]context.CancelFunc)
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
		messageType, message, err := conn.ReadMessage()
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
					"capabilities": map[string]interface{}{
						"tools":              map[string]interface{}{},
						"semanticDeployment": semanticDeploymentCapability(),
					},
				},
			})
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
			go func() {
				defer closeGitBody(id)
				gw.handleGitHTTPOverWS(req.ID, params, pr, writeJSON)
			}()
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

func semanticDeploymentCapability() map[string]interface{} {
	return map[string]interface{}{
		"reconcile": map[string]string{
			"tool":            "doops_cicd_reconcile",
			"input":           "DeploymentPlan",
			"output":          "CICDReconcileResult",
			"contractVersion": "doops.sh/v2",
		},
	}
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
		switch strings.ToLower(strings.TrimSpace(args.Mode)) {
		case "metadata":
			result, err := gw.handleAgentMetadataWS(ctx, sessionID)
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
			} else {
				writeJSON(buildSuccessResponse(reqID, result))
			}
			return
		case "history":
			result, err := gw.handleAgentHistoryWS(ctx, sessionID)
			if err != nil {
				writeJSON(buildErrorResponse(reqID, -32603, err.Error()))
			} else {
				writeJSON(buildSuccessResponse(reqID, result))
			}
			return
		}
		gw.handleAgentPromptWS(ctx, reqID, sessionID, args.Instruction, args.Model, pushProgress, writeJSON, nil)

	case "doops_cicd_reconcile":
		var args api.CICDReconcileParams
		if err := json.Unmarshal(argBytes, &args); err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, "invalid doops_cicd_reconcile params"))
			return
		}
		plan, err := validateCICDReconcilePlan(args)
		if err != nil {
			writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
			return
		}
		gw.handleCICDReconcileWS(ctx, reqID, args, plan, pushProgress, writeJSON)

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

type agentPromptOptions struct {
	sseCollector    doagentSSECollector
	successResponse func() api.JSONRPCResponse
}

// handleAgentPromptWS 封装 doops_agent_prompt 处理逻辑，通过 ACP HTTP API 调用本地 doagent 服务。
func (gw *Gateway) handleAgentPromptWS(ctx context.Context, reqID interface{}, doopsSessionID string, instr string, model string, pushProgress notificationSender, writeJSON func(v interface{}), options *agentPromptOptions) {
	log.Printf("🤖 WS Running doagent via ACP HTTP: %s [Model: %s]", instr, model)

	doagentURL := os.Getenv("DO_AGENT_URL")
	if doagentURL == "" {
		doagentURL = "http://127.0.0.1:9000"
	}
	if err := ensureDoagentAvailable(doagentURL); err != nil {
		writeJSON(buildErrorResponse(reqID, -32603, "doagent unavailable: "+err.Error()))
		return
	}

	// 查找已有的 doagent session 映射
	gw.sessionMapMu.RLock()
	entry := gw.sessionMap[doopsSessionID]
	gw.sessionMapMu.RUnlock()

	var targetSessionID string
	if entry != nil {
		targetSessionID = entry.doagentSessionID
		gw.sessionMapMu.Lock()
		entry.lastUsed = time.Now()
		gw.sessionMapMu.Unlock()
	}

	// 首次会话：创建 doagent session 并注入系统提示词
	if targetSessionID == "" {
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
				"sessionId":    doopsSessionID,
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

		gw.sessionMapMu.Lock()
		gw.sessionMap[doopsSessionID] = &sessionEntry{
			doagentSessionID: targetSessionID,
			lastUsed:         time.Now(),
		}
		gw.sessionMapMu.Unlock()
		log.Printf("🔗 Bound Doops Session %s -> doagent Session %s", doopsSessionID, targetSessionID)

		// 仅当显式开启 DOOPS_AGENT_AUTO_APPROVE=1 时才切换到 build 模式
		// (always_allow=["*"]，所有工具调用自动批准)。默认保持 doagent 的
		// 安全默认模式，需要人工/逐项确认，避免无人值守地放行任意工具。
		if agentAutoApproveEnabled() {
			doagentRPC(doagentURL, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      "setmode-" + doopsSessionID,
				"method":  "session/setMode",
				"params": map[string]interface{}{
					"sessionId": targetSessionID,
					"modeId":    "build",
				},
			})
			log.Printf("🔓 Session %s set to build mode (auto-approve all tools)", targetSessionID)
		} else {
			log.Printf("🔒 Session %s using default mode; set DOOPS_AGENT_AUTO_APPROVE=1 to auto-approve tool calls", targetSessionID)
		}

		// 设置模型（仅当调用方显式指定时覆盖默认模型）
		targetModel := model
		if targetModel != "" {
			if !strings.Contains(targetModel, "/") {
				targetModel = "openai/" + targetModel
			}
			doagentRPC(doagentURL, map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      "setmodel-" + doopsSessionID,
				"method":  "session/setModel",
				"params": map[string]interface{}{
					"sessionId": targetSessionID,
					"model":     targetModel,
				},
			})
		}
	}

	// 启动 SSE 事件订阅（在 prompt 之前连接，防止丢失事件）
	sseCtx, sseCancel := context.WithTimeout(ctx, 30*time.Minute)
	defer sseCancel()

	sseDone := make(chan error, 1)
	go func() {
		sseDone <- subscribeDoagentSSEWithCollector(sseCtx, doagentURL, targetSessionID, pushProgress, optionsCollector(options))
	}()

	// 等待 SSE 连接建立（本地连接 <10ms，200ms 留足余量）
	time.Sleep(200 * time.Millisecond)

	// 发送 prompt（doagent 对长任务返回 202 Accepted，实际执行异步进行）
	go func() {
		_, err := doagentRPC(doagentURL, map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      "prompt-" + doopsSessionID,
			"method":  "session/prompt",
			"params": map[string]interface{}{
				"sessionId": targetSessionID,
				"prompt":    instr,
			},
		})
		if err != nil {
			log.Printf("⚠️ doagent prompt RPC returned: %v", err)
		}
	}()

	// 等待 SSE 完成（agent_message/error 事件或超时）
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

	if options != nil && options.successResponse != nil {
		writeJSON(options.successResponse())
		return
	}
	writeJSON(buildSuccessResponse(reqID, "Operation complete."))
}

func optionsCollector(options *agentPromptOptions) doagentSSECollector {
	if options == nil {
		return nil
	}
	return options.sseCollector
}

func (gw *Gateway) handleCICDReconcileWS(ctx context.Context, reqID interface{}, args api.CICDReconcileParams, plan cicdReconcilePlan, pushProgress notificationSender, writeJSON func(v interface{})) {
	if err := verifyCICDPlanAttestation(plan); err != nil {
		writeJSON(buildToolErrorResponse(reqID, err.Error()))
		return
	}
	doagentURL := os.Getenv("DO_AGENT_URL")
	if doagentURL == "" {
		doagentURL = "http://127.0.0.1:9000"
	}
	if err := requireCICDStructuredReportCapability(doagentURL, args.SessionID); err != nil {
		writeJSON(buildToolErrorResponse(reqID, err.Error()))
		return
	}
	instruction, err := buildCICDReconcileInstruction(args.Plan, args.DryRun)
	if err != nil {
		writeJSON(buildErrorResponse(reqID, -32602, err.Error()))
		return
	}

	var report map[string]interface{}
	gw.handleAgentPromptWS(ctx, reqID, args.SessionID, instruction, "", pushProgress, writeJSON, &agentPromptOptions{
		sseCollector: cicdReportCollector(plan, &report),
		successResponse: func() api.JSONRPCResponse {
			return buildStructuredSuccessResponse(reqID, "CI/CD reconciliation report received.", report)
		},
	})
}

func requireCICDStructuredReportCapability(doagentURL string, doopsSessionID string) error {
	response, err := doagentRPC(doagentURL, map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      "capabilities-" + doopsSessionID,
		"method":  "initialize",
		"params":  map[string]interface{}{},
	})
	if err != nil {
		return fmt.Errorf("doagent capability check failed: %w", err)
	}
	result, _ := response["result"].(map[string]interface{})
	if supportsCICDStructuredReport(result) {
		return nil
	}
	meta, _ := result["_meta"].(map[string]interface{})
	if supportsCICDStructuredReport(meta) {
		return nil
	}
	return errors.New("doagent does not advertise cicdStructuredReport capability")
}

func supportsCICDStructuredReport(value map[string]interface{}) bool {
	capabilities, _ := value["capabilities"].(map[string]interface{})
	supported, _ := capabilities["cicdStructuredReport"].(bool)
	return supported
}

func validateCICDReconcilePlan(args api.CICDReconcileParams) (cicdReconcilePlan, error) {
	if strings.TrimSpace(args.SessionID) == "" {
		return cicdReconcilePlan{}, errors.New("session_id is required")
	}
	var plan cicdReconcilePlan
	if err := json.Unmarshal(args.Plan, &plan); err != nil {
		return cicdReconcilePlan{}, errors.New("plan must be a JSON object")
	}
	if plan.APIVersion != "doops.sh/v2" || plan.Kind != "DeploymentPlan" {
		return cicdReconcilePlan{}, errors.New("plan must be a doops.sh/v2 DeploymentPlan")
	}
	if !cicdOCIDigestPattern.MatchString(plan.Digest) {
		return cicdReconcilePlan{}, errors.New("plan.digest must be an OCI sha256 digest")
	}
	expectedPlanDigest, err := digestCICDPlanJSON(args.Plan)
	if err != nil {
		return cicdReconcilePlan{}, err
	}
	if plan.Digest != expectedPlanDigest {
		return cicdReconcilePlan{}, errors.New("plan.digest does not match the canonical DeploymentPlan")
	}
	if strings.TrimSpace(plan.Spec.Target.Environment) == "" ||
		strings.TrimSpace(plan.Spec.Target.ExecutionTarget) == "" ||
		!cicdOCIDigestPattern.MatchString(plan.Spec.Target.ProfileDigest) ||
		len(plan.Spec.Target.Profile) == 0 {
		return cicdReconcilePlan{}, errors.New("plan must include a resolved environment profile")
	}
	var profile cicdReconcileEnvironmentProfile
	if err := json.Unmarshal(plan.Spec.Target.Profile, &profile); err != nil {
		return cicdReconcilePlan{}, errors.New("plan resolved environment profile is invalid")
	}
	if err := validateCICDReconcileEnvironmentProfile(profile); err != nil {
		return cicdReconcilePlan{}, err
	}
	if profile.Target != plan.Spec.Target.ExecutionTarget {
		return cicdReconcilePlan{}, errors.New("plan execution target does not match the resolved environment profile")
	}
	expectedProfileDigest, err := digestCICDValue(plan.Spec.Target.Profile)
	if err != nil {
		return cicdReconcilePlan{}, err
	}
	if plan.Spec.Target.ProfileDigest != expectedProfileDigest {
		return cicdReconcilePlan{}, errors.New("plan environment profile digest does not match the resolved environment profile")
	}
	if err := validateCICDReconcileArtifactContract(plan.Spec.ArtifactContract); err != nil {
		return cicdReconcilePlan{}, err
	}
	if strings.TrimSpace(plan.Spec.DesiredState.Application) == "" ||
		strings.TrimSpace(plan.Spec.DesiredState.Delivery) == "" ||
		strings.TrimSpace(plan.Spec.DesiredState.ConfigurationSource) == "" ||
		strings.TrimSpace(plan.Spec.DesiredState.Authorization) == "" {
		return cicdReconcilePlan{}, errors.New("plan desiredState is incomplete")
	}
	if !validCICDEvidenceKinds(plan.Spec.Acceptance.RequiredEvidence) || !validCICDEvidenceKinds(plan.Spec.Acceptance.RequiredFailureEvidence) {
		return cicdReconcilePlan{}, errors.New("plan must require non-empty, unique success and failure evidence")
	}
	if plan.Spec.Policy.Mutation != "require-explicit-approval" ||
		plan.Spec.Policy.Convergence != "until-verified" ||
		plan.Spec.Policy.FailureMode != "restore-last-known-good" {
		return cicdReconcilePlan{}, errors.New("plan policy is not supported")
	}
	if (plan.Spec.Release.Source == nil) == (plan.Spec.Release.Manifest == nil) {
		return cicdReconcilePlan{}, errors.New("plan release requires exactly one of source or manifest")
	}
	if source := plan.Spec.Release.Source; source != nil &&
		(strings.TrimSpace(source.Repository) == "" || !cicdGitCommitPattern.MatchString(source.Revision)) {
		return cicdReconcilePlan{}, errors.New("plan source release must include an immutable 40-character Git commit")
	}
	if manifest := plan.Spec.Release.Manifest; manifest != nil &&
		(strings.TrimSpace(manifest.Repository) == "" ||
			strings.TrimSpace(manifest.Reference) == "" ||
			!cicdOCIDigestPattern.MatchString(manifest.Digest)) {
		return cicdReconcilePlan{}, errors.New("plan manifest release must include a repository, reference, and OCI digest")
	}
	if field, found, err := findForbiddenCICDPlanField(args.Plan); err != nil {
		return cicdReconcilePlan{}, err
	} else if found {
		return cicdReconcilePlan{}, fmt.Errorf("plan contains forbidden command-driven field %q", field)
	}
	return plan, nil
}

var (
	cicdGitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	cicdOCIDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

var forbiddenCICDPlanFields = map[string]struct{}{
	"stages":                    {},
	"uses":                      {},
	"task":                      {},
	"run":                       {},
	"requiredCommand":           {},
	"verificationCommand":       {},
	"dryRunVerificationCommand": {},
	"script":                    {},
}

type cicdReconcileEnvironmentProfile struct {
	Target         string                    `json:"target"`
	Cluster        string                    `json:"cluster"`
	Instance       string                    `json:"instance"`
	Namespace      string                    `json:"namespace"`
	Release        string                    `json:"release"`
	Registry       string                    `json:"registry"`
	Chart          string                    `json:"chart"`
	Values         string                    `json:"values"`
	RuntimeFiles   string                    `json:"runtimeFiles"`
	DeploymentMode string                    `json:"deploymentMode"`
	HealthChecks   cicdReconcileHealthChecks `json:"healthChecks"`
	Authz          map[string]string         `json:"authz"`
}

type cicdReconcileHealthChecks struct {
	Public    []cicdReconcilePublicHealthCheck   `json:"public"`
	Workloads []cicdReconcileWorkloadHealthCheck `json:"workloads"`
}

type cicdReconcilePublicHealthCheck struct {
	ID             string `json:"id"`
	URL            string `json:"url"`
	ExpectedStatus int    `json:"expectedStatus"`
}

type cicdReconcileWorkloadHealthCheck struct {
	Service          string `json:"service"`
	MinReadyReplicas int    `json:"minReadyReplicas"`
	RequireEndpoints bool   `json:"requireEndpoints"`
}

type cicdReconcileArtifactContract struct {
	SourceRepository     string                 `json:"sourceRepository"`
	SourceBranch         string                 `json:"sourceBranch"`
	Services             []string               `json:"services"`
	ImageTagPattern      string                 `json:"imageTagPattern"`
	ImageReferenceFormat string                 `json:"imageReferenceFormat"`
	HelmImageBindings    map[string]string      `json:"helmImageBindings"`
	ManifestRepository   string                 `json:"manifestRepository"`
	Authz                map[string]interface{} `json:"authz"`
}

type cicdReconcilePlan struct {
	APIVersion  string               `json:"apiVersion"`
	Kind        string               `json:"kind"`
	Digest      string               `json:"digest"`
	Attestation *cicdPlanAttestation `json:"attestation"`
	Spec        struct {
		Release struct {
			Source   *cicdReconcileSourceRelease   `json:"source"`
			Manifest *cicdReconcileManifestRelease `json:"manifest"`
		} `json:"release"`
		Target struct {
			Environment     string          `json:"environment"`
			ExecutionTarget string          `json:"executionTarget"`
			ProfileDigest   string          `json:"profileDigest"`
			Profile         json.RawMessage `json:"profile"`
		} `json:"target"`
		ArtifactContract cicdReconcileArtifactContract `json:"artifactContract"`
		DesiredState     struct {
			Application         string `json:"application"`
			Delivery            string `json:"delivery"`
			ConfigurationSource string `json:"configurationSource"`
			Authorization       string `json:"authorization"`
		} `json:"desiredState"`
		Acceptance struct {
			RequiredEvidence        []string `json:"requiredEvidence"`
			RequiredFailureEvidence []string `json:"requiredFailureEvidence"`
		} `json:"acceptance"`
		Policy struct {
			Mutation    string `json:"mutation"`
			Convergence string `json:"convergence"`
			FailureMode string `json:"failureMode"`
		} `json:"policy"`
	} `json:"spec"`
}

type cicdPlanAttestation struct {
	Algorithm  string `json:"algorithm"`
	Issuer     string `json:"issuer"`
	PlanDigest string `json:"planDigest"`
	Signature  string `json:"signature"`
}

type cicdReconcileSourceRelease struct {
	Repository string `json:"repository"`
	Revision   string `json:"revision"`
}

type cicdReconcileManifestRelease struct {
	Repository string `json:"repository"`
	Reference  string `json:"reference"`
	Digest     string `json:"digest"`
}

func buildCICDReconcileInstruction(plan json.RawMessage, dryRun bool) (string, error) {
	request, err := json.Marshal(struct {
		Plan   json.RawMessage `json:"plan"`
		DryRun bool            `json:"dry_run"`
	}{
		Plan:   plan,
		DryRun: dryRun,
	})
	if err != nil {
		return "", fmt.Errorf("marshal CI/CD reconciliation request: %w", err)
	}
	return "Reconcile the following CI/CD request. The resolved environment profile and artifact contract in the DeploymentPlan are authoritative; do not infer a target from a domain, business number, or historical name. Completion is valid only when you emit a session/update event with sessionUpdate \"agent_report\" and an object report containing planDigest, status, evidence, and violations. A converged report must include every requiredEvidence item, including public business API checks and post-deploy log scan. On any rollout or health failure, preserve the last known good revision, collect every requiredFailureEvidence item, restore the last known good revision, and return Failed or Blocked with rollback-state evidence. Never scale a failing workload to zero. Do not encode the report in agent_message text.\n" + string(request), nil
}

func validateCICDReconcileEnvironmentProfile(profile cicdReconcileEnvironmentProfile) error {
	required := map[string]string{
		"target":         profile.Target,
		"cluster":        profile.Cluster,
		"instance":       profile.Instance,
		"namespace":      profile.Namespace,
		"release":        profile.Release,
		"registry":       profile.Registry,
		"chart":          profile.Chart,
		"values":         profile.Values,
		"deploymentMode": profile.DeploymentMode,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("plan resolved environment profile %s is required", field)
		}
	}
	if len(profile.Authz) == 0 {
		return errors.New("plan resolved environment profile authorization is required")
	}
	if len(profile.HealthChecks.Public) == 0 {
		return errors.New("plan resolved environment profile public health checks are required")
	}
	for _, check := range profile.HealthChecks.Public {
		if strings.TrimSpace(check.ID) == "" || strings.TrimSpace(check.URL) == "" || check.ExpectedStatus <= 0 {
			return errors.New("plan resolved environment profile public health checks are invalid")
		}
	}
	if profile.DeploymentMode == "application" {
		if strings.TrimSpace(profile.RuntimeFiles) == "" {
			return errors.New("plan resolved application environment runtime files are required")
		}
		if len(profile.HealthChecks.Workloads) == 0 {
			return errors.New("plan resolved application environment workload health checks are required")
		}
		for _, check := range profile.HealthChecks.Workloads {
			if strings.TrimSpace(check.Service) == "" || check.MinReadyReplicas < 1 || !check.RequireEndpoints {
				return errors.New("plan resolved application environment workload health checks are invalid")
			}
		}
	}
	return nil
}

func validateCICDReconcileArtifactContract(artifact cicdReconcileArtifactContract) error {
	required := map[string]string{
		"sourceRepository":     artifact.SourceRepository,
		"sourceBranch":         artifact.SourceBranch,
		"imageTagPattern":      artifact.ImageTagPattern,
		"imageReferenceFormat": artifact.ImageReferenceFormat,
		"manifestRepository":   artifact.ManifestRepository,
	}
	for field, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("plan artifact contract %s is required", field)
		}
	}
	if len(artifact.Services) == 0 || len(artifact.HelmImageBindings) != len(artifact.Services) {
		return errors.New("plan artifact contract must bind every service to Helm")
	}
	for _, service := range artifact.Services {
		if strings.TrimSpace(service) == "" || strings.TrimSpace(artifact.HelmImageBindings[service]) == "" {
			return errors.New("plan artifact contract service binding is invalid")
		}
	}
	if len(artifact.Authz) == 0 {
		return errors.New("plan artifact contract authorization is required")
	}
	return nil
}

func verifyCICDPlanAttestation(plan cicdReconcilePlan) error {
	if plan.Attestation == nil {
		return errors.New("deployment plan must be signed by the CI/CD compiler")
	}
	if plan.Attestation.Algorithm != "ed25519" ||
		plan.Attestation.Issuer != "doops-cicd-compiler" ||
		plan.Attestation.PlanDigest != plan.Digest ||
		strings.TrimSpace(plan.Attestation.Signature) == "" {
		return errors.New("deployment plan attestation is invalid")
	}
	rawPublicKey := strings.TrimSpace(os.Getenv("DOOPS_CICD_PLAN_PUBLIC_KEY"))
	if rawPublicKey == "" {
		return errors.New("DOOPS_CICD_PLAN_PUBLIC_KEY is required for CI/CD reconciliation")
	}
	publicKey, err := base64.StdEncoding.DecodeString(rawPublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return errors.New("DOOPS_CICD_PLAN_PUBLIC_KEY must be a base64 Ed25519 public key")
	}
	signature, err := base64.StdEncoding.DecodeString(plan.Attestation.Signature)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), []byte(plan.Digest), signature) {
		return errors.New("deployment plan attestation signature is invalid")
	}
	return nil
}

func validCICDEvidenceKinds(kinds []string) bool {
	if len(kinds) == 0 {
		return false
	}
	seen := make(map[string]bool, len(kinds))
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" || seen[kind] {
			return false
		}
		seen[kind] = true
	}
	return true
}

func digestCICDPlanJSON(raw json.RawMessage) (string, error) {
	var plan map[string]interface{}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&plan); err != nil || plan == nil {
		return "", errors.New("plan must be a JSON object")
	}
	plan["digest"] = ""
	delete(plan, "attestation")
	return digestCICDValue(plan)
}

func digestCICDValue(value interface{}) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode CI/CD value: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var canonical interface{}
	if err := decoder.Decode(&canonical); err != nil {
		return "", fmt.Errorf("decode CI/CD value: %w", err)
	}
	data, err = json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("canonicalize CI/CD value: %w", err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func findForbiddenCICDPlanField(raw json.RawMessage) (string, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value interface{}
	if err := decoder.Decode(&value); err != nil {
		return "", false, errors.New("plan must be a JSON object")
	}
	field, found := findForbiddenCICDPlanValue(value)
	return field, found, nil
}

func findForbiddenCICDPlanValue(value interface{}) (string, bool) {
	switch value := value.(type) {
	case map[string]interface{}:
		for key, nested := range value {
			if _, forbidden := forbiddenCICDPlanFields[key]; forbidden {
				return key, true
			}
			if field, found := findForbiddenCICDPlanValue(nested); found {
				return field, true
			}
		}
	case []interface{}:
		for _, nested := range value {
			if field, found := findForbiddenCICDPlanValue(nested); found {
				return field, true
			}
		}
	}
	return "", false
}

func cicdReportCollector(plan cicdReconcilePlan, destination *map[string]interface{}) doagentSSECollector {
	return func(update map[string]interface{}) (bool, bool, error) {
		updateType, _ := update["sessionUpdate"].(string)
		switch updateType {
		case "agent_report":
			report, err := validateCICDReport(update["report"], plan)
			if err != nil {
				return true, false, err
			}
			*destination = report
			return true, true, nil
		case "agent_message", "completed":
			return true, false, errors.New("agent_report missing from doagent session/update")
		case "usage_update":
			return true, false, nil
		default:
			return false, false, nil
		}
	}
}

func validateCICDReport(value interface{}, plan cicdReconcilePlan) (map[string]interface{}, error) {
	report, ok := value.(map[string]interface{})
	if !ok || report == nil {
		return nil, errors.New("agent_report.report must be an object")
	}
	reportDigest, ok := report["planDigest"].(string)
	if !ok || reportDigest != plan.Digest {
		return nil, errors.New("agent_report.report.planDigest does not match plan.digest")
	}
	if _, ok := report["status"].(string); !ok {
		return nil, errors.New("agent_report.report.status must be a string")
	}
	status, _ := report["status"].(string)
	switch status {
	case "Pending", "Reconciling", "Converged", "Blocked", "Failed":
	default:
		return nil, errors.New("agent_report.report.status is invalid")
	}
	evidence, ok := report["evidence"].([]interface{})
	if !ok {
		return nil, errors.New("agent_report.report.evidence must be an array")
	}
	for _, item := range evidence {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return nil, errors.New("agent_report.report.evidence entries must be objects")
		}
		kind, _ := entry["kind"].(string)
		reference, _ := entry["reference"].(string)
		if strings.TrimSpace(kind) == "" || strings.TrimSpace(reference) == "" {
			return nil, errors.New("agent_report.report.evidence entries require kind and reference")
		}
	}
	violations, ok := report["violations"].([]interface{})
	if !ok {
		return nil, errors.New("agent_report.report.violations must be an array")
	}
	for _, item := range violations {
		entry, ok := item.(map[string]interface{})
		if !ok {
			return nil, errors.New("agent_report.report.violations entries must be objects")
		}
		code, _ := entry["code"].(string)
		message, _ := entry["message"].(string)
		if strings.TrimSpace(code) == "" || strings.TrimSpace(message) == "" {
			return nil, errors.New("agent_report.report.violations entries require code and message")
		}
	}
	actualEvidence := make(map[string]bool, len(evidence))
	for _, item := range evidence {
		entry := item.(map[string]interface{})
		actualEvidence[entry["kind"].(string)] = true
	}
	switch status {
	case "Converged":
		for _, kind := range plan.Spec.Acceptance.RequiredEvidence {
			if !actualEvidence[kind] {
				return nil, fmt.Errorf("converged report is missing required evidence %q", kind)
			}
		}
	case "Blocked", "Failed":
		if len(violations) == 0 {
			return nil, fmt.Errorf("%s report requires at least one violation", strings.ToLower(status))
		}
		for _, kind := range plan.Spec.Acceptance.RequiredFailureEvidence {
			if !actualEvidence[kind] {
				return nil, fmt.Errorf("%s report is missing required failure evidence %q", strings.ToLower(status), kind)
			}
		}
	}
	return report, nil
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

// subscribeDoagentSSE 订阅 doagent 的 SSE 事件流，将内容实时转发到 WebSocket 客户端。
// 当收到 agent_message（完成）或 error（失败）事件时返回。
func subscribeDoagentSSE(ctx context.Context, baseURL string, sessionID string, pushProgress notificationSender) error {
	return subscribeDoagentSSEWithCollector(ctx, baseURL, sessionID, pushProgress, nil)
}

func subscribeDoagentSSEWithCollector(ctx context.Context, baseURL string, sessionID string, pushProgress notificationSender, collector doagentSSECollector) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", baseURL+"/events?sid="+sessionID, nil)
	if err != nil {
		return fmt.Errorf("create SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("SSE connect: %w", err)
	}
	defer resp.Body.Close()

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
					// agent 完成最终回复：content 同样是 map 或 []interface{}
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
					return nil
				case "completed", "usage_update":
					// 任务完成 / token 统计
					return nil
				}

			case "permission.updated":
				// 权限请求 — 在 build 模式下不应出现，但出现时自动批准
				if perm, ok := params["permission"].(map[string]interface{}); ok {
					permID, _ := perm["id"].(string)
					sessionID, _ := params["sessionId"].(string)
					if permID != "" {
						log.Printf("🔑 Auto-approving permission %s for session %s", permID, sessionID)
						go doagentRPC(baseURL, map[string]interface{}{
							"jsonrpc": "2.0",
							"id":      "perm-" + permID,
							"method":  "permission/reply",
							"params": map[string]interface{}{
								"permissionId": permID,
								"decision":     "allow",
							},
						})
					}
				}

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
