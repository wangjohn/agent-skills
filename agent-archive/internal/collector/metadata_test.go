package collector

import (
	"bytes"
	"context"
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
