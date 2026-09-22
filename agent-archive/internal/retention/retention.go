// Package retention deletes local-machine-owned archive data that has aged
// out: source snapshots superseded more than a grace period ago, and,
// separately, entire sessions once their most recently captured evidence is
// older than a configured retention window.
//
// Before deleting anything it fetches the session's remote metadata.json
// and treats that, not the local cache, as the current pointer: the current
// source is never deleted regardless of age, and neither is its immediate
// predecessor (the most recently superseded snapshot), so a reader that
// just fetched metadata always has a snapshot to fall back to. Whole-session
// expiry deletes metadata before sources, so an interruption leaves at
// worst unreferenced objects, never a live pointer to missing data; a
// failed expiry keeps the local registration (ownership) so the next sweep
// retries it. It never considers another machine's sessions — registrations
// only ever exist on the machine that created them.
package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// Options configures one Sweep call.
type Options struct {
	AcceptSession func(archive.SessionRegistration) bool
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// GracePeriod bounds how long a superseded (no longer current) source
	// snapshot stays downloadable before deletion. It applies only to
	// snapshots older than the immediate predecessor of the current source,
	// which is retained regardless of age. Defaults to 24h, matching the
	// spec's proposed grace period.
	GracePeriod time.Duration
	// SessionMaxAge is whole-session retention: once a session's most
	// recently captured evidence is older than this, its metadata and every
	// source snapshot are deleted together, never a source-only rule that
	// could leave a live metadata pointer dangling. Zero disables
	// whole-session deletion.
	SessionMaxAge time.Duration
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) gracePeriod() time.Duration {
	if o.GracePeriod > 0 {
		return o.GracePeriod
	}
	return 24 * time.Hour
}

// Result summarizes one Sweep call.
type Result struct {
	DeletedSnapshots int
	DeletedSessions  []string
	Errors           map[string]error
}

// Sweep processes every session registered on this machine. One session's
// failure is isolated in Result.Errors and left to retry on the next call,
// like collector.Run.
func Sweep(ctx context.Context, local *collector.LocalStore, store storage.ObjectStore, opts Options) (Result, error) {
	now := opts.now()
	result := Result{Errors: map[string]error{}}

	registrations, err := local.LoadRegistrations()
	if err != nil {
		return Result{}, fmt.Errorf("load registrations: %w", err)
	}
	for _, reg := range registrations {
		if opts.AcceptSession != nil && !opts.AcceptSession(reg) {
			continue
		}
		if err := sweepSession(ctx, local, store, reg, opts, now, &result); err != nil {
			result.Errors[reg.ArchiveSessionID] = err
		}
	}
	return result, nil
}

