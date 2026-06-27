package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Pull syncs a remote session workspace back to a local directory using the
// same Git transport model as Push. read remains a small-text viewing command.
func Pull(server Server, dest, sessionID string) error {
	if strings.TrimSpace(sessionID) == "" {
		return fmt.Errorf("session ID 必传：请通过 -session 指定要拉取的远端工作区")
	}
	if strings.TrimSpace(dest) == "" {
		dest = sessionID
	}
	absDest, err := filepath.Abs(dest)
	if err != nil {
		return fmt.Errorf("解析目标路径失败: %v", err)
	}

	token := ResolveToken(server.Name, server.Token)
	gitURL, err := buildGitRemoteURLForServer(server, sessionID, token)
	if err != nil {
		return err
	}

	fmt.Printf("📦 正在让远端工作区生成 Git 快照: /root/ws/%s\n", sessionID)
	client := NewMCPClient(server, NewSessionStore(), sessionID, false)
	client.Token = token
	commit, err := client.CallAndCapture("doops_shell", map[string]interface{}{
		"command": buildGitWorkspaceCommitCommand(sessionID),
	})
	client.Close()
	if err != nil {
		return fmt.Errorf("远端工作区生成 Git 快照失败: %v", err)
	}
	commit = strings.TrimSpace(commit)
	if commit != "" {
		fmt.Printf("✅ 远端快照已生成: %s\n", firstLine(commit))
	}

	if err := pullGitWorkspace(gitURL, absDest); err != nil {
		return err
	}
	fmt.Printf("✅ 已拉取远端工作区到本地: %s\n", absDest)
	return nil
}

func buildGitWorkspaceCommitCommand(sessionID string) string {
	wsPath := "/root/ws/" + sessionID
	repoPath := "/tmp/repos/" + sessionID + ".git"
	return strings.Join([]string{
		"set -e",
		"test -d " + shellQuote(wsPath),
		"mkdir -p /tmp/repos",
		"if [ ! -d " + shellQuote(repoPath) + " ]; then git init --bare " + shellQuote(repoPath) + " >/dev/null; fi",
		"git --git-dir " + shellQuote(repoPath) + " symbolic-ref HEAD refs/heads/master",
		"cd " + shellQuote(wsPath),
		"if [ ! -d .git ]; then git init -b master >/dev/null; fi",
		"git config user.name doops",
		"git config user.email doops@localhost",
		"git add -A",
		"git commit --allow-empty -m 'DoopsPullSnapshot' >/dev/null",
		"git push -f " + shellQuote(repoPath) + " HEAD:master >/dev/null",
		"git rev-parse HEAD",
	}, "\n")
}

func pullGitWorkspace(gitURL, dest string) error {
	if info, err := os.Stat(dest); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("目标路径已存在且不是目录: %s", dest)
		}
		if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
			fmt.Printf("🔄 本地目录已存在，执行 Git fetch/reset: %s\n", dest)
			if err := runGit(dest, "fetch", gitURL, "master"); err != nil {
				return err
			}
			return runGit(dest, "reset", "--hard", "FETCH_HEAD")
		}
		entries, err := os.ReadDir(dest)
		if err != nil {
			return fmt.Errorf("读取目标目录失败: %v", err)
		}
		if len(entries) > 0 {
			return fmt.Errorf("目标目录已存在且非空，也不是 Git 仓库: %s", dest)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查目标路径失败: %v", err)
	}

	fmt.Printf("⬇️  正在 Git clone 远端工作区到: %s\n", dest)
	return runGit("", "clone", "--branch", "master", gitURL, dest)
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %v failed: %v", redactGitArgs(args), err)
	}
	return nil
}

func firstLine(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 {
		return strings.TrimSpace(s[:idx])
	}
	return strings.TrimSpace(s)
}
