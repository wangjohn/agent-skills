package cli

import (
	"fmt"
	"io"
	"strings"
)

var commandHelp = map[string]string{
	"setup": `Usage: agent-archive setup

Choose apps and projects, connect storage, then review and enable capture.
Run again to continue saved setup or edit capture, storage, or retention.
Credentials are entered privately; never pass them as command arguments.
Example: agent-archive setup
`,
	"status": `Usage: agent-archive status [--json]

Show local capture evidence, background health, and a next step.
No conversations are printed and no cloud request is made.
--json prints the same status as a versioned JSON document.
Example: agent-archive status --json
`,
	"sync": `Usage: agent-archive sync

Collect and upload once. Failures leave pending work available to retry.
If paused, no work is started; run agent-archive resume first.
Example: agent-archive sync
`,
	"pause": `Usage: agent-archive pause

Persistently pause collection, uploads, and cleanup. Current work must finish
before the command can confirm pause; retry if another operation is running.
Already registered sessions can catch up after resume, including activity
written during the pause. New sessions begun while paused are not imported.
Example: agent-archive pause
`,
	"resume": `Usage: agent-archive resume

Resume scheduled collection, uploads, and cleanup. Already registered sessions
can catch up, including activity written during the pause. For an immediate
pass, run agent-archive sync.
Example: agent-archive resume
`,
	"uninstall": `Usage: agent-archive uninstall [--delete-local-data]

Remove hooks and the background collector. Keep local evidence, settings,
and credentials by default, so setup can restore the installation.
--delete-local-data also removes owned local files and stored credentials,
including unpublished evidence, after a separate confirmation.
Remote archives and unrelated files are always kept.
Example: agent-archive uninstall
`,
	"list": `Usage: agent-archive list [options]

Find sessions using metadata; does not download conversation content.
  --harness codex|claude|cursor   Filter by application
  --model NAME                   Filter by model
  --skill NAME                   Filter by skill
  --skill-sha256 HEX             Filter by exact lowercase skill SHA-256
  --skill-usage used|available|eligible_no_use
                                 eligible_no_use cannot return sessions yet:
                                 no parser version records both a complete
                                 eligible-skill set and complete use
                                 observation, so non-use is never proven. The
                                 value stays accepted for forward compatibility.
  --since DATE|AGE               For example 2026-01-31 or 7d
  --complete                     Require complete parser coverage
Example: agent-archive list --skill review-pr --skill-sha256 HASH --since 7d
`,
	"show": `Usage: agent-archive show ID [--harness NAME] [--normalized]

Print session metadata as JSON. --normalized explicitly downloads and verifies
its source bundle and prints conversation content as well.
Example: agent-archive show SESSION_ID --normalized
`,
	"feedback": `Usage: agent-archive feedback ID --file PATH

Attach an explicit user assessment to a locally captured session. The file is
read locally, privacy-filtered, and queued for the next collection pass. Its
path is not archived. Collection remains paused until you resume it.
Example: agent-archive feedback SESSION_ID --file /private/path/feedback.txt
`,
}

// Preflight never resolves paths, credentials, or runtime dependencies.
func commandPreflight(args []string, out, errOut io.Writer) (bool, int) {
	cmd := args[0]
	if cmd == "help" || cmd == "--help" || cmd == "-h" {
		if len(args) == 1 {
			fmt.Fprint(out, usage)
			return true, 0
		}
		if cmd == "help" && len(args) == 2 {
			if help, ok := commandHelp[args[1]]; ok {
				fmt.Fprint(out, help)
				return true, 0
			}
		}
		fmt.Fprintln(errOut, "Use agent-archive help COMMAND.")
		return true, 2
	}
	if cmd == "version" || cmd == "--version" || cmd == "-v" {
		if len(args) != 1 {
			fmt.Fprintln(errOut, "--version takes no arguments.")
			return true, 2
		}
		return false, 0
	}
	help, public := commandHelp[cmd]
	if !public {
		return false, 0
	}
	for _, arg := range args[1:] {
		if arg == "--help" || arg == "-h" {
			fmt.Fprint(out, help)
			return true, 0
		}
	}
	if cmd == "list" || cmd == "show" || cmd == "feedback" {
		return false, 0
	} // Their parsers validate before I/O.
	allowed := ""
	if cmd == "status" {
		allowed = "--json"
	}
	if cmd == "uninstall" {
		allowed = "--delete-local-data"
	}
	if len(args) > 1 && (len(args) != 2 || allowed == "" || args[1] != allowed) {
		fmt.Fprintf(errOut, "agent-archive %s: unexpected arguments %s\nRun agent-archive %s --help.\n", cmd, strings.Join(args[1:], " "), cmd)
		return true, 2
	}
	return false, 0
}
