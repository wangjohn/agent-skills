package evidence

import (
	"fmt"
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
	if len(got) != 4 || got[2].Kind != archive.EvidenceKindSkillInventory || got[3].Kind != archive.EvidenceKindSkillSnapshot {
		t.Fatalf("evidence=%#v", got)
	}
	if got[2].Payload["coverage"] != string(archive.SkillCoverageInstalledOnly) || got[2].Payload["root_status"] != "present" {
		t.Fatalf("inventory=%#v", got[2])
	}
	snapshot := got[3].Payload
	if snapshot["name"] != "reviewed-name" || len(snapshot["sha256"].(string)) != 64 || snapshot["redacted"] != true {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if text := snapshot["snapshot"].(string); strings.Contains(text, "synthetic-secret-value") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("snapshot text=%q", text)
	}
}

func TestObserveSkillsAbsentRootsAreScopedAndLeaveUseKnowledgeUnknown(t *testing.T) {
	now := time.Now().UTC()
	got, err := ObserveSkills(SkillOptions{Harness: "claude", ProjectRoot: t.TempDir(), UserHome: t.TempDir(), ObservedAt: time.Now()})
	if err != nil || len(got) != 2 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	for _, item := range got {
		if item.Kind != archive.EvidenceKindSkillInventory || item.Payload["root_status"] != "absent" || item.Payload["coverage"] != string(archive.SkillCoverageInstalledOnly) || len(item.Payload["skills"].([]any)) != 0 {
			t.Fatalf("observation=%#v", item)
		}
	}
	bundle := archive.SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: archive.SourceCapture{Harness: archive.Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "1", SourceFormat: "jsonl", FilterVersion: archive.FilterVersion, CapturedAt: now}, SupplementalEvidence: got}
	metadata, err := archive.BuildMetadata(bundle, "machine", now, now, archive.SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}, archive.ParserInfo{})
	if err != nil || metadata.SkillDetection != archive.SkillDetectionUnavailable || len(metadata.SkillsUsed) != 0 {
		t.Fatalf("metadata=%#v err=%v", metadata, err)
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
	if err != nil || len(got) != 3 {
		t.Fatalf("got=%#v err=%v", got, err)
	}
	for _, item := range got {
		if item.Kind != archive.EvidenceKindSkillInventory || item.Payload["root_status"] != "absent" {
			t.Fatalf("unexpected evidence=%#v", got)
		}
	}
}

func TestObserveSkillsRecordsRemovalAndDeduplicatesRepeatedAbsence(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	dir := filepath.Join(root, "review")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 9, 21, 1, 0, 0, 0, time.UTC)
	present, err := ObserveSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: first})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	absent, err := ObserveSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: first.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	merged := archive.MergeSupplementalEvidence(present, absent)
	if len(merged) != len(present)+1 {
		t.Fatalf("removal was not recorded exactly once: present=%#v absent=%#v merged=%#v", present, absent, merged)
	}
	repeated, err := ObserveSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: first.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if again := archive.MergeSupplementalEvidence(merged, repeated); len(again) != len(merged) {
		t.Fatalf("repeated absence changed history: before=%#v after=%#v", merged, again)
	}
}

func TestObserveSkillsRecordsExistingEmptyRoot(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".agents", "skills"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := ObserveSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Payload["root_status"] != "empty" || got[0].Payload["inventory_complete"] != true || len(got[0].Payload["skills"].([]any)) != 0 {
		t.Fatalf("empty root=%#v", got[0])
	}
}

func TestObserveSkillsUsesAggregateSnapshotBudgetAndDistinctCodexScopes(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	items := []struct{ root, name string }{
		{filepath.Join(home, ".agents", "skills"), "current"},
		{filepath.Join(home, ".agents", "skills"), "extra-one"},
		{filepath.Join(home, ".agents", "skills"), "extra-two"},
		{filepath.Join(home, ".codex", "skills"), "legacy"},
		{filepath.Join(project, ".agents", "skills"), "project"},
	}
	for _, item := range items {
		dir := filepath.Join(item.root, item.name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		body := "---\nname: " + item.name + "\n---\n" + strings.Repeat("x", maxSkillBytes-64)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ObserveSkills(SkillOptions{Harness: "codex", ProjectRoot: project, UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	scopes := map[string]bool{}
	snapshotBytes, omitted := 0, false
	for _, item := range got {
		if item.Kind == archive.EvidenceKindSkillInventory {
			scopes[item.Payload["scope"].(string)] = true
			if count, _ := item.Payload["snapshot_omitted_count"].(float64); count > 0 {
				omitted = true
			}
		}
		if item.Kind == archive.EvidenceKindSkillSnapshot {
			snapshotBytes += len(item.Payload["snapshot"].(string))
		}
	}
	for _, scope := range []string{"user_agents", "user_codex_legacy", "project_agents"} {
		if !scopes[scope] {
			t.Fatalf("missing scope %q in %#v", scope, scopes)
		}
	}
	if snapshotBytes > maxSnapshotBytes || !omitted {
		t.Fatalf("snapshotBytes=%d omitted=%v", snapshotBytes, omitted)
	}
}

func TestObserveSkillsLabelsTruncatedInventory(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".agents", "skills")
	for i := 0; i < maxSkillsPerRoot+1; i++ {
		dir := filepath.Join(root, fmt.Sprintf("skill-%03d", i))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: small\n---\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := ObserveSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 || got[0].Payload["truncated"] != true || got[0].Payload["inventory_complete"] != false || got[0].Payload["omitted_count"] != float64(1) {
		t.Fatalf("inventory=%#v", got)
	}
}

func TestObserveSkillsLabelsUnscannedLegacySubtrees(t *testing.T) {
	home := t.TempDir()
	nested := filepath.Join(home, ".codex", "skills", ".system", "imagegen")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "SKILL.md"), []byte("---\nname: imagegen\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ObserveSkills(SkillOptions{Harness: "codex", UserHome: home, ObservedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Payload["scope"] != "user_codex_legacy" || got[1].Payload["inventory_complete"] != false || got[1].Payload["omitted_count"] != float64(1) {
		t.Fatalf("evidence=%#v", got)
	}
}
