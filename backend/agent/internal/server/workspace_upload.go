package server

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/user/doops/agent/api"
)

var uploadLocks sync.Map

type workspaceBeginParams struct {
	SessionID string `json:"session_id"`
	Commit    string `json:"commit"`
}

type workspaceChunkParams struct {
	UploadID string `json:"upload_id"`
	Seq      int    `json:"seq"`
	DataB64  string `json:"data_b64"`
}

type workspaceCommitParams struct {
	UploadID  string `json:"upload_id"`
	SessionID string `json:"session_id"`
	Commit    string `json:"commit"`
}

type workspacePullBeginParams struct {
	SessionID string `json:"session_id"`
}

type workspacePullChunkParams struct {
	BundleID string `json:"bundle_id"`
	Offset   int64  `json:"offset"`
	Limit    int64  `json:"limit,omitempty"`
}

type workspacePullBeginResult struct {
	BundleID  string `json:"bundle_id"`
	SessionID string `json:"session_id"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	ChunkSize int64  `json:"chunk_size"`
}

type workspacePullChunkResult struct {
	BundleID   string `json:"bundle_id"`
	Offset     int64  `json:"offset"`
	NextOffset int64  `json:"next_offset"`
	Size       int64  `json:"size"`
	EOF        bool   `json:"eof"`
	DataB64    string `json:"data_b64"`
}

type checkDeploymentParams struct {
	Namespace  string `json:"namespace"`
	Deployment string `json:"deployment"`
	Image      string `json:"image"`
}

type cleanWorkspaceParams struct {
	Workspace string `json:"workspace"`
}

type agentUpgradeParams struct {
	Image     string `json:"image"`
	Mode      string `json:"mode"`
	Namespace string `json:"namespace"`
	Workload  string `json:"workload"`
	Container string `json:"container"`
	DryRun    bool   `json:"dry_run"`
}

type workspaceUploadMeta struct {
	SessionID string    `json:"session_id"`
	Commit    string    `json:"commit"`
	NextSeq   int       `json:"next_seq"`
	Size      int64     `json:"size"`
	CreatedAt time.Time `json:"created_at"`
}

const (
	defaultMaxWorkspaceUploadBytes = int64(512 << 20)
	defaultWorkspaceUploadMaxAge   = 24 * time.Hour
	defaultMaxFileReadBytes        = int64(1 << 20)
	defaultMaxFileWriteBytes       = int64(16 << 20)
	defaultWorkspacePullChunkBytes = int64(512 << 10)
	defaultWebSocketMessageBytes   = int64(32 << 20)
	defaultToolExecutionTimeout    = 30 * time.Minute
	defaultCompletedTaskMaxAge     = 24 * time.Hour
)

func handleFileWrite(argBytes json.RawMessage) (string, error) {
	var args api.FileWriteParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid file write params: %w", err)
	}
	if int64(len(args.Content)) > maxFileWriteBytes() {
		return "", fmt.Errorf("file write exceeds max size: %d > %d", len(args.Content), maxFileWriteBytes())
	}
	path, err := resolveWorkspaceFilePath(args.SessionID, args.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(args.Content), 0644); err != nil {
		return "", err
	}
	return fmt.Sprintf("Wrote %d bytes to %s", len(args.Content), path), nil
}

func handleFileRead(argBytes json.RawMessage) (string, error) {
	var args api.FileReadParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid file read params: %w", err)
	}
	path, err := resolveWorkspaceFilePath(args.SessionID, args.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", fmt.Errorf("doops read is for small text files only: %s is a directory", path)
	}
	if info.Size() > maxFileReadBytes() {
		return "", fmt.Errorf("doops read is for viewing small text files only; file size %d exceeds limit %d", info.Size(), maxFileReadBytes())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !looksLikeText(data) {
		return "", fmt.Errorf("doops read is for viewing small text files only; %s looks like binary data", path)
	}
	return string(data), nil
}

func resolveWorkspaceFilePath(sessionID, rawPath string) (string, error) {
	if err := validateSession(sessionID); err != nil {
		return "", err
	}
	rawPath = strings.TrimSpace(rawPath)
	if rawPath == "" {
		return "", fmt.Errorf("path is required")
	}
	if filepath.IsAbs(rawPath) || strings.Contains(rawPath, "\x00") {
		return "", fmt.Errorf("unsafe workspace path: %q", rawPath)
	}
	clean := filepath.Clean(rawPath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("unsafe workspace path: %q", rawPath)
	}
	root, err := workspacePath(sessionID)
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, clean)
	if !pathWithinRoot(root, path) {
		return "", fmt.Errorf("workspace path escapes session: %q", rawPath)
	}
	return path, nil
}

func pathWithinRoot(root, path string) bool {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)))
}

func looksLikeText(data []byte) bool {
	if len(data) == 0 {
		return true
	}
	for _, b := range data {
		if b == 0 {
			return false
		}
	}
	return true
}

func handleWorkspacePullBegin(argBytes json.RawMessage) (string, error) {
	var args workspacePullBeginParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid workspace pull begin params: %w", err)
	}
	sessionID := strings.TrimSpace(args.SessionID)
	if err := validateSession(sessionID); err != nil {
		return "", err
	}
	wsPath := filepath.Join(workspaceRoot(), sessionID)
	if info, err := os.Stat(wsPath); err != nil {
		return "", err
	} else if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", wsPath)
	}

	if err := ensureWorkspaceGitSnapshot(wsPath, sessionID); err != nil {
		return "", err
	}

	if err := os.MkdirAll(workspacePullBundleDir(), 0700); err != nil {
		return "", err
	}
	bundleID := generateID()
	bundlePath := workspacePullBundlePath(bundleID)
	if err := runCommand(wsPath, "git", "bundle", "create", bundlePath, "master"); err != nil {
		return "", err
	}
	info, err := os.Stat(bundlePath)
	if err != nil {
		return "", err
	}
	sum, err := sha256File(bundlePath)
	if err != nil {
		return "", err
	}
	result := workspacePullBeginResult{
		BundleID:  bundleID,
		SessionID: sessionID,
		Size:      info.Size(),
		SHA256:    sum,
		ChunkSize: workspacePullChunkBytes(),
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func handleWorkspacePullChunk(argBytes json.RawMessage) (string, error) {
	var args workspacePullChunkParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid workspace pull chunk params: %w", err)
	}
	if !validID(args.BundleID) {
		return "", fmt.Errorf("invalid bundle_id")
	}
	if args.Offset < 0 {
		return "", fmt.Errorf("offset must be >= 0")
	}
	limit := args.Limit
	maxChunk := workspacePullChunkBytes()
	if limit <= 0 || limit > maxChunk {
		limit = maxChunk
	}
	bundlePath := workspacePullBundlePath(args.BundleID)
	info, err := os.Stat(bundlePath)
	if err != nil {
		return "", err
	}
	if args.Offset > info.Size() {
		return "", fmt.Errorf("offset exceeds bundle size: %d > %d", args.Offset, info.Size())
	}
	f, err := os.Open(bundlePath)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.Seek(args.Offset, io.SeekStart); err != nil {
		return "", err
	}
	buf := make([]byte, limit)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", err
	}
	buf = buf[:n]
	next := args.Offset + int64(n)
	result := workspacePullChunkResult{
		BundleID:   args.BundleID,
		Offset:     args.Offset,
		NextOffset: next,
		Size:       info.Size(),
		EOF:        next >= info.Size(),
		DataB64:    base64.StdEncoding.EncodeToString(buf),
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func ensureWorkspaceGitSnapshot(wsPath, sessionID string) error {
	if _, err := os.Stat(filepath.Join(wsPath, ".git")); os.IsNotExist(err) {
		if err := runCommand(wsPath, "git", "init", "-b", "master"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := runCommand(wsPath, "git", "config", "user.name", "doops"); err != nil {
		return err
	}
	if err := runCommand(wsPath, "git", "config", "user.email", "doops@localhost"); err != nil {
		return err
	}
	if err := runCommand(wsPath, "git", "add", "-A"); err != nil {
		return err
	}
	if err := runCommand(wsPath, "git", "commit", "--allow-empty", "-m", "DoopsPullSnapshot"); err != nil {
		return err
	}
	if err := os.MkdirAll("/tmp/repos", 0755); err != nil {
		return err
	}
	repoPath := filepath.Join("/tmp/repos", sessionID+".git")
	if err := ensureBareRepo(repoPath); err != nil {
		return err
	}
	return runCommand(wsPath, "git", "push", "-f", repoPath, "HEAD:master")
}

func ensureBareRepo(repoPath string) error {
	if _, err := os.Stat(repoPath); err == nil {
		if err := runCommand("", "git", "--git-dir", repoPath, "rev-parse", "--is-bare-repository"); err == nil {
			return nil
		}
		if err := os.RemoveAll(repoPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return runCommand("", "git", "init", "--bare", repoPath)
}

func runCommand(dir string, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s failed: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func workspacePullBundleDir() string {
	return filepath.Join(os.TempDir(), "doops-pull-bundles")
}

func workspacePullBundlePath(bundleID string) string {
	return filepath.Join(workspacePullBundleDir(), bundleID+".bundle")
}

func validID(id string) bool {
	if id == "" {
		return false
	}
	for _, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func workspacePullChunkBytes() int64 {
	return positiveEnvInt64("DOOPS_WORKSPACE_PULL_CHUNK_BYTES", defaultWorkspacePullChunkBytes)
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func handleCheckDeployment(argBytes json.RawMessage) (string, error) {
	var args checkDeploymentParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid check deployment params: %w", err)
	}
	namespace := strings.TrimSpace(args.Namespace)
	if namespace == "" {
		namespace = "default"
	}
	deployment := strings.TrimSpace(args.Deployment)
	wantImage := strings.TrimSpace(args.Image)
	if deployment == "" || wantImage == "" {
		return "", fmt.Errorf("namespace, deployment and image are required")
	}
	cmd := exec.Command("kubectl", "get", "deploy", deployment, "-n", namespace, "-o", "jsonpath={.spec.template.spec.containers[0].image}")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl get deploy %s/%s failed: %v: %s", namespace, deployment, err, strings.TrimSpace(string(out)))
	}
	got := strings.Trim(strings.TrimSpace(string(out)), `"'`)
	if got == "" {
		return "", fmt.Errorf("empty image from kubectl for %s/%s", namespace, deployment)
	}
	if !imageRefMatches(wantImage, got) {
		return "", fmt.Errorf("image mismatch: want %q got %q", wantImage, got)
	}
	return fmt.Sprintf("check ok: %s/%s -> %s", namespace, deployment, got), nil
}

