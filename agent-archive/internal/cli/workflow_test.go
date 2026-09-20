package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func writeCodexTranscript(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "codex.jsonl")
	content := `{"type":"turn_context","model":"gpt-test"}
{"type":"response_item","id":"m1","payload":{"type":"message","role":"assistant","content":"visible"}}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSyncEndToEndFromHookThroughPublish(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, home, now)
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "1 published") {
		t.Fatalf("stdout=%s", stdout.String())
	}

	stdout.Reset()
	if code := runStatusCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Pending:        0") {
		t.Fatalf("status did not reflect the publish: %s", stdout.String())
	}
}

func TestSyncReportsNotSetUp(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(stderr.String(), "run `agent-archive setup`") {
		t.Fatalf("stderr=%s", stderr.String())
	}
}

func TestPauseBlocksSyncAndResumeUnblocksIt(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	env := testEnv(t, home, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))

	var out, errOut bytes.Buffer
	if code := runPauseCommand(&out, &errOut, env, true); code != 0 {
		t.Fatalf("code=%d", code)
	}

	out.Reset()
	if code := runSyncCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatalf("paused sync should exit 0, not fail: code=%d", code)
	}
	if !strings.Contains(out.String(), "paused") {
		t.Fatalf("sync should say it's paused, not just do nothing silently: %s", out.String())
	}

	out.Reset()
	if code := runPauseCommand(&out, &errOut, env, false); code != 0 {
		t.Fatalf("code=%d", code)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || cfg.Paused {
		t.Fatalf("cfg=%#v found=%v err=%v", cfg, found, err)
	}
}

func TestCollectCommandSilentlyNoopsWhenPausedOrNotSetUp(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 || errOut.Len() != 0 {
		t.Fatalf("not-set-up _collect should be silent: code=%d stderr=%s", code, errOut.String())
	}

	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 || errOut.Len() != 0 {
		t.Fatalf("paused _collect should be silent: code=%d stderr=%s", code, errOut.String())
	}
}

func TestCollectCommandRunsQuietlyOnSuccess(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	env := testEnv(t, home, now)
	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 0 || errOut.Len() != 0 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errOut.String())
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.LastPublishedAt.IsZero() {
		t.Fatal("expected a publish to have happened")
	}
}

func TestSyncReportsLockContentionAsAnError(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	env := testEnv(t, home, time.Now())
	var out, errOut bytes.Buffer
	if code := runSyncCommand(nil, &out, &errOut, env); code != 1 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errOut.String(), "already running") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

func TestStatusShowsNotSetUp(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var out, errOut bytes.Buffer
	if code := runStatusCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "Not set up") {
		t.Fatalf("out=%s", out.String())
	}
}

func TestSyncRunsRetentionSweepAndDeletesExpiredSession(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	// setUpTestConfig doesn't set RetentionDays; give it a short window here.
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 1
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	var mem *storage.MemoryStore
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		if mem == nil {
			mem = storage.NewMemoryStore()
		}
		return mem, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	// Two days later, well past the 1-day retention window.
	later := now.Add(48 * time.Hour)
	env2 := env
	env2.Now = func() time.Time { return later }
	stdout.Reset()
	stderr.Reset()
	if code := runSyncCommand(nil, &stdout, &stderr, env2); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("expected the session to have aged out: %#v", regs)
	}
}

func TestCollectCommandRecordsPreflightFailureInStatus(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))

	env := testEnv(t, home, time.Now())
	openErr := errors.New("simulated broken storage credentials")
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, openErr }

	var out, errOut bytes.Buffer
	if code := runCollectCommand(nil, &out, &errOut, env); code != 1 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.LastError, openErr.Error()) {
		t.Fatalf("expected status.LastError to record the preflight failure, got %q", status.LastError)
	}
}

// failingDeleteStore fails every Delete, used to force retention.Sweep to
// report a per-session error without needing to know its internal object
// key naming.
type failingDeleteStore struct{ storage.ObjectStore }

func (f failingDeleteStore) Delete(ctx context.Context, key string) error {
	return errors.New("simulated delete failure")
}

func TestSyncSurfacesRetentionErrorsInResultAndStatus(t *testing.T) {
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RetentionDays = 1
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	var mem *storage.MemoryStore
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		if mem == nil {
			mem = storage.NewMemoryStore()
		}
		return mem, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}

	// Two days later, well past the 1-day retention window, but every
	// delete this pass attempts now fails.
	later := now.Add(48 * time.Hour)
	env2 := env
	env2.Now = func() time.Time { return later }
	env2.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return failingDeleteStore{mem}, nil
	}
	stdout.Reset()
	stderr.Reset()
	code := runSyncCommand(nil, &stdout, &stderr, env2)
	if code != 1 {
		t.Fatalf("expected sync to report the retention failure as an error: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "retention:") {
		t.Fatalf("expected sync's report to mention the retention failure: stdout=%s", stdout.String())
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(status.LastError, "failed to scan, publish, or clean up") {
		t.Fatalf("expected status.LastError to reflect the retention failure, got %q", status.LastError)
	}
	// The session must still be registered locally: a failed delete must
	// never be treated as if it had succeeded.
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 {
		t.Fatalf("expected the session to remain registered after a failed retention delete: %#v", regs)
	}
}
