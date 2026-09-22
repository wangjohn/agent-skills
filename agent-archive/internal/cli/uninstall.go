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

// Uninstall leaves data and credentials available for reinstall unless the
// explicit destructive option and its separate confirmation are supplied.
func runUninstallCommand(args []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if err := uninstall(args, stdin, stdout, env); err != nil {
		fmt.Fprintf(stderr, "Uninstall incomplete: %v\n", err)
		return 1
	}
	return 0
}
func uninstall(args []string, stdin io.Reader, out io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	if err = checkRemovableHome(home, userHome); err != nil {
		return err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	if transactionPending(home) {
		return fmt.Errorf("run agent-archive setup to recover the interrupted installation first")
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return err
	}
	purge := containsString(args, "--delete-local-data")
	fmt.Fprintln(out, "Remove the archive's hooks and background collector from this Mac. Remote archives are kept.")
	if purge {
		fmt.Fprintf(out, "Also delete owned local state and credentials under %s.\n", home)
	} else {
		fmt.Fprintln(out, "Local evidence, settings, and credentials will be kept. Run setup to reinstall.")
	}
	p := newPrompter(stdin, out)
	yes, err := p.yesNo("Remove integrations?", false)
	if err != nil {
		return err
	}
	if !yes {
		fmt.Fprintln(out, "Cancelled. No changes were made.")
		return nil
	}
	previewPending := 0
	if purge {
		pending, e := pendingSessions(home, config.Config{})
		if e != nil {
			return e
		}
		previewPending = pending
		fmt.Fprintf(out, "%d pending session(s) and all owned local caches will be removed. Unpublished evidence cannot be recovered from the bucket.\n", pending)
		yes, err = p.yesNo("Delete local data and stored credentials too?", false)
		if err != nil {
			return err
		}
		if !yes {
			fmt.Fprintln(out, "Cancelled. No changes were made.")
			return nil
		}
	}
	unlock, err := local.Lock(home)
	if err != nil {
		return fmt.Errorf("another operation is finishing; retry uninstall: %w", err)
	}
	defer unlock()
	releaseHooks, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		return err
	}
	defer releaseHooks()
	// Reload after acquiring the lock; pause/resume may have completed meanwhile.
	cfg, found, err = config.Load(home)
	if err != nil {
		return err
	}
	if purge {
		pending, e := pendingSessions(home, config.Config{})
		if e != nil {
			return e
		}
		if pending > previewPending {
			return fmt.Errorf("new pending evidence appeared while confirming; rerun uninstall to review it")
		}
	}
	changes, err := hooks.PlanRemoval(userHome, allHarnesses)
	if err != nil {
		return err
	}
	plist := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	state := env.jobState(plist)
	if state == "unknown" {
		return fmt.Errorf("cannot determine background job state; restore access to launchctl and retry")
	}
	if state == "running" || state == "loaded" {
		if err = env.unloadLaunchAgent(plist); err != nil {
			return fmt.Errorf("stop collector: %w", err)
		}
	}
	// Disable capture before removing hooks. A partial uninstall remains safely disabled.
	if found {
		cfg.Archive.Enabled = false
		if err = config.Save(home, cfg); err != nil {
			return err
		}
	}
	if err = hooks.Apply(changes); err != nil {
		return err
	}
	if err = os.Remove(plist); err != nil && !os.IsNotExist(err) {
		return err
	}
	if purge {
		refs := map[string]bool{}
		for _, ref := range cfg.RetiredCredentialRefs {
			refs[ref] = true
		}
		if cfg.Storage.R2CredentialRef != "" {
			refs[cfg.Storage.R2CredentialRef] = true
		}
		for _, old := range cfg.PreviousDestinations {
			if old.R2CredentialRef != "" {
				refs[old.R2CredentialRef] = true
			}
		}
		var draft setupDraft
		if e := local.Read(filepath.Join(home, "setup-draft.json"), &draft); e == nil && draft.CredentialRef != "" {
			refs[draft.CredentialRef] = true
			for _, ref := range draft.StagedRefs {
				refs[ref] = true
			}
		} else if e != nil && !os.IsNotExist(e) {
			return e
		}
		if len(refs) > 0 {
			kc, e := env.keychain()
			if e != nil {
				return e
			}
			for ref := range refs {
				if e = kc.Delete(context.Background(), ref); e != nil && !errors.Is(e, credentials.ErrMissingCredential) {
					return e
				}
			}
		}
		leftovers, e := removeLocalState(home)
		if e != nil {
			return e
		}
		if len(leftovers) > 0 {
			return fmt.Errorf("unrelated files were kept in %s: %s", home, strings.Join(leftovers, ", "))
		}
	}
	fmt.Fprintln(out, "Uninstall complete. Remote archives and the CLI executable were kept.")
	return nil
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
	"config.json", "setup-draft.json", "setup-transaction.json",
	"registrations", "requests", "request-locks", "published", "pending", "sessions", "superseded", "pending-scans", "subagent-candidates",
	"status.json", "storage-health.json", "capture-diagnostics.json", "application-versions.json",
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
		if name == "setup.lock" || name == "hooks.lock" || name == "collector.lock" {
			continue
		}
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
	// Lock files remain to avoid replacing an inode still locked by another process.
	return nil, nil
}
