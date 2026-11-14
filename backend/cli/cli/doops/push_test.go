package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestWriteWorkspaceSourceManifestBindsImmutableRevision(t *testing.T) {
	root := t.TempDir()
	source := &WorkspaceSourceIdentity{
		Repository: "https://github.com/example/example.git",
		Revision:   strings.Repeat("a", 40),
		Branch:     "main",
	}

	rel, err := writeWorkspaceSourceManifest(root, source)
	if err != nil {
		t.Fatalf("write source manifest: %v", err)
	}
	if rel != ".doops/source.json" {
		t.Fatalf("unexpected source manifest path %q", rel)
	}
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read source manifest: %v", err)
	}
	var manifest workspaceSourceManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("decode source manifest: %v", err)
	}
	if manifest.APIVersion != workspaceManifestAPIVersion || manifest.Kind != "WorkspaceSource" {
		t.Fatalf("unexpected source manifest type: %#v", manifest)
	}
	if manifest.Source != *source {
		t.Fatalf("source manifest must preserve the declared immutable identity: %#v", manifest)
	}
}

func TestWriteWorkspaceSourceManifestRejectsInvalidBranchWithoutTrimming(t *testing.T) {
	for _, test := range []struct {
		branch string
		valid  bool
	}{
		{branch: "", valid: false},
		{branch: " main", valid: false},
		{branch: "main ", valid: false},
		{branch: "main", valid: true},
	} {
		t.Run(fmt.Sprintf("%q", test.branch), func(t *testing.T) {
			root := t.TempDir()
			rel, err := writeWorkspaceSourceManifest(root, &WorkspaceSourceIdentity{
				Repository: "https://github.com/example/example.git",
				Revision:   strings.Repeat("a", 40),
				Branch:     test.branch,
			})
			if test.valid {
				if err != nil {
					t.Fatalf("source manifest branch %q must be accepted: %v", test.branch, err)
				}
				if rel != workspaceSourceManifestPath {
					t.Fatalf("unexpected source manifest path %q", rel)
				}
				return
			}
			if err == nil {
				t.Fatalf("source manifest branch %q must be rejected", test.branch)
			}
			if rel != "" {
				t.Fatalf("rejected source branch must not return a manifest path: %q", rel)
			}
			if _, statErr := os.Lstat(filepath.Join(root, filepath.FromSlash(workspaceSourceManifestPath))); !os.IsNotExist(statErr) {
				t.Fatalf("rejected source branch must not generate a manifest, stat error: %v", statErr)
			}
		})
	}
}

func TestPushWorkspaceSnapshotRejectsInvalidBranchBeforeRemotePush(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests++
	}))
	defer server.Close()

	_, err := pushWorkspaceSnapshotWithSource(
		Server{Name: "test", Gateway: server.URL},
		t.TempDir(),
		"",
		false,
		nil,
		"invalid-branch",
		&WorkspaceSourceIdentity{
			Repository: "https://github.com/example/example.git",
			Revision:   strings.Repeat("a", 40),
			Branch:     "main ",
		},
	)
	if err == nil {
		t.Fatal("invalid source branch must be rejected")
	}
	if requests != 0 {
		t.Fatalf("invalid source branch must fail before remote push, got %d requests", requests)
	}
}

