package server

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func isolateSSHExitTestClient(t *testing.T) {
	t.Helper()
	client, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("SSH lifecycle tests require openssh-client: ", err)
	}
	version, err := exec.Command(client, "-V").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "OpenSSH_") {
		t.Fatalf("SSH lifecycle tests require real OpenSSH: %s, %v", version, err)
	}
	// Keep the real client, but never read operator SSH config/keys or write known_hosts.
	dir := t.TempDir()
	script := "#!/bin/sh\nexec " + shellQuote(client) +
		" -F /dev/null -oUserKnownHostsFile=/dev/null -oGlobalKnownHostsFile=/dev/null" +
		" -oIdentityAgent=none -oIdentityFile=none \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("DOOPS_WORKSPACE_ROOT", t.TempDir())
}

func startSSHExitTestServer(t *testing.T, run func(*ssh.ServerConn, ssh.Channel, string)) []string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-done:
		case <-time.After(12 * time.Second):
			t.Error("loopback SSH server did not stop")
		}
	})
	go func() {
		defer close(done)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		server, channels, requests, err := ssh.NewServerConn(conn, config)
		if err != nil {
			t.Error("loopback SSH handshake: ", err)
			return
		}
		defer server.Close()
		go ssh.DiscardRequests(requests)
		for incoming := range channels {
			if incoming.ChannelType() != "session" {
				_ = incoming.Reject(ssh.UnknownChannelType, "session required")
				continue
			}
			channel, requests, err := incoming.Accept()
			if err != nil {
				t.Error(err)
				return
			}
			defer channel.Close()
			for request := range requests {
				if request.Type != "exec" {
					_ = request.Reply(false, nil)
					continue
				}
				var payload struct{ Command string }
				if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
					t.Error(err)
					return
				}
				_ = request.Reply(true, nil)
				run(server, channel, payload.Command)
				// Keep the transport alive until OpenSSH consumes exit-status
				// and closes the session; an immediate TCP close can race it.
				_ = channel.Close()
				var networkErr net.Error
				if err := server.Wait(); errors.As(err, &networkErr) && networkErr.Timeout() {
					t.Error("loopback SSH client did not close the connection: ", err)
				}
				return
			}
		}
	}()
	return []string{"-p", fmt.Sprint(listener.Addr().(*net.TCPAddr).Port),
		"-oBatchMode=yes", "-oStrictHostKeyChecking=no", "-oConnectTimeout=3"}
}

func startSSHExitTestAgent(t *testing.T, f *executionLifecycleFixture) {
	t.Helper()
	agent := &Gateway{}
	conn := dialFakeGatewayAgent(t, f.server.URL, "dev", "local")
	done := make(chan struct{})
	go func() {
		defer close(done)
		agent.ServeGatewayWebSocketConnWithReady(conn, "ssh-exit-agent", nil)
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("real Agent did not stop")
		}
	})
	waitForGatewayAgent(t, f.hub, "dev", "local")
}

func TestSSHExitLifecycleOpenSSH(t *testing.T) {
	isolateSSHExitTestClient(t)
	for _, tc := range []struct {
		name       string
		command    []string
		exitCode   int
		disconnect bool
	}{
		{"false", []string{"false"}, 1, false},
		{"exit42", []string{"sh", "-c", "exit 42"}, 42, false},
		{"exit254", []string{"sh", "-c", "exit 254"}, 254, false},
		{"exit255", []string{"sh", "-c", "exit 255"}, 255, false},
		{"disconnect", []string{"true"}, 255, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := startSSHExitTestServer(t, func(server *ssh.ServerConn, channel ssh.Channel, command string) {
				if tc.disconnect {
					_ = server.Close()
					return
				}
				cmd := exec.Command("sh", "-c", command)
				cmd.Stdout, cmd.Stderr = channel, channel.Stderr()
				err := cmd.Run()
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.exitCode {
					t.Errorf("loopback command exit = %v, want %d", err, tc.exitCode)
					return
				}
				if _, err := channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(tc.exitCode)})); err != nil {
					t.Error(err)
				}
			})
			f := newExecutionLifecycleFixture(t, GatewayHubOptions{})
			startSSHExitTestAgent(t, f)
			client := f.client(t, f.admin)
			lifecycleSend(t, client, 2, "doops_ssh", "ssh-exit", map[string]interface{}{
				"host": "test@127.0.0.1", "options": options, "command": tc.command,
			})
			wantError := ""
			if tc.exitCode == 255 {
				wantError = "execution_outcome_unknown"
			}
			response := lifecycleResponse(t, client, 2, wantError)
			if wantError == "" {
				result := response["result"].(map[string]interface{})
				if result["isError"] != true || result["sshExitCode"] != float64(tc.exitCode) {
					t.Fatalf("missing structured nonzero exit evidence: %#v", response)
				}
			}
			requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
			audits, err := f.store.ListAudit(10)
			wantStatus := "error"
			if wantError != "" {
				wantStatus = "outcome_unknown"
			}
			if err != nil || len(audits) != 1 || audits[0].Status != wantStatus {
				t.Fatalf("SSH audit = %#v, %v; want %s", audits, err, wantStatus)
			}
			originalAudits := audits
			lifecycleSend(t, client, 3, "doops_shell", "after-ssh", map[string]interface{}{"command": "true"})
			next := lifecycleResponse(t, client, 3, "")
			if next["result"].(map[string]interface{})["isError"] == true {
				t.Fatalf("exec after SSH nonzero failed: %#v", next)
			}
			requireEventually(t, 3*time.Second, func() bool { return len(f.hub.listActiveOperations()) == 0 })
			audits, err = f.store.ListAudit(10)
			if err != nil || len(audits) != len(originalAudits)+1 || audits[0].Status != "success" ||
				!reflect.DeepEqual(audits[1:], originalAudits) {
				t.Fatalf("independent command did not add exactly one success audit while preserving original SSH audit: before=%#v, after=%#v, %v", originalAudits, audits, err)
			}
		})
	}
}
