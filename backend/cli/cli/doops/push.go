package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const workspaceSourceManifestPath = ".doops/source.json"

type workspaceSourceManifest struct {
	APIVersion string                  `json:"apiVersion"`
	Kind       string                  `json:"kind"`
	Source     WorkspaceSourceIdentity `json:"source"`
}

var immutableGitCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

var (
	gitAuthCleanupOnce   sync.Once
	gitAuthCleanupMu     sync.Mutex
	signalCleanupDirs    = make(map[string]struct{})
	activeGitProcesses   = make(map[int]struct{})
	gitShutdownRequested bool
)

// 默认排除的目录/文件
var defaultExcludes = []string{
	".git",
	".doops",
	".agent",
	".claude",
	".local",
	".pnpm-store",
	".npm",
	".cache",
	"node_modules",
	"vendor",
	"__pycache__",
	".idea",
	".vscode",
	".turbo",
	".next",
	".nuxt",
	".svelte-kit",
	"coverage",
	"tmp",
	"temp",
	".DS_Store",
}

// Push 将本地代码无感隔离投递至大模型探针服务，不落宿主机磁盘、不污染用户 Git
func Push(server Server, src, dest string, dryRun bool, extraExcludes []string, sessionID string) error {
	_, err := PushWithCommit(server, src, dest, dryRun, extraExcludes, sessionID)
	return err
}

func PushWithCommit(server Server, src, dest string, dryRun bool, extraExcludes []string, sessionID string) (string, error) {
	return pushWorkspaceSnapshot(server, src, dest, dryRun, extraExcludes, sessionID)
}

func pushWorkspaceSnapshot(server Server, src, dest string, dryRun bool, extraExcludes []string, sessionID string) (string, error) {
	return pushWorkspaceSnapshotWithSource(server, src, dest, dryRun, extraExcludes, sessionID, nil)
}

func pushWorkspaceSnapshotWithSource(
	server Server,
	src, dest string,
	dryRun bool,
	extraExcludes []string,
	sessionID string,
	source *WorkspaceSourceIdentity,
) (string, error) {
	return pushSnapshot(server, src, dest, dryRun, extraExcludes, sessionID, source, "gateway", "", "", nil)
}

