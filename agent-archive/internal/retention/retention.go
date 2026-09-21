// Package retention deletes local-machine-owned archive data that has aged
// out: source snapshots superseded more than a grace period ago, and,
// separately, entire sessions once their most recently captured evidence is
// older than a configured retention window. It never deletes a currently
// referenced object, and it never considers another machine's sessions —
// registrations only ever exist on the machine that created them.
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
	if !locallyExpired && len(superseded) == 0 {
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
