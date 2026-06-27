package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitRepoPasswordIsEncryptedAndRecoverable(t *testing.T) {
	t.Setenv("DOOPS_GATEWAY_SECRET_KEY", "test-secret-key")
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	repo, err := store.CreateGitRepo(GitRepoInput{
		Name:     "private",
		URL:      "https://example.com/org/repo.git",
		Branch:   "main",
		Username: "deploy",
		Password: "token-123",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}
	if repo.PasswordHash != "" {
		t.Fatalf("repo should not expose stored password material, got %q", repo.PasswordHash)
	}
	if !repo.HasPassword {
		t.Fatal("repo should indicate stored password exists")
	}

	secret, err := store.GitRepoPassword(repo.ID)
	if err != nil {
		t.Fatalf("recover repo password: %v", err)
	}
	if secret != "token-123" {
		t.Fatalf("recovered password = %q, want token-123", secret)
	}

	var raw string
	if err := store.db.QueryRow(`SELECT password_ciphertext FROM git_repos WHERE id = ?`, repo.ID).Scan(&raw); err != nil {
		t.Fatalf("read raw ciphertext: %v", err)
	}
	if raw == "" || strings.Contains(raw, "token-123") {
		t.Fatalf("password ciphertext was not encrypted: %q", raw)
	}
}

func TestHandleAdminRepoTestRunsGitLsRemote(t *testing.T) {
	store, token := newAdminStoreAndToken(t)
	defer store.Close()
	repoPath := createLocalBareGitRepo(t)

	repo, err := store.CreateGitRepo(GitRepoInput{
		Name:   "local repo",
		URL:    repoPath,
		Branch: "main",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	hub := NewGatewayHub(store, GatewayHubOptions{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/repos/test?id="+repo.ID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	hub.HandleAdminRepoTest(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for reachable repo, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !body.OK || !strings.Contains(body.Message, "main") {
		t.Fatalf("unexpected response: %#v", body)
	}
	updated, err := store.GetGitRepo(repo.ID)
	if err != nil {
		t.Fatalf("get updated repo: %v", err)
	}
	if updated.LastUsedAt == nil {
		t.Fatal("successful test should update last_used_at")
	}
}

func TestHandleAdminRepoTestRejectsMissingBranch(t *testing.T) {
	store, token := newAdminStoreAndToken(t)
	defer store.Close()
	repoPath := createLocalBareGitRepo(t)

	repo, err := store.CreateGitRepo(GitRepoInput{
		Name:   "local repo",
		URL:    repoPath,
		Branch: "missing",
	})
	if err != nil {
		t.Fatalf("create repo: %v", err)
	}

	hub := NewGatewayHub(store, GatewayHubOptions{})
	req := httptest.NewRequest(http.MethodPost, "/v1/admin/repos/test?id="+repo.ID, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()

	hub.HandleAdminRepoTest(w, req)

	if w.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 for missing branch, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "branch") {
		t.Fatalf("expected branch error, got %s", w.Body.String())
	}
}

func TestGitCloneToolActionSummaryDoesNotExposePassword(t *testing.T) {
	raw := mustJSON(t, map[string]interface{}{
		"session_id": "deploy",
		"url":        "https://example.com/org/repo.git",
		"branch":     "main",
		"username":   "deploy",
		"password":   "secret-token",
	})
	if got := actionForTool("doops_git_clone", raw); got != ActionPull {
		t.Fatalf("doops_git_clone action = %q, want %q", got, ActionPull)
	}
	summary := summarizeToolCall("doops_git_clone", raw)
	if strings.Contains(summary, "secret-token") {
		t.Fatalf("summary exposed credential material: %s", summary)
	}
	if !strings.Contains(summary, "https://example.com/org/repo.git") || !strings.Contains(summary, "main") {
		t.Fatalf("summary omitted repo context: %s", summary)
	}
}

func newAdminStoreAndToken(t *testing.T) (*GatewayStore, string) {
	t.Helper()
	store, err := OpenGatewayStore(t.TempDir() + "/gateway.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	user, err := store.CreateUser("admin")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	if err := store.GrantUser(user.ID, ScopeGrant{
		Cluster:  "*",
		Instance: "*",
		Actions:  []GatewayAction{ActionAdmin},
	}); err != nil {
		t.Fatalf("grant admin: %v", err)
	}
	token, err := store.CreateToken(CreateTokenRequest{Kind: TokenKindUser, UserID: user.ID, Name: "admin"})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}
	return store, token.Plaintext
}

func createLocalBareGitRepo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bare := filepath.Join(root, "repo.git")
	runGit(t, root, "init", "--bare", bare)
	runGit(t, root, "init", "-b", "main", work)
	runGit(t, work, "config", "user.email", "test@example.com")
	runGit(t, work, "config", "user.name", "Doops Test")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("# test\n"), 0644); err != nil {
		t.Fatalf("write test file: %v", err)
	}
	runGit(t, work, "add", "README.md")
	runGit(t, work, "commit", "-m", "init")
	runGit(t, work, "remote", "add", "origin", bare)
	runGit(t, work, "push", "origin", "main")
	return bare
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}
