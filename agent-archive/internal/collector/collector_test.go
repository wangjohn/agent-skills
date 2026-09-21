package collector

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

const codexTranscript = `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"assistant","content":"visible"}}`

func writeTranscript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func registration(t *testing.T, transcriptPath string) archive.SessionRegistration {
	t.Helper()
	return archive.SessionRegistration{
		ArchiveSessionID: "session-1", NativeSessionID: "native-1", ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "codex"}, TranscriptPath: transcriptPath,
		SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		RegisteredAt:     time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	}
}

func newTestStore(t *testing.T) *LocalStore {
	t.Helper()
	store, err := NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func fetchMetadata(t *testing.T, store storage.ObjectStore, harness, sessionID string) archive.Metadata {
	t.Helper()
	key, err := archive.MetadataObjectKey(harness, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	var m archive.Metadata
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func fetchBundle(t *testing.T, store storage.ObjectStore, metadata archive.Metadata) archive.SourceBundle {
	t.Helper()
	data, err := store.Get(context.Background(), metadata.SourceBundle.Key)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var bundle archive.SourceBundle
	if err := json.NewDecoder(reader).Decode(&bundle); err != nil {
		t.Fatal(err)
	}
	return bundle
}

type metadataFailStore struct {
	*storage.MemoryStore
	failMetadata bool
}

func (s *metadataFailStore) Put(ctx context.Context, key string, data []byte) error {
	if s.failMetadata && filepath.Base(key) == "metadata.json" {
		return errors.New("injected metadata failure")
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func TestRunPublishesNewSession(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || result.Published[0] != "session-1" || len(result.Errors) != 0 {
		t.Fatalf("result=%#v", result)
	}
	metadata := fetchMetadata(t, store, "codex", "session-1")
	if !metadata.CapturedAt.Equal(now) {
		t.Fatalf("captured_at=%s want=%s", metadata.CapturedAt, now)
	}
	if err := metadata.ValidateSourceReference(); err != nil {
		t.Fatalf("published metadata has no verified source reference: %v", err)
	}
}

func TestRunSkipsUnchangedSessionAndReusesCapturedAt(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	first := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return first }}); err != nil {
		t.Fatal(err)
	}
	firstMetadata := fetchMetadata(t, store, "codex", "session-1")

	second := first.Add(10 * time.Minute)
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return second }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || result.Skipped[0] != "session-1" || len(result.Published) != 0 {
		t.Fatalf("result=%#v", result)
	}
	secondMetadata := fetchMetadata(t, store, "codex", "session-1")
	if !secondMetadata.CapturedAt.Equal(firstMetadata.CapturedAt) {
		t.Fatalf("captured_at changed on an unchanged scan: %s -> %s", firstMetadata.CapturedAt, secondMetadata.CapturedAt)
	}
	if !secondMetadata.MetadataDerivedAt.Equal(firstMetadata.MetadataDerivedAt) {
		t.Fatalf("metadata republished though nothing changed: derived_at %s -> %s", firstMetadata.MetadataDerivedAt, secondMetadata.MetadataDerivedAt)
	}
}

func TestRunRateLimitsRepublishAndReusesFirstDetectedCapturedAt(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}

	// New evidence arrives, but well within the minimum upload interval.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	tDetected := t0.Add(30 * time.Second)
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return tDetected }, MinUploadInterval: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Skipped) != 1 || len(result.Published) != 0 {
		t.Fatalf("expected rate-limited skip, got %#v", result)
	}
	// Metadata in storage must still be the original, unpublished candidate.
	unchanged := fetchMetadata(t, store, "codex", "session-1")
	if !unchanged.CapturedAt.Equal(t0) {
		t.Fatalf("rate-limited candidate must not be published: captured_at=%s", unchanged.CapturedAt)
	}

	// No further content change; enough time has now passed to publish the
	// pending candidate, which must carry the timestamp of when it was first
	// detected, not this run's time.
	tPublish := t0.Add(4 * time.Minute)
	result, err = Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return tPublish }, MinUploadInterval: 3 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("expected the withheld candidate to publish, got %#v", result)
	}
	published := fetchMetadata(t, store, "codex", "session-1")
	if !published.CapturedAt.Equal(tDetected) {
		t.Fatalf("captured_at=%s want=%s (first detection time, not publish time)", published.CapturedAt, tDetected)
	}
}

