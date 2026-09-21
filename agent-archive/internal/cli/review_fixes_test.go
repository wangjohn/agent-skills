package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

type failingUpdateStore struct{ storage.ObjectStore }

func (s failingUpdateStore) Put(context.Context, string, []byte) error { return errors.New("offline") }

func TestFailedScheduledUpdateBlocksDestinationSwitchUntilRetry(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, dir, now.Add(-time.Hour))
	path := writeCodexTranscript(t, dir)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "session_id": "native", "cwd": dir, "transcript_path": path}, now); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	ls, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	cloud := storage.NewMemoryStore()
	opts := collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return now }, Retry: storage.RetryPolicy{MaxAttempts: 1}}
	run := func(store storage.ObjectStore, fail bool) {
		t.Helper()
		r, e := collector.Run(context.Background(), ls, store, opts)
		if e != nil || (len(r.Errors) > 0) != fail {
			t.Fatalf("result=%+v err=%v", r, e)
		}
	}
	run(cloud, false)
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(bytes), "visible", "updated")), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	run(failingUpdateStore{cloud}, true)
	// Reopen the store through pendingSessions to verify persistence across runs.
	pending, err := pendingSessions(home, cfg)
	if err != nil || pending != 1 {
		t.Fatalf("pending=%d err=%v", pending, err)
	}
	next := cfg
	next.Storage.Bucket = "another-bucket"
	if err := reviewChanges(home, cfg, next, newPrompter(strings.NewReader(""), os.Stdout), testEnv(t, home, now)); err == nil {
		t.Fatal("destination switch allowed with a failed update")
	}
	run(cloud, false)
	pending, err = pendingSessions(home, cfg)
	if err != nil || pending != 0 {
		t.Fatalf("pending after retry=%d err=%v", pending, err)
	}
}

func TestHookWaitsForOverlappingRegistration(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	now := time.Now()
	setUpTestConfig(t, home, dir, now.Add(-time.Hour))
	unlock, err := local.NamedLock(home, "hooks.lock")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "session_id": "overlap", "cwd": dir}, now)
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("hook did not wait: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	regs, err := collector.OpenLocalStoreReadOnly(home).LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%+v err=%v", regs, err)
	}
}

func TestStatusReportsJournalWithoutConfiguration(t *testing.T) {
	home := t.TempDir()
	if err := local.Write(journalPath(home), setupJournal{}); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(testEnv(t, home, time.Now()))
	if err != nil || view.State != "Setup needs recovery" || view.Code != "recovery_required" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}
