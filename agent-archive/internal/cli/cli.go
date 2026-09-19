// Package cli implements the agent-archive command-line interface: the
// hidden `_hook` and `_collect` entry points a real installation's hooks
// and LaunchAgent invoke, and the user-facing setup/status/sync/pause/resume
// commands. It is the only package that touches process-level state
// (args, stdio, the real clock, the real home directory) directly; every
// other package in this module stays free of that so it can be tested
// without a real environment.
package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// Version is the released version string. main overrides it via
// -ldflags "-X .../cli.Version=..." at build time (see the Distribution PR);
// it stays "dev" for a local build.
var Version = "dev"

// Env carries the process-level dependencies a command needs, so tests can
// substitute a temporary home directory, a fixed clock, and an in-memory
// object store. A nil field defaults to the real thing.
type Env struct {
	Home func() (string, error)
	Now  func() time.Time
	// OpenStore builds the object store a collector pass publishes to, from
	// this machine's configured storage destination. Defaults to
	// openConfiguredStore, which resolves real AWS/R2 credentials.
	OpenStore func(config.Config) (storage.ObjectStore, error)
}

func (e Env) home() (string, error) {
	if e.Home != nil {
		return e.Home()
	}
	return local.Home()
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e Env) openStore(cfg config.Config) (storage.ObjectStore, error) {
	if e.OpenStore != nil {
		return e.OpenStore(cfg)
	}
	return openConfiguredStore(cfg)
}

const usage = `agent-archive manages a private, local-first archive of coding-agent
sessions across Codex, Claude Code, and Cursor.

Usage:
  agent-archive setup     Guided first-time setup or safe reconfiguration
  agent-archive status    Show storage, collector, hooks, and capture coverage
  agent-archive sync      Run one collection/upload pass now
  agent-archive pause     Persistently pause collection and uploads
  agent-archive resume    Resume scheduled work
  agent-archive --help    Show this help
  agent-archive --version Show the version

No account or hosted service is used. You supply your own private
Cloudflare R2 or Amazon S3 bucket during setup.
`

// Run dispatches one CLI invocation and returns a process exit code. It
// never panics on malformed input; every command reports a problem through
// stderr and a nonzero exit code instead.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return 0
	case "-v", "--version", "version":
		fmt.Fprintln(stdout, Version)
		return 0
	case "_hook":
		return runHookCommand(args[1:], stdin, stderr, env)
	case "_collect":
		return runCollectCommand(args[1:], stdout, stderr, env)
	case "status":
		return runStatusCommand(args[1:], stdout, stderr, env)
	case "sync":
		return runSyncCommand(args[1:], stdout, stderr, env)
	case "pause":
		return runPauseCommand(stdout, stderr, env, true)
	case "resume":
		return runPauseCommand(stdout, stderr, env, false)
	case "setup":
		return runSetupCommand(args[1:], stdin, stdout, stderr, env)
	default:
		fmt.Fprintf(stderr, "agent-archive: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
