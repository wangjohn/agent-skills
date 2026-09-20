// Command agent-archive is the CLI entry point. It stays a thin wrapper:
// all behavior lives in internal/cli so it can be tested without a real
// process, stdio, or home directory.
package main

import (
	"os"

	"github.com/wangjohn/agent-skills/agent-archive/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, cli.Env{}))
}
