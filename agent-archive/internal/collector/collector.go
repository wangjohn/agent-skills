package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// Options configures one Run call. The caller is responsible for holding
// local.Lock(home) around Run; Run itself does not acquire it, so it stays
// simple to call directly from tests.
type Options struct {
	AcceptSession func(archive.SessionRegistration) bool
	// MachineID identifies this machine in published metadata. Required.
	MachineID string
	// Now returns the current time. Defaults to time.Now; tests override it
	// for deterministic timestamps and rate-limit behavior.
	Now func() time.Time
	// Retry controls storage retry/backoff. Zero value uses storage's default.
	Retry storage.RetryPolicy
	// MinUploadInterval bounds how often one session may be republished.
	// Defaults to three minutes, matching the spec's cadence.
	MinUploadInterval time.Duration
	// RequireSkillUse, when true, skips publishing a session that has no
	// detected skill use. The spec's default is to capture such sessions
	// anyway (to preserve comparison evidence), so the zero value (false)
	// matches that default rather than requiring every caller to opt in.
	RequireSkillUse bool
}

func (o Options) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

func (o Options) minUploadInterval() time.Duration {
	if o.MinUploadInterval > 0 {
		return o.MinUploadInterval
	}
	return 3 * time.Minute
}

// Result summarizes one Run call. It never includes transcript content.
type Result struct {
	Scanned   int
	Published []string
	Skipped   []string
	Errors    map[string]error
}

// Run performs one collector pass over every registered session: for each,
// read its transcript, filter it, and compare the result against the last
// published bundle. A session with no meaningful change is left untouched at
// no storage cost. A session with new evidence is published source-before-
// metadata, subject to MinUploadInterval. One session's failure does not
// stop the pass; it is recorded in Result.Errors and left retryable on the
// next call.
func Run(ctx context.Context, local *LocalStore, store storage.ObjectStore, opts Options) (Result, error) {
	if local == nil || store == nil {
		return Result{}, errors.New("local store and object store are required")
	}
	if opts.MachineID == "" {
		return Result{}, errors.New("machine ID is required")
	}
	now := opts.now()

	registrations, err := local.LoadRegistrations()
	if err != nil {
		return Result{}, fmt.Errorf("load registrations: %w", err)
	}
	requestsByID := map[string]Request{}
	if requests, err := local.LoadRequests(); err != nil {
		return Result{}, fmt.Errorf("load requests: %w", err)
	} else {
		for _, req := range requests {
			requestsByID[req.ArchiveSessionID] = req
		}
	}

	result := Result{Errors: map[string]error{}}
	pending := 0
	for _, reg := range registrations {
		if opts.AcceptSession != nil && !opts.AcceptSession(reg) {
			continue
		}
		result.Scanned++
		req := requestsByID[reg.ArchiveSessionID]

		outcome, err := processSession(ctx, local, store, reg, req, now, opts)
		if err != nil {
			result.Errors[reg.ArchiveSessionID] = err
			pending++
			continue
		}
		// The session was scanned without error: any pending request's hook
		// evidence has been folded into this pass's candidate bundle (whether
		// or not that candidate was actually published), so the request is
		// fulfilled. Leaving it would only replay the same evidence forever.
		if _, hadRequest := requestsByID[reg.ArchiveSessionID]; hadRequest {
			if err := local.CompleteRequest(reg.ArchiveSessionID); err != nil {
				result.Errors[reg.ArchiveSessionID] = fmt.Errorf("complete request: %w", err)
				pending++
				continue
			}
		}
		switch outcome {
		case outcomePublished:
			result.Published = append(result.Published, reg.ArchiveSessionID)
		case outcomeRateLimited:
			pending++
			result.Skipped = append(result.Skipped, reg.ArchiveSessionID)
		case outcomeSkipped:
			result.Skipped = append(result.Skipped, reg.ArchiveSessionID)
		}
	}

	previousStatus, err := local.LoadStatus()
	if err != nil {
		return result, err
	}
	status := Status{LastScanAt: now.UTC(), PendingCount: pending, LastPublishedAt: previousStatus.LastPublishedAt}
	if len(result.Published) > 0 {
		status.LastPublishedAt = now.UTC()
	}
	if len(result.Errors) > 0 {
		status.LastError = fmt.Sprintf("%d session(s) failed to scan or publish", len(result.Errors))
	}
	if err := local.SaveStatus(status); err != nil {
		return result, fmt.Errorf("save status: %w", err)
	}
	return result, nil
}