func TestRunLeavesRequestPendingOnScanErrorAndIsolatesOtherSessions(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	badReg := registration(t, filepath.Join(dir, "does-not-exist.jsonl"))
	badReg.ArchiveSessionID = "bad-session"
	if err := local.SaveRegistration(badReg); err != nil {
		t.Fatal(err)
	}
	goodPath := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration(t, goodPath)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.SaveRequest("bad-session", "stop", now); err != nil {
		t.Fatal(err)
	}

	store := storage.NewMemoryStore()
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || result.Published[0] != "session-1" {
		t.Fatalf("good session did not publish: %#v", result)
	}
	if _, ok := result.Errors["bad-session"]; !ok {
		t.Fatalf("expected an error for bad-session, got %#v", result)
	}
	requests, err := local.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].ArchiveSessionID != "bad-session" {
		t.Fatalf("request should remain pending for retry: %#v", requests)
	}
}

func TestRunFoldsHookEvidenceAndCompletesRequest(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}

	// Transcript content is unchanged, but a stop hook delivers new
	// supplemental evidence (a final response). That alone must count as new
	// evidence worth republishing.
	t1 := t0.Add(10 * time.Minute)
	evidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t1, Provenance: "hook", Payload: map[string]any{"turn_id": "t1"}}
	if err := local.SaveRequest("session-1", "stop", t1, evidence); err != nil {
		t.Fatal(err)
	}

	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("expected hook evidence to trigger a republish: %#v", result)
	}
	requests, err := local.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatalf("request should be completed after being folded in: %#v", requests)
	}
}

func TestRunUnsafeTranscriptNeverPublishes(t *testing.T) {
	dir := t.TempDir()
	// A record with no recognized type and no role is an unknown record;
	// combined with nothing else retainable, NewSourceBundle refuses it.
	path := writeTranscript(t, dir, "codex.jsonl", `{"type":"unknown_kind","x":1}`)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 0 || len(result.Errors) != 1 {
		t.Fatalf("result=%#v", result)
	}
	if _, err := store.List(context.Background(), "sessions"); err != nil {
		t.Fatal(err)
	} else if objs, _ := store.List(context.Background(), "sessions"); len(objs) != 0 {
		t.Fatalf("no object should have been uploaded: %#v", objs)
	}
}

func TestRunSurvivesRestartAcrossRateLimitedPass(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	local1, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := local1.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), local1, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	tDetected := t0.Add(30 * time.Second)
	if result, err := Run(context.Background(), local1, store, Options{MachineID: "m", Now: func() time.Time { return tDetected }}); err != nil {
		t.Fatal(err)
	} else if len(result.Skipped) != 1 {
		t.Fatalf("expected rate-limited skip before restart: %#v", result)
	}

	// Simulate a process restart: a fresh LocalStore handle over the same
	// root directory, as a newly started collector process would construct.
	local2, err := NewLocalStore(root)
	if err != nil {
		t.Fatal(err)
	}
	tPublish := t0.Add(4 * time.Minute)
	result, err := Run(context.Background(), local2, store, Options{MachineID: "m", Now: func() time.Time { return tPublish }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("withheld candidate should survive restart and publish: %#v", result)
	}
	published := fetchMetadata(t, store, "codex", "session-1")
	if !published.CapturedAt.Equal(tDetected) {
		t.Fatalf("captured_at=%s want=%s across restart", published.CapturedAt, tDetected)
	}
}

