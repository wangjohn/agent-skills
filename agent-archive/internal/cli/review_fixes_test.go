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

func TestSkillObserverReadsUserScopeOncePerHarnessAndProjectScopePerProject(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	projectA, projectB := t.TempDir(), t.TempDir()
	write := func(root, name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "SKILL.md"), []byte("---\nname: "+name+"\n---\n"+body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	userRoot := filepath.Join(userHome, ".claude", "skills")
	write(userRoot, "shared", "v1")
	write(filepath.Join(projectA, ".claude", "skills"), "alpha", "a")
	write(filepath.Join(projectB, ".claude", "skills"), "beta", "b")
	env := testEnv(t, home, time.Now())
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	observe := skillObserver(env)
	harness := archive.Harness{Name: "claude"}
	first, err := observe(archive.SessionRegistration{ProjectRoot: projectA, Harness: harness}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// Changing the user-scope skill between sessions of the same pass must
	// not be observed again: the user root is read once per harness.
	write(userRoot, "shared", "v2")
	second, err := observe(archive.SessionRegistration{ProjectRoot: projectB, Harness: harness}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	snapshots := func(items []archive.SupplementalEvidence) map[string]string {
		out := map[string]string{}
		for _, item := range items {
			if item.Kind == archive.EvidenceKindSkillSnapshot {
				out[item.Payload["scope"].(string)+"/"+item.Payload["name"].(string)] = item.Payload["snapshot"].(string)
			}
		}
		return out
	}
	got1, got2 := snapshots(first), snapshots(second)
	if !strings.HasSuffix(got1["user_claude/shared"], "v1") || got1["user_claude/shared"] != got2["user_claude/shared"] {
		t.Fatalf("user scope was re-read per project: first=%v second=%v", got1, got2)
	}
	if got1["project_claude/alpha"] == "" || got1["project_claude/beta"] != "" || got2["project_claude/beta"] == "" || got2["project_claude/alpha"] != "" {
		t.Fatalf("project scope was not keyed per project: first=%v second=%v", got1, got2)
	}
	for _, items := range [][]archive.SupplementalEvidence{first, second} {
		if len(items) < 2 || items[0].Payload["scope"] != "user_claude" || items[len(items)-2].Payload["scope"] != "project_claude" {
			t.Fatalf("scope order changed: %#v", items)
		}
	}
}

type failingUpdateStore struct{ storage.ObjectStore }

func (s failingUpdateStore) Put(context.Context, string, []byte) error { return errors.New("offline") }

func TestFailedScheduledUpdateBlocksDestinationSwitchUntilRetry(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, dir, now.Add(-time.Hour))
	path := writeCodexTranscript(t, dir)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": dir, "transcript_path": path}, now); err != nil {
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
	update := `{"type":"response_item","id":"update","payload":{"type":"message","role":"user","content":"updated"}}`
	if err := os.WriteFile(path, append(append(bytes, '\n'), update...), 0600); err != nil {
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
		done <- handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "overlap", "cwd": dir}, now)
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
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": project, "transcript_path": path}, now); err != nil {
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
	// Append a record: an in-place edit would be a rewrite (a recorded
	// capture gap), not a rate-limited update.
	update := `{"type":"response_item","id":"m2","payload":{"type":"message","role":"assistant","content":"updated"}}`
	if err := os.WriteFile(path, append(append(b, '\n'), update...), 0600); err != nil {
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

func TestBlockedCaptureIsNotPendingAndStatusReportsGap(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	path := writeCodexTranscript(t, project)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native", "cwd": project, "transcript_path": path}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	before, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	// Compaction rewrote the transcript to something that no longer extends
	// the published snapshot, and a stop hook asked for a flush.
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"gpt-test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": "native", "cwd": project, "transcript_path": path}, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(2 * time.Second) }
	for pass := 0; pass < 2; pass++ {
		result, err := runOnePass(env, false)
		if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
			t.Fatalf("pass %d: result=%+v err=%v", pass, result, err)
		}
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := pendingSessions(home, cfg); err != nil || pending != 0 {
		t.Fatalf("blocked session counted as pending: %d err=%v", pending, err)
	}
	next := cfg
	next.Storage.Bucket = "another-bucket"
	if err := reviewChanges(home, cfg, next, newPrompter(strings.NewReader(""), os.Stdout), env); err != nil {
		t.Fatalf("blocked session prevented a destination change: %v", err)
	}
	after, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if after.Collector.LastError != "" || after.Collector.PendingCount != 0 || len(after.Apps[0].CaptureGaps) != 1 || !after.Apps[0].LastPublishedAt.Equal(before.Apps[0].LastPublishedAt) || after.Apps[0].State != "published; source verified" {
		t.Fatalf("status=%+v", after)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "1 with a capture gap") || strings.Contains(out.String(), "Last error") {
		t.Fatalf("status output=%s", out.String())
	}
}