func TestStageWorkspaceSnapshotCommitsSourceManifestWithoutDirtyingSource(t *testing.T) {
	sourceRoot := t.TempDir()
	runTestGit(t, sourceRoot, "init", "-b", "main")
	runTestGit(t, sourceRoot, "config", "user.name", "doops")
	runTestGit(t, sourceRoot, "config", "user.email", "doops@localhost")
	if err := os.WriteFile(filepath.Join(sourceRoot, "README.md"), []byte("release\n"), 0o644); err != nil {
		t.Fatalf("write source fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sourceRoot, ".gitignore"), []byte(".doops/\n"), 0o644); err != nil {
		t.Fatalf("write source gitignore: %v", err)
	}
	runTestGit(t, sourceRoot, "add", "README.md", ".gitignore")
	runTestGit(t, sourceRoot, "commit", "-m", "release")
	revisionBytes, err := exec.Command("git", "-C", sourceRoot, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatalf("resolve source revision: %v", err)
	}
	source := &WorkspaceSourceIdentity{
		Repository: "https://github.com/example/example.git",
		Revision:   strings.TrimSpace(string(revisionBytes)),
		Branch:     "main",
	}
	snapshotRoot := t.TempDir()

	files, err := stageWorkspaceSnapshot(sourceRoot, snapshotRoot, nil, false, source)
	if err != nil {
		t.Fatalf("stage workspace snapshot: %v", err)
	}
	if !containsString(files, workspaceSourceManifestPath) {
		t.Fatalf("source manifest must be part of the staged snapshot: %#v", files)
	}
	commit, err := createWorkspaceSnapshotCommit(snapshotRoot, source)
	if err != nil {
		t.Fatalf("create transport snapshot commit: %v", err)
	}
	tree, err := exec.Command(
		"git",
		"-C",
		snapshotRoot,
		"ls-tree",
		"--name-only",
		commit,
		workspaceSourceManifestPath,
	).Output()
	if err != nil {
		t.Fatalf("inspect snapshot commit: %v", err)
	}
	if strings.TrimSpace(string(tree)) != workspaceSourceManifestPath {
		t.Fatalf("source manifest must be committed in the transport snapshot: %q", tree)
	}
	status, err := exec.Command("git", "-C", sourceRoot, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("inspect source worktree: %v", err)
	}
	if strings.TrimSpace(string(status)) != "" {
		t.Fatalf("snapshot metadata must not dirty the source worktree: %s", status)
	}
}

func TestValidateWorkspaceCheckoutProofRequiresExactManifestBlob(t *testing.T) {
	commit := strings.Repeat("a", 40)
	manifestBlob := strings.Repeat("b", 40)
	tests := []struct {
		name  string
		proof string
		want  bool
	}{
		{
			name: "matching bare tree and checkout",
			proof: "ready:" + commit + "\n" +
				"bare:" + commit + "\n" +
				"tree:" + manifestBlob + "\n" +
				"checkout:" + manifestBlob + "\n",
		},
		{
			name: "bare branch differs",
			proof: "ready:" + commit + "\n" +
				"bare:" + strings.Repeat("c", 40) + "\n" +
				"tree:" + manifestBlob + "\n" +
				"checkout:" + manifestBlob + "\n",
			want: true,
		},
		{
			name: "checkout omits source manifest",
			proof: "ready:" + commit + "\n" +
				"bare:" + commit + "\n" +
				"tree:" + manifestBlob + "\n",
			want: true,
		},
		{
			name: "checkout manifest differs from transport tree",
			proof: "ready:" + commit + "\n" +
				"bare:" + commit + "\n" +
				"tree:" + manifestBlob + "\n" +
				"checkout:" + strings.Repeat("c", 40) + "\n",
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateWorkspaceCheckoutProof(test.proof, commit, manifestBlob)
			if test.want && err == nil {
				t.Fatal("invalid remote workspace proof must be rejected")
			}
			if !test.want && err != nil {
				t.Fatalf("matching remote workspace proof rejected: %v", err)
			}
		})
	}
}

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

