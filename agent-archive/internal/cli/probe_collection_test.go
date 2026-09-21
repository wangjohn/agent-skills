package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

type relativeKeyStore struct{ *storage.MemoryStore }

func (s relativeKeyStore) Put(ctx context.Context, key string, data []byte) error {
	if _, err := storage.Prefix("configured-prefix", key); err != nil {
		return err
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func TestScheduledProbeContinuesToPublication(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	localStore, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "s1", NativeSessionID: "n1", ProjectID: "p1", ProjectRoot: project, Harness: archive.Harness{Name: "codex"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := relativeKeyStore{storage.NewMemoryStore()}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	result, err := runOnePass(env, true)
	if err != nil || len(result.Errors) > 0 || len(result.Published) != 1 {
		t.Fatalf("scheduled publication: %+v, %v", result, err)
	}
	evidence, err := readVerification(home, "s1")
	if err != nil || evidence.VerifiedAt.IsZero() {
		t.Fatalf("readback: %+v %v", evidence, err)
	}
	objects, err := remote.List(context.Background(), ".setup-test/")
	if err != nil || len(objects) != 0 {
		t.Fatalf("probe cleanup: %v %v", objects, err)
	}
}
