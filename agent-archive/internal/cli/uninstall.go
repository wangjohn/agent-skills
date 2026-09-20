package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// runUninstallCommand reverses what setup installed on this Mac: it stops
// and removes the LaunchAgent, strips only our own handlers from each
// included application's hook configuration, deletes the R2 secret setup
// stored in Keychain (when the configuration references one), and finally
// deletes the private data directory (config, per-session cache, logs).
//
// Nothing is touched until the user confirms the printed plan. Remote
// objects in the bucket are never read, listed, or deleted: the archive
// itself stays exactly where it is, and the binary is left in place for the
// user to remove. Each step after confirmation is best-effort and
// independent, so one failing does not stop the others from being tried;
// the exit code reports whether everything succeeded.
func runUninstallCommand(_ []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	ctx := context.Background()
	home, err := env.home()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: uninstall: resolve home: %v\n", err)
		return 1
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: uninstall: resolve user home: %v\n", err)
		return 1
	}
	if err := checkRemovableHome(home, userHome); err != nil {
		fmt.Fprintf(stderr, "agent-archive: uninstall: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: uninstall: load config: %v\n", err)
		return 1
	}

	// Which hook files to inspect. Without a config (setup never finished,
	// or its rollback did not) every supported application is checked, which
	// is safe because removal only ever strips our own marker entries.
	harnesses := cfg.Harnesses
	if !found || len(harnesses) == 0 {
		harnesses = allHarnesses
	}
	hookChanges, err := hooks.PlanRemoval(userHome, harnesses)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: uninstall: plan hook removal: %v\n", err)
		return 1
	}
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	plistExists := false
	if _, err := os.Stat(plistPath); err == nil {
		plistExists = true
	} else if !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "agent-archive: uninstall: check LaunchAgent: %v\n", err)
		return 1
	}
	credentialRef := ""
	if found && cfg.Storage.Provider == credentials.ProviderR2 {
		credentialRef = cfg.Storage.R2CredentialRef
	}

	if !found {
		fmt.Fprintln(stdout, "No agent-archive configuration found; removing any leftover pieces.")
	}
	fmt.Fprintln(stdout, "\nThis will remove:")
	if plistExists {
		fmt.Fprintf(stdout, "  - the background collector LaunchAgent (%s)\n", plistPath)
	} else {
		fmt.Fprintln(stdout, "  - the background collector LaunchAgent (not installed)")
	}
	if len(hookChanges) > 0 {
		var paths []string
		for _, c := range hookChanges {
			paths = append(paths, c.Path)
		}
		fmt.Fprintf(stdout, "  - agent-archive's own hook entries from %s (unrelated hooks are kept)\n", strings.Join(paths, ", "))
	} else {
		fmt.Fprintln(stdout, "  - agent-archive's own hook entries (none installed)")
	}
	if credentialRef != "" {
		fmt.Fprintf(stdout, "  - the stored R2 credentials in Keychain (%s / %s)\n", credentials.KeychainService, credentialRef)
	}
	fmt.Fprintf(stdout, "  - local state: config, per-session cache, and logs under %s\n", home)
	fmt.Fprintln(stdout, "\nNothing in your bucket is touched: every archived session stays exactly where it is.")

	p := newPrompter(stdin, stdout)
	confirm, err := p.yesNo("\nRemove agent-archive from this Mac?", false)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: uninstall: %v\n", err)
		return 1
	}
	if !confirm {
		fmt.Fprintln(stdout, "Cancelled. No changes were made.")
		return 0
	}

	var removed, failed []string
	fail := func(what string, err error) {
		fmt.Fprintf(stderr, "agent-archive: uninstall: %s: %v\n", what, err)
		failed = append(failed, what)
	}

	// 1. Stop and remove the LaunchAgent first, so no further scheduled
	// collection starts while the rest is being taken apart.
	if plistExists {
		if err := env.unloadLaunchAgent(plistPath); err != nil {
			// Not fatal: the plist may simply not be loaded (setup's own load
			// attempt failed, or a previous uninstall stopped partway).
			fmt.Fprintf(stdout, "Warning: could not stop the background collector: %v\n", err)
		}
		if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
			fail("remove LaunchAgent plist", err)
		} else {
			removed = append(removed, "background collector LaunchAgent")
		}
	}

	// 2. Hooks: all-or-nothing across files. Apply refuses a file edited
	// since the plan was made and rolls back any it already rewrote.
	if len(hookChanges) > 0 {
		if err := hooks.Apply(hookChanges); err != nil {
			fail("remove hook entries", err)
		} else {
			removed = append(removed, "hook entries")
		}
	}

	// 3. Keychain. Only the reference is ever printed, never the secret.
	if credentialRef != "" {
		keychain, err := env.keychain()
		if err != nil {
			fail("open Keychain", err)
		} else if err := keychain.Delete(ctx, credentialRef); err != nil && !errors.Is(err, credentials.ErrMissingCredential) {
			fail("delete stored R2 credentials", err)
		} else {
			removed = append(removed, "stored R2 credentials")
		}
	}

	// 4. Local state. A collector pass still finishing (the LaunchAgent was
	// only just unloaded) must not have its directory deleted underneath it.
	unlock, err := local.Lock(home)
	if err != nil {
		if errors.Is(err, local.ErrBusy) {
			err = errors.New("a collector pass is still running; wait a moment and run `agent-archive uninstall` again to remove local state")
		}
		fail("remove local state", err)
	} else {
		leftover, removeErr := removeLocalState(home)
		unlock()
		if removeErr != nil {
			fail("remove local state", removeErr)
		} else if len(leftover) > 0 {
			fail("remove local state", fmt.Errorf("%s still contains entries agent-archive did not create (%s); remove it yourself once you have checked them", home, strings.Join(leftover, ", ")))
		} else {
			removed = append(removed, "local state ("+home+")")
		}
	}

	fmt.Fprintln(stdout)
	if len(removed) > 0 {
		fmt.Fprintf(stdout, "Removed: %s.\n", strings.Join(removed, "; "))
	} else {
		fmt.Fprintln(stdout, "Nothing was removed.")
	}
	if executable, err := env.executable(); err == nil {
		fmt.Fprintf(stdout, "The agent-archive binary was left in place at %s; delete it yourself if you no longer want it.\n", executable)
	}
	if len(failed) > 0 {
		fmt.Fprintf(stderr, "agent-archive: uninstall: could not complete: %s. See agent-archive/docs/install.md for the manual steps.\n", strings.Join(failed, "; "))
		return 1
	}
	fmt.Fprintln(stdout, "Uninstall complete.")
	return 0
}

