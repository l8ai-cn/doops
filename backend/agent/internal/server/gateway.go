package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/user/doops/agent/api"
)

type bgTask struct {
	TaskID     string
	PID        int
	Cmd        *exec.Cmd
	LogPath    string
	Done       bool
	ExitCode   int
	finishedAt time.Time
	done       chan struct{}
	cancel     context.CancelFunc
	err        error
}

// sessionEntry 存储 Doops->doagent 会话映射及最近使用时间（用于 TTL 驱逐）
type sessionEntry struct {
	doagentSessionID string
	lastUsed         time.Time
}

type Gateway struct {
	mu sync.RWMutex

	// 异步任务注册表
	tasks   map[string]*bgTask
	tasksMu sync.RWMutex

	// Doops SessionID -> doagent SessionID 映射（带 TTL）
	sessionMap   map[string]*sessionEntry
	sessionMapMu sync.RWMutex

	// 容器运行时 (启动时自动检测: nerdctl > docker > podman)
	containerRuntime string

	// Git HTTP handler is invoked only through the authorized reverse tunnel.
	gitHandler http.Handler

	proxyAllowHosts map[string]struct{}
}

func NewGateway() *Gateway {
	gw := &Gateway{
		tasks:           make(map[string]*bgTask),
		sessionMap:      make(map[string]*sessionEntry),
		proxyAllowHosts: proxyAllowlistFromEnv(),
	}
	gw.detectContainerRuntime()
	gw.startSessionReaper()
	gw.startTaskReaper()
	return gw
}

func proxyAllowlistFromEnv() map[string]struct{} {
	allowed := make(map[string]struct{})
	for _, raw := range strings.Split(os.Getenv("DOOPS_PROXY_ALLOW_HOSTS"), ",") {
		host := strings.TrimSpace(strings.ToLower(raw))
		if host != "" {
			allowed[host] = struct{}{}
		}
	}
	return allowed
}

// SetProxyAllowlist replaces the exact upstream host allowlist used by proxy
// requests. An empty list intentionally rejects every proxy request.
func (gw *Gateway) SetProxyAllowlist(hosts []string) {
	allowed := make(map[string]struct{}, len(hosts))
	for _, host := range hosts {
		host = strings.TrimSpace(strings.ToLower(host))
		if host != "" {
			allowed[host] = struct{}{}
		}
	}
	gw.mu.Lock()
	gw.proxyAllowHosts = allowed
	gw.mu.Unlock()
}

func (gw *Gateway) proxyHostAllowed(host string) bool {
	gw.mu.RLock()
	defer gw.mu.RUnlock()
	_, ok := gw.proxyAllowHosts[strings.ToLower(strings.TrimSpace(host))]
	return ok
}

// startSessionReaper 定期驱逐超过 2 小时未使用的 doops->doagent 会话映射，防止 sessionMap 无限增长。
func (gw *Gateway) startSessionReaper() {
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			gw.sessionMapMu.Lock()
			expired := 0
			for k, v := range gw.sessionMap {
				if time.Since(v.lastUsed) > 2*time.Hour {
					delete(gw.sessionMap, k)
					expired++
				}
			}
			gw.sessionMapMu.Unlock()
			if expired > 0 {
				log.Printf("[session-reaper] 清理 %d 个超时会话映射", expired)
			}
		}
	}()
	log.Println("[session-reaper] ✅ Session reaper started (TTL=2h, interval=1h)")
}

func (gw *Gateway) startTaskReaper() {
	interval := time.Minute
	if maxAge := maxCompletedTaskAge(); maxAge < 2*interval {
		interval = maxAge / 2
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond
		}
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for range ticker.C {
			gw.tasksMu.Lock()
			gw.cleanupCompletedTasksLocked(maxCompletedTaskAge())
			gw.tasksMu.Unlock()
		}
	}()
	log.Printf("[task-reaper] started (interval=%s)", interval)
}

// detectContainerRuntime 启动时自动探测容器运行时: nerdctl > docker > podman
func (gw *Gateway) detectContainerRuntime() {
	for _, rt := range []string{"nerdctl", "docker", "podman"} {
		if _, err := exec.LookPath(rt); err == nil {
			gw.containerRuntime = rt
			log.Printf("[gateway] 检测到容器运行时: %s", rt)
			return
		}
	}
	gw.containerRuntime = "docker" // 兜底默认
	log.Println("[gateway] ⚠️ 未检测到任何容器运行时，默认使用 docker")
}

// SetupGitHandler 配置原生的 Git HTTP 代理，并在每次收到 push 后通过钩子自动向 /root/ws/{sessionID} 释放代码
func (gw *Gateway) SetupGitHandler() {
	service, err := newGitHTTPHandler("/tmp/repos", `#!/bin/bash
set -e
SESSION_ID=$(basename "$PWD" .git)
case "$SESSION_ID" in
  ""|.|..|*/*|*..*)
    echo "remote: ❌ doops-agent: refusing unsafe SESSION_ID: $SESSION_ID" >&2
    exit 1
    ;;
esac
DEST="/root/ws/$SESSION_ID"
READY_FILE="$DEST/.doops-ready"
echo "remote: 🚀 doops-agent: Preparing to deploy to $DEST"
mkdir -p "$DEST"
rm -f "$READY_FILE" "$READY_FILE.tmp"
git --work-tree="$DEST" clean -fdx
if ! git --work-tree="$DEST" checkout -f HEAD; then
    rm -f "$READY_FILE" "$READY_FILE.tmp"
    echo "remote: ❌ doops-agent: Checkout failed!" >&2
    exit 1
fi
COMMIT_SHA=$(git rev-parse HEAD)
printf "%s\n" "$COMMIT_SHA" > "$READY_FILE.tmp"
mv "$READY_FILE.tmp" "$READY_FILE"
echo "remote: ✅ doops-agent: Successfully deployed code to $DEST"
`)
	if err != nil {
		log.Fatalf("Failed to setup git service: %v", err)
	}

	gw.gitHandler = http.StripPrefix("/git", service)
	log.Printf("   Git handler ready for Gateway reverse-tunnel requests")
}

