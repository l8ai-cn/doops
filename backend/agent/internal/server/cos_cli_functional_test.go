package server

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Only the object-store HTTP service and CA trust are test fixtures. The built
// CLI, Gateway, SDK object calls, reverse Agent connection and file sync are real.
func TestCOSCLIThroughGatewayAndRealAgent(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOOPS_WORKSPACE_ROOT", filepath.Join(root, "workspaces"))
	objects := map[string][]byte{}
	var mu sync.Mutex
	s3 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/"+cosBucketName)
		if key == "" || key == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "read failed", 500)
				return
			}
			objects[key] = body
			w.Header().Set("ETag", `"synthetic"`)
		case http.MethodGet, http.MethodHead:
			body, ok := objects[key]
			if !ok {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method == http.MethodGet {
				_, _ = w.Write(body)
			}
		default:
			http.Error(w, "unexpected method", 405)
		}
	}))
	defer s3.Close()
	ca := filepath.Join(root, "cos-test-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s3.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CA_BUNDLE", ca)
	t.Setenv("DOOPS_COS_ACCESS_KEY_ID", "synthetic-id")
	t.Setenv("DOOPS_COS_SECRET_ACCESS_KEY", "synthetic-key")
	t.Setenv("DOOPS_COS_ENDPOINT", s3.URL)
	t.Setenv("DOOPS_COS_REGION", "test")
	t.Setenv("DOOPS_COS_BUCKET", cosBucketName)
	// Agent uses the normal HTTP download client; give it only this test CA.
	originalTransport := http.DefaultTransport
	http.DefaultTransport = s3.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	// Go's macOS system verifier does not use SSL_CERT_FILE. Add only the test
	// CA to the compiled CLI's transport through a build overlay, without
	// disabling TLS verification or changing any production source/API logic.
	trustSource := filepath.Join(root, "test_ca.go")
	trustCode := `package main
import("crypto/tls";"crypto/x509";"net/http";"os")
func init(){ b,e:=os.ReadFile(os.Getenv("AWS_CA_BUNDLE"));if e!=nil{panic(e)};p:=x509.NewCertPool();if !p.AppendCertsFromPEM(b){panic("invalid test CA")};tr:=http.DefaultTransport.(*http.Transport).Clone();tr.TLSClientConfig=&tls.Config{RootCAs:p};http.DefaultTransport=tr }
`
	if err := os.WriteFile(trustSource, []byte(trustCode), 0600); err != nil {
		t.Fatal(err)
	}
	cliRoot, err := filepath.Abs("../../../cli")
	if err != nil {
		t.Fatal(err)
	}
	overlay := filepath.Join(root, "overlay.json")
	data, _ := json.Marshal(map[string]interface{}{"Replace": map[string]string{filepath.Join(cliRoot, "cli/doops/zz_cos_test_ca.go"): trustSource}})
	if err := os.WriteFile(overlay, data, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "doops")
	buildCtx, stopBuild := context.WithTimeout(context.Background(), time.Minute)
	defer stopBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-overlay", overlay, "-o", binary, "./cli/doops")
	build.Dir = cliRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, out)
	}

	f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
	agent := &Gateway{tasks: make(map[string]*bgTask), sessionMap: make(map[string]*sessionEntry)}
	conn := dialFakeGatewayAgent(t, f.server.URL, "dev", "local")
	done := make(chan struct{})
	go func() { defer close(done); agent.ServeGatewayWebSocketConnWithReady(conn, "cos-cli-functional", nil) }()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("real Agent did not stop")
		}
	})
	waitForGatewayAgent(t, f.hub, "dev", "local")
	config := writeAccountCLIConfig(t, f.server.URL, f.admin.Plaintext)
	runCLI := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, binary, append([]string{"-session", "cos-functional"}, args...)...)
		command.Env = append(os.Environ(), "DOOPS_CONFIG="+config)
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("CLI %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	source, dest := filepath.Join(root, "source"), filepath.Join(root, "download")
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(source, "run.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf COS_EXECUTABLE_OK\n"), 0755); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(root, "workspaces", "cos-functional", "run.sh")
	for _, mode := range []os.FileMode{0755, 0644} {
		if err := os.Chmod(script, mode); err != nil {
			t.Fatal(err)
		}
		out := runCLI("push", "--target", "allowed", "--src", source)
		if !strings.Contains(out, "workspace: synced") {
			t.Fatalf("workspace not synced: %s", out)
		}
		runCLI("pull", "--target", "allowed", "--dest", dest)
		for _, file := range []string{remote, filepath.Join(dest, "run.sh")} {
			info, err := os.Stat(file)
			if err != nil || info.Mode().Perm() != mode {
				t.Fatalf("%s mode: %v, %v", file, info, err)
			}
			out, err := exec.Command(file).CombinedOutput()
			if mode == 0755 && (err != nil || string(out) != "COS_EXECUTABLE_OK") {
				t.Fatalf("execute %s: %v %s", file, err, out)
			}
			if mode == 0644 && !os.IsPermission(err) {
				t.Fatalf("nonexecutable %s err=%v", file, err)
			}
		}
		if mode == 0755 {
			out := runCLI("exec", "--target", "allowed", "--cmd", strconv.Quote(remote))
			if !strings.Contains(out, "COS_EXECUTABLE_OK") {
				t.Fatalf("Agent command lost output: %s", out)
			}
			t.Log("0755: Agent workspace and CLI pull directly execute COS_EXECUTABLE_OK; CLI exec confirms remote output")
		} else {
			t.Log("0644: second push/pull revoke execution on existing Agent and local files; both direct executions fail with permission denied")
		}
	}
}
