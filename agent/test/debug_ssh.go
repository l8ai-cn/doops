package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"golang.org/x/crypto/ssh"
)

func SSHExec(host, user, password, port, command string) (int, string, string, error) {
	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}

	addr := net.JoinHostPort(host, port)
	client, err := ssh.Dial("tcp", addr, config)
	if err != nil {
		return 1, "", "", err
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return 1, "", "", err
	}
	defer session.Close()

	var stdout, stderr io.Reader
	stdout, _ = session.StdoutPipe()
	stderr, _ = session.StderrPipe()

	if err := session.Start(command); err != nil {
		return 1, "", "", err
	}

	go io.Copy(os.Stdout, stdout)
	go io.Copy(os.Stderr, stderr)

	err = session.Wait()
	if err != nil {
		if exitErr, ok := err.(*ssh.ExitError); ok {
			return exitErr.ExitStatus(), "", "", nil
		}
		return 1, "", "", err
	}

	return 0, "", "", nil
}

func main() {
	// No hard-coded credentials: provide them via environment variables so
	// secrets never get committed.
	host := os.Getenv("DOOPS_DEBUG_SSH_HOST")
	user := os.Getenv("DOOPS_DEBUG_SSH_USER")
	pass := os.Getenv("DOOPS_DEBUG_SSH_PASSWORD")
	port := os.Getenv("DOOPS_DEBUG_SSH_PORT")
	if port == "" {
		port = "22"
	}
	if host == "" || user == "" || pass == "" {
		fmt.Fprintln(os.Stderr, "set DOOPS_DEBUG_SSH_HOST, DOOPS_DEBUG_SSH_USER and DOOPS_DEBUG_SSH_PASSWORD (optional DOOPS_DEBUG_SSH_PORT)")
		os.Exit(2)
	}

	fmt.Println("--- REMOTE DIAGNOSTICS ---")
	SSHExec(host, user, pass, port, "date; ls -l /tmp/doops-agent; ls -ld /tmp/skills; ps aux | grep doops-agent | grep -v grep; netstat -tulpn | grep 42222; tail -n 100 /tmp/doops-agent.log")
}