// --- 异步后台任务管理 ---

// submitBgTask 提交一个完全脱离 PTY 的后台任务。
// 使用 setsid 创建独立进程组，输出重定向到日志文件，不受 PTY 生命周期影响。
func (gw *Gateway) submitBgTask(sessionID, command, logPath string) (*bgTask, error) {
	if err := validateSession(sessionID); err != nil {
		return nil, err
	}
	cmd, script, err := bashCommand(command)
	if err != nil {
		return nil, err
	}
	if script != nil {
		defer script.Close()
	}
	taskID := generateID()
	sessionRoot, err := workspacePath(sessionID)
	if err != nil {
		return nil, err
	}
	if logPath == "" {
		logPath = filepath.Join(sessionRoot, ".doops-tasks", taskID+".log")
	} else {
		resolved, err := resolveWorkspaceFilePath(sessionID, logPath)
		if err != nil {
			return nil, err
		}
		logPath = resolved
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0755); err != nil {
		return nil, err
	}

	// 构造独立进程: setsid bash -c 'command' > logPath 2>&1
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // 独立会话组

	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("cannot create log file %s: %w", logPath, err)
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("failed to start background task: %w", err)
	}

	task := &bgTask{
		TaskID:  taskID,
		PID:     cmd.Process.Pid,
		Cmd:     cmd,
		LogPath: logPath,
		done:    make(chan struct{}),
	}
	taskCtx, cancel := context.WithCancel(context.Background())
	task.cancel = cancel

	gw.tasksMu.Lock()
	gw.cleanupCompletedTasksLocked(maxCompletedTaskAge())
	gw.tasks[taskID] = task
	gw.tasksMu.Unlock()

	// 后台等待进程结束并更新状态
	go func() {
		defer cancel()
		waitDone := make(chan error, 1)
		go func() { waitDone <- cmd.Wait() }()
		err := waitProcessGroup(taskCtx, task.PID, waitDone)
		if logFile != nil {
			state := "CMD FINISH"
			if errors.Is(err, errExecutionOutcomeUnknown) {
				state = "CMD OUTCOME UNKNOWN"
			}
			logFile.WriteString(fmt.Sprintf("\n========== [%s: %s (Err: %v)] ==========\n\n", state, time.Now().Format(time.RFC3339), err))
			logFile.Close()
		}
		gw.tasksMu.Lock()
		task.err = err
		task.Done = !errors.Is(err, errExecutionOutcomeUnknown)
		task.finishedAt = time.Now()
		if err != nil {
			if exitErr, ok := err.(*exec.ExitError); ok {
				task.ExitCode = exitErr.ExitCode()
			} else {
				task.ExitCode = -1
			}
		}
		gw.cleanupCompletedTasksLocked(maxCompletedTaskAge())
		gw.tasksMu.Unlock()
		close(task.done)
		log.Printf("Background task %s: done=%t exit=%d err=%v", taskID, task.Done, task.ExitCode, err)
	}()

	log.Printf("🚀 Background task submitted: id=%s pid=%d log=%s", taskID, task.PID, logPath)
	return task, nil
}

func (gw *Gateway) cleanupCompletedTasksLocked(maxAge time.Duration) {
	if maxAge <= 0 {
		return
	}
	now := time.Now()
	for id, task := range gw.tasks {
		if task != nil && !task.finishedAt.IsZero() && now.Sub(task.finishedAt) > maxAge {
			delete(gw.tasks, id)
		}
	}
}

// getTaskStatus 获取异步任务的状态和日志尾部。
func (gw *Gateway) getTaskStatus(taskID string, lines int) (*api.TaskInfo, error) {
	gw.tasksMu.RLock()
	task, ok := gw.tasks[taskID]
	if ok {
		snapshot := *task
		task = &snapshot
	}
	gw.tasksMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	if lines <= 0 {
		lines = 30
	}

	status := "running"
	if errors.Is(task.err, errExecutionOutcomeUnknown) {
		status = "unknown"
	} else if task.Done {
		if task.ExitCode == 0 {
			status = "done"
		} else {
			status = "failed"
		}
	}

	// 读取日志尾部
	var logTail string
	tailCmd := exec.Command("tail", "-n", strconv.Itoa(lines), task.LogPath)
	if out, err := tailCmd.Output(); err == nil {
		logTail = string(out)
	}

	return &api.TaskInfo{
		TaskID:   task.TaskID,
		PID:      task.PID,
		Status:   status,
		ExitCode: task.ExitCode,
		LogPath:  task.LogPath,
		LogTail:  logTail,
	}, nil
}

// generateID 生成 8 字节 hex 随机 ID，用于 sentinel 和 taskID。
func generateID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
