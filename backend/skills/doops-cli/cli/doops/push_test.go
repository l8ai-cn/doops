package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestStageSnapshotIncludesBuildDirectory(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "build", "issue-9-k8s"), 0755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "build", "issue-9-k8s", "pod.yaml"), []byte("apiVersion: v1\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	files, err := stageSnapshot(src, filepath.Join(root, "tmp"), nil, true)
	if err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	if !containsString(files, filepath.Join("build", "issue-9-k8s", "pod.yaml")) {
		t.Fatalf("expected build manifest to be included, got %#v", files)
	}
}

func TestCICDSourceExcludesDropBulkyTrees(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	for _, rel := range []string{
		"deploy/test/values.yaml",
		"ops/cicd/zhiyong.deploy.yaml",
		"zhiyong-frontend/package.json",
		".codex-work/scu-datasets/big.tar.gz.b64",
		"tools/heavy.bin",
		"docs/readme.md",
	} {
		path := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte("x\n"), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}

	files, err := stageSnapshot(src, filepath.Join(root, "tmp"), cicdSourceExcludes, true)
	if err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	for _, want := range []string{
		filepath.Join("deploy", "test", "values.yaml"),
		filepath.Join("ops", "cicd", "zhiyong.deploy.yaml"),
		filepath.Join("zhiyong-frontend", "package.json"),
	} {
		if !containsString(files, want) {
			t.Fatalf("expected %s kept, got %#v", want, files)
		}
	}
	for _, f := range files {
		if strings.Contains(f, ".codex-work") || strings.HasPrefix(f, "tools"+string(os.PathSeparator)) || strings.HasPrefix(f, "docs"+string(os.PathSeparator)) {
			t.Fatalf("expected bulky tree excluded, got %#v", files)
		}
	}
}

func TestDetectGitRepoNormalizesTmpAliasPaths(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS /tmp -> /private/tmp alias only")
	}
	src, err := os.MkdirTemp("/tmp", "doops-detect-git-")
	if err != nil {
		t.Fatalf("mkdir temp under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(src) })

	cmd := exec.Command("git", "init", "-b", "main")
	cmd.Dir = src
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("ok\n"), 0644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	// Pass the /tmp form even though git reports /private/tmp as toplevel.
	repoRoot, repoPrefix, ok := detectGitRepo(src)
	if !ok {
		t.Fatalf("detectGitRepo failed for %s", src)
	}
	if repoPrefix != "" {
		t.Fatalf("expected empty prefix for repo root, got %q (root=%q src=%q)", repoPrefix, repoRoot, src)
	}
	if strings.Contains(repoPrefix, "..") {
		t.Fatalf("prefix must not escape repo: %q", repoPrefix)
	}

	filtered, err := filterGitIgnored(src+"/", []string{"README.md", ".agents/skills/edu-solid-geometry/SKILL.md"})
	if err != nil {
		t.Fatalf("filterGitIgnored: %v", err)
	}
	if !containsString(filtered, "README.md") {
		t.Fatalf("expected README.md kept, got %#v", filtered)
	}
}

func TestBuildGitRemoteURLForGatewayTarget(t *testing.T) {
	server := Server{
		Name:     "oilan",
		Gateway:  "https://gateway.example.com:42222",
		Cluster:  "doops-oilan",
		Instance: "oilan-node",
	}

	got, err := buildGitRemoteURLForServer(server, "deploy-main", "secret-token")
	if err != nil {
		t.Fatalf("build gateway git URL: %v", err)
	}
	want := "https://doops:secret-token@gateway.example.com:42222/v1/git/doops-oilan/oilan-node/deploy-main.git"
	if got != want {
		t.Fatalf("gateway git URL mismatch:\nwant %s\n got %s", want, got)
	}
}

func TestPrebuiltCLIUsesGitSyncPushBackend(t *testing.T) {
	bin := filepath.Join("..", "..", "bin", "doops-"+runtime.GOOS+"-"+runtime.GOARCH)
	assertBinaryUsesGitSyncPushBackend(t, bin)
}

func TestBuiltCLIUsesGitSyncPushBackend(t *testing.T) {
	out := filepath.Join(t.TempDir(), "doops")
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, "./")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build doops cli: %v\n%s", err, output)
	}
	assertBinaryUsesGitSyncPushBackend(t, out)
}

func assertBinaryUsesGitSyncPushBackend(t *testing.T, bin string) {
	t.Helper()
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("stat %s: %v", bin, err)
	}
	output, err := exec.Command("strings", bin).Output()
	if err != nil {
		t.Fatalf("strings %s: %v", bin, err)
	}
	text := string(output)
	disallowed := []string{
		"pushArchiveViaGateway",
		"doops_workspace_begin",
		"doops_workspace_chunk",
		"doops_workspace_commit",
	}
	for _, marker := range disallowed {
		if strings.Contains(text, marker) {
			t.Fatalf("%s contains disallowed archive/chunk push marker %q", bin, marker)
		}
	}
	required := []string{
		"git push -f",
		"doops-sync-",
		"AutoSync",
		"/git/%s.git",
	}
	for _, marker := range required {
		if !strings.Contains(text, marker) {
			t.Fatalf("%s is missing required git sync marker %q", bin, marker)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
