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

// relativeKeyStore rejects absolute keys on every operation, mirroring the
// S3 store's prefix composition, so a probe that passes a pre-prefixed key
// fails the same way it would against a configured bucket.
type relativeKeyStore struct{ *storage.MemoryStore }

func (s relativeKeyStore) Put(ctx context.Context, key string, data []byte) error {
	if err := requireRelativeKey(key); err != nil {
		return err
	}
	return s.MemoryStore.Put(ctx, key, data)
}

func (s relativeKeyStore) Get(ctx context.Context, key string) ([]byte, error) {
	if err := requireRelativeKey(key); err != nil {
		return nil, err
	}
	return s.MemoryStore.Get(ctx, key)
}

func (s relativeKeyStore) List(ctx context.Context, prefix string) ([]storage.Object, error) {
	if prefix != "" {
		if err := requireRelativeKey(prefix); err != nil {
			return nil, err
		}
	}
	return s.MemoryStore.List(ctx, prefix)
}

func (s relativeKeyStore) Delete(ctx context.Context, key string) error {
	if err := requireRelativeKey(key); err != nil {
		return err
	}
	return s.MemoryStore.Delete(ctx, key)
}

func requireRelativeKey(key string) error {
	_, err := storage.Prefix("configured-prefix", key)
	return err
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
