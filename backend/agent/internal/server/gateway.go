package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/sosedoff/gitkit"
	"github.com/user/doops/agent/api"
	"github.com/user/doops/agent/internal/dispatcher"
)

type bgTask struct {
	TaskID     string
	PID        int
	Cmd        *exec.Cmd
	LogPath    string
	Done       bool
	ExitCode   int
	finishedAt time.Time
}

// sessionEntry 存储 Doops->doagent 会话映射及最近使用时间（用于 TTL 驱逐）
type sessionEntry struct {
	doagentSessionID string
	lastUsed         time.Time
}

type Gateway struct {
	Dispatcher *dispatcher.Dispatcher
	mu         sync.RWMutex
	Port       string
	Token      string

	// InsecureGit allows anonymous git push/pull when no Token is configured.
	// It must be explicitly opted into (e.g. via --insecure-git); otherwise
	// the git endpoint default-denies when Token is empty.
	InsecureGit bool

	// 异步任务注册表
	tasks   map[string]*bgTask
	tasksMu sync.RWMutex

	// Doops SessionID -> doagent SessionID 映射（带 TTL）
	sessionMap   map[string]*sessionEntry
	sessionMapMu sync.RWMutex

	// 容器运行时 (启动时自动检测: nerdctl > docker > podman)
	containerRuntime string

	// Git HTTP handler is used by both the local /git endpoint and the
	// reverse gateway tunnel endpoint.
	gitHandler http.Handler
}

func NewGateway(port string) *Gateway {
	gw := &Gateway{
		Dispatcher: dispatcher.NewDispatcher(),
		Port:       port,
		tasks:      make(map[string]*bgTask),
		sessionMap: make(map[string]*sessionEntry),
	}
	gw.detectContainerRuntime()
	gw.startZombieReaper()
	gw.startSessionReaper()
	return gw
}

// startZombieReaper 定期回收僵尸子进程。
// 在容器环境中 doops-agent 通常作为 PID 1 运行，
// Linux 内核要求 PID 1 承担 subreaper 角色回收所有孤儿进程的退出状态。
// Go runtime 不会自动做这件事，因此需要显式的 Wait4 循环。
func (gw *Gateway) startZombieReaper() {
	var reaperTotal int64
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			reaped := 0
			for {
				var ws syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
				if pid <= 0 || err != nil {
					break
				}
				reaped++
				atomic.AddInt64(&reaperTotal, 1)
				log.Printf("[reaper] 回收僵尸进程 PID=%d exit=%d (累计: %d)",
					pid, ws.ExitStatus(), atomic.LoadInt64(&reaperTotal))
			}
			if reaped > 0 {
				log.Printf("[reaper] 本轮回收 %d 个僵尸进程", reaped)
			}
		}
	}()
	log.Println("[reaper] ✅ Zombie reaper started (interval=30s)")
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
	// Require authentication unless the operator explicitly opted into
	// anonymous git via InsecureGit while no Token is configured. When a Token
	// is set we always require auth. When no Token is set and InsecureGit is
	// false we still enable auth so the (default-deny) AuthFunc rejects pushes.
	requireAuth := gw.Token != "" || !gw.InsecureGit
	if gw.Token == "" && gw.InsecureGit {
		log.Printf("   ⚠️ Git endpoint running WITHOUT authentication (--insecure-git); anyone who can reach it may push/pull")
	}
	service := gitkit.New(gitkit.Config{
		Dir:        "/tmp/repos",
		AutoCreate: true,
		AutoHooks:  true,
		Auth:       requireAuth,
		Hooks: &gitkit.HookScripts{
			PostReceive: `#!/bin/bash
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
if ! git --work-tree="$DEST" checkout -f HEAD; then
    rm -f "$READY_FILE" "$READY_FILE.tmp"
    echo "remote: ❌ doops-agent: Checkout failed!" >&2
    exit 1
fi
COMMIT_SHA=$(git rev-parse HEAD)
printf "%s\n" "$COMMIT_SHA" > "$READY_FILE.tmp"
mv "$READY_FILE.tmp" "$READY_FILE"
echo "remote: ✅ doops-agent: Successfully deployed code to $DEST"
`,
		},
	})

	if err := service.Setup(); err != nil {
		log.Fatalf("Failed to setup git service: %v", err)
	}

	service.AuthFunc = func(cred gitkit.Credential, _ *gitkit.Request) (bool, error) {
		if gw.Token == "" {
			// No token configured: only allow when anonymous git is explicitly enabled.
			return gw.InsecureGit, nil
		}
		return secureTokenEqual(cred.Password, gw.Token), nil
	}

	gw.gitHandler = http.StripPrefix("/git", service)
	http.Handle("/git/", gw.gitHandler)
	log.Printf("   Git endpoint: http://127.0.0.1:%s/git/", gw.Port)
}

// --- 异步后台任务管理 ---

// submitBgTask 提交一个完全脱离 PTY 的后台任务。
// 使用 setsid 创建独立进程组，输出重定向到日志文件，不受 PTY 生命周期影响。
func (gw *Gateway) submitBgTask(sessionID, command, logPath string) (*bgTask, error) {
	if err := validateSession(sessionID); err != nil {
		return nil, err
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
	cmd := exec.Command("bash", "-c", command)
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
	}

	gw.tasksMu.Lock()
	gw.cleanupCompletedTasksLocked(maxCompletedTaskAge())
	gw.tasks[taskID] = task
	gw.tasksMu.Unlock()

	// 后台等待进程结束并更新状态
	go func() {
		err = cmd.Wait()
		if logFile != nil {
			logFile.WriteString(fmt.Sprintf("\n========== [CMD FINISH: %s (Err: %v)] ==========\n\n", time.Now().Format(time.RFC3339), err))
			logFile.Close()
		}
		gw.tasksMu.Lock()
		task.Done = true
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
		log.Printf("✅ Background task %s finished (exit=%d)", taskID, task.ExitCode)
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
		if task != nil && task.Done && !task.finishedAt.IsZero() && now.Sub(task.finishedAt) > maxAge {
			delete(gw.tasks, id)
		}
	}
}

// getTaskStatus 获取异步任务的状态和日志尾部。
func (gw *Gateway) getTaskStatus(taskID string, lines int) (*api.TaskInfo, error) {
	gw.tasksMu.RLock()
	task, ok := gw.tasks[taskID]
	gw.tasksMu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}

	if lines <= 0 {
		lines = 30
	}

	status := "running"
	if task.Done {
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
