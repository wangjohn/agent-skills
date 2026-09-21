package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
func regenerateMetadata(ctx context.Context, store *LocalStore, remote storage.ObjectStore, reg archive.SessionRegistration, now time.Time, opts Options) (sessionOutcome, bool, error) {
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
	if len(encoded) == 0 {
		// One-time migration for publications made before metadata was cached.
		encoded, err = remote.Get(ctx, key)
		if err != nil {
			return outcomeSkipped, false, fmt.Errorf("read published metadata for parser migration: %w", err)
		}
	}
	var prior archive.Metadata
	if err := json.Unmarshal(encoded, &prior); err != nil {
		return outcomeSkipped, false, fmt.Errorf("decode published metadata: %w", err)
	}
	if prior.SessionID != reg.ArchiveSessionID || prior.MachineID != opts.MachineID {
		return outcomeSkipped, false, fmt.Errorf("published metadata does not match this machine's session")
	}
	if prior.Parser.Version == opts.parserVersion() && prior.Parser.Status != archive.ParserStatusFailed {
		if err := store.cacheMetadata(reg.ArchiveSessionID, encoded); err != nil {
			return outcomeSkipped, false, err
		}
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
	if prior.SourceBundle.Key != sourceKey || prior.SourceBundle.SHA256 != compressed.SHA256 {
		return outcomeSkipped, false, fmt.Errorf("published metadata does not match the cached source")
	}
	next, buildErr := archive.BuildMetadata(bundle, opts.MachineID, reg.SessionStartedAt, now, prior.SourceBundle, archive.ParserInfo{Version: opts.parserVersion()})
	if buildErr != nil && !archive.IsParseError(buildErr) {
		return outcomeSkipped, false, buildErr
	}
	comparison := next
	comparison.MetadataDerivedAt = prior.MetadataDerivedAt
	oldBytes, _ := json.Marshal(prior)
	comparisonBytes, _ := json.Marshal(comparison)
	if bytes.Equal(oldBytes, comparisonBytes) {
		return outcomeSkipped, false, nil
	}
	metadataBytes, err := json.Marshal(next)
	if err != nil {
		return outcomeSkipped, false, err
	}
	pending := PendingPublication{Bundle: bundle, SourceKey: sourceKey, MetadataKey: key, SourceSHA256: compressed.SHA256, SourceBytes: compressed.Bytes, MetadataBytes: metadataBytes, ReadyAt: now}
	if err := store.SavePending(reg.ArchiveSessionID, pending); err != nil {
		return outcomeSkipped, false, err
	}
	outcome, err := publishPending(ctx, store, remote, reg.ArchiveSessionID, pending, now, opts)
	return outcome, true, err
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
