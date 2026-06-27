package main

import (
	"bufio"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
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
	"target",
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
	// 1. 解析源目录
	src, err := filepath.Abs(src)
	if err != nil {
		return fmt.Errorf("解析源路径失败: %v", err)
	}

	info, err := os.Stat(src)
	if err != nil {
		return fmt.Errorf("源路径不存在: %s", src)
	}
	if !info.IsDir() {
		return fmt.Errorf("源路径必须是目录: %s", src)
	}

	if !strings.HasSuffix(src, "/") {
		src += "/"
	}

	// 优先级：显式 sessionID > dest 推导
	if sessionID == "" && dest != "" {
		clean := strings.TrimRight(dest, "/")
		sessionID = filepath.Base(clean)
	}
	if sessionID == "" {
		return fmt.Errorf("session ID 必传：请通过 -session 指定会话 ID 以隔离工作区 (例如: doops exec -session prod ...)")
	}

	token := ResolveToken(server.Name, server.Token)
	gitURL, err := buildGitRemoteURLForServer(server, sessionID, token)
	if err != nil {
		return err
	}
	workspacePath := fmt.Sprintf("/root/ws/%s", sessionID)

	// 2. 建立本地幽灵沙盒
	tmpDir := filepath.Join(os.TempDir(), "doops-sync-"+sessionID)
	os.RemoveAll(tmpDir) // 清理上次可能由于强杀遗留的
	if err := os.MkdirAll(tmpDir, 0755); err != nil {
		return fmt.Errorf("创建幽灵临时目录失败: %v", err)
	}
	// 进程退出时挫骨扬灰，实现真正的无痕搬运
	defer os.RemoveAll(tmpDir)

	start := time.Now()
	fmt.Printf("🧊 正在构建本地隔离快照（大型仓库首次扫描可能较慢）...\n")
	stagedFiles, err := stageSnapshot(src, tmpDir, extraExcludes, dryRun)
	if err != nil {
		return fmt.Errorf("本地缓存集结失败: %v", err)
	}

	if dryRun {
		fmt.Printf("📋 [DRY-RUN] 预计推送 %d 个文件到远端工作区 %s\n", len(stagedFiles), workspacePath)
		for _, rel := range stagedFiles {
			fmt.Println(rel)
		}
		return nil
	}

	// 4. 在幽灵目录中进行纯净 Git 化与差分推送
	fmt.Printf("📦 本地隔离快照整理完成: %d 个文件\n", len(stagedFiles))
	fmt.Printf("🚀 进行极速无感增量推送至远端工作区: %s (%s)\n", workspacePath, server.Name)
	fmt.Printf("🔧 正在初始化幽灵 Git 仓库并生成同步提交...\n")

	gitCmds := [][]string{
		{"init", "-b", "master"}, // 规避老版本默认 master 警告并强制契合服务端空 bare 库的默认 HEAD
		{"add", "."},
		{"-c", "user.name=doops", "-c", "user.email=doops@localhost", "commit", "--allow-empty", "-m", "AutoSync"},
		{"push", "-f", gitURL, "HEAD:master"},
	}

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
			cmd := exec.Command("git", gargs...)
			cmd.Dir = tmpDir

			// 我们只让最后一步 push 原样输出网络传输进度给用户
			if gargs[0] == "push" {
				cmd.Stdout = os.Stdout
				cmd.Stderr = os.Stderr
				if attempt > 1 {
					fmt.Printf("⚠️ 网络异常或超时，正在进行第 %d 次重试推送...\n", attempt)
					time.Sleep(2 * time.Second)
				}
			}

			if err := cmd.Run(); err != nil {
				lastErr = err
				if attempt < maxRetries && gargs[0] == "push" {
					continue // 准备重推
				}
				return fmt.Errorf("幽灵传输阶段执行失败 (%v): %v", redactGitArgs(gargs), lastErr)
			}
			break // 成功则跳出重试循环
		}
	}

	localCommit, err := gitCommandOutput(tmpDir, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("读取本地幽灵提交失败: %v", err)
	}
	localCommit = strings.TrimSpace(localCommit)

	fmt.Printf("🔍 正在确认远端工作区已完成 checkout 并就绪...\n")
	representativeRel := ""
	if len(stagedFiles) > 0 {
		representativeRel = stagedFiles[0]
	}
	if err := waitForWorkspaceReady(server, token, sessionID, workspacePath, representativeRel, localCommit, 45*time.Second); err != nil {
		return fmt.Errorf("push 已完成，但远端工作区未确认就绪: %v", err)
	}

	elapsed := time.Since(start)
	shortCommit := localCommit
	if len(shortCommit) > 12 {
		shortCommit = shortCommit[:12]
	}
	fmt.Printf("✅ 同步完成 (%s) 🎯 远端工作区已就绪: %s @ %s\n", elapsed.Round(time.Millisecond), workspacePath, shortCommit)
	return nil
}