func pushSnapshot(
	server Server,
	src, dest string,
	dryRun bool,
	extraExcludes []string,
	sessionID string,
	source *WorkspaceSourceIdentity,
	storage, credential, token string,
	transfer cosTransfer,
) (string, error) {
	// 1. 解析源目录
	src, err := filepath.Abs(src)
	if err != nil {
		return "", fmt.Errorf("解析源路径失败: %v", err)
	}

	info, err := os.Stat(src)
	if err != nil {
		return "", fmt.Errorf("源路径不存在: %s", src)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("源路径必须是目录: %s", src)
	}

	if !strings.HasSuffix(src, "/") {
		src += "/"
	}

	// 优先级：显式 sessionID > dest 推导
	if sessionID == "" && dest != "" {
		clean := strings.TrimRight(dest, "/")
		sessionID = filepath.Base(clean)
	}
	sessionID = resolveSession(sessionID)

	mode, err := normalizeStorageMode(storage)
	if err != nil {
		return "", err
	}
	if mode == "cos" {
		if strings.TrimSpace(token) == "" {
			return "", fmt.Errorf("gateway_auth_missing")
		}
		if !dryRun {
			var closeTransfer func()
			transfer, closeTransfer, err = ensureCOSTransfer(server, sessionID, token, transfer)
			if err != nil {
				return "", err
			}
			defer closeTransfer()
			if err := transfer.ProbeSnapshot(); err != nil {
				return "", err
			}
		}
	} else {
		token, err = ResolveToken(server)
		if err != nil {
			return "", err
		}
	}
	workspacePath := fmt.Sprintf("/root/ws/%s", sessionID)

	// 2. 建立本地幽灵沙盒
	tmpDir, err := createWorkspaceSnapshotDirectory(sessionID)
	if err != nil {
		return "", fmt.Errorf("创建幽灵临时目录失败: %v", err)
	}
	defer cleanupSignalDirectory(tmpDir)

	start := time.Now()
	fmt.Printf("🧊 正在构建本地隔离快照（大型仓库首次扫描可能较慢）...\n")
	stagedFiles, err := stageWorkspaceSnapshot(src, tmpDir, extraExcludes, dryRun, source)
	if err != nil {
		return "", fmt.Errorf("本地缓存集结失败: %v", err)
	}

	if dryRun {
		fmt.Printf("📋 [DRY-RUN] 预计推送 %d 个文件到远端工作区 %s\n", len(stagedFiles), workspacePath)
		for _, rel := range stagedFiles {
			fmt.Println(rel)
		}
		return "", nil
	}

	// 4. 在幽灵目录中进行纯净 Git 化。gateway 模式再差分推送；cos 模式把同一份快照对象写入 COS。
	fmt.Printf("📦 本地隔离快照整理完成: %d 个文件\n", len(stagedFiles))
	fmt.Printf("🔧 正在初始化幽灵 Git 仓库并生成同步提交...\n")

	localCommit, err := createWorkspaceSnapshotCommit(tmpDir, source)
	if err != nil {
		return "", err
	}
	if mode == "cos" {
		fmt.Printf("🚀 正在把隔离快照上传到 COS 桶 %s: %s (%s)\n", cosBucketName, workspacePath, server.Name)
		status, err := uploadCOSSnapshot(tmpDir, stagedFiles, localCommit, transfer)
		if err != nil {
			return "", err
		}
		elapsed := time.Since(start)
		shortCommit := localCommit
		if len(shortCommit) > 12 {
			shortCommit = shortCommit[:12]
		}
		fmt.Printf("✅ COS 快照已写入 (%s) @ %s\n", elapsed.Round(time.Millisecond), shortCommit)
		if status == "" || status == "synced" {
			fmt.Printf("workspace: synced %s\n", workspacePath)
		} else {
			fmt.Printf("workspace: %s\n", status)
		}
		return localCommit, nil
	}

	fmt.Printf("🚀 进行极速无感增量推送至远端工作区: %s (%s)\n", workspacePath, server.Name)
	sourceManifestBlob := ""
	if source != nil {
		sourceManifestBlob, err = workspaceSourceManifestBlobID(tmpDir, localCommit)
		if err != nil {
			return "", err
		}
	}
	gitURL, err := buildGitRemoteURLForServer(server, sessionID, true)
	if err != nil {
		return "", err
	}

	gitCmds := [][]string{
		{"push", "-f", gitURL, "HEAD:master"},
	}
	gitEnv, cleanupGitAuth, err := prepareGitAuthEnvironment(token)
	if err != nil {
		return "", err
	}
	defer cleanupGitAuth()
	for _, gargs := range gitCmds {
		stepLabel := describeGitStep(gargs)
		if stepLabel != "" {
			fmt.Printf("⏳ %s...\n", stepLabel)
		}

		maxRetries := 1
		if gargs[0] == "push" {
			maxRetries = 3
		}

		var lastErr error
		for attempt := 1; attempt <= maxRetries; attempt++ {
			gitArgs := append([]string{"-c", "credential.helper="}, gargs...)
			cmd := exec.Command("git", gitArgs...)
			cmd.Dir = tmpDir
			cmd.Env = gitEnv

			// 我们只让最后一步 push 原样输出网络传输进度给用户
			if gargs[0] == "push" {
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if attempt > 1 {
					fmt.Printf("⚠️ 网络异常或超时，正在进行第 %d 次重试推送...\n", attempt)
					time.Sleep(2 * time.Second)
				}
			}

			if err := runIsolatedGitCommand(cmd); err != nil {
				lastErr = err
				if attempt < maxRetries && gargs[0] == "push" {
					continue // 准备重推
				}
				return "", fmt.Errorf("幽灵传输阶段执行失败 (%v): %v", redactGitArgs(gargs), lastErr)
			}
			break // 成功则跳出重试循环
		}
	}

	fmt.Printf("🔍 正在确认远端工作区已完成 checkout 并就绪...\n")
	representativeRel := ""
	if len(stagedFiles) > 0 {
		representativeRel = stagedFiles[0]
	}
	if err := waitForWorkspaceReady(
		server,
		token,
		sessionID,
		workspacePath,
		representativeRel,
		localCommit,
		sourceManifestBlob,
		45*time.Second,
	); err != nil {
		return "", fmt.Errorf("push 已完成，但远端工作区未确认就绪: %v", err)
	}

	elapsed := time.Since(start)
	shortCommit := localCommit
	if len(shortCommit) > 12 {
		shortCommit = shortCommit[:12]
	}
	fmt.Printf("✅ 同步完成 (%s) 🎯 远端工作区已就绪: %s @ %s\n", elapsed.Round(time.Millisecond), workspacePath, shortCommit)
	return localCommit, nil
}

func createWorkspaceSnapshotDirectory(sessionID string) (string, error) {
	ensureSignalCleanupHandler()
	gitAuthCleanupMu.Lock()
	defer gitAuthCleanupMu.Unlock()
	if gitShutdownRequested {
		return "", fmt.Errorf("workspace snapshot canceled by shutdown")
	}
	dir, err := os.MkdirTemp("", "doops-sync-"+sessionID+"-")
	if err != nil {
		return "", err
	}
	signalCleanupDirs[dir] = struct{}{}
	return dir, nil
}

