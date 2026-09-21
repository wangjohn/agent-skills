package reader

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func fixture(t *testing.T) (archive.Metadata, archive.SourceBundle, *storage.MemoryStore) {
	t.Helper()
	ctx := context.Background()
	store := storage.NewMemoryStore()
	filtered, err := archive.CodexAdapter{}.FilterJSONL(strings.NewReader(`{"type":"turn_context","model":"gpt-test"}` + "\n" + `{"type":"response_item","id":"m1","payload":{"type":"message","role":"assistant","content":"visible"}}`))
	if err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "session-1", NativeSessionID: "native-1", ProjectID: "project-1", ProjectRoot: "/p", Harness: archive.Harness{Name: "codex"}, SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	bundle, err := archive.NewSourceBundle(reg, archive.CodexAdapter{}, filtered, time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := archive.BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.SourceObjectKey(bundle, packed.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, key, packed.Bytes); err != nil {
		t.Fatal(err)
	}
	metadata, err := archive.BuildMetadata(bundle, "machine", reg.SessionStartedAt, time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC), archive.SourceReference{Key: key, SHA256: packed.SHA256, CompressedBytes: len(packed.Bytes)}, archive.ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Put(ctx, "sessions/codex/session-1/metadata.json", data); err != nil {
		t.Fatal(err)
	}
	return metadata, bundle, store
}

func TestListAndLoadVerifiedSource(t *testing.T) {
	metadata, want, store := fixture(t)
	listed, err := ListMetadata(context.Background(), store, "sessions", Filter{Harness: "codex", Model: "gpt-test"})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %#v", listed)
	}
	got, err := LoadSource(context.Background(), store, metadata, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got.ArchiveSessionID != want.ArchiveSessionID {
		t.Fatalf("source identity %#v", got)
	}
	if err = store.Put(context.Background(), metadata.SourceBundle.Key, []byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadSource(context.Background(), store, metadata, Limits{}); err == nil {
		t.Fatal("tampered source accepted")
	}
}

func TestRefreshRequiredForDeletedSource(t *testing.T) {
	metadata, _, store := fixture(t)
	if err := store.Delete(context.Background(), metadata.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSource(context.Background(), store, metadata, Limits{}); err != ErrRefreshRequired {
		t.Fatalf("err=%v", err)
	}
}

func TestEligibleNoUseRequiresObservedEligibility(t *testing.T) {
	f := Filter{Skill: "review", SkillUsage: SkillUsageEligibleNoUse}
	m := archive.Metadata{SkillDetection: archive.SkillDetectionObservedNone, SkillsAvailable: []archive.SkillSnapshot{{Name: "review", Coverage: archive.SkillCoverageInstalledOnly}}}
	if matches(m, f) {
		t.Fatal("installed_only treated as eligible")
	}
	m.SkillsAvailable[0].Coverage = archive.SkillCoverageEligible
	if !matches(m, f) {
		t.Fatal("eligible observed-none excluded")
	}
	m.SkillDetection = archive.SkillDetectionUnavailable
	if matches(m, f) {
		t.Fatal("unknown detection treated as no use")
	}
}

func TestSkillAvailableEligibleEntrySurvivesLaterNonEligibleEntry(t *testing.T) {
	f := Filter{Skill: "review", SkillUsage: SkillUsageAvailable}
	m := archive.Metadata{SkillsAvailable: []archive.SkillSnapshot{
		{Name: "review", SHA256: "aaa", Coverage: archive.SkillCoverageEligible},
		{Name: "review", SHA256: "zzz", Coverage: archive.SkillCoverageInstalledOnly},
	}}
	if !matches(m, f) {
		t.Fatal("eligible entry excluded by a later non-eligible entry for the same skill name")
	}
}

func TestSkillHashOnlyFilterMatchesExactUsedVersion(t *testing.T) {
	m := archive.Metadata{SkillsUsed: []archive.SkillUse{
		{Name: "review", SHA256: strings.Repeat("a", 64)},
		{Name: "deploy", SHA256: strings.Repeat("b", 64)},
	}}
	if !matches(m, Filter{SkillSHA256: strings.Repeat("b", 64)}) {
		t.Fatal("hash-only filter excluded exact used version")
	}
	if matches(m, Filter{SkillSHA256: strings.Repeat("c", 64)}) {
		t.Fatal("hash-only filter matched a different version")
	}
}

func TestReadMetadataAndFindMetadataKeys(t *testing.T) {
	metadata, _, store := fixture(t)
	ctx := context.Background()
	keys, err := FindMetadataKeys(ctx, store, "sessions", "session-1")
	if err != nil || len(keys) != 1 || keys[0] != "sessions/codex/session-1/metadata.json" {
		t.Fatalf("keys=%v err=%v", keys, err)
	}
	if none, err := FindMetadataKeys(ctx, store, "sessions", "session-2"); err != nil || len(none) != 0 {
		t.Fatalf("keys=%v err=%v", none, err)
	}
	if _, err = FindMetadataKeys(ctx, store, "sessions", "codex/session-1"); err == nil {
		t.Fatal("ID containing a path separator accepted")
	}
	got, err := ReadMetadata(ctx, store, keys[0])
	if err != nil || got.SessionID != metadata.SessionID {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	if _, err = ReadMetadata(ctx, store, "sessions/codex/missing/metadata.json"); err == nil {
		t.Fatal("missing sidecar read succeeded")
	}
	if err = store.Put(ctx, "sessions/codex/broken/metadata.json", []byte(`{"schema_version":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadMetadata(ctx, store, "sessions/codex/broken/metadata.json"); err == nil {
		t.Fatal("sidecar without a source reference accepted")
	}
}