func handleCleanWorkspace(argBytes json.RawMessage) (string, error) {
	var args cleanWorkspaceParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid clean workspace params: %w", err)
	}
	workspace := strings.TrimSpace(args.Workspace)
	if err := validateSession(workspace); err != nil {
		return "", err
	}
	wsPath := filepath.Join(workspaceRoot(), workspace)
	repoPath := filepath.Join("/tmp/repos", workspace+".git")
	if err := os.RemoveAll(wsPath); err != nil {
		return "", err
	}
	if err := os.RemoveAll(repoPath); err != nil {
		return "", err
	}
	return fmt.Sprintf("Cleaned workspace and bare repo: %s", workspace), nil
}

func handleAgentUpgrade(argBytes json.RawMessage) (string, error) {
	var args agentUpgradeParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid agent upgrade params: %w", err)
	}
	image := strings.TrimSpace(args.Image)
	if image == "" {
		return "", fmt.Errorf("image is required")
	}
	mode := strings.TrimSpace(args.Mode)
	if mode == "" || mode == "auto" {
		mode = detectAgentUpgradeMode()
	}
	switch mode {
	case "k8s", "daemonset":
		return upgradeK8sDaemonSet(args, image)
	case "docker", "container":
		return upgradeDockerContainer(args, image)
	case "unsupported", "bare":
		return "", fmt.Errorf("unsupported agent upgrade mode: bare binary or unknown runtime")
	default:
		return "", fmt.Errorf("unsupported agent upgrade mode %q", mode)
	}
}