func sweepSession(ctx context.Context, local *collector.LocalStore, store storage.ObjectStore, reg archive.SessionRegistration, opts Options, now time.Time, result *Result) error {
	bundle, _, _, found, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		return fmt.Errorf("load published cache: %w", err)
	}

	superseded, err := local.LoadSuperseded(reg.ArchiveSessionID)
	if err != nil {
		return fmt.Errorf("load superseded sources: %w", err)
	}
	locallyExpired := opts.SessionMaxAge > 0 && found && !bundle.Capture.CapturedAt.IsZero() && now.Sub(bundle.Capture.CapturedAt) >= opts.SessionMaxAge
	// Skip the remote round trip when this pass could not delete anything:
	// the predecessor stays in the ledger forever, so without this every
	// session ever republished would cost one GET per sync indefinitely.
	if !locallyExpired && !anySupersededExpirable(superseded, now, opts.gracePeriod()) {
		return nil
	}

	metadataKey, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return err
	}
	data, remoteErr := store.Get(ctx, metadataKey)
	var metadata archive.Metadata
	if remoteErr == nil {
		if err := json.Unmarshal(data, &metadata); err != nil {
			return fmt.Errorf("decode current metadata: %w", err)
		}
		if err := metadata.ValidateSourceReference(); err != nil {
			return fmt.Errorf("invalid current metadata: %w", err)
		}
		if metadata.SessionID != reg.ArchiveSessionID || metadata.Harness.Name != reg.Harness.Name || !strings.HasPrefix(metadata.SourceBundle.Key, fmt.Sprintf("sessions/%s/%s/", reg.Harness.Name, reg.ArchiveSessionID)) {
			return fmt.Errorf("current metadata belongs to another session")
		}
	} else if !errors.Is(remoteErr, storage.ErrNotFound) {
		return fmt.Errorf("read current metadata before cleanup: %w", remoteErr)
	}
	// Protect evidence published remotely just before a local acknowledgement failed.
	capturedAt := bundle.Capture.CapturedAt
	if remoteErr == nil && metadata.CapturedAt.After(capturedAt) {
		capturedAt = metadata.CapturedAt
	}

	if opts.SessionMaxAge > 0 && found && !capturedAt.IsZero() && now.Sub(capturedAt) >= opts.SessionMaxAge {
		if err := deleteWholeSession(ctx, store, reg.Harness.Name, reg.ArchiveSessionID); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
		if err := local.ForgetSession(reg.ArchiveSessionID, reg.NativeSessionID); err != nil {
			return fmt.Errorf("forget session: %w", err)
		}
		result.DeletedSessions = append(result.DeletedSessions, reg.ArchiveSessionID)
		return nil
	}

	if len(superseded) == 0 {
		return nil
	}

	if remoteErr != nil {
		return fmt.Errorf("current metadata is missing; preserve superseded sources")
	}
	currentKey := metadata.SourceBundle.Key
	// Append order records supersession order even if the clock moves backward.
	var predecessorKey string
	for _, s := range superseded {
		if !strings.HasPrefix(s.Key, fmt.Sprintf("sessions/%s/%s/source.", reg.Harness.Name, reg.ArchiveSessionID)) {
			return fmt.Errorf("superseded source belongs to another session")
		}
		if s.Key != currentKey {
			predecessorKey = s.Key
		}
	}
	for _, s := range superseded {
		if s.Key == currentKey {
			// Defensive: a key must never be both current and superseded;
			// if it somehow is, trust "current" and just clean the ledger.
			if err := local.RemoveSuperseded(reg.ArchiveSessionID, s.Key); err != nil {
				return fmt.Errorf("clean up ledger entry: %w", err)
			}
			continue
		}
		if s.Key == predecessorKey || now.Sub(s.SupersededAt) < opts.gracePeriod() {
			continue
		}
		if err := store.Delete(ctx, s.Key); err != nil {
			return fmt.Errorf("delete superseded source %q: %w", s.Key, err)
		}
		if err := local.RemoveSuperseded(reg.ArchiveSessionID, s.Key); err != nil {
			return fmt.Errorf("update superseded ledger: %w", err)
		}
		result.DeletedSnapshots++
	}
	return nil
}

// anySupersededExpirable reports whether a sweep could delete at least one
// ledger entry now. The last entry is either the current source or its
// immediate predecessor, and neither is ever deleted, so only an earlier
// entry past its grace period can be. This is a conservative necessary
// condition: it may say yes for an entry the full check then retains, but
// never no when a deletion is possible, so skipping on false is safe.
func anySupersededExpirable(superseded []collector.SupersededSource, now time.Time, grace time.Duration) bool {
	for i, s := range superseded {
		if i == len(superseded)-1 {
			return false
		}
		if now.Sub(s.SupersededAt) >= grace {
			return true
		}
	}
	return false
}

// Remove discovery first. If source deletion is interrupted, unreferenced
// objects remain for the next sweep, but no live metadata points at missing data.
func deleteWholeSession(ctx context.Context, store storage.ObjectStore, harness, archiveSessionID string) error {
	metadataKey, err := archive.MetadataObjectKey(harness, archiveSessionID)
	if err != nil {
		return err
	}
	if err := store.Delete(ctx, metadataKey); err != nil {
		return fmt.Errorf("delete metadata: %w", err)
	}
	prefix := fmt.Sprintf("sessions/%s/%s/", harness, archiveSessionID)
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list %q: %w", prefix, err)
	}
	for _, obj := range objects {
		if err := store.Delete(ctx, obj.Key); err != nil {
			return fmt.Errorf("delete %q: %w", obj.Key, err)
		}
	}
	return nil
}
