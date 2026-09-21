package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func TestStatusRequiresEveryApplicationProjectPair(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	projectA, projectB := t.TempDir(), t.TempDir()
	cfg := pairTestConfig(now, []string{"codex", "claude"}, projectA, projectB)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	publishPairSession(t, home, store, remote, cfg, now, "codex-a", "codex", projectA, true)
	publishPairSession(t, home, store, remote, cfg, now, "codex-b", "codex", projectB, false)

	env := pairStatusEnv(t, home, userHome, now, "codex", "claude")
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified || len(view.Apps[0].Projects) != 2 || !view.Apps[0].Projects[0].ReadBackVerified || view.Apps[0].Projects[1].ReadBackVerified {
		t.Fatalf("Codex pair coverage=%#v", view.Apps[0])
	}
	if view.Apps[1].ReadBackVerified || view.Apps[1].Projects[0].HookObserved {
		t.Fatalf("Claude inherited another app's evidence: %#v", view.Apps[1])
	}
	if !strings.Contains(view.Next, projectB) {
		t.Fatalf("next action does not name uncovered pair: %q", view.Next)
	}
}

func TestPairVerificationSurvivesRemovalButNotReactivationOrDestinationChange(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	projectA, projectB := t.TempDir(), t.TempDir()
	cfg := pairTestConfig(now, []string{"codex"}, projectA, projectB)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	remote := storage.NewMemoryStore()
	publishPairSession(t, home, store, remote, cfg, now, "codex-a", "codex", projectA, true)
	env := pairStatusEnv(t, home, userHome, now, "codex")

	cfg.Archive.Projects = cfg.Archive.Projects[:1]
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].ReadBackVerified {
		t.Fatalf("unchanged pair lost verification: %#v", view.Apps[0])
	}
	cfg.Harnesses = append(cfg.Harnesses, "claude")
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].ReadBackVerified {
		t.Fatalf("adding an app invalidated unchanged pair: %#v", view.Apps[0])
	}
	cfg.Harnesses = []string{"codex"}

	cfg.Archive.Projects[0].ActivatedAt = now.Add(time.Hour)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified || view.Apps[0].Projects[0].HookObserved {
		t.Fatalf("pre-reactivation evidence accepted: %#v", view.Apps[0])
	}

	cfg.Archive.Projects[0].ActivatedAt = now.Add(-time.Hour)
	cfg.Storage.AWSProfile = "changed-destination-identity"
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified {
		t.Fatalf("destination change retained verification: %#v", view.Apps[0])
	}
}

func pairTestConfig(now time.Time, apps []string, roots ...string) config.Config {
	projects := make([]archive.ProjectActivation, 0, len(roots))
	for _, root := range roots {
		projects = append(projects, archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: now.Add(-time.Hour)})
	}
	return config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: apps, Archive: archive.Config{Enabled: true, Projects: projects}}
}

func publishPairSession(t *testing.T, home string, store *collector.LocalStore, remote *storage.MemoryStore, cfg config.Config, now time.Time, id, harness, project string, verify bool) {
	t.Helper()
	path := filepath.Join(project, id+".jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic","cli_version":"1.2.3"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: id, NativeSessionID: "native-" + id, ProjectID: archive.ProjectID(project), ProjectRoot: project, Harness: archive.Harness{Name: harness, Version: "1.2.3"}, TranscriptPath: path, SessionStartedAt: now, RegisteredAt: now}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	result, err := collector.Run(context.Background(), store, remote, collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return now }})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("publish result=%#v err=%v", result, err)
	}
	if verify {
		env := testEnv(t, home, now)
		if err := verifyPublications(home, cfg, env, store, remote, &result); err != nil || len(result.Errors) != 0 {
			t.Fatalf("verify result=%#v err=%v", result, err)
		}
	}
}

func pairStatusEnv(t *testing.T, home, userHome string, now time.Time, apps ...string) Env {
	t.Helper()
	executable := "/opt/agent-archive/bin/agent-archive"
	plan, err := hooks.Plan(userHome, executable, apps)
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Apply(plan); err != nil {
		t.Fatal(err)
	}
	env := testEnv(t, home, now)
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	env.Executable = func() (string, error) { return executable, nil }
	env.JobState = func(string) string { return "running" }
	return env
}

func TestStatusDoesNotSuggestImpossibleCursorCapture(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	cfg := pairTestConfig(now, []string{"cursor"}, project)
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(pairStatusEnv(t, home, userHome, now, "cursor"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(view.Next, "then start a new session") || !strings.Contains(view.Next, "version-specific") {
		t.Fatalf("unsupported capability action: %q", view.Next)
	}
}