func detectAgentUpgradeMode() string {
	if _, err := os.Stat("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		return "k8s"
	}
	if _, err := exec.LookPath("docker"); err == nil {
		return "docker"
	}
	if _, err := exec.LookPath("nerdctl"); err == nil {
		return "docker"
	}
	return "unsupported"
}

func upgradeK8sDaemonSet(args agentUpgradeParams, image string) (string, error) {
	namespace := firstNonEmptyString(args.Namespace, readNamespaceFile(), "oilan-system")
	workload := firstNonEmptyString(args.Workload, "daemonset/doops-agent")
	container := firstNonEmptyString(args.Container, "doops-agent")
	if args.DryRun {
		return fmt.Sprintf("dry-run: pull image before rollout: %s\nkubectl -n %s set image %s %s=%s", image, namespace, workload, container, image), nil
	}
	pullOut, err := prePullK8sImage(image)
	if err != nil {
		return "", err
	}
	setCmd := exec.Command("kubectl", "-n", namespace, "set", "image", workload, container+"="+image)
	setOut, err := setCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl set image failed: %v: %s", err, strings.TrimSpace(string(setOut)))
	}
	statusCmd := exec.Command("kubectl", "-n", namespace, "rollout", "status", workload, "--timeout=180s")
	statusOut, err := statusCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("kubectl rollout status failed: %v: %s", err, strings.TrimSpace(string(statusOut)))
	}
	return fmt.Sprintf("upgrade k8s ok: %s %s=%s\n%s\n%s\n%s", workload, container, image, strings.TrimSpace(pullOut), strings.TrimSpace(string(setOut)), strings.TrimSpace(string(statusOut))), nil
}

