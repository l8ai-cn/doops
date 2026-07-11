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

func TestCICDSourceExcludesKeepRequiredBuildInputs(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	for _, rel := range []string{
		"docs/readme.md",
		"output/staged.json",
		"zhiyong-flow/web/src/index.ts",
		"zhiyong-lab-api/docs/docs.go",
		"zhiyong-frontend/src/common/components/notebook/components/output/OutputArea.tsx",
		"deploy/tools/environments/test/middleware/values.yaml",
		"ops/cicd/tools/build_release_images.py",
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
		filepath.Join("zhiyong-flow", "web", "src", "index.ts"),
		filepath.Join("zhiyong-lab-api", "docs", "docs.go"),
		filepath.Join("zhiyong-frontend", "src", "common", "components", "notebook", "components", "output", "OutputArea.tsx"),
		filepath.Join("deploy", "tools", "environments", "test", "middleware", "values.yaml"),
		filepath.Join("ops", "cicd", "tools", "build_release_images.py"),
	} {
		if !containsString(files, want) {
			t.Fatalf("expected required build input %s kept, got %#v", want, files)
		}
	}
	if containsString(files, filepath.Join("docs", "readme.md")) {
		t.Fatalf("expected repository documentation tree excluded, got %#v", files)
	}
	if containsString(files, filepath.Join("output", "staged.json")) {
		t.Fatalf("expected root output tree excluded, got %#v", files)
	}
}

func TestStageSnapshotUsesGitIgnoreSemanticsAndKeepsTrackedFiles(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(filepath.Join(src, "frontend", "src", "common", "ecp"), 0755); err != nil {
		t.Fatalf("mkdir fixture: %v", err)
	}
	runTestGit(t, src, "init", "-b", "main")
	runTestGit(t, src, "config", "user.name", "doops")
	runTestGit(t, src, "config", "user.email", "doops@localhost")

	writeFixture := func(rel, content string) {
		t.Helper()
		path := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatalf("mkdir %s: %v", rel, err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	writeFixture(".gitignore", "/ecp\n*.log\n")
	writeFixture("frontend/src/common/ecp/contract.test.ts", "export {};\n")
	writeFixture("tracked.log", "tracked release evidence\n")
	runTestGit(t, src, "add", ".gitignore", "frontend/src/common/ecp/contract.test.ts")
	runTestGit(t, src, "add", "-f", "tracked.log")
	runTestGit(t, src, "commit", "-m", "fixture")
	writeFixture("untracked.log", "local only\n")

	files, err := stageSnapshot(src, filepath.Join(root, "tmp"), cicdSourceExcludes, true)
	if err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	for _, want := range []string{
		filepath.Join("frontend", "src", "common", "ecp", "contract.test.ts"),
		"tracked.log",
	} {
		if !containsString(files, want) {
			t.Fatalf("expected tracked file %q to be included, got %#v", want, files)
		}
	}
	if containsString(files, "untracked.log") {
		t.Fatalf("expected ignored untracked file to be excluded, got %#v", files)
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
