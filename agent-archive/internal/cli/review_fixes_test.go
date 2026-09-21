package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"os"
	"path/filepath"
	"reflect"
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

type settingsProbeStore struct {
	storage.ObjectStore
	fail bool
}

func (s settingsProbeStore) Put(ctx context.Context, key string, value []byte) error {
	if s.fail {
		return errors.New("incorrect region or folder")
	}
	return s.ObjectStore.Put(ctx, key, value)
}

func TestFailedProbeAllowsRegionAndPrefixCorrection(t *testing.T) {
	for _, choice := range []string{"region", "prefix"} {
		t.Run(choice, func(t *testing.T) {
			home := t.TempDir()
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			attempts := 0
			env.OpenStore = func(cfg config.Config) (storage.ObjectStore, error) {
				attempts++
				fixed := cfg.Storage.Region == "eu-west-1"
				if choice == "prefix" {
					fixed = cfg.Storage.Prefix == "allowed/"
				}
				return settingsProbeStore{storage.NewMemoryStore(), !fixed}, nil
			}
			value := "eu-west-1"
			if choice == "prefix" {
				value = "allowed/"
			}
			input := strings.TrimSuffix(s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), "y\n")
			setupRun(t, env, input+"edit\n"+choice+"\n"+value+"\ny\n", 0)
			if attempts != 2 {
				t.Fatalf("attempts=%d", attempts)
			}
			cfg, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatalf("saved=%v err=%v", found, err)
			}
			if choice == "region" && cfg.Storage.Region != value || choice == "prefix" && cfg.Storage.Prefix != value {
				t.Fatal(cfg.Storage)
			}
		})
	}
}

func TestStatusDetectsPartialHooks(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir()), 0)
	path := filepath.Join(userHome, ".codex", "hooks.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		t.Fatal(err)
	}
	delete(root["hooks"].(map[string]any), "SessionStart")
	if err := local.Write(path, root); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil || view.State != "Needs attention" || view.Apps[0].Hooks == "installed" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
}

func TestResumeDraftLeftAfterDestinationCommitPreservesOwnership(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("original", "us-east-1", "profile", true, false, false, project), 0)
	old, _, _ := config.Load(home)
	stale := setupDraft{Version: 1, Step: 2, Config: old}
	stale.Config.Storage.Bucket = "new-bucket"
	// Persist the pre-commit draft, then commit without the wizard's deletion:
	// exactly the state left by a crash or failed draft removal after apply.
	if err := local.Write(filepath.Join(home, "setup-draft.json"), stale); err != nil {
		t.Fatal(err)
	}
	next := stale.Config
	exe, err := env.executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := applySetup(home, userHome, exe, old, &next, env); err != nil {
		t.Fatal(err)
	}
	committed, _, _ := config.Load(home)
	if committed.DestinationSince.IsZero() {
		t.Fatal("switch did not establish boundary")
	}
	env.Now = func() time.Time { return now.Add(time.Hour) }
	setupRun(t, env, "continue\ny\n", 0)
	resumed, _, _ := config.Load(home)
	if !resumed.DestinationSince.Equal(committed.DestinationSince) || !reflect.DeepEqual(resumed.PreviousDestinations, committed.PreviousDestinations) {
		t.Fatalf("ownership changed: before=%+v after=%+v", committed, resumed)
	}
	if resumed.AcceptSession(archive.SessionRegistration{SessionStartedAt: now.Add(-time.Minute), Harness: archive.Harness{Name: "codex"}, ProjectRoot: resumed.Archive.Projects[0].Root}) {
		t.Fatal("retired session became eligible")
	}
}

func TestStatusPreservesPublicationDuringRateLimitedUpdate(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	path := writeCodexTranscript(t, project)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "session_id": "native", "cwd": project, "transcript_path": path}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	before, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(string(b), "visible", "updated")), 0600); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(time.Second) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	after, err := readStatus(env)
	if err != nil || after.Apps[0].LastPublishedAt.IsZero() || !after.Apps[0].LastPublishedAt.Equal(before.Apps[0].LastPublishedAt) || after.State == "Waiting for capture" || after.Collector.PendingCount != 1 {
		t.Fatalf("status=%+v err=%v", after, err)
	}
}