func TestStageSnapshotKeepsNestedSourceDirectoryNamedTarget(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatalf("mkdir source root: %v", err)
	}
	runTestGit(t, src, "init", "-b", "main")
	runTestGit(t, src, "config", "user.name", "doops")
	runTestGit(t, src, "config", "user.email", "doops@localhost")

	relative := filepath.Join(
		"ecp-backend",
		"ecp-authz",
		"src",
		"main",
		"java",
		"cn",
		"l8ai",
		"authz",
		"service",
		"target",
		"TargetPermissionSystemService.java",
	)
	path := filepath.Join(src, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatalf("mkdir source package: %v", err)
	}
	if err := os.WriteFile(path, []byte("package target;\n"), 0644); err != nil {
		t.Fatalf("write source package: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, ".gitignore"), []byte("target/\n"), 0644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	buildOutput := filepath.Join("ecp-backend", "ecp-authz", "target", "classes", "App.class")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(src, buildOutput)), 0755); err != nil {
		t.Fatalf("mkdir build output: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, buildOutput), []byte("bytecode\n"), 0644); err != nil {
		t.Fatalf("write build output: %v", err)
	}
	runTestGit(t, src, "add", ".gitignore")
	runTestGit(t, src, "add", "-f", relative)
	runTestGit(t, src, "commit", "-m", "fixture")

	files, err := stageSnapshot(src, filepath.Join(root, "tmp"), nil, true)
	if err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	if !containsString(files, relative) {
		t.Fatalf("source directory named target must not be excluded: %#v", files)
	}
	if containsString(files, buildOutput) {
		t.Fatalf("ignored Maven build output must not be included: %#v", files)
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

	files, err := stageSnapshot(src, filepath.Join(root, "tmp"), nil, true)
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

func TestStageSnapshotHonorsGitIgnoreNegationForNonGitSources(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(src, rel), []byte(content), 0644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write(".gitignore", "settings.json\n!/settings.json\nprivate.key\n*.log\n!keep.log\n*.log\n")
	write("settings.json", `{"provider":{}}`)
	write("private.key", "synthetic-private-canary")
	write("keep.log", "ignored again by the last rule")

	snapshot := filepath.Join(root, "snapshot")
	files, err := stageSnapshot(src, snapshot, nil, false)
	if err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	if !containsString(files, "settings.json") {
		t.Fatalf("negated contract file was excluded: %#v", files)
	}
	for _, excluded := range []string{"private.key", "keep.log"} {
		if containsString(files, excluded) {
			t.Fatalf("ignored file %s was unexpectedly included: %#v", excluded, files)
		}
	}
	if _, err := createWorkspaceSnapshotCommit(snapshot, nil); err != nil {
		t.Fatal(err)
	}
	got, err := gitCommandOutput(snapshot, "show", "HEAD:settings.json")
	if err != nil || !strings.Contains(got, `"provider"`) {
		t.Fatalf("transport commit omitted the build contract: %s, %v", got, err)
	}
}

func TestStageSnapshotSkipsGitSubmodulesBeforeGitIgnoreFiltering(t *testing.T) {
	root := t.TempDir()
	submoduleSrc := filepath.Join(root, "submodule-src")
	if err := os.MkdirAll(submoduleSrc, 0755); err != nil {
		t.Fatalf("mkdir submodule source: %v", err)
	}
	runTestGit(t, submoduleSrc, "init", "-b", "main")
	runTestGit(t, submoduleSrc, "config", "user.name", "doops")
	runTestGit(t, submoduleSrc, "config", "user.email", "doops@localhost")
	if err := os.WriteFile(filepath.Join(submoduleSrc, "nested.txt"), []byte("submodule\n"), 0644); err != nil {
		t.Fatalf("write submodule fixture: %v", err)
	}
	runTestGit(t, submoduleSrc, "add", "nested.txt")
	runTestGit(t, submoduleSrc, "commit", "-m", "submodule fixture")

	src := filepath.Join(root, "src")
	if err := os.MkdirAll(src, 0755); err != nil {
		t.Fatalf("mkdir source: %v", err)
	}
	runTestGit(t, src, "init", "-b", "main")
	runTestGit(t, src, "config", "user.name", "doops")
	runTestGit(t, src, "config", "user.email", "doops@localhost")
	runTestGit(t, src, "-c", "protocol.file.allow=always", "submodule", "add", submoduleSrc, "modules/vendor-lib")
	if err := os.MkdirAll(filepath.Join(src, "modules", "ordinary-lib"), 0755); err != nil {
		t.Fatalf("mkdir ordinary fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "modules", "ordinary-lib", "keep.txt"), []byte("ordinary\n"), 0644); err != nil {
		t.Fatalf("write ordinary fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, ".gitignore"), []byte("ignored.txt\n"), 0644); err != nil {
		t.Fatalf("write gitignore: %v", err)
	}
	if err := os.WriteFile(filepath.Join(src, "ignored.txt"), []byte("ignored\n"), 0644); err != nil {
		t.Fatalf("write ignored fixture: %v", err)
	}
	runTestGit(t, src, "add", ".gitignore", ".gitmodules", "modules/vendor-lib", "modules/ordinary-lib/keep.txt")
	runTestGit(t, src, "commit", "-m", "fixture")

	files, err := stageSnapshot(src, filepath.Join(root, "tmp"), nil, true)
	if err != nil {
		t.Fatalf("stage snapshot: %v", err)
	}
	if containsString(files, filepath.Join("modules", "vendor-lib", "nested.txt")) {
		t.Fatalf("submodule content must not be included: %#v", files)
	}
	if containsString(files, filepath.Join("modules", "vendor-lib")) {
		t.Fatalf("submodule gitlink must not be copied as a snapshot file: %#v", files)
	}
	if !containsString(files, filepath.Join("modules", "ordinary-lib", "keep.txt")) {
		t.Fatalf("ordinary directory content must still be included: %#v", files)
	}
	if containsString(files, "ignored.txt") {
		t.Fatalf("gitignored file must still be excluded: %#v", files)
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

	got, err := buildGitRemoteURLForServer(server, "deploy-main", true)
	if err != nil {
		t.Fatalf("build gateway git URL: %v", err)
	}
	want := "https://doops@gateway.example.com:42222/v1/git/doops-oilan/oilan-node/deploy-main.git"
	if got != want {
		t.Fatalf("gateway git URL mismatch:\nwant %s\n got %s", want, got)
	}
}

func TestBuildGitRemoteURLRejectsLegacyDirectTarget(t *testing.T) {
	_, err := buildGitRemoteURLForServer(Server{Name: "missing-gateway"}, "deploy-main", true)
	if err == nil || !strings.Contains(err.Error(), "requires a gateway target") {
		t.Fatalf("legacy direct Git transport must be rejected, got %v", err)
	}
}

func TestBuildGitRemoteURLRequiresExplicitRoute(t *testing.T) {
	for _, server := range []Server{
		{Name: "missing-cluster", Gateway: "https://gateway.example.com", Instance: "node"},
		{Name: "missing-instance", Gateway: "https://gateway.example.com", Cluster: "prod"},
	} {
		if _, err := buildGitRemoteURLForServer(server, "deploy-main", true); err == nil {
			t.Fatalf("target %#v must be rejected", server)
		}
	}
}

func TestWorkspaceSnapshotDirectoriesAreIsolatedPerPush(t *testing.T) {
	first, err := createWorkspaceSnapshotDirectory("shared-session")
	if err != nil {
		t.Fatalf("create first snapshot directory: %v", err)
	}
	defer cleanupSignalDirectory(first)
	second, err := createWorkspaceSnapshotDirectory("shared-session")
	if err != nil {
		t.Fatalf("create second snapshot directory: %v", err)
	}
	defer cleanupSignalDirectory(second)
	if first == second {
		t.Fatalf("concurrent pushes reused snapshot directory %s", first)
	}
}

func TestPrepareGitAuthEnvironmentKeepsTokenOutOfArgumentsAndCleansFiles(t *testing.T) {
	const token = "token-must-never-enter-git-argv"
	t.Setenv("GIT_TRACE_CURL", "1")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "credential.helper")
	t.Setenv("GIT_CONFIG_VALUE_0", "store")
	env, cleanup, err := prepareGitAuthEnvironment(token)
	if err != nil {
		t.Fatalf("prepare Git authentication: %v", err)
	}

	var askPassPath, tokenPath string
	var disabledSystemConfig, disabledGlobalConfig bool
	for _, entry := range env {
		if strings.Contains(entry, token) {
			t.Fatalf("Git environment exposed token value: %q", entry)
		}
		switch {
		case strings.HasPrefix(entry, "GIT_ASKPASS="):
			askPassPath = strings.TrimPrefix(entry, "GIT_ASKPASS=")
		case strings.HasPrefix(entry, "DOOPS_GIT_TOKEN_FILE="):
			tokenPath = strings.TrimPrefix(entry, "DOOPS_GIT_TOKEN_FILE=")
		case entry == "GIT_CONFIG_NOSYSTEM=1":
			disabledSystemConfig = true
		case entry == "GIT_CONFIG_GLOBAL=/dev/null":
			disabledGlobalConfig = true
		}
		if strings.HasPrefix(entry, "GIT_TRACE") || strings.HasPrefix(entry, "GIT_CONFIG_KEY_") ||
			strings.HasPrefix(entry, "GIT_CONFIG_VALUE_") || strings.HasPrefix(entry, "GIT_CONFIG_COUNT=") {
			t.Fatalf("unsafe inherited Git environment was not removed: %q", entry)
		}
	}
	if askPassPath == "" || tokenPath == "" {
		t.Fatalf("Git authentication environment is incomplete: %#v", env)
	}
	if !disabledSystemConfig || !disabledGlobalConfig {
		t.Fatalf("Git system/global config was not disabled: %#v", env)
	}
	if mode := fileModePerm(t, askPassPath); mode != 0o700 {
		t.Fatalf("askpass mode = %#o, want 0700", mode)
	}
	if mode := fileModePerm(t, tokenPath); mode != 0o600 {
		t.Fatalf("token file mode = %#o, want 0600", mode)
	}

	cmd := exec.Command(askPassPath, "Password for https://doops@example.com")
	cmd.Env = env
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("run askpass helper: %v", err)
	}
	if strings.TrimSpace(string(output)) != token {
		t.Fatalf("askpass helper returned an unexpected token")
	}

	authDir := filepath.Dir(tokenPath)
	cleanup()
	if _, err := os.Stat(authDir); !os.IsNotExist(err) {
		t.Fatalf("Git authentication directory was not removed: %v", err)
	}
}

func TestCleanupDeadGitAuthDirectoriesUsesOwnershipLock(t *testing.T) {
	dir := filepath.Join(os.TempDir(), "doops-git-auth-lock-test")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create auth directory: %v", err)
	}
	defer os.RemoveAll(dir)
	lockFile, err := os.OpenFile(filepath.Join(dir, "active.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("create auth lock: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("lock auth directory: %v", err)
	}
	cleanupDeadGitAuthDirectories()
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("active auth directory was removed: %v", err)
	}
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatalf("unlock auth directory: %v", err)
	}
	if err := lockFile.Close(); err != nil {
		t.Fatalf("close auth lock: %v", err)
	}
	cleanupDeadGitAuthDirectories()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("inactive auth directory survived lock cleanup: %v", err)
	}
}

func fileModePerm(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return info.Mode().Perm()
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
		"HEAD:master",
		"doops-sync-",
		"AutoSync",
		"DoopsPullSnapshot",
		"update-ref refs/heads/doops-pull",
		"pull-commit:%s",
		"Git workspace transport requires a gateway target",
		"credential.helper=",
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