func validWorkspaceCommit(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func createWorkspaceSnapshotCommit(root string, source *WorkspaceSourceIdentity) (string, error) {
	gitCmds := [][]string{
		{"init", "-b", "master"},
		{"add", "--force", "."},
	}
	if source != nil {
		gitCmds = append(gitCmds, []string{"add", "-f", "--", workspaceSourceManifestPath})
	}
	gitCmds = append(gitCmds,
		// Internal snapshot commit: never use the host's signing setup.
		[]string{"-c", "user.name=doops", "-c", "user.email=doops@localhost", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "AutoSync"},
	)

	for _, args := range gitCmds {
		if label := describeGitStep(args); label != "" {
			fmt.Printf("⏳ %s...\n", label)
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if output, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("创建幽灵提交失败 (%v): %v: %s", args, err, strings.TrimSpace(string(output)))
		}
	}

	commit, err := gitCommandOutput(root, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("读取本地幽灵提交失败: %v", err)
	}
	commit = strings.TrimSpace(commit)
	if source != nil {
		if err := validateWorkspaceSourceManifestInCommit(root, commit, *source); err != nil {
			return "", err
		}
	}
	return commit, nil
}

func validateWorkspaceSourceManifestInCommit(root, commit string, source WorkspaceSourceIdentity) error {
	object := commit + ":" + workspaceSourceManifestPath
	objectType, err := gitCommandOutput(root, "cat-file", "-t", object)
	if err != nil {
		return fmt.Errorf("transport commit is missing %s: %w", workspaceSourceManifestPath, err)
	}
	if strings.TrimSpace(objectType) != "blob" {
		return fmt.Errorf("transport commit %s must be a regular file", workspaceSourceManifestPath)
	}
	data, err := gitCommandOutput(root, "show", object)
	if err != nil {
		return fmt.Errorf("read transport source manifest: %w", err)
	}
	var manifest workspaceSourceManifest
	if err := json.Unmarshal([]byte(data), &manifest); err != nil {
		return fmt.Errorf("decode transport source manifest: %w", err)
	}
	if manifest.APIVersion != workspaceManifestAPIVersion || manifest.Kind != "WorkspaceSource" {
		return fmt.Errorf("transport source manifest has invalid type")
	}
	if manifest.Source != source {
		return fmt.Errorf("transport source manifest does not match declared source identity")
	}
	return nil
}

func workspaceSourceManifestBlobID(root, commit string) (string, error) {
	blob, err := gitCommandOutput(root, "rev-parse", commit+":"+workspaceSourceManifestPath)
	if err != nil {
		return "", fmt.Errorf("read transport source manifest blob: %w", err)
	}
	blob = strings.TrimSpace(blob)
	if !validWorkspaceCommit(blob) {
		return "", fmt.Errorf("transport source manifest blob ID is invalid")
	}
	return blob, nil
}

func stageWorkspaceSnapshot(
	src, tmpDir string,
	extraExcludes []string,
	dryRun bool,
	source *WorkspaceSourceIdentity,
) ([]string, error) {
	stagedFiles, err := stageSnapshot(src, tmpDir, extraExcludes, dryRun)
	if err != nil {
		return nil, err
	}
	if source == nil {
		return stagedFiles, nil
	}
	if dryRun {
		stagedFiles = append(stagedFiles, workspaceSourceManifestPath)
	} else {
		manifestPath, err := writeWorkspaceSourceManifest(tmpDir, source)
		if err != nil {
			return nil, fmt.Errorf("write source identity manifest: %w", err)
		}
		stagedFiles = append(stagedFiles, manifestPath)
	}
	sort.Strings(stagedFiles)
	return stagedFiles, nil
}

func writeWorkspaceSourceManifest(root string, source *WorkspaceSourceIdentity) (string, error) {
	if source == nil {
		return "", fmt.Errorf("source identity is required")
	}
	if strings.TrimSpace(source.Repository) == "" {
		return "", fmt.Errorf("source repository is required")
	}
	if !immutableGitCommitPattern.MatchString(strings.TrimSpace(source.Revision)) {
		return "", fmt.Errorf("source revision must be an immutable Git commit")
	}
	if strings.TrimSpace(source.Branch) == "" || strings.TrimSpace(source.Branch) != source.Branch {
		return "", fmt.Errorf("source branch is required")
	}
	manifest := workspaceSourceManifest{
		APIVersion: workspaceManifestAPIVersion,
		Kind:       "WorkspaceSource",
		Source:     *source,
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode source manifest: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(root, filepath.FromSlash(workspaceSourceManifestPath))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("create source manifest directory: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", fmt.Errorf("write source manifest: %w", err)
	}
	return workspaceSourceManifestPath, nil
}

func buildGitRemoteURLForServer(server Server, sessionID string, authenticated bool) (string, error) {
	if strings.TrimSpace(server.Gateway) == "" {
		return "", fmt.Errorf("Git workspace transport requires a gateway target")
	}

	rawGateway := strings.TrimSpace(server.Gateway)
	if !strings.Contains(rawGateway, "://") {
		rawGateway = "https://" + rawGateway
	}
	u, err := url.Parse(rawGateway)
	if err != nil {
		return "", fmt.Errorf("invalid gateway URL %q: %w", server.Gateway, err)
	}
	switch u.Scheme {
	case "http", "https":
	case "ws":
		u.Scheme = "http"
	case "wss":
		u.Scheme = "https"
	default:
		return "", fmt.Errorf("unsupported gateway scheme %q", u.Scheme)
	}
	if err := enforceSecureGatewayURL(server.Gateway, u); err != nil {
		return "", err
	}
	if authenticated {
		u.User = url.User("doops")
	}
	cluster := strings.TrimSpace(server.Cluster)
	if cluster == "" {
		return "", fmt.Errorf("Git workspace transport requires target cluster")
	}
	instance := strings.TrimSpace(server.Instance)
	if instance == "" {
		return "", fmt.Errorf("Git workspace transport requires target instance")
	}
	u.Path = path.Join(u.Path, "v1", "git", cluster, instance, sessionID+".git")
	u.RawQuery = ""
	return u.String(), nil
}

func prepareGitAuthEnvironment(token string) ([]string, func(), error) {
	cleanupDeadGitAuthDirectories()
	env := filteredEnvironment(
		os.Environ(),
		"GIT_ASKPASS",
		"GIT_TERMINAL_PROMPT",
		"GIT_CONFIG_GLOBAL",
		"GIT_CONFIG_NOSYSTEM",
		"GIT_CONFIG_COUNT",
		"DOOPS_GIT_TOKEN_FILE",
	)
	env = filteredEnvironmentPrefixes(env, "GIT_TRACE", "GIT_CONFIG_KEY_", "GIT_CONFIG_VALUE_")
	env = append(env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	if token == "" {
		return env, func() {}, nil
	}

	ensureSignalCleanupHandler()
	gitAuthCleanupMu.Lock()
	if gitShutdownRequested {
		gitAuthCleanupMu.Unlock()
		return nil, nil, fmt.Errorf("Git authentication canceled by shutdown")
	}
	buildingDir, err := os.MkdirTemp("", ".doops-git-auth-")
	if err != nil {
		gitAuthCleanupMu.Unlock()
		return nil, nil, fmt.Errorf("create Git authentication directory: %w", err)
	}
	lockPath := filepath.Join(buildingDir, "active.lock")
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.RemoveAll(buildingDir)
		gitAuthCleanupMu.Unlock()
		return nil, nil, fmt.Errorf("create Git authentication lock: %w", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lockFile.Close()
		_ = os.RemoveAll(buildingDir)
		gitAuthCleanupMu.Unlock()
		return nil, nil, fmt.Errorf("lock Git authentication directory: %w", err)
	}
	authDir := filepath.Join(
		filepath.Dir(buildingDir),
		fmt.Sprintf("doops-git-auth-%d-%s", os.Getpid(), strings.TrimPrefix(filepath.Base(buildingDir), ".doops-git-auth-")),
	)
	if err := os.Rename(buildingDir, authDir); err != nil {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		_ = os.RemoveAll(buildingDir)
		gitAuthCleanupMu.Unlock()
		return nil, nil, fmt.Errorf("activate Git authentication directory: %w", err)
	}
	signalCleanupDirs[authDir] = struct{}{}
	gitAuthCleanupMu.Unlock()
	cleanup := func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		cleanupSignalDirectory(authDir)
	}
	tokenPath := filepath.Join(authDir, "token")
	if err := os.WriteFile(tokenPath, []byte(token), 0o600); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write Git authentication token: %w", err)
	}
	askPassPath := filepath.Join(authDir, "askpass")
	askPass := `#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' 'doops' ;;
  *Password*) cat "$DOOPS_GIT_TOKEN_FILE"; printf '\n' ;;
  *) exit 1 ;;
esac
`
	if err := os.WriteFile(askPassPath, []byte(askPass), 0o700); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("write Git authentication helper: %w", err)
	}
	env = append(env, "GIT_ASKPASS="+askPassPath, "DOOPS_GIT_TOKEN_FILE="+tokenPath)
	return env, cleanup, nil
}