func TestRunRetriesPersistedBytesAndDoesNotAcknowledgeNewerRequest(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local1, err := NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := local1.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstEvidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0, Provenance: "hook", Payload: map[string]any{"turn_id": "first"}}
	if err := local1.SaveRequest("session-1", "stop", t0, firstEvidence); err != nil {
		t.Fatal(err)
	}
	store := &metadataFailStore{MemoryStore: storage.NewMemoryStore(), failMetadata: true}
	result, err := Run(context.Background(), local1, store, Options{MachineID: "m", Now: func() time.Time { return t0 }, Retry: storage.RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("expected injected publication failure: %#v", result)
	}
	pending, found, err := local1.LoadPending("session-1")
	if err != nil || !found || !pending.Attempted {
		t.Fatalf("pending=%#v found=%v err=%v", pending, found, err)
	}
	if completed, err := local1.CompleteRequest("session-1", pending.RequestToken); err != nil || !completed {
		t.Fatalf("simulate pre-crash acknowledgement: completed=%v err=%v", completed, err)
	}

	// Both the transcript and request advance before restart. Retrying the
	// interrupted transaction must still use the already-rendered bytes, and
	// must not acknowledge the request revision it did not cover.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"later","payload":{"type":"message","role":"user","content":"later"}}`)
	t1 := t0.Add(time.Minute)
	secondEvidence := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t1, Provenance: "hook", Payload: map[string]any{"turn_id": "second"}}
	if err := local1.SaveRequest("session-1", "session_end", t1, secondEvidence); err != nil {
		t.Fatal(err)
	}
	store.failMetadata = false
	local2, err := NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	result, err = Run(context.Background(), local2, store, Options{MachineID: "m", Now: func() time.Time { return t1 }, Retry: storage.RetryPolicy{MaxAttempts: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 {
		t.Fatalf("persisted publication was not retried: %#v", result)
	}
	gotSource, err := store.Get(context.Background(), pending.SourceKey)
	if err != nil || !bytes.Equal(gotSource, pending.SourceBytes) {
		t.Fatalf("source retry changed persisted bytes: err=%v", err)
	}
	gotMetadata, err := store.Get(context.Background(), pending.MetadataKey)
	if err != nil || !bytes.Equal(gotMetadata, pending.MetadataBytes) {
		t.Fatalf("metadata retry changed persisted bytes: err=%v", err)
	}
	requests, err := local2.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].Token == pending.RequestToken {
		t.Fatalf("newer request was incorrectly acknowledged: %#v err=%v", requests, err)
	}
}

func TestStopRequestFlushesRateLimitAndPreservesEarlierHookEvidence(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	e0 := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0, Provenance: "hook", Payload: map[string]any{"turn_id": "first"}}
	if err := local.SaveRequest("session-1", "stop", t0, e0); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	t1 := t0.Add(30 * time.Second)
	if result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t1 }}); err != nil || len(result.Published) != 0 {
		t.Fatalf("expected an active rate limit: result=%#v err=%v", result, err)
	}
	t2 := t1.Add(time.Second)
	e1 := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t2, Provenance: "hook", Payload: map[string]any{"turn_id": "second"}}
	if err := local.SaveRequest("session-1", "stop", t2, e1); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t2 }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("stop request did not flush debounce: result=%#v err=%v", result, err)
	}
	bundle := fetchBundle(t, store, fetchMetadata(t, store, "codex", "session-1"))
	if len(bundle.SupplementalEvidence) != 2 {
		t.Fatalf("hook evidence was lost across scans: %#v", bundle.SupplementalEvidence)
	}
}

