package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildGitWorkspaceCommitCommand(t *testing.T) {
	got := buildGitWorkspaceCommitCommand("release-1")
	for _, want := range []string{
		"cd '/root/ws/release-1'",
		"git init -b master",
		"git --git-dir '/tmp/repos/release-1.git' symbolic-ref HEAD refs/heads/master",
		"git add -A",
		"git commit --allow-empty -m 'DoopsPullSnapshot'",
		"git push -f '/tmp/repos/release-1.git' HEAD:master",
	} {
		if !contains(got, want) {
			t.Fatalf("command missing %q\n%s", want, got)
		}
	}
}

func TestPullGitWorkspaceClonesFromBundle(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "course.tgz"), []byte("course data"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	runTestGit(t, src, "init", "-b", "master")
	runTestGit(t, src, "config", "user.name", "doops")
	runTestGit(t, src, "config", "user.email", "doops@localhost")
	runTestGit(t, src, "add", "-A")
	runTestGit(t, src, "commit", "-m", "snapshot")
	bundle := filepath.Join(root, "workspace.bundle")
	runTestGit(t, src, "bundle", "create", bundle, "master")

	dest := filepath.Join(root, "dest")
	if err := pullGitWorkspace(bundle, dest); err != nil {
		t.Fatalf("pull bundle: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dest, "course.tgz"))
	if err != nil {
		t.Fatalf("read pulled file: %v", err)
	}
	if string(got) != "course data" {
		t.Fatalf("pulled content mismatch: %q", got)
	}
}

func runTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