func buildGitRemoteURL(host, port, sessionID, token string) string {
	remote := url.URL{
		Scheme: "http",
		Host:   fmt.Sprintf("%s:%s", host, port),
		Path:   fmt.Sprintf("/git/%s.git", sessionID),
	}
	if token != "" {
		remote.User = url.UserPassword("doops", token)
	}
	return remote.String()
}

func buildGitRemoteURLForServer(server Server, sessionID, token string) (string, error) {
	if strings.TrimSpace(server.Gateway) == "" {
		port := server.Port
		if port == "" {
			port = "42222"
		}
		return buildGitRemoteURL(server.IP, port, sessionID, token), nil
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
	if token != "" {
		u.User = url.UserPassword("doops", token)
	}
	cluster := strings.TrimSpace(server.Cluster)
	if cluster == "" {
		cluster = "default"
	}
	instance := strings.TrimSpace(server.Instance)
	if instance == "" {
		instance = server.Name
	}
	u.Path = path.Join(u.Path, "v1", "git", cluster, instance, sessionID+".git")
	u.RawQuery = ""
	return u.String(), nil
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

func waitForWorkspaceReady(server Server, token string, sessionID string, workspacePath string, representativeRel string, expectedCommit string, timeout time.Duration) error {
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
		cmd := fmt.Sprintf("if [ -f %q ]; then printf 'ready:'; tr -d '\\r\\n' < %q; exit 0; fi", readyFile, readyFile)
		cmd += "; exit 3"
		stdout, err := mcpClient.CallAndCapture("doops_shell", map[string]interface{}{
			"command": cmd,
		})
		lastOut = strings.TrimSpace(stdout)
		lastErr = ""
		if err == nil {
			if strings.HasPrefix(lastOut, "ready:") {
				remoteCommit := strings.TrimSpace(strings.TrimPrefix(lastOut, "ready:"))
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

func stageSnapshot(src, tmpDir string, extraExcludes []string, dryRun bool) ([]string, error) {
	doopsPatterns, err := loadIgnorePatterns(filepath.Join(src, ".doopsignore"))
	if err != nil {
		return nil, err
	}
	gitPatterns, err := loadIgnorePatterns(filepath.Join(src, ".gitignore"))
	if err != nil {
		return nil, err
	}

	allExcludes := dedupePatterns(append(append([]string{}, defaultExcludes...), extraExcludes...))
	candidates, err := walkSourceFiles(src, allExcludes)
	if err != nil {
		return nil, err
	}

	filtered := make([]string, 0, len(candidates))
	for _, rel := range candidates {
		if shouldExcludePath(rel, doopsPatterns) || shouldExcludePath(rel, gitPatterns) {
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

func filterGitIgnored(src string, relPaths []string) ([]string, error) {
	if len(relPaths) == 0 {
		return relPaths, nil
	}

	repoRoot, repoPrefix, ok := detectGitRepo(src)
	if !ok {
		return relPaths, nil
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

	ignored, err := gitIgnoredPaths(repoRoot, repoPaths)
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
	repoPrefix, err = filepath.Rel(repoRoot, cleanSrc)
	if err != nil {
		return "", "", false
	}
	if repoPrefix == "." {
		repoPrefix = ""
	}
	repoPrefix = filepath.ToSlash(repoPrefix)
	return repoRoot, repoPrefix, true
}

func gitIgnoredPaths(repoRoot string, repoPaths []string) (map[string]bool, error) {
	if len(repoPaths) == 0 {
		return map[string]bool{}, nil
	}

	cmd := exec.Command("git", "-C", repoRoot, "check-ignore", "--stdin", "--verbose", "--non-matching", "--no-index")
	cmd.Stdin = strings.NewReader(strings.Join(repoPaths, "\n") + "\n")

	out, err := cmd.CombinedOutput()
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 1 {
			return nil, fmt.Errorf("git check-ignore 失败: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}

	ignored := make(map[string]bool, len(repoPaths))
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		repoPath := filepath.ToSlash(strings.TrimSpace(parts[1]))
		ignored[repoPath] = !strings.HasPrefix(parts[0], "::")
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("解析 git check-ignore 输出失败: %v", err)
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
