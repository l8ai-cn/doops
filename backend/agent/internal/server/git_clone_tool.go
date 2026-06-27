package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/user/doops/agent/api"
)

func handleGitClone(raw json.RawMessage) (string, error) {
	var args api.GitCloneParams
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid git clone request")
	}
	sessionID := strings.TrimSpace(args.SessionID)
	if err := validateSession(sessionID); err != nil {
		return "", err
	}
	repoURL := strings.TrimSpace(args.URL)
	if repoURL == "" {
		return "", fmt.Errorf("repo url is required")
	}
	branch := strings.TrimSpace(args.Branch)
	if branch == "" {
		branch = "main"
	}
	dirName := strings.TrimSpace(args.Directory)
	if dirName == "" {
		dirName = "repo"
	}
	if strings.Contains(dirName, "/") || strings.Contains(dirName, "\\") || dirName == "." || dirName == ".." {
		return "", fmt.Errorf("directory must be a simple name")
	}
	workspace, err := workspacePath(sessionID)
	if err != nil {
		return "", err
	}
	dest := filepath.Join(workspace, dirName)
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("destination already exists: %s", dirName)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git executable not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--branch", branch, repoURL, dest)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if strings.TrimSpace(args.Password) != "" && needsAskPass(repoURL, args.Password) {
		askpass, err := writeGitAskPass(args.Username, args.Password)
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(filepath.Dir(askpass))
		cmd.Env = append(cmd.Env, "GIT_ASKPASS="+askpass)
		if runtime.GOOS != "windows" {
			cmd.Env = append(cmd.Env, "SSH_ASKPASS="+askpass)
		}
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git clone timed out")
	}
	if err != nil {
		return "", fmt.Errorf("git clone failed: %s", strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("cloned %s branch %s into %s", repoURL, branch, dest), nil
}
