package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
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

func registration(id, transcriptPath string) archive.SessionRegistration {
	return archive.SessionRegistration{
		ArchiveSessionID: id, NativeSessionID: "native-" + id, ProjectID: "project-1", ProjectRoot: "/p",
		Harness: archive.Harness{Name: "codex"}, TranscriptPath: transcriptPath,
		SessionStartedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		RegisteredAt:     time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC),
	}
}

func newTestStore(t *testing.T) *collector.LocalStore {
	t.Helper()
	store, err := collector.NewLocalStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// publishTwice registers a session, publishes it, then republishes changed
// content well past the collector's rate limit, so the first snapshot
// becomes superseded. It returns the first (now superseded) source key.
func publishTwice(t *testing.T, local *collector.LocalStore, store storage.ObjectStore, id, dir string, t0 time.Time) (firstKey string) {
	t.Helper()
	path := writeTranscript(t, dir, id+".jsonl", codexTranscript)
	reg := registration(id, path)
	reg.ArchiveSessionID = id
	if err := local.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Run(context.Background(), local, store, collector.Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	firstMeta := fetchMetadata(t, store, "codex", id)

	writeTranscript(t, dir, id+".jsonl", codexTranscript+"\n"+`{"type":"response_item","id":"m2","payload":{"type":"message","role":"user","content":"more"}}`)
	t1 := t0.Add(10 * time.Minute)
	if _, err := collector.Run(context.Background(), local, store, collector.Options{MachineID: "m", Now: func() time.Time { return t1 }}); err != nil {
		t.Fatal(err)
	}
	return firstMeta.SourceBundle.Key
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

func TestSweepRespectsGracePeriodBeforeDeletingSupersededSource(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKey := publishTwice(t, local, store, "s1", dir, t0)
	supersededAt := t0.Add(10 * time.Minute)

	// Sweep soon after supersession: still within the grace period.
	soon := supersededAt.Add(time.Hour)
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return soon }, GracePeriod: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.DeletedSnapshots != 0 {
		t.Fatalf("result=%#v", result)
	}
	if _, err := store.Get(context.Background(), firstKey); err != nil {
		t.Fatalf("superseded source must survive within the grace period: %v", err)
	}

	// The predecessor survives indefinitely, even after its grace period.
	later := supersededAt.Add(25 * time.Hour)
	result, err = Sweep(context.Background(), local, store, Options{Now: func() time.Time { return later }, GracePeriod: 24 * time.Hour})
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 0 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := store.Get(context.Background(), firstKey); err != nil {
		t.Fatal(err)
	}
	// A third publication makes the first source eligible, but retains second.
	secondKey := fetchMetadata(t, store, "codex", "s1").SourceBundle.Key
	publishThird(t, local, store, "s1", dir, later)
	result, err = Sweep(context.Background(), local, store, Options{Now: func() time.Time { return later.Add(25 * time.Hour) }})
	if err != nil || len(result.Errors) != 0 || result.DeletedSnapshots != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := store.Get(context.Background(), firstKey); err == nil {
		t.Fatal("older source survives")
	}
	if _, err := store.Get(context.Background(), secondKey); err != nil {
		t.Fatal("predecessor deleted", err)
	}

}

func TestSweepNeverDeletesTheCurrentSource(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	publishTwice(t, local, store, "s1", dir, t0)
	currentMeta := fetchMetadata(t, store, "codex", "s1")

	far := t0.Add(365 * 24 * time.Hour)
	if _, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return far }, GracePeriod: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(context.Background(), currentMeta.SourceBundle.Key); err != nil {
		t.Fatalf("the current source must never be deleted: %v", err)
	}
}

func TestSweepDeletesWholeSessionPastRetentionWindow(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, dir, "s1.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration("s1", path)); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Run(context.Background(), local, store, collector.Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}
	meta := fetchMetadata(t, store, "codex", "s1")

	past := t0.Add(91 * 24 * time.Hour)
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return past }, SessionMaxAge: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DeletedSessions) != 1 || result.DeletedSessions[0] != "s1" {
		t.Fatalf("result=%#v", result)
	}
	if _, err := store.Get(context.Background(), meta.SourceBundle.Key); err == nil {
		t.Fatal("expected the source to be deleted")
	}
	metaKey, _ := archive.MetadataObjectKey("codex", "s1")
	if _, err := store.Get(context.Background(), metaKey); err == nil {
		t.Fatal("expected the metadata to be deleted")
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 0 {
		t.Fatalf("expected local state forgotten: regs=%#v err=%v", regs, err)
	}
}