func prePullK8sImage(image string) (string, error) {
	if path, err := exec.LookPath("nerdctl"); err == nil {
		out, pullErr := exec.Command(path, "-n", "k8s.io", "pull", image).CombinedOutput()
		if pullErr == nil {
			return string(out), nil
		}
		return "", fmt.Errorf("pre-pull image with nerdctl failed: %v: %s", pullErr, strings.TrimSpace(string(out)))
	}
	if path, err := exec.LookPath("crictl"); err == nil {
		out, pullErr := exec.Command(path, "pull", image).CombinedOutput()
		if pullErr == nil {
			return string(out), nil
		}
		return "", fmt.Errorf("pre-pull image with crictl failed: %v: %s", pullErr, strings.TrimSpace(string(out)))
	}
	return "", fmt.Errorf("pre-pull image failed: neither nerdctl nor crictl is available")
}

func upgradeDockerContainer(args agentUpgradeParams, image string) (string, error) {
	container := firstNonEmptyString(args.Container, "doops-agent")
	runtime := "docker"
	if _, err := exec.LookPath("docker"); err != nil {
		if _, err := exec.LookPath("nerdctl"); err == nil {
			runtime = "nerdctl"
		} else {
			return "", fmt.Errorf("docker/nerdctl not found")
		}
	}
	if args.DryRun {
		return fmt.Sprintf("dry-run: %s pull %s && restart container %s with preserved config", runtime, image, container), nil
	}
	cmd := exec.Command("sh", "-c", fmt.Sprintf(`set -e
%s pull %s
old_id=$(%s ps -q --filter name=^/%s$ 2>/dev/null || true)
if [ -z "$old_id" ]; then
  old_id=$(%s ps -q --filter name=%s 2>/dev/null | head -1 || true)
fi
if [ -z "$old_id" ]; then
  echo "image pulled; no running container named %s found for automatic replacement"
  exit 0
fi
echo "image pulled; container replacement for $old_id must be handled by supervisor"
`, runtime, shellEscape(image), runtime, shellEscape(container), runtime, shellEscape(container), shellEscape(container)))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker upgrade preparation failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func readNamespaceFile() string {
	data, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func shellEscape(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func handleWorkspaceBegin(argBytes json.RawMessage) (string, error) {
	var args workspaceBeginParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid workspace begin params: %w", err)
	}
	if err := validateSession(args.SessionID); err != nil {
		return "", err
	}
	commit, err := normalizeSHA256(args.Commit)
	if err != nil {
		return "", err
	}
	if err := cleanupExpiredUploads(defaultWorkspaceUploadMaxAge); err != nil {
		return "", err
	}
	uploadID := "upl_" + randomHex(16)
	if err := os.MkdirAll(uploadDir(), 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(uploadArchivePath(uploadID), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = cleanupUpload(uploadID)
		return "", err
	}
	if err := saveUploadMeta(uploadID, workspaceUploadMeta{
		SessionID: args.SessionID,
		Commit:    commit,
		NextSeq:   0,
		Size:      0,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		_ = cleanupUpload(uploadID)
		return "", err
	}
	return uploadID, nil
}

func handleWorkspaceChunk(argBytes json.RawMessage) (string, error) {
	var args workspaceChunkParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid workspace chunk params: %w", err)
	}
	if err := validateUploadID(args.UploadID); err != nil {
		return "", err
	}
	unlock := lockUpload(args.UploadID)
	defer unlock()
	if args.Seq < 0 {
		return "", fmt.Errorf("invalid chunk sequence: %d", args.Seq)
	}
	meta, err := loadUploadMeta(args.UploadID)
	if err != nil {
		return "", err
	}
	if args.Seq != meta.NextSeq {
		return "", fmt.Errorf("unexpected chunk sequence: got %d want %d", args.Seq, meta.NextSeq)
	}
	data, err := base64.StdEncoding.DecodeString(args.DataB64)
	if err != nil {
		return "", fmt.Errorf("decode chunk %d: %w", args.Seq, err)
	}
	if meta.Size+int64(len(data)) > maxWorkspaceUploadBytes() {
		_ = cleanupUpload(args.UploadID)
		return "", fmt.Errorf("upload exceeds max size: %d > %d", meta.Size+int64(len(data)), maxWorkspaceUploadBytes())
	}
	f, err := os.OpenFile(uploadArchivePath(args.UploadID), os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		_ = cleanupUpload(args.UploadID)
		return "", err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = cleanupUpload(args.UploadID)
		return "", err
	}
	if err := f.Close(); err != nil {
		_ = cleanupUpload(args.UploadID)
		return "", err
	}
	meta.NextSeq++
	meta.Size += int64(len(data))
	if err := saveUploadMeta(args.UploadID, meta); err != nil {
		_ = cleanupUpload(args.UploadID)
		return "", err
	}
	return fmt.Sprintf("chunk %d accepted (%d bytes)", args.Seq, len(data)), nil
}

func lockUpload(uploadID string) func() {
	value, _ := uploadLocks.LoadOrStore(uploadID, &sync.Mutex{})
	mu := value.(*sync.Mutex)
	mu.Lock()
	return func() {
		mu.Unlock()
	}
}

func handleWorkspaceCommit(argBytes json.RawMessage) (string, error) {
	var args workspaceCommitParams
	if err := json.Unmarshal(argBytes, &args); err != nil {
		return "", fmt.Errorf("invalid workspace commit params: %w", err)
	}
	if err := validateUploadID(args.UploadID); err != nil {
		return "", err
	}
	if err := validateSession(args.SessionID); err != nil {
		return "", err
	}
	commit, err := normalizeSHA256(args.Commit)
	if err != nil {
		return "", err
	}
	meta, err := loadUploadMeta(args.UploadID)
	if err != nil {
		return "", err
	}
	cleanupOnFailure := true
	defer func() {
		if cleanupOnFailure {
			_ = cleanupUpload(args.UploadID)
		}
	}()
	if meta.SessionID != args.SessionID {
		return "", fmt.Errorf("upload session mismatch: got %q want %q", args.SessionID, meta.SessionID)
	}
	if meta.Commit != commit {
		return "", fmt.Errorf("upload commit mismatch: got %q want %q", commit, meta.Commit)
	}
	archivePath := uploadArchivePath(args.UploadID)
	actual, err := fileSHA256Hex(archivePath)
	if err != nil {
		return "", err
	}
	if actual != commit {
		return "", fmt.Errorf("sha256 mismatch: got %s want %s", actual, commit)
	}
	dest, err := workspacePath(args.SessionID)
	if err != nil {
		return "", err
	}
	root := workspaceRoot()
	if err := os.MkdirAll(root, 0755); err != nil {
		return "", err
	}
	tmpDest := filepath.Join(root, "."+args.SessionID+".tmp-"+randomHex(8))
	if err := os.MkdirAll(tmpDest, 0755); err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDest)
	if err := extractTarGz(archivePath, tmpDest, maxWorkspaceUploadBytes()); err != nil {
		return "", err
	}
	ready := filepath.Join(tmpDest, ".doops-ready")
	tmp := ready + ".tmp"
	if err := os.WriteFile(tmp, []byte(commit+"\n"), 0644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, ready); err != nil {
		return "", err
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Rename(tmpDest, dest); err != nil {
		return "", err
	}
	tmpDest = ""
	if err := cleanupUpload(args.UploadID); err != nil {
		return "", err
	}
	cleanupOnFailure = false
	return fmt.Sprintf("workspace ready: %s", dest), nil
}

func extractTarGz(archivePath, dest string, maxBytes int64) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var extracted int64
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(hdr.Name)
		if name == "." || filepath.IsAbs(name) || strings.HasPrefix(name, ".."+string(os.PathSeparator)) || name == ".." {
			return fmt.Errorf("unsafe archive path: %s", hdr.Name)
		}
		target := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&0777); err != nil {
				return err
			}
		case tar.TypeReg, tar.TypeRegA:
			if hdr.Size < 0 {
				return fmt.Errorf("invalid archive file size: %s", hdr.Name)
			}
			extracted += hdr.Size
			if maxBytes > 0 && extracted > maxBytes {
				return fmt.Errorf("extracted workspace exceeds max size: %d > %d", extracted, maxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, os.FileMode(hdr.Mode)&0777)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(out, tr)
			closeErr := out.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
}

func workspaceRoot() string {
	if root := strings.TrimSpace(os.Getenv("DOOPS_WORKSPACE_ROOT")); root != "" {
		return root
	}
	return "/root/ws"
}

func workspacePath(sessionID string) (string, error) {
	if err := validateSession(sessionID); err != nil {
		return "", err
	}
	return filepath.Join(workspaceRoot(), sessionID), nil
}

func uploadDir() string {
	return filepath.Join(os.TempDir(), "doops-uploads")
}

func uploadArchivePath(uploadID string) string {
	return filepath.Join(uploadDir(), uploadID+".tar.gz")
}

func uploadMetaPath(uploadID string) string {
	return filepath.Join(uploadDir(), uploadID+".json")
}

func saveUploadMeta(uploadID string, meta workspaceUploadMeta) error {
	if err := validateUploadID(uploadID); err != nil {
		return err
	}
	if err := validateSession(meta.SessionID); err != nil {
		return err
	}
	commit, err := normalizeSHA256(meta.Commit)
	if err != nil {
		return err
	}
	meta.Commit = commit
	if meta.CreatedAt.IsZero() {
		meta.CreatedAt = time.Now().UTC()
	}
	if err := os.MkdirAll(uploadDir(), 0700); err != nil {
		return err
	}
	data, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	tmp := uploadMetaPath(uploadID) + "." + randomHex(4) + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, uploadMetaPath(uploadID)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func loadUploadMeta(uploadID string) (workspaceUploadMeta, error) {
	if err := validateUploadID(uploadID); err != nil {
		return workspaceUploadMeta{}, err
	}
	data, err := os.ReadFile(uploadMetaPath(uploadID))
	if err != nil {
		return workspaceUploadMeta{}, fmt.Errorf("load upload metadata: %w", err)
	}
	var meta workspaceUploadMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return workspaceUploadMeta{}, fmt.Errorf("parse upload metadata: %w", err)
	}
	if err := validateSession(meta.SessionID); err != nil {
		return workspaceUploadMeta{}, err
	}
	commit, err := normalizeSHA256(meta.Commit)
	if err != nil {
		return workspaceUploadMeta{}, err
	}
	meta.Commit = commit
	return meta, nil
}

func cleanupUpload(uploadID string) error {
	if err := validateUploadID(uploadID); err != nil {
		return err
	}
	return errors.Join(removeIfExists(uploadArchivePath(uploadID)), removeIfExists(uploadMetaPath(uploadID)))
}

func cleanupExpiredUploads(maxAge time.Duration) error {
	if maxAge <= 0 {
		return nil
	}
	entries, err := os.ReadDir(uploadDir())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var joined error
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "upl_") && strings.HasSuffix(name, ".json") {
			uploadID := strings.TrimSuffix(name, ".json")
			if validateUploadID(uploadID) != nil {
				continue
			}
			createdAt, ok := uploadMetadataCreatedAt(uploadID, entry)
			if ok && now.Sub(createdAt) > maxAge {
				joined = errors.Join(joined, cleanupUpload(uploadID))
			}
			continue
		}
		if strings.HasPrefix(name, "upl_") && strings.HasSuffix(name, ".tar.gz") {
			uploadID := strings.TrimSuffix(name, ".tar.gz")
			if validateUploadID(uploadID) != nil {
				continue
			}
			if _, err := os.Stat(uploadMetaPath(uploadID)); err == nil {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				joined = errors.Join(joined, err)
				continue
			}
			if now.Sub(info.ModTime().UTC()) > maxAge {
				joined = errors.Join(joined, cleanupUpload(uploadID))
			}
		}
	}
	return joined
}

