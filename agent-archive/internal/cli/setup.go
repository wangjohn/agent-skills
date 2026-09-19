package cli

import (
	"fmt"
	"io"
)

// runSetupCommand is a placeholder. The guided onboarding flow (connect
// storage, select applications and projects, install hooks and the
// LaunchAgent, verify real capture) is implemented in a follow-up PR; this
// stub exists so dispatch and --help are complete now.
func runSetupCommand(_ []string, _ io.Reader, _ io.Writer, stderr io.Writer, _ Env) int {
	fmt.Fprintln(stderr, "agent-archive: setup is not implemented in this build yet")
	return 1
}
