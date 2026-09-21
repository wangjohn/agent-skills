package cli

import (
	"bytes"
	"context"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
	"strings"
	"testing"
	"time"
)

type privateTestStore struct {
	*storage.MemoryStore
	calls int
}

func (s *privateTestStore) InspectPrivacy(ctx context.Context) storage.PrivacyReport {
	s.calls++
	if _, ok := ctx.Deadline(); !ok {
		panic("privacy inspection requires timeout")
	}
	p := storage.UnknownPrivacy("s3")
	p.State = "verified_private"
	p.Reason = "all_bucket_public_access_blocks_enabled"
	return p
}
func TestPrivacyEvidenceIsScopedAndExpires(t *testing.T) {
	cfg := config.Config{Storage: credentialsTestConfig()}
	at := time.Now().UTC()
	remote := &privateTestStore{MemoryStore: storage.NewMemoryStore()}
	cfg.BucketPrivacy = inspectBucketPrivacy(cfg, remote, at)
	if remote.calls != 1 {
		t.Fatal("inspection not called")
	}
	if currentBucketPrivacy(cfg, at).State != "verified_private" {
		t.Fatal("missing current evidence")
	}
	for _, when := range []time.Time{at.Add(25 * time.Hour), at.Add(-time.Second)} {
		if currentBucketPrivacy(cfg, when).State != "not_verified" {
			t.Fatal("stale/future evidence trusted")
		}
	}
	cfg.Storage.Bucket = "changed"
	if p := currentBucketPrivacy(cfg, at); p.State != "not_verified" || p.Reason != "storage_configuration_changed" {
		t.Fatalf("%+v", p)
	}
	var out bytes.Buffer
	printBucketPrivacy(&out, currentBucketPrivacy(cfg, at))
	if !strings.Contains(out.String(), "https://") || !strings.Contains(out.String(), "not verified") {
		t.Fatal(out.String())
	}
}

func TestSetupPersistsPrivacyAndStatusNeverInspects(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	remote := &privateTestStore{MemoryStore: storage.NewMemoryStore()}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	output := setupRun(t, env, s3SetupInput("synthetic", "us-east-1", "profile", true, false, false, project), 0)
	if !strings.Contains(output, "native public access blocked") {
		t.Fatal(output)
	}
	cfg, found, err := config.Load(home)
	if err != nil || !found || cfg.BucketPrivacy == nil {
		t.Fatalf("privacy missing: %+v %v", cfg, err)
	}
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		t.Fatal("status opened remote store")
		return nil, nil
	}
	view, err := readStatus(env)
	if err != nil || view.Privacy != "verified_private" {
		t.Fatalf("status: %+v %v", view, err)
	}
	if remote.calls != 1 {
		t.Fatalf("repeated inspection: %d", remote.calls)
	}
	env.Now = func() time.Time { return at.Add(25 * time.Hour) }
	view, err = readStatus(env)
	if err != nil || view.Privacy != "not_verified" || view.PrivacyEvidence.Reason != "inspection_stale" {
		t.Fatalf("stale status: %+v %v", view, err)
	}
}
