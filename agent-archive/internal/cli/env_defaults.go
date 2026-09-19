package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// detectHarnesses best-effort-detects installed applications by checking
// for the same per-harness config directory internal/hooks.Plan targets
// (.codex, .claude, .cursor under the user home). A directory existing is
// not proof the application is currently installed, and its absence is not
// proof it isn't; this only pre-selects setup's prompts, which the user can
// override either way.
func detectHarnesses(userHome string) []string {
	var found []string
	for _, h := range []struct{ name, dir string }{
		{"codex", ".codex"}, {"claude", ".claude"}, {"cursor", ".cursor"},
	} {
		if info, err := os.Stat(filepath.Join(userHome, h.dir)); err == nil && info.IsDir() {
			found = append(found, h.name)
		}
	}
	return found
}

// loadLaunchAgent loads a just-installed LaunchAgent so scheduled
// collection starts immediately rather than waiting for the next login.
// This shells out to launchctl and has not been verified against a real
// launchd (see docs/agent-archive-implementation.md); a failure here is
// reported to the user as a warning, not a setup failure, since the plist
// is already written and will load on the next login regardless.
func loadLaunchAgent(plistPath string) error {
	cmd := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
	}
	return nil
}
