package cli

import (
	"fmt"
	"io"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
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
	if _, err := config.SetPaused(home, paused); err != nil {
		fmt.Fprintf(stderr, "agent-archive: %v\n", err)
		return 1
	}
	if paused {
		fmt.Fprintln(stdout, "Paused. Run `agent-archive resume` to continue.")
	} else {
		fmt.Fprintln(stdout, "Resumed.")
	}
	return 0
}