// checkRemovableHome refuses to touch a data directory that is obviously
// not agent-archive's own: AGENT_ARCHIVE_HOME pointed at the user's home
// directory or a filesystem root. removeLocalState is the second guard: it
// only ever deletes entries agent-archive itself creates.
func checkRemovableHome(home, userHome string) error {
	clean := filepath.Clean(home)
	if clean == filepath.Clean(userHome) || filepath.Dir(clean) == clean {
		return fmt.Errorf("refusing to remove %s: not an agent-archive data directory", home)
	}
	return nil
}

// localStateEntries is every top-level entry agent-archive creates under its
// data directory: internal/config's config.json, collector.LocalStore's
// per-session directories, the lineage ledger, local.Lock's lock file, the
// collector status file, and the LaunchAgent's log files. Keep it in sync
// with those packages; an entry missing here is left behind by uninstall
// (and reported), never silently deleted.
var localStateEntries = []string{
	"config.json",
	"registrations", "requests", "published", "sessions", "superseded",
	"status.json",
	"collector.lock", "collector.log", "collector-error.log",
}

// removeLocalState deletes agent-archive's own entries under home (see
// localStateEntries, plus local.WriteBytes's ".pending-*" temp files) and
// then the directory itself. It never deletes anything else: a data
// directory a user pointed AGENT_ARCHIVE_HOME at may hold their own files,
// and those are returned as leftover, with the directory left in place,
// rather than removed.
func removeLocalState(home string) (leftover []string, err error) {
	entries, err := os.ReadDir(home)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	known := map[string]bool{}
	for _, name := range localStateEntries {
		known[name] = true
	}
	for _, entry := range entries {
		name := entry.Name()
		if !known[name] && !strings.HasPrefix(name, ".pending-") {
			leftover = append(leftover, name)
			continue
		}
		if err := os.RemoveAll(filepath.Join(home, name)); err != nil {
			return nil, err
		}
	}
	if len(leftover) > 0 {
		return leftover, nil
	}
	return nil, os.Remove(home)
}