func ensureSignalCleanupHandler() {
	gitAuthCleanupOnce.Do(func() {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
		go func() {
			sig := <-signals
			gitAuthCleanupMu.Lock()
			gitShutdownRequested = true
			dirs := make([]string, 0, len(signalCleanupDirs))
			for dir := range signalCleanupDirs {
				dirs = append(dirs, dir)
			}
			pids := make([]int, 0, len(activeGitProcesses))
			for pid := range activeGitProcesses {
				pids = append(pids, pid)
			}
			gitAuthCleanupMu.Unlock()
			terminateGitProcessGroups(pids)
			for _, dir := range dirs {
				_ = os.RemoveAll(dir)
			}
			if value, ok := sig.(syscall.Signal); ok {
				os.Exit(128 + int(value))
			}
			os.Exit(1)
		}()
	})
}

func runIsolatedGitCommand(cmd *exec.Cmd) error {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	gitAuthCleanupMu.Lock()
	if gitShutdownRequested {
		gitAuthCleanupMu.Unlock()
		return fmt.Errorf("Git command canceled by shutdown")
	}
	if err := cmd.Start(); err != nil {
		gitAuthCleanupMu.Unlock()
		return err
	}
	pid := cmd.Process.Pid
	activeGitProcesses[pid] = struct{}{}
	gitAuthCleanupMu.Unlock()
	err := cmd.Wait()
	gitAuthCleanupMu.Lock()
	delete(activeGitProcesses, pid)
	gitAuthCleanupMu.Unlock()
	return err
}

