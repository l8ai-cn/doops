package server

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type GitRepoTestResult struct {
	Branch string
	Ref    string
}

func (s *GatewayStore) TestGitRepoConnection(ctx context.Context, repo GitRepo) (GitRepoTestResult, error) {
	branch := strings.TrimSpace(repo.Branch)
	if branch == "" {
		branch = "main"
	}
	password, err := s.GitRepoPassword(repo.ID)
	if err != nil {
		return GitRepoTestResult{}, err
	}
	return gitLsRemote(ctx, repo.URL, branch, repo.Username, password)
}

func gitLsRemote(ctx context.Context, repoURL, branch, username, password string) (GitRepoTestResult, error) {
	repoURL = strings.TrimSpace(repoURL)
	branch = strings.TrimSpace(branch)
	if repoURL == "" {
		return GitRepoTestResult{}, fmt.Errorf("repo url is required")
	}
	if branch == "" {
		return GitRepoTestResult{}, fmt.Errorf("repo branch is required")
	}
	if _, err := exec.LookPath("git"); err != nil {
		return GitRepoTestResult{}, fmt.Errorf("git executable not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "ls-remote", "--heads", repoURL, "refs/heads/"+branch)
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	if needsAskPass(repoURL, password) {
		askpass, err := writeGitAskPass(username, password)
		if err != nil {
			return GitRepoTestResult{}, err
		}
		defer os.RemoveAll(filepath.Dir(askpass))
		cmd.Env = append(cmd.Env, "GIT_ASKPASS="+askpass)
		if runtime.GOOS != "windows" {
			cmd.Env = append(cmd.Env, "SSH_ASKPASS="+askpass)
		}
	}
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return GitRepoTestResult{}, fmt.Errorf("git ls-remote timed out")
	}
	if err != nil {
		return GitRepoTestResult{}, fmt.Errorf("git ls-remote failed: %s", strings.TrimSpace(string(out)))
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return GitRepoTestResult{}, fmt.Errorf("branch %q was not found", branch)
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return GitRepoTestResult{}, fmt.Errorf("unexpected git ls-remote output: %s", line)
	}
	return GitRepoTestResult{Branch: branch, Ref: fields[0]}, nil
}

func needsAskPass(repoURL, password string) bool {
	if strings.TrimSpace(password) == "" {
		return false
	}
	u, err := url.Parse(repoURL)
	if err != nil {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}

func writeGitAskPass(username, password string) (string, error) {
	dir, err := os.MkdirTemp("", "doops-git-askpass-")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "askpass.sh")
	user := shellSingleQuote(strings.TrimSpace(username))
	pass := shellSingleQuote(password)
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"*Username*) printf %s " + user + " ;;\n" +
		"*) printf %s " + pass + " ;;\n" +
		"esac\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return path, nil
}

func shellSingleQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", "'\\''") + "'"
}
