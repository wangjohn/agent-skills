package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
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
		lockErr := fmt.Errorf("acquire lock: %w", err)
		recordPreflightError(localStore, lockErr)
		return collector.Result{}, lockErr
	}
	defer unlock()

	objectStore, err := env.openStore(cfg)
	if err != nil {
		storeErr := fmt.Errorf("open storage: %w", err)
		recordPreflightError(localStore, storeErr)
		return collector.Result{}, storeErr
	}
	return collector.Run(context.Background(), localStore, objectStore, collector.Options{MachineID: cfg.MachineID, Now: env.Now})
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