func terminateGitProcessGroups(pids []int) {
	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		allExited := true
		for _, pid := range pids {
			if processGroupIsAlive(pid) {
				allExited = false
				break
			}
		}
		if allExited {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

func cleanupSignalDirectory(dir string) {
	gitAuthCleanupMu.Lock()
	err := os.RemoveAll(dir)
	delete(signalCleanupDirs, dir)
	gitAuthCleanupMu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: remove temporary directory: %v\n", err)
	}
}

func cleanupDeadGitAuthDirectories() {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "doops-git-auth-") {
			continue
		}
		dir := filepath.Join(os.TempDir(), entry.Name())
		lockFile, err := os.OpenFile(filepath.Join(dir, "active.lock"), os.O_RDWR, 0)
		if err != nil {
			_ = os.RemoveAll(dir)
			continue
		}
		if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = lockFile.Close()
			continue
		}
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
		_ = os.RemoveAll(dir)
	}
}

func processGroupIsAlive(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || err == syscall.EPERM
}

func filteredEnvironment(env []string, keys ...string) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if _, found := blocked[key]; found {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func filteredEnvironmentPrefixes(env []string, prefixes ...string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		blocked := false
		for _, prefix := range prefixes {
			if strings.HasPrefix(key, prefix) {
				blocked = true
				break
			}
		}
		if !blocked {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func redactGitArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	for i, arg := range redacted {
		if strings.HasPrefix(arg, "http://") || strings.HasPrefix(arg, "https://") {
			redacted[i] = redactRemoteURL(arg)
		}
	}
	return redacted
}

func redactRemoteURL(remote string) string {
	parsed, err := url.Parse(remote)
	if err != nil || parsed.User == nil {
		return remote
	}
	parsed.User = url.User(parsed.User.Username())
	return parsed.String()
}

func describeGitStep(args []string) string {
	if len(args) == 0 {
		return ""
	}
	switch args[0] {
	case "init":
		return "初始化临时 Git 仓库"
	case "add":
		return "写入快照索引"
	case "push":
		return "推送增量到远端探针"
	case "-c":
		if len(args) >= 4 && args[2] == "commit" {
			return "生成同步提交"
		}
	}
	return ""
}

func gitCommandOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s 失败: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func waitForWorkspaceReady(
	server Server,
	token string,
	sessionID string,
	workspacePath string,
	representativeRel string,
	expectedCommit string,
	expectedSourceManifestBlob string,
	timeout time.Duration,
) error {
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	readyFile := filepath.ToSlash(filepath.Join(workspacePath, ".doops-ready"))
	representativePath := ""
	if representativeRel != "" {
		representativePath = filepath.ToSlash(filepath.Join(workspacePath, representativeRel))
	}
	deadline := time.Now().Add(timeout)
	mcpClient := NewMCPClient(server, NewSessionStore(), sessionID, false)
	mcpClient.Token = token
	defer mcpClient.Close()

	var lastOut string
	var lastErr string
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		cmd := ""
		if expectedSourceManifestBlob != "" {
			cmd = workspaceCheckoutProofCommand(workspacePath, sessionID, expectedCommit)
		} else {
			cmd = fmt.Sprintf("if [ -f %q ]; then printf 'ready:'; tr -d '\\r\\n' < %q; exit 0; fi", readyFile, readyFile)
			cmd += "; exit 3"
		}
		stdout, err := mcpClient.CallAndCapture("doops_shell", map[string]interface{}{
			"command": cmd,
		})
		lastOut = strings.TrimSpace(stdout)
		lastErr = ""
		if err == nil {
			if expectedSourceManifestBlob != "" {
				if err := validateWorkspaceCheckoutProof(lastOut, expectedCommit, expectedSourceManifestBlob); err == nil {
					return nil
				} else {
					lastErr = err.Error()
				}
			} else if remoteCommit, ready := workspaceReadyCommit(lastOut); ready {
				if remoteCommit == expectedCommit {
					return nil
				}
				if remoteCommit != "" {
					return fmt.Errorf("远端 ready 哨兵存在，但提交不匹配: want=%s got=%s", expectedCommit, remoteCommit)
				}
			}
		} else {
			lastErr = err.Error()
		}
		if attempt == 1 || attempt%3 == 0 {
			fmt.Printf("⏳ 等待远端释放代码到 %s（第 %d 次探测）...\n", workspacePath, attempt)
		}
		time.Sleep(2 * time.Second)
	}

	if lastOut != "" {
		return fmt.Errorf("等待超时，最后一次 ready 输出=%q stderr=%q", lastOut, lastErr)
	}
	if representativePath != "" {
		return fmt.Errorf("等待超时，未发现远端 ready 哨兵 %s（代表文件 %s 不能替代提交校验）", readyFile, representativePath)
	}
	return fmt.Errorf("等待超时，未发现远端 ready 哨兵 %s", readyFile)
}

func workspaceCheckoutProofCommand(workspacePath, sessionID, expectedCommit string) string {
	readyFile := filepath.ToSlash(filepath.Join(workspacePath, ".doops-ready"))
	manifestFile := filepath.ToSlash(filepath.Join(workspacePath, workspaceSourceManifestPath))
	bareRepository := filepath.ToSlash(filepath.Join("/tmp/repos", sessionID+".git"))
	return fmt.Sprintf(
		`if [ -f %q ] && [ -f %q ]; then `+
			`ready=$(tr -d '\r\n' < %q); `+
			`bare=$(git --git-dir=%q rev-parse master 2>/dev/null); `+
			`tree=$(git --git-dir=%q rev-parse %q 2>/dev/null); `+
			`checkout=$(git hash-object %q 2>/dev/null); `+
			`printf 'ready:%%s\nbare:%%s\ntree:%%s\ncheckout:%%s\n' "$ready" "$bare" "$tree" "$checkout"; `+
			`exit 0; fi; exit 3`,
		readyFile,
		manifestFile,
		readyFile,
		bareRepository,
		bareRepository,
		expectedCommit+":"+workspaceSourceManifestPath,
		manifestFile,
	)
}

func validateWorkspaceCheckoutProof(output, expectedCommit, expectedManifestBlob string) error {
	fields := make(map[string]string, 4)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || key == "" || value == "" {
			return fmt.Errorf("invalid remote workspace proof line %q", line)
		}
		if _, exists := fields[key]; exists {
			return fmt.Errorf("duplicate remote workspace proof field %q", key)
		}
		switch key {
		case "ready", "bare", "tree", "checkout":
			fields[key] = value
		default:
			return fmt.Errorf("unexpected remote workspace proof field %q", key)
		}
	}
	for _, field := range []string{"ready", "bare", "tree", "checkout"} {
		if fields[field] == "" {
			return fmt.Errorf("remote workspace proof is missing %s", field)
		}
		if !validWorkspaceCommit(fields[field]) {
			return fmt.Errorf("remote workspace proof %s is not a Git object ID", field)
		}
	}
	if fields["ready"] != expectedCommit {
		return fmt.Errorf("remote ready commit mismatch: got %s want %s", fields["ready"], expectedCommit)
	}
	if fields["bare"] != expectedCommit {
		return fmt.Errorf("remote bare master mismatch: got %s want %s", fields["bare"], expectedCommit)
	}
	if fields["tree"] != expectedManifestBlob {
		return fmt.Errorf("remote transport tree manifest mismatch: got %s want %s", fields["tree"], expectedManifestBlob)
	}
	if fields["checkout"] != expectedManifestBlob {
		return fmt.Errorf("remote checkout manifest mismatch: got %s want %s", fields["checkout"], expectedManifestBlob)
	}
	return nil
}

