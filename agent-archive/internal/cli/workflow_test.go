package cli

import (
	"bytes"
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