func uploadMetadataCreatedAt(uploadID string, entry os.DirEntry) (time.Time, bool) {
	meta, err := loadUploadMeta(uploadID)
	if err == nil && !meta.CreatedAt.IsZero() {
		return meta.CreatedAt.UTC(), true
	}
	info, err := entry.Info()
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime().UTC(), true
}

func removeIfExists(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func maxWorkspaceUploadBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("DOOPS_MAX_WORKSPACE_UPLOAD_BYTES"))
	if raw == "" {
		return defaultMaxWorkspaceUploadBytes
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return defaultMaxWorkspaceUploadBytes
	}
	return n
}

func maxFileReadBytes() int64 {
	return positiveEnvInt64("DOOPS_MAX_FILE_READ_BYTES", defaultMaxFileReadBytes)
}

func maxFileWriteBytes() int64 {
	return positiveEnvInt64("DOOPS_MAX_FILE_WRITE_BYTES", defaultMaxFileWriteBytes)
}

func maxWebSocketMessageBytes() int64 {
	return positiveEnvInt64("DOOPS_MAX_WS_MESSAGE_BYTES", defaultWebSocketMessageBytes)
}

func maxToolExecutionDuration() time.Duration {
	return positiveEnvDuration("DOOPS_TOOL_TIMEOUT", defaultToolExecutionTimeout)
}

