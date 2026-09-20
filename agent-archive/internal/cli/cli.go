// Package cli implements the agent-archive command-line interface: the
// hidden `_hook` and `_collect` entry points a real installation's hooks
// and LaunchAgent invoke, and the user-facing
// setup/status/sync/pause/resume/uninstall and read-only list/show
// commands. It is the only package that touches process-level state
// (args, stdio, the real clock, the real home directory) directly; every
// other package in this module stays free of that so it can be tested
// without a real environment.
package cli

import (
	"fmt"
	"io"
	"os"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
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
	// Executable returns the absolute path setup installs into hook
	// commands and the LaunchAgent. Defaults to os.Executable.
	Executable func() (string, error)
	// UserHomeDir is the real user home directory — where hook config files
	// and ~/Library/LaunchAgents live — as distinct from Home, which is
	// agent-archive's own (possibly redirected) private data directory.
	// Defaults to os.UserHomeDir.
	UserHomeDir func() (string, error)
	// DetectHarnesses best-effort detects which applications appear
	// installed under a user home directory, to pre-select setup's
	// application prompts; the user can still include or exclude any of
	// them regardless of what this reports. Defaults to detectHarnesses.
	DetectHarnesses func(userHome string) []string
	// LoadLaunchAgent loads the just-written LaunchAgent plist so scheduled
	// collection starts without a login/logout cycle. Defaults to shelling
	// out to launchctl; unverified against a real launchd (see the
	// implementation ledger).
	LoadLaunchAgent func(plistPath string) error
	// UnloadLaunchAgent undoes a successful LoadLaunchAgent: it rolls setup
	// back if a later step (config.Save) fails after the LaunchAgent was
	// already loaded, and stops the collector during uninstall. Defaults to
	// shelling out to launchctl; unverified against a real launchd, same as
	// LoadLaunchAgent.
	UnloadLaunchAgent func(plistPath string) error
	// Keychain opens the credential store setup saves R2 secrets to and
	// uninstall deletes them from.
	// Defaults to credentials.NewKeychainStore, which is only available on
	// a darwin+cgo build.
	Keychain func() (credentials.CredentialStore, error)
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

func (e Env) executable() (string, error) {
	if e.Executable != nil {
		return e.Executable()
	}
	return os.Executable()
}

func (e Env) userHomeDir() (string, error) {
	if e.UserHomeDir != nil {
		return e.UserHomeDir()
	}
	return os.UserHomeDir()
}

func (e Env) detectHarnesses(userHome string) []string {
	if e.DetectHarnesses != nil {
		return e.DetectHarnesses(userHome)
	}
	return detectHarnesses(userHome)
}

func (e Env) loadLaunchAgent(plistPath string) error {
	if e.LoadLaunchAgent != nil {
		return e.LoadLaunchAgent(plistPath)
	}
	return loadLaunchAgent(plistPath)
}

func (e Env) unloadLaunchAgent(plistPath string) error {
	if e.UnloadLaunchAgent != nil {
		return e.UnloadLaunchAgent(plistPath)
	}
	return unloadLaunchAgent(plistPath)
}

func (e Env) keychain() (credentials.CredentialStore, error) {
	if e.Keychain != nil {
		return e.Keychain()
	}
	return credentials.NewKeychainStore(credentials.KeychainService)
}

const usage = `agent-archive manages a private, local-first archive of coding-agent
sessions across Codex, Claude Code, and Cursor.

Usage:
  agent-archive setup     Guided first-time setup or safe reconfiguration
  agent-archive status    Show storage, collector, hooks, and capture coverage
  agent-archive sync      Run one collection/upload pass now
  agent-archive pause     Persistently pause collection and uploads
  agent-archive resume    Resume scheduled work
  agent-archive uninstall Remove hooks, the collector, and local state (never the bucket)
  agent-archive list      List archived sessions (metadata only)
  agent-archive show ID   Print one archived session's metadata sidecar
  agent-archive --help    Show this help
  agent-archive --version Show the version

Inspecting the archive:
  agent-archive list [--harness NAME] [--model NAME] [--skill NAME]
                     [--skill-usage used|available|eligible_no_use]
                     [--since DATE|AGE] [--complete]
  agent-archive show <archive-session-id> [--harness NAME] [--normalized]

list and show read the configured bucket and print metadata only. show
prints conversation content only with --normalized, which downloads and
verifies the session's source bundle before printing its normalized view.

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
	case "uninstall":
		return runUninstallCommand(args[1:], stdin, stdout, stderr, env)
	case "list":
		return runListCommand(args[1:], stdout, stderr, env)
	case "show":
		return runShowCommand(args[1:], stdout, stderr, env)
	default:
		fmt.Fprintf(stderr, "agent-archive: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