func TestRunPreservesLastGoodSnapshotAcrossTranscriptRewrite(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	before := fetchMetadata(t, store, "codex", "session-1")
	writeTranscript(t, dir, "codex.jsonl", `{"type":"turn_context","model":"gpt-test"}`)
	t1 := t0.Add(10 * time.Minute)
	if err := local.SaveRequest("session-1", "stop", t1); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t1 }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("rewrite should remain an explicit capture failure: %#v", result)
	}
	after := fetchMetadata(t, store, "codex", "session-1")
	if after.SourceBundle.SHA256 != before.SourceBundle.SHA256 {
		t.Fatalf("rewrite replaced richer last-good source: before=%s after=%s", before.SourceBundle.SHA256, after.SourceBundle.SHA256)
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 {
		t.Fatalf("rewrite request should remain pending: %#v err=%v", requests, err)
	}
}

func TestStableSupplementalObservationDoesNotRepublish(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	now := t0
	provider := func(_ archive.SessionRegistration, observedAt time.Time) ([]archive.SupplementalEvidence, error) {
		return []archive.SupplementalEvidence{{Kind: archive.EvidenceKindSkillInventory, ObservedAt: observedAt, Provenance: "filesystem", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": "review", "sha256": "abc"}}}}}, nil
	}
	options := Options{MachineID: "m", Now: func() time.Time { return now }, SupplementalEvidence: provider}
	if _, err := Run(context.Background(), local, store, options); err != nil {
		t.Fatal(err)
	}
	first := fetchMetadata(t, store, "codex", "session-1")
	now = t0.Add(10 * time.Minute)
	result, err := Run(context.Background(), local, store, options)
	if err != nil || len(result.Published) != 0 {
		t.Fatalf("poll timestamp manufactured a publication: result=%#v err=%v", result, err)
	}
	second := fetchMetadata(t, store, "codex", "session-1")
	if !second.MetadataDerivedAt.Equal(first.MetadataDerivedAt) {
		t.Fatalf("stable inventory changed metadata timestamp: %s -> %s", first.MetadataDerivedAt, second.MetadataDerivedAt)
	}
}

func TestChangedSupplementalInventoryPreservesEarlierObservation(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	first := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillInventory, ObservedAt: t0, Provenance: "filesystem", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": "one"}}}}
	second := archive.SupplementalEvidence{Kind: archive.EvidenceKindSkillInventory, ObservedAt: t0.Add(time.Hour), Provenance: "filesystem", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": "two"}}}}
	merged := mergeSupplementalEvidence([]archive.SupplementalEvidence{first}, []archive.SupplementalEvidence{second})
	if len(merged) != 2 || !merged[0].ObservedAt.Equal(t0) {
		t.Fatalf("inventory history was not preserved: %#v", merged)
	}
}

func TestRunUpgradesAndCompletesLegacyTokenlessRequest(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	store := newTestStore(t)
	if err := store.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if err := local.Write(store.requestPath("session-1"), Request{ArchiveSessionID: "session-1", Reasons: []string{"stop"}, RequestedAt: now}); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), store, storage.NewMemoryStore(), Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil || len(result.Published) != 1 {
		t.Fatalf("legacy request was not processed: result=%#v err=%v", result, err)
	}
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 0 {
		t.Fatalf("legacy request remained pending: %#v err=%v", requests, err)
	}
}

func TestRunRejectsTranscriptAboveCollectionLimit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxTranscriptBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	result, err := Run(context.Background(), local, store, Options{MachineID: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 1 {
		t.Fatalf("oversized transcript was not rejected: %#v", result)
	}
	objects, _ := store.List(context.Background(), "sessions")
	if len(objects) != 0 {
		t.Fatalf("oversized transcript uploaded objects: %#v", objects)
	}
}

func TestRunIgnoresIncompleteFinalJSONLRecord(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	before := fetchMetadata(t, store, "codex", "session-1")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("\n" + `{"type":"response_item","id":"partial"`); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0.Add(10 * time.Minute) }})
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("partial final record changed capture: result=%#v err=%v", result, err)
	}
	after := fetchMetadata(t, store, "codex", "session-1")
	if after.SourceBundle.SHA256 != before.SourceBundle.SHA256 {
		t.Fatalf("partial record changed source: %s -> %s", before.SourceBundle.SHA256, after.SourceBundle.SHA256)
	}
}