func maxCompletedTaskAge() time.Duration {
	return positiveEnvDuration("DOOPS_COMPLETED_TASK_MAX_AGE", defaultCompletedTaskMaxAge)
}

func positiveEnvInt64(name string, fallback int64) int64 {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return n
}

func positiveEnvDuration(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err == nil && d > 0 {
		return d
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return fallback
	}
	return time.Duration(n) * time.Second
}

func imageRefMatches(want, got string) bool {
	want = strings.TrimSpace(want)
	got = strings.TrimSpace(got)
	if want == got {
		return true
	}
	if strings.HasPrefix(got, want+":") || strings.HasPrefix(got, want+"@") {
		return true
	}
	if strings.HasPrefix(want, got+":") || strings.HasPrefix(want, got+"@") {
		return true
	}
	return false
}

func normalizeSHA256(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if len(value) != sha256.Size*2 {
		return "", fmt.Errorf("commit must be a sha256 hex digest")
	}
	if _, err := hex.DecodeString(value); err != nil {
		return "", fmt.Errorf("commit must be a sha256 hex digest: %w", err)
	}
	return value, nil
}

func fileSHA256Hex(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func validateSession(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" || sessionID == "." || sessionID == "/" || strings.Contains(sessionID, "..") || strings.ContainsAny(sessionID, `/\`) {
		return fmt.Errorf("unsafe session id: %q", sessionID)
	}
	return nil
}

func validateUploadID(uploadID string) error {
	uploadID = strings.TrimSpace(uploadID)
	if !strings.HasPrefix(uploadID, "upl_") || strings.ContainsAny(uploadID, `/\.`) {
		return fmt.Errorf("unsafe upload id: %q", uploadID)
	}
	return nil
}
