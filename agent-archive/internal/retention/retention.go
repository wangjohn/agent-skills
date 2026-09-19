// Package retention deletes local-machine-owned archive data that has aged
// out: source snapshots superseded more than a grace period ago, and,
// separately, entire sessions once their most recently captured evidence is
// older than a configured retention window. It never deletes a currently
// referenced object, and it never considers another machine's sessions —
// registrations only ever exist on the machine that created them.
package retention

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// Options configures one Sweep call.
type Options struct {
	// Now returns the current time. Defaults to time.Now.
	Now func() time.Time
	// GracePeriod bounds how long a superseded (no longer current) source
	// snapshot stays downloadable before deletion. Defaults to 24h,
	// matching the spec's proposed grace period.
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
		if err := sweepSession(ctx, local, store, reg, opts, now, &result); err != nil {
			result.Errors[reg.ArchiveSessionID] = err
		}
	}
	return result, nil
}

func sweepSession(ctx context.Context, local *collector.LocalStore, store storage.ObjectStore, reg archive.SessionRegistration, opts Options, now time.Time, result *Result) error {
	bundle, _, status, found, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		return fmt.Errorf("load published cache: %w", err)
	}

	if opts.SessionMaxAge > 0 && found && !bundle.Capture.CapturedAt.IsZero() && now.Sub(bundle.Capture.CapturedAt) >= opts.SessionMaxAge {
		if err := deleteWholeSession(ctx, store, reg.Harness.Name, reg.ArchiveSessionID); err != nil {
			return fmt.Errorf("delete session: %w", err)
		}
		if err := local.ForgetSession(reg.ArchiveSessionID, reg.NativeSessionID); err != nil {
			return fmt.Errorf("forget session: %w", err)
		}
		result.DeletedSessions = append(result.DeletedSessions, reg.ArchiveSessionID)
		return nil
	}

	superseded, err := local.LoadSuperseded(reg.ArchiveSessionID)
	if err != nil {
		return fmt.Errorf("load superseded sources: %w", err)
	}
	if len(superseded) == 0 {
		return nil
	}

	var currentKey string
	if found && status == collector.CacheStatusPublished {
		// If either of these fails, currentKey must not silently stay "":
		// the loop below treats a non-matching currentKey as "not the
		// current source," so an empty one would defeat the "never delete
		// the currently referenced object" guard below instead of just
		// skipping the delete. Both calls are deterministic recomputations
		// of a bundle that was already successfully published, so a
		// failure here means something is genuinely wrong; abort this
		// session's sweep (isolated by the caller, retried next pass)
		// rather than risk deleting a live source.
		compressed, err := archive.BuildCompressedSource(bundle)
		if err != nil {
			return fmt.Errorf("recompute current source key: %w", err)
		}
		currentKey, err = archive.SourceObjectKey(bundle, compressed.SHA256)
		if err != nil {
			return fmt.Errorf("recompute current source key: %w", err)
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
		if now.Sub(s.SupersededAt) < opts.gracePeriod() {
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

// deleteWholeSession deletes every source snapshot before metadata, so an
// interruption partway leaves at worst a metadata pointer to an
// already-deleted source — the exact case reader.LoadSource already handles
// safely via ErrRefreshRequired — never the reverse (a source with no
// metadata pointing to it is merely unreferenced, not dangling). Not
// finding every object it expects on a retry is not an error: a prior,
// interrupted sweep may have already deleted some of them.
func deleteWholeSession(ctx context.Context, store storage.ObjectStore, harness, archiveSessionID string) error {
	prefix := fmt.Sprintf("sessions/%s/%s/", harness, archiveSessionID)
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list %q: %w", prefix, err)
	}
	var metadataKey string
	for _, obj := range objects {
		if strings.HasSuffix(obj.Key, "/metadata.json") {
			metadataKey = obj.Key
			continue
		}
		if err := store.Delete(ctx, obj.Key); err != nil {
			return fmt.Errorf("delete %q: %w", obj.Key, err)
		}
	}
	if metadataKey != "" {
		if err := store.Delete(ctx, metadataKey); err != nil {
			return fmt.Errorf("delete %q: %w", metadataKey, err)
		}
	}
	return nil
}