func TestSaveRequestCoalescesReasonsAndEvidence(t *testing.T) {
	local := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	e1 := archive.SupplementalEvidence{Kind: archive.EvidenceKindFinalResponse, ObservedAt: t0, Provenance: "hook", Payload: map[string]any{"a": 1}}
	if err := local.SaveRequest("s1", "stop", t0, e1); err != nil {
		t.Fatal(err)
	}
	t1 := t0.Add(time.Second)
	e2 := archive.SupplementalEvidence{Kind: archive.EvidenceKindLifecycleHook, ObservedAt: t1, Provenance: "hook", Payload: map[string]any{"b": 2}}
	if err := local.SaveRequest("s1", "session_end", t1, e2); err != nil {
		t.Fatal(err)
	}
	requests, err := local.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatalf("expected coalesced single request, got %d", len(requests))
	}
	req := requests[0]
	if !req.RequestedAt.Equal(t1) || len(req.Reasons) != 2 || len(req.HookEvidence) != 2 {
		t.Fatalf("req=%#v", req)
	}
}

// TestRunComposesWithLocalLock proves the intended usage pattern for a
// future scheduled caller: local.Lock(home) around Run prevents a second
// overlapping collector process from acting on the same home concurrently.
// Locking itself is internal/local's responsibility and is tested there.
func TestRunComposesWithLocalLock(t *testing.T) {
	home := t.TempDir()
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	if _, err := local.Lock(home); err != local.ErrBusy {
		t.Fatalf("expected a second collector run to be excluded, got %v", err)
	}

	localStore, err := NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Run(context.Background(), localStore, storage.NewMemoryStore(), Options{MachineID: "m"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureArchiveSessionIDPersistsAndReuses(t *testing.T) {
	local := newTestStore(t)
	id1, created1, err := local.EnsureArchiveSessionID("native-abc")
	if err != nil {
		t.Fatal(err)
	}
	if !created1 || id1 == "" {
		t.Fatalf("id1=%q created1=%v", id1, created1)
	}
	id2, created2, err := local.EnsureArchiveSessionID("native-abc")
	if err != nil {
		t.Fatal(err)
	}
	if created2 || id2 != id1 {
		t.Fatalf("expected reuse: id1=%q id2=%q created2=%v", id1, id2, created2)
	}
	id3, created3, err := local.EnsureArchiveSessionID("native-xyz")
	if err != nil {
		t.Fatal(err)
	}
	if !created3 || id3 == id1 {
		t.Fatalf("expected a distinct fresh ID for a different native session: id3=%q", id3)
	}
}

func TestArchiveSessionIDRejectsPathLikeInputSafely(t *testing.T) {
	home := t.TempDir()
	local, err := NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	// A native session ID is harness-controlled input; it must not be usable
	// to escape the sessions/ directory even though it is only ever hashed,
	// not used directly as a path component.
	id, _, err := local.EnsureArchiveSessionID("../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("expected a valid archive session ID")
	}
	// Checked against home, the store's actual base directory — not some
	// other, unrelated temp directory — so this would actually catch a
	// future regression that built a path from the native ID directly.
	if _, err := os.Stat(filepath.Join(home, "..", "..", "etc", "passwd")); err == nil {
		t.Fatal("unexpected file escape")
	}
}

func TestRunDeclinesPublishWhenSkillUseRequiredAndAbsent(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }, RequireSkillUse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("expected a policy decline, not a publish: %#v", result)
	}
	if objs, err := store.List(context.Background(), "sessions"); err != nil || len(objs) != 0 {
		t.Fatalf("nothing should have been uploaded: objs=%#v err=%v", objs, err)
	}

	// A later scan with unchanged content must not retry the publish just
	// because time passed (unlike a rate-limited candidate).
	later := now.Add(24 * time.Hour)
	result, err = Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return later }, RequireSkillUse: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 0 || len(result.Skipped) != 1 {
		t.Fatalf("a decline must not auto-publish later: %#v", result)
	}
}

