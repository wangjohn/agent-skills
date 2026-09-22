package collector

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

type countedPublications struct {
	storage.ObjectStore
	keys []string
}

func (s *countedPublications) Put(ctx context.Context, key string, data []byte) error {
	s.keys = append(s.keys, key)
	return s.ObjectStore.Put(ctx, key, data)
}

func TestParserUpgradeReusesSourceAfterNativeLogDisappears(t *testing.T) {
	local := newTestStore(t)
	path := writeTranscript(t, t.TempDir(), "session.jsonl", codexTranscript)
	reg := registration(t, path)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedPublications{ObjectStore: storage.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	source, _ := remote.Get(context.Background(), old.SourceBundle.Key)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	remote.keys = nil
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.SourceBundle != old.SourceBundle || !next.CapturedAt.Equal(old.CapturedAt) || next.Parser.Version != "two" || !next.MetadataDerivedAt.Equal(now) {
		t.Fatalf("changed durable source or wrong summary: %+v", next)
	}
	key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if len(remote.keys) != 1 || remote.keys[0] != key {
		t.Fatalf("metadata-only upgrade wrote %v", remote.keys)
	}
	after, _ := remote.Get(context.Background(), old.SourceBundle.Key)
	if !bytes.Equal(source, after) {
		t.Fatal("source bytes changed")
	}
	// Restore identical native input: further scans do not write anything.
	writeTranscript(t, filepath.Dir(path), "session.jsonl", codexTranscript)
	remote.keys = nil
	now = now.Add(time.Hour)
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(remote.keys) != 0 {
		t.Fatalf("%#v %v %v", result, err, remote.keys)
	}
}

func TestParserMetadataRetryUsesSavedBytes(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &metadataFailStore{MemoryStore: storage.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	if result, err := Run(context.Background(), local, remote, opts); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	old := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	remote.failMetadata = true
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	pending, found, err := local.LoadPending(reg.ArchiveSessionID)
	if err != nil || !found {
		t.Fatalf("%v %v", found, err)
	}
	remote.failMetadata = false
	now = now.Add(24 * time.Hour)
	restarted, err := NewLocalStore(local.home)
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(context.Background(), restarted, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	actual, err := remote.Get(context.Background(), pending.MetadataKey)
	if err != nil || !bytes.Equal(actual, pending.MetadataBytes) {
		t.Fatal("retry changed pending metadata")
	}
	next := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if next.SourceBundle != old.SourceBundle {
		t.Fatal("parser retry changed source")
	}
}

func TestMetadataUpgradePreservesNewerDeclinedCandidate(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	now := reg.RegisteredAt.Add(time.Hour)
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	opts := Options{MachineID: "m", ParserVersion: "one", Now: func() time.Time { return now }}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	bundle, at, _, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	richer := bundle
	richer.NativeRecords = append(richer.NativeRecords, map[string]any{"type": "event_msg", "message": "retained candidate"})
	richer.Capture.CapturedAt = now.Add(time.Minute)
	if err := local.SavePublished(reg.ArchiveSessionID, richer, at, CacheStatusDeclined); err != nil {
		t.Fatal(err)
	}
	opts.ParserVersion = "two"
	now = now.Add(time.Hour)
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	candidate, _, status, _, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if status != CacheStatusDeclined || !candidate.Capture.CapturedAt.Equal(richer.Capture.CapturedAt) || len(candidate.NativeRecords) != len(richer.NativeRecords) {
		t.Fatal("metadata-only update discarded local evidence")
	}
	actual, _, _, err := local.LoadLastPublished(reg.ArchiveSessionID)
	if err != nil || len(actual.NativeRecords) != len(bundle.NativeRecords) {
		t.Fatal("publication ledger points to unpublished candidate")
	}
}

type countedGets struct {
	storage.ObjectStore
	gets int
}

func (s *countedGets) Get(ctx context.Context, key string) ([]byte, error) {
	s.gets++
	return s.ObjectStore.Get(ctx, key)
}

const grownCodexTranscript = codexTranscript + "\n" + `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"more"}}`

// publishOnce runs a first scan with parser "one" and returns what it published.
func publishOnce(t *testing.T, local *LocalStore, remote storage.ObjectStore, reg archive.SessionRegistration, opts *Options) archive.Metadata {
	t.Helper()
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, remote, *opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	return fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
}

func putMetadata(t *testing.T, remote storage.ObjectStore, reg archive.SessionRegistration, metadata archive.Metadata) {
	t.Helper()
	key, err := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Put(context.Background(), key, encoded); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyMetadataMigrationFailureNeverBlocksCapture(t *testing.T) {
	cases := map[string]func(t *testing.T, remote *storage.MemoryStore, reg archive.SessionRegistration, published archive.Metadata){
		"missing remote metadata": func(t *testing.T, remote *storage.MemoryStore, reg archive.SessionRegistration, published archive.Metadata) {
			key, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
			if err := remote.Delete(context.Background(), key); err != nil {
				t.Fatal(err)
			}
		},
		"metadata from another machine": func(t *testing.T, remote *storage.MemoryStore, reg archive.SessionRegistration, published archive.Metadata) {
			published.MachineID = "elsewhere"
			putMetadata(t, remote, reg, published)
		},
		"metadata for a different source": func(t *testing.T, remote *storage.MemoryStore, reg archive.SessionRegistration, published archive.Metadata) {
			published.SourceBundle.SHA256 = "0000000000000000000000000000000000000000000000000000000000000000"
			putMetadata(t, remote, reg, published)
		},
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			local := newTestStore(t)
			dir := t.TempDir()
			reg := registration(t, writeTranscript(t, dir, "s.jsonl", codexTranscript))
			remote := storage.NewMemoryStore()
			now := reg.RegisteredAt.Add(time.Hour)
			opts := Options{MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
			before := publishOnce(t, local, remote, reg, &opts)
			// State written before metadata was cached locally.
			if err := local.cacheMetadata(reg.ArchiveSessionID, nil); err != nil {
				t.Fatal(err)
			}
			corrupt(t, remote, reg, before)

			writeTranscript(t, dir, "s.jsonl", grownCodexTranscript)
			now = now.Add(time.Hour)
			opts.ParserVersion = "two"
			result, err := Run(context.Background(), local, remote, opts)
			if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
				t.Fatalf("new content was not captured: %#v %v", result, err)
			}
			after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
			if after.SourceBundle.SHA256 == before.SourceBundle.SHA256 || after.Parser.Version != "two" {
				t.Fatalf("capture did not publish the grown transcript with the current parser: %+v", after)
			}
			cached, err := local.loadPublishedMetadata(reg.ArchiveSessionID)
			if err != nil || len(cached) == 0 {
				t.Fatalf("publication did not cache its metadata: %v", err)
			}
		})
	}
}

func TestLegacyFailedParseMigratesOnceWithoutRebuilding(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	remote := &countedGets{ObjectStore: storage.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	published := publishOnce(t, local, remote, reg, &opts)
	if err := local.cacheMetadata(reg.ArchiveSessionID, nil); err != nil {
		t.Fatal(err)
	}
	published.Parser.Status = archive.ParserStatusFailed
	putMetadata(t, remote, reg, published)

	for scan := 1; scan <= 2; scan++ {
		now = now.Add(time.Hour)
		remote.gets = 0
		result, err := Run(context.Background(), local, remote, opts)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("scan %d: %#v %v", scan, result, err)
		}
		want := 1
		if scan > 1 {
			want = 0
		}
		if remote.gets != want {
			t.Fatalf("scan %d performed %d remote reads, want %d", scan, remote.gets, want)
		}
		cached, err := local.loadPublishedMetadata(reg.ArchiveSessionID)
		if err != nil || len(cached) == 0 {
			t.Fatalf("scan %d did not cache the migrated metadata: %v", scan, err)
		}
	}
}

func TestParserUpgradeWithNewContentPublishesOnce(t *testing.T) {
	local := newTestStore(t)
	dir := t.TempDir()
	reg := registration(t, writeTranscript(t, dir, "s.jsonl", codexTranscript))
	remote := &countedPublications{ObjectStore: storage.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{MachineID: "machine", ParserVersion: "one", MinUploadInterval: 3 * time.Minute, Now: func() time.Time { return now }}
	before := publishOnce(t, local, remote, reg, &opts)

	writeTranscript(t, dir, "s.jsonl", grownCodexTranscript)
	now = now.Add(10 * time.Minute)
	opts.ParserVersion = "two"
	remote.keys = nil
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	metadataWrites := 0
	for _, key := range remote.keys {
		if key == metadataKey {
			metadataWrites++
		}
	}
	if metadataWrites != 1 || len(remote.keys) != 2 {
		t.Fatalf("parser upgrade with new content wrote %v, want one source and one metadata object", remote.keys)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.SourceBundle.SHA256 == before.SourceBundle.SHA256 || after.Parser.Version != "two" {
		t.Fatalf("content publication missing or stale parser: %+v", after)
	}
	if _, found, err := local.LoadPending(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("content publication was rate-limited behind a metadata-only one: pending=%v err=%v", found, err)
	}
	if _, _, status, _, err := local.LoadPublished(reg.ArchiveSessionID); err != nil || status != CacheStatusPublished {
		t.Fatalf("status=%q err=%v", status, err)
	}
}

func TestBlockedSessionRegeneratesFromLastPublicationOnly(t *testing.T) {
	local := newTestStore(t)
	dir := t.TempDir()
	reg := registration(t, writeTranscript(t, dir, "s.jsonl", codexTranscript))
	remote := &countedPublications{ObjectStore: storage.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{MachineID: "machine", ParserVersion: "one", Now: func() time.Time { return now }}
	before := publishOnce(t, local, remote, reg, &opts)

	// A rewrite blocks the session; the blocked candidate is cached beside
	// the real publication.
	writeTranscript(t, dir, "s.jsonl", `{"type":"turn_context","model":"gpt-test"}`)
	now = now.Add(10 * time.Minute)
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if _, blocked, err := local.LoadBlocked(reg.ArchiveSessionID); err != nil || !blocked {
		t.Fatalf("blocked=%v err=%v", blocked, err)
	}
	blockedCandidate, _, _, _, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil {
		t.Fatal(err)
	}

	now = now.Add(10 * time.Minute)
	opts.ParserVersion = "two"
	remote.keys = nil
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 1 {
		t.Fatalf("%#v %v", result, err)
	}
	metadataKey, _ := archive.MetadataObjectKey("codex", reg.ArchiveSessionID)
	if len(remote.keys) != 1 || remote.keys[0] != metadataKey {
		t.Fatalf("blocked session upgrade wrote %v", remote.keys)
	}
	after := fetchMetadata(t, remote, "codex", reg.ArchiveSessionID)
	if after.SourceBundle != before.SourceBundle || after.Parser.Version != "two" {
		t.Fatalf("regeneration did not use the real last publication: %+v", after)
	}
	if len(fetchBundle(t, remote, after).NativeRecords) != len(fetchBundle(t, remote, before).NativeRecords) {
		t.Fatal("regeneration published the blocked candidate")
	}
	reason, blocked, err := local.LoadBlocked(reg.ArchiveSessionID)
	if err != nil || !blocked || reason != BlockedReasonTranscriptRewritten {
		t.Fatalf("metadata-only publish cleared the block: blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	cached, _, _, _, err := local.LoadPublished(reg.ArchiveSessionID)
	if err != nil || len(cached.NativeRecords) != len(blockedCandidate.NativeRecords) {
		t.Fatalf("metadata-only publish replaced the blocked comparison bundle: %v", err)
	}
}

func TestBlockedSessionWithoutPublicationSkipsRegeneration(t *testing.T) {
	local := newTestStore(t)
	reg := registration(t, writeTranscript(t, t.TempDir(), "s.jsonl", codexTranscript))
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := &countedGets{ObjectStore: storage.NewMemoryStore()}
	now := reg.RegisteredAt.Add(time.Hour)
	opts := Options{MachineID: "machine", ParserVersion: "one", MaxTranscriptBytes: 8, Now: func() time.Time { return now }}
	result, err := Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if reason, blocked, err := local.LoadBlocked(reg.ArchiveSessionID); err != nil || !blocked || reason != BlockedReasonTranscriptTooLarge {
		t.Fatalf("blocked=%v reason=%q err=%v", blocked, reason, err)
	}
	if _, _, found, err := local.LoadLastPublished(reg.ArchiveSessionID); err != nil || found {
		t.Fatalf("unexpected publication: found=%v err=%v", found, err)
	}

	now = now.Add(time.Hour)
	opts.ParserVersion = "two"
	remote.gets = 0
	result, err = Run(context.Background(), local, remote, opts)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 || remote.gets != 0 {
		t.Fatalf("blocked session without a publication was not skipped cleanly: %#v %v gets=%d", result, err, remote.gets)
	}
}