func workspaceReadyCommit(output string) (string, bool) {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ready:") {
			continue
		}
		commit := strings.TrimSpace(strings.TrimPrefix(line, "ready:"))
		return commit, commit != ""
	}
	return "", false
}

func stageSnapshot(src, tmpDir string, extraExcludes []string, dryRun bool) ([]string, error) {
	doopsPatterns, err := loadIgnorePatterns(filepath.Join(src, ".doopsignore"))
	if err != nil {
		return nil, err
	}
	allExcludes := dedupePatterns(append(append([]string{}, defaultExcludes...), extraExcludes...))
	submoduleExcludes, err := gitSubmoduleExcludes(src)
	if err != nil {
		return nil, err
	}
	allExcludes = dedupePatterns(append(allExcludes, submoduleExcludes...))
	candidates, err := walkSourceFiles(src, allExcludes)
	if err != nil {
		return nil, err
	}

	filtered := make([]string, 0, len(candidates))
	for _, rel := range candidates {
		if shouldExcludePath(rel, doopsPatterns) {
			continue
		}
		filtered = append(filtered, rel)
	}

	filtered, err = filterGitIgnored(src, filtered)
	if err != nil {
		return nil, err
	}
	sort.Strings(filtered)

	if dryRun {
		return filtered, nil
	}

	for _, rel := range filtered {
		srcPath := filepath.Join(src, rel)
		dstPath := filepath.Join(tmpDir, rel)
		if err := copySnapshotEntry(srcPath, dstPath); err != nil {
			return nil, fmt.Errorf("复制 %s 失败: %v", rel, err)
		}
	}

	return filtered, nil
}

