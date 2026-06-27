package main

import (
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
)

// debug.go is a throwaway SSH diagnostics helper. It intentionally has NO
// hard-coded credentials: supply connection details via environment variables
//
//	DOOPS_DEBUG_SSH_HOST (host:port), DOOPS_DEBUG_SSH_USER, DOOPS_DEBUG_SSH_PASSWORD
//
// so that secrets never land in version control.
func main() {
	host := os.Getenv("DOOPS_DEBUG_SSH_HOST")
	user := os.Getenv("DOOPS_DEBUG_SSH_USER")
	password := os.Getenv("DOOPS_DEBUG_SSH_PASSWORD")
	if host == "" || user == "" || password == "" {
		fmt.Fprintln(os.Stderr, "set DOOPS_DEBUG_SSH_HOST (host:port), DOOPS_DEBUG_SSH_USER and DOOPS_DEBUG_SSH_PASSWORD")
		os.Exit(2)
	}

	config := &ssh.ClientConfig{
		User: user,
		Auth: []ssh.AuthMethod{
			ssh.Password(password),
		},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
	}

	client, err := ssh.Dial("tcp", host, config)
	if err != nil {
		panic(err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		panic(err)
	}
	defer session.Close()

	out, _ := session.CombinedOutput("ls -la /tmp/doops*")
	fmt.Println(string(out))
}
