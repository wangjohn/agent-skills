package cli

import (
	"context"
	"fmt"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
// reported as an incomplete setup, with rollback and a retry path.
func loadLaunchAgent(plistPath string) error {
	cmd := exec.Command("launchctl", "bootstrap", fmt.Sprintf("gui/%d", os.Getuid()), plistPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
	}
	return nil
}

// unloadLaunchAgent undoes a successful loadLaunchAgent, used only to roll
// setup back if a later step fails after the LaunchAgent was already
// loaded. Like loadLaunchAgent, unverified against a real launchd.
func unloadLaunchAgent(plistPath string) error {
	cmd := exec.Command("launchctl", "bootout", fmt.Sprintf("gui/%d", os.Getuid()), plistPath)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout: %w: %s", err, output)
	}
	return nil
}

func (e Env) jobState(plist string) string {
	if e.JobState != nil {
		return e.JobState(plist)
	}
	// Injected schedulers are not the user's launchd.
	if e.LoadLaunchAgent != nil {
		return "missing"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "launchctl", "print", fmt.Sprintf("gui/%d/%s", os.Getuid(), strings.TrimSuffix(filepath.Base(plist), ".plist"))).CombinedOutput()
	if err != nil {
		if strings.Contains(string(output), "Could not find service") {
			return "missing"
		}
		return "unknown"
	}
	if strings.Contains(string(output), "state = running") {
		return "running"
	}
	return "loaded"
}