func loadIgnorePatterns(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取 %s 失败: %v", filepath.Base(path), err)
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("解析 %s 失败: %v", filepath.Base(path), err)
	}
	return dedupePatterns(patterns), nil
}

func dedupePatterns(patterns []string) []string {
	seen := make(map[string]struct{})
	result := make([]string, 0, len(patterns))
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if _, ok := seen[pattern]; ok {
			continue
		}
		seen[pattern] = struct{}{}
		result = append(result, pattern)
	}
	return result
}

func walkSourceFiles(root string, excludes []string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(root, func(current string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current == root {
			return nil
		}

		rel, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		rel = filepath.Clean(rel)

		if shouldExcludePath(rel, excludes) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := info.Mode()
		if mode.IsRegular() || mode&os.ModeSymlink != 0 {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

func gitSubmoduleExcludes(src string) ([]string, error) {
	repoRoot, repoPrefix, ok := detectGitRepo(src)
	if !ok {
		return nil, nil
	}

	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "-z", "--stage")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("读取 Git submodule 路径失败: %v", err)
	}

	var excludes []string
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		metaAndPath := strings.SplitN(record, "\t", 2)
		if len(metaAndPath) != 2 {
			continue
		}
		fields := strings.Fields(metaAndPath[0])
		if len(fields) == 0 || fields[0] != "160000" {
			continue
		}
		rel, ok := repoPathWithinPrefix(filepath.ToSlash(metaAndPath[1]), repoPrefix)
		if !ok || rel == "" || rel == "." {
			continue
		}
		excludes = append(excludes, rel+"/")
	}
	return dedupePatterns(excludes), nil
}

func repoPathWithinPrefix(repoPath, repoPrefix string) (string, bool) {
	repoPath = path.Clean(filepath.ToSlash(strings.TrimSpace(repoPath)))
	repoPrefix = path.Clean(filepath.ToSlash(strings.TrimSpace(repoPrefix)))
	if repoPath == "." {
		return "", false
	}
	if repoPrefix == "." || repoPrefix == "" {
		return repoPath, true
	}
	if repoPath == repoPrefix {
		return ".", true
	}
	prefix := repoPrefix + "/"
	if !strings.HasPrefix(repoPath, prefix) {
		return "", false
	}
	return strings.TrimPrefix(repoPath, prefix), true
}

func filterGitIgnored(src string, relPaths []string) ([]string, error) {
	if len(relPaths) == 0 {
		return relPaths, nil
	}

	repoRoot, repoPrefix, ok := detectGitRepo(src)
	gitDir := ""
	if !ok {
		var err error
		gitDir, err = os.MkdirTemp("", "doops-ignore-")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(gitDir)
		cmd := exec.Command("git", "init", "--bare", "--quiet", gitDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("initialize snapshot ignore context: %w: %s", err, strings.TrimSpace(string(out)))
		}
		repoRoot, repoPrefix = resolvePath(src), ""
	}

	repoPaths := make([]string, 0, len(relPaths))
	relByRepoPath := make(map[string]string, len(relPaths))
	for _, rel := range relPaths {
		repoPath := filepath.ToSlash(rel)
		if repoPrefix != "" {
			repoPath = path.Join(repoPrefix, repoPath)
		}
		repoPaths = append(repoPaths, repoPath)
		relByRepoPath[repoPath] = rel
	}

	ignored, err := gitIgnoredPaths(repoRoot, repoPaths, gitDir)
	if err != nil {
		return nil, err
	}

	filtered := make([]string, 0, len(relPaths))
	for _, repoPath := range repoPaths {
		if ignored[repoPath] {
			continue
		}
		filtered = append(filtered, relByRepoPath[repoPath])
	}
	return filtered, nil
}

func detectGitRepo(src string) (repoRoot string, repoPrefix string, ok bool) {
	cmd := exec.Command("git", "-C", src, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", "", false
	}

	repoRoot = strings.TrimSpace(string(out))
	cleanSrc := strings.TrimRight(src, string(os.PathSeparator))
	// Resolve symlinks on both sides so macOS /tmp vs /private/tmp (and similar
	// alias paths) do not produce a "../../../tmp/..." prefix that breaks
	// git check-ignore --stdin.
	repoRoot = resolvePath(repoRoot)
	cleanSrc = resolvePath(cleanSrc)
	repoPrefix, err = filepath.Rel(repoRoot, cleanSrc)
	if err != nil {
		return "", "", false
	}
	if repoPrefix == "." {
		repoPrefix = ""
	}
	if strings.HasPrefix(repoPrefix, "..") {
		// Source is not inside the detected repo after normalization; skip
		// git-ignore filtering rather than feeding invalid paths to git.
		return "", "", false
	}
	repoPrefix = filepath.ToSlash(repoPrefix)
	return repoRoot, repoPrefix, true
}

func resolvePath(path string) string {
	clean := filepath.Clean(strings.TrimSpace(path))
	if clean == "" {
		return clean
	}
	if resolved, err := filepath.EvalSymlinks(clean); err == nil {
		return resolved
	}
	return clean
}

func gitIgnoredPaths(repoRoot string, repoPaths []string, gitDir string) (map[string]bool, error) {
	if len(repoPaths) == 0 {
		return map[string]bool{}, nil
	}

	args := []string{"-C", repoRoot}
	if gitDir != "" {
		args = append(args, "--git-dir="+gitDir, "--work-tree="+repoRoot)
	}
	args = append(args, "check-ignore", "--stdin", "--verbose", "--non-matching", "-z")
	cmd := exec.Command("git", args...)
	cmd.Stdin = strings.NewReader(strings.Join(repoPaths, "\x00") + "\x00")

	out, err := cmd.CombinedOutput()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("git check-ignore 失败: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	ignored := make(map[string]bool, len(repoPaths))
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(out) == 0 {
		return ignored, nil
	}
	if len(fields)%4 != 0 {
		return nil, fmt.Errorf("invalid git check-ignore response")
	}
	for index := 0; index < len(fields); index += 4 {
		pattern, repoPath := fields[index+2], fields[index+3]
		ignored[filepath.ToSlash(repoPath)] = pattern != "" && !strings.HasPrefix(pattern, "!")
	}

	return ignored, nil
}

func copySnapshotEntry(srcPath, dstPath string) error {
	info, err := os.Lstat(srcPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
		return err
	}

	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(srcPath)
		if err != nil {
			return err
		}
		return os.Symlink(target, dstPath)
	}

	if !info.Mode().IsRegular() {
		return nil
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	dstFile, err := os.OpenFile(dstPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
	if err != nil {
		return err
	}

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		dstFile.Close()
		return err
	}

	if err := dstFile.Close(); err != nil {
		return err
	}
	return os.Chmod(dstPath, info.Mode().Perm())
}

func shouldExcludePath(relPath string, patterns []string) bool {
	if len(patterns) == 0 {
		return false
	}

	normalized := filepath.ToSlash(relPath)
	for _, pattern := range patterns {
		if matchIgnorePattern(normalized, pattern) {
			return true
		}
	}
	return false
}

func matchIgnorePattern(relPath, pattern string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" {
		return false
	}

	anchored := strings.HasPrefix(pattern, "/")
	dirOnly := strings.HasSuffix(pattern, "/")
	pattern = strings.TrimSuffix(pattern, "/")
	pattern = strings.TrimPrefix(pattern, "./")
	pattern = strings.TrimPrefix(pattern, "/")
	if pattern == "" {
		return false
	}

	hasMeta := strings.ContainsAny(pattern, "*?[")
	if strings.Contains(pattern, "/") {
		if matched, _ := path.Match(pattern, relPath); matched {
			return true
		}
		if !hasMeta && (relPath == pattern || strings.HasPrefix(relPath, pattern+"/")) {
			return true
		}
	}
	if anchored {
		if matched, _ := path.Match(pattern, relPath); matched {
			return true
		}
		return !hasMeta && (relPath == pattern || strings.HasPrefix(relPath, pattern+"/"))
	}

	base := path.Base(relPath)
	if matched, _ := path.Match(pattern, base); matched {
		return true
	}

	if !hasMeta {
		for _, segment := range strings.Split(relPath, "/") {
			if segment == pattern {
				return true
			}
		}
		if dirOnly && strings.HasPrefix(relPath, pattern+"/") {
			return true
		}
	}

	return false
}

// Clean 通过 MCP doops_shell 清理远端的挂载工作区及残留裸点仓库
func Clean(client *MCPClient, workspace string) error {
	wsPath := "/root/ws/" + workspace
	repoPath := "/tmp/repos/" + workspace + ".git"

	fmt.Printf("🗑️  清理远端工作区与源仓库: %s, %s\n", wsPath, repoPath)

	// 安全防注入检查
	if workspace == "" || workspace == "/" || strings.Contains(workspace, "..") {
		return fmt.Errorf("不安全的 workspace 名称: %s", workspace)
	}

	err := client.Call("doops_clean_workspace", map[string]interface{}{
		"workspace": workspace,
	})
	if err != nil {
		return fmt.Errorf("清理失败: %v", err)
	}

	return nil
}
