package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/evidence"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/retention"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

var (
	errNotSetUp = errors.New("not set up yet; run `agent-archive setup` first")
	errPaused   = errors.New("collection is paused; run `agent-archive resume` first")
)

// runCollectCommand implements the hidden `_collect` entry point
// install.LaunchAgent schedules every 60 seconds. Unlike `sync`, it never
// reports "already running" as a problem: a scheduled tick finding the
// previous one still working is the lock doing its job, not an error.
func runCollectCommand(_ []string, _ io.Writer, stderr io.Writer, env Env) int {
	_, err := runOnePass(env, true)
	if err != nil {
		if errors.Is(err, errNotSetUp) || errors.Is(err, errPaused) {
			return 0
		}
		fmt.Fprintf(stderr, "agent-archive: collect: %v\n", err)
		return 1
	}
	return 0
}

// runOnePass loads configuration, honors pause, takes the machine lock, and
// runs one collector.Run pass. quietOnBusy controls whether a contended lock
// is reported as an error or treated as an expected, silent no-op.
//
// Once localStore exists, any failure before collector.Run gets its own
// chance to record Status is written into that same Status's LastError.
// Without this, a broken lock or bad storage credentials would fail every
// scheduled _collect tick while `status` kept reporting the last successful
// scan's LastError (typically empty), leaving a misconfigured install
// looking healthy.
func runOnePass(env Env, quietOnBusy bool) (collector.Result, error) {
	home, err := env.home()
	if err != nil {
		return collector.Result{}, fmt.Errorf("resolve home: %w", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return collector.Result{}, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return collector.Result{}, errNotSetUp
	}
	if cfg.Paused {
		return collector.Result{}, errPaused
	}

	localStore, err := collector.NewLocalStore(home)
	if err != nil {
		return collector.Result{}, fmt.Errorf("open local store: %w", err)
	}

	unlock, err := local.Lock(home)
	if err != nil {
		if errors.Is(err, local.ErrBusy) {
			if quietOnBusy {
				return collector.Result{}, nil
			}
			return collector.Result{}, err
		}
		// Not recorded into Status here: without the lock, a concurrent
		// holder's own SaveStatus (from collector.Run or this same
		// function) could race an unguarded read-modify-write to
		// status.json and lose an update. Every failure below this point
		// runs only after the lock is held, so it can record safely.
		return collector.Result{}, fmt.Errorf("acquire lock: %w", err)
	}
	defer unlock()
	if transactionPending(home) {
		return collector.Result{}, fmt.Errorf("setup needs recovery; run agent-archive setup")
	}
	cfg, found, err = config.Load(home)
	if err != nil {
		return collector.Result{}, err
	}
	if !found || !cfg.Archive.Enabled {
		return collector.Result{}, errNotSetUp
	}
	if cfg.Paused {
		return collector.Result{}, errPaused
	}

	objectStore, err := env.openStore(cfg)
	if err != nil {
		storeErr := fmt.Errorf("open storage: %w", err)
		recordPreflightError(localStore, storeErr)
		_ = recordStorageHealth(home, cfg, env, quietOnBusy, "credentials_unavailable")
		return collector.Result{}, storeErr
	}
	if quietOnBusy {
		var prior storageHealth
		healthErr := local.Read(filepath.Join(home, "storage-health.json"), &prior)
		if healthErr != nil || prior.ConfigurationID != configurationID(cfg) || prior.Context != "background_collector" || prior.State != "verified" || env.now().Sub(prior.CheckedAt) > 5*time.Minute {
			probeErr := storage.VerifyAccess(context.Background(), objectStore, "")
			state := "verified"
			if probeErr != nil {
				state = storageFailureState(probeErr)
			}
			if err := recordStorageHealth(home, cfg, env, true, state); err != nil {
				return collector.Result{}, err
			}
			if probeErr != nil {
				recordPreflightError(localStore, fmt.Errorf("background storage access failed; restore credentials or connectivity and retry"))
				return collector.Result{}, probeErr
			}
		}
	}

	result, err := collector.Run(context.Background(), localStore, objectStore, collector.Options{
		MachineID:            cfg.MachineID,
		SupplementalEvidence: skillObserver(env),
		AcceptSession:        cfg.AcceptSession,
		Now:                  env.Now,
		RequireSkillUse:      cfg.RequireSkillUse,
	})
	if err != nil {
		return result, err
	}

	if err := verifyPublications(home, cfg, env, localStore, objectStore, &result); err != nil {
		return result, err
	}
	health := "not_checked"
	if len(result.Published) > 0 {
		health = "verified"
	}
	for _, sessionErr := range result.Errors {
		if state := storageFailureState(sessionErr); state != "storage_unavailable" {
			health = state
			break
		}
		health = "not_checked"
	}
	if health != "not_checked" {
		if err := recordStorageHealth(home, cfg, env, quietOnBusy, health); err != nil {
			return result, err
		}
	}
	if len(result.Errors) > 0 {
		state, readErr := localStore.LoadStatus()
		if readErr != nil {
			return result, readErr
		}
		state.SessionIssues = map[string]string{}
		for id, issue := range result.Errors {
			code := "capture_or_publication_failed"
			switch {
			case strings.Contains(issue.Error(), "truncated, compacted, or rewritten"):
				code = "transcript_discontinuity"
			case strings.Contains(issue.Error(), "collection limit"):
				code = "transcript_size_limit"
			case strings.Contains(issue.Error(), "read-back verification"):
				code = "read_back_failed"
			}
			state.SessionIssues[id] = code
		}
		if err := localStore.SaveStatus(state); err != nil {
			return result, err
		}
		recordPreflightError(localStore, fmt.Errorf("%d session(s) need capture, publication, or read-back verification", len(result.Errors)))
	}

	sweepResult, sweepErr := retention.Sweep(context.Background(), localStore, objectStore, retention.Options{
		Now:           env.Now,
		AcceptSession: cfg.AcceptSession,
		SessionMaxAge: time.Duration(cfg.RetentionDays) * 24 * time.Hour,
	})
	if sweepErr != nil {
		return result, fmt.Errorf("collection succeeded but retention cleanup failed: %w", sweepErr)
	}
	if len(sweepResult.Errors) > 0 {
		recordRetentionErrors(localStore, &result, sweepResult)
	}
	return result, nil
}

// recordRetentionErrors merges retention.Sweep's per-session failures into
// result, so sync's existing report/exit-code logic (which only knows about
// collector.Result) covers them too without its own retention-specific
// path, and updates Status.LastError the same way collector.Run already
// does for its own per-session errors — otherwise a retention failure would
// never reach `status` at all, since, unlike collector.Run, Sweep does not
// persist a Status of its own.
func recordRetentionErrors(localStore *collector.LocalStore, result *collector.Result, sweep retention.Result) {
	if result.Errors == nil {
		result.Errors = map[string]error{}
	}
	for id, sweepErr := range sweep.Errors {
		result.Errors[id] = fmt.Errorf("retention: %w", sweepErr)
	}
	status, err := localStore.LoadStatus()
	if err != nil {
		return
	}
	status.LastError = fmt.Sprintf("%d session(s) failed to scan, publish, or clean up", len(result.Errors))
	_ = localStore.SaveStatus(status)
}

// recordPreflightError persists a failure that happened before collector.Run
// could record its own Status, so `status` reflects it. Best-effort: if the
// status write itself fails, the original error is still what the caller
// returns and reports.
func recordPreflightError(localStore *collector.LocalStore, preflightErr error) {
	status, err := localStore.LoadStatus()
	if err != nil {
		return
	}
	status.LastError = preflightErr.Error()
	_ = localStore.SaveStatus(status)
}

// openConfiguredStore resolves cfg.Storage into a live ObjectStore. A
// Keychain being unavailable (a non-darwin build, or cgo disabled) is only
// fatal if the configured provider is R2 and therefore actually needs it;
// storage.NewConfiguredStore surfaces that.
func openConfiguredStore(cfg config.Config) (storage.ObjectStore, error) {
	keychain, keychainErr := credentials.NewKeychainStore(credentials.KeychainService)
	if keychainErr != nil && cfg.Storage.Provider == credentials.ProviderR2 {
		return nil, fmt.Errorf("keychain unavailable: %w", keychainErr)
	}
	return storage.NewConfiguredStore(context.Background(), cfg.Storage, keychain)
}

// skillObserver shares bounded observations within a pass: user-scope skill
// roots are read once per harness, project-scope roots once per
// harness/project. Each read carries its own instruction-content cap, so a
// pass reads at most that cap per harness plus that cap per project.
func skillObserver(env Env) func(archive.SessionRegistration, time.Time) ([]archive.SupplementalEvidence, error) {
	userCache := map[string][]archive.SupplementalEvidence{}
	projectCache := map[string][]archive.SupplementalEvidence{}
	return func(reg archive.SessionRegistration, at time.Time) ([]archive.SupplementalEvidence, error) {
		userScope, ok := userCache[reg.Harness.Name]
		if !ok {
			userHome, err := env.userHomeDir()
			if err != nil {
				return nil, err
			}
			userScope, err = evidence.ObserveSkills(evidence.SkillOptions{Harness: reg.Harness.Name, UserHome: userHome, ObservedAt: at})
			if err != nil {
				return nil, err
			}
			userCache[reg.Harness.Name] = userScope
		}
		projectKey := reg.Harness.Name + "\x00" + reg.ProjectRoot
		projectScope, ok := projectCache[projectKey]
		if !ok {
			var err error
			projectScope, err = evidence.ObserveSkills(evidence.SkillOptions{Harness: reg.Harness.Name, ProjectRoot: reg.ProjectRoot, ObservedAt: at})
			if err != nil {
				return nil, err
			}
			projectCache[projectKey] = projectScope
		}
		return append(append([]archive.SupplementalEvidence(nil), userScope...), projectScope...), nil
	}
}
