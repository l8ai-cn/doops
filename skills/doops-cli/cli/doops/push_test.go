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

func TestBuildGitRemoteURLForGatewayTarget(t *testing.T) {
	server := Server{
		Name:     "oilan",
		Gateway:  "http://gateway.example.com:42222",
		Cluster:  "doops-oilan",
		Instance: "oilan-node",
	}

	got, err := buildGitRemoteURLForServer(server, "deploy-main", "secret-token")
	if err != nil {
		t.Fatalf("build gateway git URL: %v", err)
	}
	want := "http://doops:secret-token@gateway.example.com:42222/v1/git/doops-oilan/oilan-node/deploy-main.git"
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