type sessionOutcome int

const (
	outcomeSkipped sessionOutcome = iota
	outcomeRateLimited
	outcomePublished
)

func processSession(ctx context.Context, local *LocalStore, store storage.ObjectStore, reg archive.SessionRegistration, req Request, now time.Time, opts Options) (sessionOutcome, error) {
	if reg.TranscriptPath == "" {
		return outcomeSkipped, errors.New("registration has no transcript path")
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return outcomeSkipped, err
	}
	filtered, err := filterTranscript(adapter, reg)
	if err != nil {
		// Unsafe format: never upload; the last published snapshot, if any,
		// remains untouched and readable.
		return outcomeSkipped, fmt.Errorf("filter transcript: %w", err)
	}

	prevBundle, prevPublishedAt, prevStatus, havePrev, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("load published cache: %w", err)
	}

	// now is a placeholder here; bundleEvidenceEqual ignores CapturedAt, so
	// it has no effect on the comparison below. The real value is assigned
	// once we know whether this is genuinely new evidence.
	candidate, err := archive.NewSourceBundle(reg, adapter, filtered, now, req.HookEvidence)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("build source bundle: %w", err)
	}

	changedFromCache := true
	if havePrev {
		same, err := bundleEvidenceEqual(prevBundle, candidate)
		if err != nil {
			return outcomeSkipped, fmt.Errorf("compare source bundles: %w", err)
		}
		changedFromCache = !same
	}

	switch {
	case !havePrev, changedFromCache:
		// Evidence not seen before, whether relative to the last publish or
		// to a still-pending rate-limited candidate: this is a new snapshot,
		// first observed now.
		candidate.Capture.CapturedAt = now
	case prevStatus == CacheStatusRateLimited:
		// Unchanged since the last withheld candidate: it is the same
		// pending snapshot, so reuse its already-assigned capture time
		// rather than manufacturing a new one on every retry.
		candidate.Capture.CapturedAt = prevBundle.Capture.CapturedAt
	default:
		// Unchanged since the last actual publish, or since a policy
		// decline: nothing to do. A decline is reconsidered only by a
		// genuine further content change, never by time alone.
		return outcomeSkipped, nil
	}

	if !prevPublishedAt.IsZero() && now.Sub(prevPublishedAt) < opts.minUploadInterval() {
		if err := local.SavePublished(reg.ArchiveSessionID, candidate, prevPublishedAt, CacheStatusRateLimited); err != nil {
			return outcomeSkipped, fmt.Errorf("cache rate-limited candidate: %w", err)
		}
		return outcomeRateLimited, nil
	}

	compressed, err := archive.BuildCompressedSource(candidate)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("compress source bundle: %w", err)
	}
	sourceKey, err := archive.SourceObjectKey(candidate, compressed.SHA256)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("derive source key: %w", err)
	}
	metadataKey, err := archive.MetadataObjectKey(candidate.Capture.Harness.Name, candidate.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("derive metadata key: %w", err)
	}

	reference := archive.SourceReference{Key: sourceKey, SHA256: compressed.SHA256, CompressedBytes: len(compressed.Bytes)}
	metadata, buildErr := archive.BuildMetadata(candidate, opts.MachineID, reg.SessionStartedAt, now, reference, archive.ParserInfo{})
	if buildErr != nil && !archive.IsParseError(buildErr) {
		return outcomeSkipped, fmt.Errorf("derive metadata: %w", buildErr)
	}
	// A ParseError here still yields a minimal, safe-to-publish metadata
	// document with parser.status "failed", per the spec's failure table:
	// archive the filtered source and retry parsing later. A ParseError
	// also means metadata.SkillsUsed is necessarily empty (parsing never
	// got far enough to derive it), so RequireSkillUse must not read that
	// as "no skill use" and decline the session — that would silently and
	// permanently skip every session whose transcript fails to parse,
	// contradicting the comment above and the failure table it cites.
	parseFailed := archive.IsParseError(buildErr)

	if opts.RequireSkillUse && !parseFailed && len(metadata.SkillsUsed) == 0 {
		// Nothing was ever actually published, so this candidate carries no
		// real publish history; a zero PublishedAt correctly signals that
		// to the rate-limit check above once this decline is reconsidered
		// by a later content change, rather than a manufactured timestamp
		// making a genuinely-first publish look rate-limited.
		if err := local.SavePublished(reg.ArchiveSessionID, candidate, time.Time{}, CacheStatusDeclined); err != nil {
			return outcomeSkipped, fmt.Errorf("cache declined candidate: %w", err)
		}
		return outcomeSkipped, nil
	}

	metadataBytes, err := json.Marshal(metadata)
	if err != nil {
		return outcomeSkipped, fmt.Errorf("marshal metadata: %w", err)
	}

	if err := storage.PutSourceThenMetadata(ctx, store, sourceKey, metadataKey, compressed.Bytes, metadataBytes, opts.Retry); err != nil {
		return outcomeSkipped, fmt.Errorf("publish: %w", err)
	}
	// A previously *actually published* bundle (not a withheld or declined
	// one, which were never the current pointer) is now superseded. Record
	// it for retention to delete once its grace period elapses; it must
	// stay downloadable until then; PutSourceThenMetadata has just made the
	// new one the current pointer.
	//
	// The `err == nil` guards below are a deliberate, accepted gap: if
	// either recomputation fails, the old snapshot is silently never
	// recorded as superseded and so never cleaned up by retention — a
	// storage leak, not a correctness or safety issue (the new source is
	// already the live pointer regardless). Treating it as fatal here would
	// be worse: PutSourceThenMetadata has already succeeded, so failing
	// this session now would report a successful publish as an error and,
	// since local.SavePublished below would never run, leave the local
	// cache stale — causing a real, unwanted republish loop on every future
	// pass instead of one already-orphaned snapshot.
	if havePrev && prevStatus == CacheStatusPublished {
		if prevCompressed, err := archive.BuildCompressedSource(prevBundle); err == nil {
			if prevKey, err := archive.SourceObjectKey(prevBundle, prevCompressed.SHA256); err == nil && prevKey != sourceKey {
				if err := local.RecordSuperseded(reg.ArchiveSessionID, prevKey, now); err != nil {
					return outcomeSkipped, fmt.Errorf("record superseded source: %w", err)
				}
			}
		}
	}
	if err := local.SavePublished(reg.ArchiveSessionID, candidate, now, CacheStatusPublished); err != nil {
		return outcomeSkipped, fmt.Errorf("update published cache: %w", err)
	}
	return outcomePublished, nil
}

