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
						"tools": map[string]interface{}{},
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

// notificationSender 抽象了发送 notifications/message 的能力
type notificationSender func(text string)

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
	pushProgress := func(text string) {
		writeJSON(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "notifications/message",
			"params": map[string]interface{}{
				"sessionID": sessionID,
				"data":      text,
			},
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
		gw.handleAgentPromptWS(ctx, reqID, sessionID, args.Instruction, args.Model, pushProgress, writeJSON)

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

// handleAgentPromptWS 封装 doops_agent_prompt 处理逻辑，通过 ACP HTTP API 调用本地 doagent 服务。
func (gw *Gateway) handleAgentPromptWS(ctx context.Context, reqID interface{}, doopsSessionID string, instr string, model string, pushProgress notificationSender, writeJSON func(v interface{})) {
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

	// [P0 自动固化 v2] 基于审计日志提炼脚本
	solidifyHint := fmt.Sprintf("\n\n[系统指令] 如果你成功执行了构建或部署操作：\n"+
		"1. 读取 /root/ws/%s/.doops-audit-log（这是真实的命令执行记录）\n"+
		"2. 从中提取最终成功的命令序列（跳过失败的尝试，但在注释中说明踩坑原因）\n"+
		"3. 如果 /root/ws/%s/deploy.sh 已存在，对比差异仅更新变化的部分\n"+
		"4. 如果不存在，则生成新的 deploy.sh，包含所有必要步骤\n"+
		"5. deploy.sh 必须基于审计日志中真正执行过且成功的命令，严禁凭空编造",
		doopsSessionID, doopsSessionID)

	fullInstr := instr + solidifyHint

	// 启动 SSE 事件订阅（在 prompt 之前连接，防止丢失事件）
	sseCtx, sseCancel := context.WithTimeout(ctx, 30*time.Minute)
	defer sseCancel()

	sseDone := make(chan error, 1)
	go func() {
		sseDone <- subscribeDoagentSSE(sseCtx, doagentURL, targetSessionID, pushProgress)
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
				"prompt":    fullInstr,
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

	writeJSON(buildSuccessResponse(reqID, "Operation complete."))
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

// subscribeDoagentSSE 订阅 doagent 的 SSE 事件流，将内容实时转发到 WebSocket 客户端。
// 当收到 agent_message（完成）或 error（失败）事件时返回。
func subscribeDoagentSSE(ctx context.Context, baseURL string, sessionID string, pushProgress notificationSender) error {
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
				pushProgress(data)
				continue
			}

			method, _ := update["method"].(string)
			params, _ := update["params"].(map[string]interface{})

			switch method {
			case "session/update":
				// session/update 包含不同类型的更新
				sessionUpdate, _ := params["update"].(map[string]interface{})
				updateType, _ := sessionUpdate["sessionUpdate"].(string)

				switch updateType {
				case "agent_message_chunk":
					// 流式文本块：content 是 map{"text":"...","type":"text"} 或直接字符串
					switch c := sessionUpdate["content"].(type) {
					case map[string]interface{}:
						if text, ok := c["text"].(string); ok && text != "" {
							pushProgress(text)
						}
					case string:
						if c != "" {
							pushProgress(c)
						}
					}
				case "tool_call_update":
					// 工具调用进度
					toolName, _ := sessionUpdate["toolName"].(string)
					status, _ := sessionUpdate["status"].(string)
					if toolName != "" && status == "in_progress" {
						pushProgress(fmt.Sprintf("[tool:%s]", toolName))
					}
				case "agent_message":
					// agent 完成最终回复：content 同样是 map 或 []interface{}
					switch c := sessionUpdate["content"].(type) {
					case map[string]interface{}:
						if text, ok := c["text"].(string); ok && text != "" {
							pushProgress(text)
						}
					case []interface{}:
						for _, item := range c {
							if m, ok := item.(map[string]interface{}); ok {
								if inner, ok := m["content"].(map[string]interface{}); ok {
									if text, ok := inner["text"].(string); ok && text != "" {
										pushProgress(text)
									}
								}
							}
						}
					case string:
						if c != "" {
							pushProgress(c)
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
				pushProgress("[error] " + errMsg)
				return fmt.Errorf("doagent: %s", errMsg)

			default:
				// 其他事件（session/update chunks 等）静默处理
				if method != "" {
					log.Printf("🔔 SSE event: %s", method)
				} else {
					pushProgress(data)
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
	return 30 * time.Second
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
		pushProgress(line)
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
			pushProgress(fmt.Sprintf("\r\033[K[agent] ⏳ 命令后台执行中... (耗时 %ds)", elapsed))
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