// TestRunDeclinedCandidateIsNotSpuriouslyRateLimitedOnLaterChange guards a
// regression where a declined candidate was cached with PublishedAt set to
// its capture time instead of a zero time. Since nothing was ever actually
// published, a later genuine content change (still within the rate-limit
// window) must not be mistaken for a retry of a real publish and withheld;
// it should be evaluated (and, here, declined again) immediately.
func TestRunDeclinedCandidateIsNotSpuriouslyRateLimitedOnLaterChange(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }, RequireSkillUse: true}); err != nil {
		t.Fatal(err)
	}
	_, publishedAt, status, found, err := local.LoadPublished("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || status != CacheStatusDeclined || !publishedAt.IsZero() {
		t.Fatalf("expected a zero-time declined cache entry: found=%v status=%v publishedAt=%v", found, status, publishedAt)
	}

	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"still no skill use"}}`)
	t1 := t0.Add(30 * time.Second)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t1 }, RequireSkillUse: true, MinUploadInterval: 3 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	_, _, status2, found2, err := local.LoadPublished("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found2 || status2 != CacheStatusDeclined {
		t.Fatalf("expected an immediate re-decline, not a spurious rate limit: found=%v status=%v", found2, status2)
	}
}

func TestRunRecordsSupersededSourceOnRepublish(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, "codex.jsonl", codexTranscript)
	local := newTestStore(t)
	if err := local.SaveRegistration(registration(t, path)); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	firstMetadata := fetchMetadata(t, store, "codex", "session-1")

	// Republish with new content well past the rate limit, so it actually
	// supersedes the first snapshot.
	writeTranscript(t, dir, "codex.jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	t1 := t0.Add(10 * time.Minute)
	if _, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return t1 }}); err != nil {
		t.Fatal(err)
	}

	superseded, err := local.LoadSuperseded("session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(superseded) != 1 || superseded[0].Key != firstMetadata.SourceBundle.Key || !superseded[0].SupersededAt.Equal(t1) {
		t.Fatalf("superseded=%#v firstKey=%q", superseded, firstMetadata.SourceBundle.Key)
	}
	// The superseded object must still exist in storage (grace period).
	if _, err := store.Get(context.Background(), firstMetadata.SourceBundle.Key); err != nil {
		t.Fatalf("superseded source must remain until retention deletes it: %v", err)
	}
}

// TestRunFallsBackToCursorTextWhenJSONLIsUnrecognized guards against a
// regression where CursorAdapter.FilterText — fully implemented and unit
// tested in the archive package — was never actually reachable from the
// real collection pipeline. The spec notes Cursor's hook-provided
// transcript path can point to either a JSONL or a plain text transcript
// depending on version; a text one must still publish, not be silently
// dropped just because it isn't JSONL.
func TestRunFallsBackToCursorTextWhenJSONLIsUnrecognized(t *testing.T) {
	dir := t.TempDir()
	textTranscript := "User: hello\nAssistant: hi there\n"
	path := writeTranscript(t, dir, "cursor.txt", textTranscript)
	local := newTestStore(t)
	reg := registration(t, path)
	reg.Harness = archive.Harness{Name: "cursor"}
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	store := storage.NewMemoryStore()
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)

	result, err := Run(context.Background(), local, store, Options{MachineID: "m", Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Published) != 1 || len(result.Errors) != 0 {
		t.Fatalf("expected the Cursor text transcript to publish via the text fallback: %#v", result)
	}
	metadata := fetchMetadata(t, store, "cursor", "session-1")
	if metadata.SourceBundle.Key == "" {
		t.Fatalf("expected a verified source reference: %#v", metadata)
	}
}
