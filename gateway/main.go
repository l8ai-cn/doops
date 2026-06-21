package main

import (
	"os"

	"github.com/user/doops/agent/pkg/gatewaycmd"
)

func main() {
	gatewaycmd.Run(os.Args[1:])
}
