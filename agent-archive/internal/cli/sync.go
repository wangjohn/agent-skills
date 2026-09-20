package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// runSyncCommand implements `agent-archive sync`: one explicit collection
// pass, preserving queued work on failure. It respects paused state and
// does not implicitly resume, per the spec.
func runSyncCommand(_ []string, stdout, stderr io.Writer, env Env) int {
	result, err := runOnePass(env, false)
	if err != nil {
		switch {
		case errors.Is(err, errPaused):
			fmt.Fprintln(stdout, "agent-archive: "+err.Error())
			return 0
		case errors.Is(err, errNotSetUp):
			fmt.Fprintln(stderr, "agent-archive: "+err.Error())
		case errors.Is(err, local.ErrBusy):
			fmt.Fprintln(stderr, "agent-archive: sync: another sync is already running")
		default:
			fmt.Fprintf(stderr, "agent-archive: sync: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "Scanned %d session(s): %d published, %d unchanged, %d failed.\n",
		result.Scanned, len(result.Published), len(result.Skipped), len(result.Errors))
	for id, sessionErr := range result.Errors {
		fmt.Fprintf(stdout, "  %s: %v\n", id, sessionErr)
	}
	if len(result.Errors) > 0 {
		return 1
	}
	return 0
}