func TestSweepWithinRetentionWindowLeavesSessionAlone(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	path := writeTranscript(t, dir, "s1.jsonl", codexTranscript)
	if err := local.SaveRegistration(registration("s1", path)); err != nil {
		t.Fatal(err)
	}
	if _, err := collector.Run(context.Background(), local, store, collector.Options{MachineID: "m", Now: func() time.Time { return t0 }}); err != nil {
		t.Fatal(err)
	}

	soon := t0.Add(time.Hour)
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return soon }, SessionMaxAge: 90 * 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.DeletedSessions) != 0 {
		t.Fatalf("result=%#v", result)
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%#v err=%v", regs, err)
	}
}

// failingDeleteStore fails Delete for one specific key, to test that one
// session's failure does not block another's cleanup.
type failingDeleteStore struct {
	storage.ObjectStore
	failKey string
}

func (f failingDeleteStore) Delete(ctx context.Context, key string) error {
	if key == f.failKey {
		return errors.New("simulated delete failure")
	}
	return f.ObjectStore.Delete(ctx, key)
}

func TestSweepIsolatesOneSessionsFailure(t *testing.T) {
	dir := t.TempDir()
	local := newTestStore(t)
	memStore := storage.NewMemoryStore()
	t0 := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	firstKeyA := publishTwice(t, local, memStore, "a", dir, t0)
	_ = publishTwice(t, local, memStore, "b", dir, t0)

	publishThird(t, local, memStore, "a", dir, t0.Add(time.Hour))
	publishThird(t, local, memStore, "b", dir, t0.Add(time.Hour))
	store := failingDeleteStore{ObjectStore: memStore, failKey: firstKeyA}
	later := t0.Add(10*time.Minute + 25*time.Hour)
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return later }, GracePeriod: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.Errors["a"]; !ok {
		t.Fatalf("expected session a to report an error: %#v", result)
	}
	if result.DeletedSnapshots != 1 {
		t.Fatalf("session b should still have been cleaned up: %#v", result)
	}
}

func publishThird(t *testing.T, local *collector.LocalStore, store storage.ObjectStore, id, dir string, at time.Time) {
	t.Helper()
	path := filepath.Join(dir, id+".jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, []byte("\n"+`{"type":"response_item","id":"m3","payload":{"type":"message","role":"user","content":"third"}}`)...), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Run(context.Background(), local, store, collector.Options{MachineID: "m", Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
}

func TestSweepFailsClosedWithUnreadableCurrentMetadata(t *testing.T) {
	local := newTestStore(t)
	store := storage.NewMemoryStore()
	at := time.Now()
	first := publishTwice(t, local, store, "s1", t.TempDir(), at)
	key, _ := archive.MetadataObjectKey("codex", "s1")
	if err := store.Put(context.Background(), key, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	result, err := Sweep(context.Background(), local, store, Options{Now: func() time.Time { return at.Add(48 * time.Hour) }})
	if err != nil || result.Errors["s1"] == nil || result.DeletedSnapshots != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	if _, err := store.Get(context.Background(), first); err != nil {
		t.Fatal(err)
	}
}

func TestWholeSessionDeletionFailureNeverLeavesDanglingPointer(t *testing.T) {
	for _, failMetadata := range []bool{true, false} {
		t.Run(fmt.Sprint(failMetadata), func(t *testing.T) {
			local := newTestStore(t)
			mem := storage.NewMemoryStore()
			at := time.Now()
			publishTwice(t, local, mem, "s1", t.TempDir(), at)
			meta := fetchMetadata(t, mem, "codex", "s1")
			key, _ := archive.MetadataObjectKey("codex", "s1")
			failKey := meta.SourceBundle.Key
			if failMetadata {
				failKey = key
			}
			store := failingDeleteStore{ObjectStore: mem, failKey: failKey}
			opts := Options{Now: func() time.Time { return at.Add(100 * 24 * time.Hour) }, SessionMaxAge: 90 * 24 * time.Hour}
			result, err := Sweep(context.Background(), local, store, opts)
			if err != nil || result.Errors["s1"] == nil {
				t.Fatalf("%#v %v", result, err)
			}
			if _, err := mem.Get(context.Background(), key); err == nil {
				if _, err := mem.Get(context.Background(), meta.SourceBundle.Key); err != nil {
					t.Fatal("dangling pointer")
				}
			} else if failMetadata {
				t.Fatal("metadata deleted despite failure")
			}
			result, err = Sweep(context.Background(), local, mem, opts)
			if err != nil || len(result.Errors) != 0 || len(result.DeletedSessions) != 1 {
				t.Fatalf("retry: %#v %v", result, err)
			}
			objects, err := mem.List(context.Background(), "sessions/codex/s1/")
			if err != nil || len(objects) != 0 {
				t.Fatalf("%#v %v", objects, err)
			}
		})
	}
}
