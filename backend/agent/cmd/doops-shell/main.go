package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/user/doops/agent/internal/dispatcher"
)

func main() {
	var cmd *exec.Cmd
	var err error
	switch {
	case len(os.Args) == 3 && os.Args[1] == "-c":
		cmd, err = dispatcher.ShellCommand(os.Args[2], true)
	case len(os.Args) >= 3 && os.Args[1] == "--argv":
		cmd, err = dispatcher.CommandContext(context.Background(), os.Args[2], os.Args[3:]...)
	default:
		fmt.Fprintln(os.Stderr, "usage: doops-shell -c command | --argv program [arg ...]")
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if cmd.Err != nil {
		fmt.Fprintln(os.Stderr, cmd.Err)
		os.Exit(1)
	}
	for _, key := range []string{"DO_AGENT_MUTATION_AUTHORITY_SECRET", "DO_AGENT_MUTATION_AUTHORITY_TOKEN"} {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	if err := syscall.Exec(cmd.Path, cmd.Args, os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
