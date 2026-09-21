package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/reader"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func TestCollectionIncludesSkillHistoryAndExplicitFeedback(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	transcript := writeCodexTranscript(t, project)
	skill := filepath.Join(project, ".agents", "skills", "review", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(skill), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(skill, []byte("---\nname: review\n---\nRead design decisions."), 0600); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "session_id": "native", "cwd": project, "transcript_path": transcript}, now); err != nil {
		t.Fatal(err)
	}
	local, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, _ := local.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatal("session was not registered")
	}
	remote := storage.NewMemoryStore()
	env := testEnv(t, home, now)
	env.Now = func() time.Time { return now }
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return remote, nil }
	result, err := runOnePass(env, false)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	key, _ := archive.MetadataObjectKey("codex", regs[0].ArchiveSessionID)
	first, err := reader.ReadMetadata(context.Background(), remote, key)
	if err != nil {
		t.Fatal(err)
	}
	// Editing a skill alone cannot renew an inactive session's retention age.
	if err := os.WriteFile(skill, []byte("---\nname: review\n---\nRead API decisions."), 0600); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	result, err = runOnePass(env, false)
	if err != nil || len(result.Errors) != 0 || len(result.Published) != 0 {
		t.Fatalf("inactive session refreshed: %#v %v", result, err)
	}
	feedback := filepath.Join(t.TempDir(), "feedback.txt")
	if err := os.WriteFile(feedback, []byte("Explain the API decision."), 0600); err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"feedback", regs[0].ArchiveSessionID, "--file", feedback}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("%s", errOut.String())
	}
	result, err = runOnePass(env, false)
	if err != nil || len(result.Errors) != 0 {
		t.Fatalf("%#v %v", result, err)
	}
	metadata, bundle, err := reader.RefreshAndLoad(context.Background(), remote, key, reader.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Counts.ExplicitFeedback == nil || *metadata.Counts.ExplicitFeedback != 1 || metadata.CapturedAt.Equal(first.CapturedAt) {
		t.Fatalf("feedback not published: %+v", metadata)
	}
	versions := map[string]bool{}
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind == archive.EvidenceKindSkillSnapshot {
			hash, _ := e.Payload["sha256"].(string)
			versions[hash] = true
		}
	}
	if len(versions) != 2 {
		t.Fatalf("instruction versions lost: %v", versions)
	}
}
