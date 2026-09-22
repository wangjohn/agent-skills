package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func (o Options) parserVersion() string {
	if o.ParserVersion != "" {
		return o.ParserVersion
	}
	return archive.DefaultParserVersion
}

// Metadata belongs in the publication state so a restart never confuses the
// parser used for a candidate with the parser actually published remotely.
func (s *LocalStore) loadPublishedMetadata(id string) ([]byte, error) {
	var state publishedState
	if err := local.Read(s.publishedPath(id), &state); err != nil {
		return nil, err
	}
	return state.MetadataBytes, nil
}

// Refresh from durable filtered evidence before opening the live transcript.
// A parser upgrade still works after the application rotates its local log.
//
// Regeneration is best effort and must never stand between a session and
// normal capture: whatever makes the retained publication unusable here (no
// cached metadata and no readable remote copy, or metadata that does not
// describe this machine's retained source) is a reason to skip, not to fail
// the session, because the next content change publishes current-parser
// metadata anyway. Only the publish attempt itself reports errors.
func regenerateMetadata(ctx context.Context, store *LocalStore, remote storage.ObjectStore, reg archive.SessionRegistration, now time.Time, opts Options) (sessionOutcome, bool, error) {
	// Only the real last publication is a valid source: a blocked, declined,
	// or rate-limited candidate cached alongside it was never made
	// discoverable, so a blocked session without one has nothing to refresh.
	bundle, _, found, err := store.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		return outcomeSkipped, false, err
	}
	encoded, err := store.loadPublishedMetadata(reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, false, err
	}
	key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
	if err != nil {
		return outcomeSkipped, false, err
	}
	legacy := len(encoded) == 0
	if legacy {
		// One-time migration for publications made before metadata was cached.
		// A missing or unreachable copy is not fatal: nothing can be refreshed
		// from it, and normal capture keeps working without it.
		encoded, err = remote.Get(ctx, key)
		if err != nil {
			return outcomeSkipped, false, nil
		}
	}
	var prior archive.Metadata
	if err := json.Unmarshal(encoded, &prior); err != nil {
		return outcomeSkipped, false, nil
	}
	if prior.SessionID != reg.ArchiveSessionID || prior.MachineID != opts.MachineID || prior.ValidateSourceReference() != nil {
		return outcomeSkipped, false, nil
	}
	sameParser := prior.Parser.Version == opts.parserVersion()
	if sameParser && !legacy {
		return outcomeSkipped, false, nil
	}
	compressed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		return outcomeSkipped, false, err
	}
	sourceKey, err := archive.SourceObjectKey(bundle, compressed.SHA256)
	if err != nil {
		return outcomeSkipped, false, err
	}
	if prior.SourceBundle.Key != sourceKey || prior.SourceBundle.SHA256 != compressed.SHA256 || prior.SourceBundle.CompressedBytes != len(compressed.Bytes) {
		return outcomeSkipped, false, nil
	}
	// Cache before any early return below, so a legacy publication is
	// migrated exactly once rather than re-read on every scan.
	if err := store.cacheMetadata(reg.ArchiveSessionID, encoded); err != nil {
		return outcomeSkipped, false, err
	}
	if sameParser {
		// The same parser over the same retained source yields the same
		// result, including a failed parse: nothing to rebuild.
		return outcomeSkipped, false, nil
	}
	if liveTranscriptChanged(store, reg, bundle, now, opts) {
		// Normal capture is about to publish current-parser metadata with
		// the new content; a metadata-only publication first would only be
		// wasted work that also starts the upload interval early.
		return outcomeSkipped, false, nil
	}
	next, buildErr := archive.BuildMetadata(bundle, opts.MachineID, reg.SessionStartedAt, now, prior.SourceBundle, archive.ParserInfo{Version: opts.parserVersion()})
	if buildErr != nil && !archive.IsParseError(buildErr) {
		return outcomeSkipped, false, buildErr
	}
	comparison := next
	comparison.MetadataDerivedAt = prior.MetadataDerivedAt
	oldBytes, err := json.Marshal(prior)
	if err != nil {
		return outcomeSkipped, false, err
	}
	comparisonBytes, err := json.Marshal(comparison)
	if err != nil {
		return outcomeSkipped, false, err
	}
	if bytes.Equal(oldBytes, comparisonBytes) {
		return outcomeSkipped, false, nil
	}
	metadataBytes, err := json.Marshal(next)
	if err != nil {
		return outcomeSkipped, false, err
	}
	pending := PendingPublication{MetadataOnly: true, Bundle: bundle, SourceKey: sourceKey, MetadataKey: key, SourceSHA256: compressed.SHA256, SourceBytes: compressed.Bytes, MetadataBytes: metadataBytes, ReadyAt: now}
	if err := store.SavePending(reg.ArchiveSessionID, pending); err != nil {
		return outcomeSkipped, false, err
	}
	outcome, err := publishPending(ctx, store, remote, reg.ArchiveSessionID, pending, now, opts)
	return outcome, true, err
}

// liveTranscriptChanged reports whether normal capture will publish this
// scan: the transcript on disk carries evidence the cached comparison bundle
// does not, and it still extends what capture guards against (the same
// rules processSession applies), so a rewrite that capture will only record
// as a gap does not count. It compares against the cached candidate rather
// than only the last publication so a declined candidate the transcript
// still matches leaves regeneration free to proceed. A transcript that
// cannot be compared (rotated, oversize, unsafe) reports no change:
// regeneration is then the only way the summary can move.
func liveTranscriptChanged(store *LocalStore, reg archive.SessionRegistration, lastPublished archive.SourceBundle, now time.Time, opts Options) bool {
	if reg.TranscriptPath == "" {
		return false
	}
	cached, _, status, found, err := store.LoadPublished(reg.ArchiveSessionID)
	if err != nil || !found {
		return false
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return false
	}
	filtered, err := filterTranscript(adapter, reg, opts.maxTranscriptBytes())
	if err != nil {
		return false
	}
	candidate, err := archive.NewSourceBundle(reg, adapter, filtered, now, cached.SupplementalEvidence)
	if err != nil {
		return false
	}
	same, err := bundleEvidenceEqual(cached, candidate)
	if err != nil || same {
		return false
	}
	guard := cached
	if status == CacheStatusBlocked {
		guard = lastPublished
	}
	return nativeEvidenceExtends(guard, candidate)
}

func (s *LocalStore) cacheMetadata(id string, metadata []byte) error {
	var state publishedState
	if err := local.Read(s.publishedPath(id), &state); err != nil {
		return err
	}
	if bytes.Equal(state.MetadataBytes, metadata) {
		return nil
	}
	state.MetadataBytes = metadata
	return local.Write(s.publishedPath(id), state)
}

// Updating a summary must not discard a richer local-only candidate.
func (s *LocalStore) saveRepublishedMetadata(id string, pending PendingPublication, at time.Time) error {
	var state publishedState
	if err := local.Read(s.publishedPath(id), &state); err != nil {
		return err
	}
	state.LastPublished = &publishedSnapshot{Bundle: pending.Bundle, PublishedAt: at}
	state.MetadataBytes = pending.MetadataBytes
	state.PublishedAt = at
	if state.Status == CacheStatusPublished {
		state.Bundle = pending.Bundle
	}
	return local.Write(s.publishedPath(id), state)
}