// filterTranscript filters reg's transcript with adapter, falling back to
// CursorAdapter's text format when Cursor's own JSONL filter reports the
// content isn't recognized as JSONL at all: the spec notes Cursor's
// hook-provided transcript path can point to either format depending on
// version, and the collector has no other way to tell which one it has
// until it tries. Any other adapter, or any other kind of filter failure,
// is returned as-is with no retry.
func filterTranscript(adapter archive.Adapter, reg archive.SessionRegistration) (archive.FilteredTranscript, error) {
	filtered, err := filterJSONLFile(adapter, reg.TranscriptPath)
	if err == nil {
		return filtered, nil
	}
	cursorAdapter, ok := adapter.(archive.CursorAdapter)
	if !ok || !errors.Is(err, archive.ErrUnsafeSourceFormat) {
		return archive.FilteredTranscript{}, err
	}
	file, openErr := os.Open(reg.TranscriptPath)
	if openErr != nil {
		return archive.FilteredTranscript{}, fmt.Errorf("open transcript: %w", openErr)
	}
	defer file.Close()
	return cursorAdapter.FilterText(file, reg.SessionStartedAt)
}

func filterJSONLFile(adapter archive.Adapter, path string) (archive.FilteredTranscript, error) {
	file, err := os.Open(path)
	if err != nil {
		return archive.FilteredTranscript{}, fmt.Errorf("open transcript: %w", err)
	}
	filtered, err := adapter.FilterJSONL(file)
	closeErr := file.Close()
	if err != nil {
		return archive.FilteredTranscript{}, err
	}
	if closeErr != nil {
		return archive.FilteredTranscript{}, fmt.Errorf("close transcript: %w", closeErr)
	}
	return filtered, nil
}

// bundleEvidenceEqual reports whether two source bundles carry the same
// retained evidence, ignoring their capture timestamp: a changed scan time
// alone must never look like a change in evidence.
func bundleEvidenceEqual(a, b archive.SourceBundle) (bool, error) {
	a.Capture.CapturedAt = time.Time{}
	b.Capture.CapturedAt = time.Time{}
	aBytes, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bBytes, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(aBytes, bBytes), nil
}
