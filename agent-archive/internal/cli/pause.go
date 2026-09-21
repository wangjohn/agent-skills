package cli

import (
	"fmt"
	"io"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// runPauseCommand implements both `pause` and `resume`. Pausing persists
// across restarts and stops new scheduled collection, uploads, and remote
// cleanup without deleting any existing configuration or archived data.
func runPauseCommand(stdout, stderr io.Writer, env Env, paused bool) int {
	home, err := env.home()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: resolve home: %v\n", err)
		return 1
	}
	unlock, err := local.Lock(home)
	if err != nil {
		fmt.Fprintln(stderr, "Another archive operation is finishing. No settings changed; retry this command when it completes.")
		return 1
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		fmt.Fprintln(stderr, "A hook is finishing. Retry this command.")
		return 1
	}
	defer releaseHooks()
	if transactionPending(home) {
		fmt.Fprintln(stderr, "Setup needs recovery. Run agent-archive setup first.")
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "Cannot read settings: %v\n", err)
		return 1
	}
	if found && !cfg.Archive.Enabled {
		fmt.Fprintln(stderr, "Integrations are not installed. Run agent-archive setup to reinstall.")
		return 1
	}
	if _, err := config.SetPaused(home, paused); err != nil {
		fmt.Fprintf(stderr, "agent-archive: %v\n", err)
		return 1
	}
	if paused {
		fmt.Fprintln(stdout, "Paused. Run `agent-archive resume` to continue. Already registered sessions can catch up, including activity written during the pause.")
	} else {
		fmt.Fprintln(stdout, "Resumed. Registered sessions can catch up; new sessions begun while paused are not imported. Run agent-archive sync for an immediate pass.")
	}
	return 0
}
