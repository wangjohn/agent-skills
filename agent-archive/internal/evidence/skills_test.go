package evidence

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
)

func TestObserveSkillsHashesOriginalAndFiltersSnapshot(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	dir := filepath.Join(project, ".agents", "skills", "folder-name")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: reviewed-name\ndescription: test\n---\nuse token=synthetic-secret-value\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)
	got, err := ObserveSkills(SkillOptions{Harness: "codex", ProjectRoot: project, UserHome: home, ObservedAt: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Kind != archive.EvidenceKindSkillInventory || got[1].Kind != archive.EvidenceKindSkillSnapshot {
		t.Fatalf("evidence=%#v", got)
	}
	if got[0].Payload["coverage"] != string(archive.SkillCoverageInstalledOnly) {
		t.Fatalf("inventory=%#v", got[0])
	}
	snapshot := got[1].Payload
	if snapshot["name"] != "reviewed-name" || len(snapshot["sha256"].(string)) != 64 || snapshot["redacted"] != true {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if text := snapshot["snapshot"].(string); strings.Contains(text, "synthetic-secret-value") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("snapshot text=%q", text)
	}
}

func TestObserveSkillsAbsentRootsLeaveKnowledgeUnknown(t *testing.T) {
	got, err := ObserveSkills(SkillOptions{Harness: "claude", ProjectRoot: t.TempDir(), UserHome: t.TempDir(), ObservedAt: time.Now()})
	if err != nil || len(got) != 0 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}

func TestObserveSkillsDoesNotWalkUnrelatedProjectDirectories(t *testing.T) {
	project := t.TempDir()
	unrelated := filepath.Join(project, "vendor", ".agents", "skills", "hidden")
	if err := os.MkdirAll(unrelated, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unrelated, "SKILL.md"), []byte("---\nname: hidden\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ObserveSkills(SkillOptions{Harness: "codex", ProjectRoot: project, UserHome: t.TempDir(), ObservedAt: time.Now()})
	if err != nil || len(got) != 0 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
}
