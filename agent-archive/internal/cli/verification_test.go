package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func TestStatusRequiresRecordedReadbackAndInvalidatesConfiguration(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	project := t.TempDir()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "s1", NativeSessionID: "n1", ProjectID: "p1", ProjectRoot: project, Harness: archive.Harness{Name: "codex", Version: "observed"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := store.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	result, err := collector.Run(context.Background(), store, remote, collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("status attempted network")
		return nil, errors.New("unexpected")
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].Published || view.Apps[0].ReadBackVerified {
		t.Fatalf("publication was presented as readback: %+v", view.Apps[0])
	}
	if err := verifyPublications(home, cfg, env, store, remote, &result); err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Apps[0].ReadBackVerified || view.Apps[0].VerifiedAt.IsZero() {
		t.Fatalf("missing verification: %+v", view.Apps[0])
	}
	// Once recorded, no remote call is needed for unchanged publication.
	if err := verifyPublications(home, cfg, env, store, nil, &result); err != nil {
		t.Fatal(err)
	}
	cfg.Storage.AWSProfile = "rotated"
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	view, err = readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].ReadBackVerified {
		t.Fatal("old credential context still verified")
	}
}

func TestBackgroundChecksStorageWithoutSessionsAndStatusDoesNotProbe(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Archive: archive.Config{Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	remote := storage.NewMemoryStore()
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	if _, err := runOnePass(env, true); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Authentication.State != "verified" || view.Authentication.Context != "background_collector" {
		t.Fatalf("%+v", view.Authentication)
	}
	objects, err := remote.List(context.Background(), "")
	if err != nil || len(objects) != 0 {
		t.Fatalf("synthetic probe leaked: %#v %v", objects, err)
	}
}
