package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlanApplyAndRollback(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude/settings.json")
	os.MkdirAll(filepath.Dir(path), 0700)
	original := []byte(`{"permissions":{"allow":["Read"]}}`)
	os.WriteFile(path, original, 0600)
	plan, e := Plan(home, "/Applications/agent-archive", []string{"claude", "codex", "cursor"})
	if e != nil {
		t.Fatal(e)
	}
	if e = Apply(plan); e != nil {
		t.Fatal(e)
	}
	if e = Rollback(plan); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(original) {
		t.Fatal("original changed")
	}
	if _, e = os.Stat(filepath.Join(home, ".codex/hooks.json")); !os.IsNotExist(e) {
		t.Fatal("new file not removed")
	}
}
func TestConcurrentEditPreserved(t *testing.T) {
	home := t.TempDir()
	plan, _ := Plan(home, "/bin/agent-archive", []string{"codex"})
	path := plan[0].Path
	os.MkdirAll(filepath.Dir(path), 0700)
	os.WriteFile(path, []byte(`{"changed":true}`), 0600)
	if Apply(plan) == nil {
		t.Fatal("overwrote concurrent edit")
	}
}
func TestLaunchAgentEscapesPaths(t *testing.T) {
	b, e := LaunchAgent("/a & b/agent-archive", "/private/data")
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(string(b), "/a &amp; b/agent-archive") {
		t.Fatal("invalid XML")
	}
	if strings.Contains(string(b), "sync") {
		t.Fatal("public sync used for scheduled internal mode")
	}
}

func TestPlanRemovalStripsOnlyOurEntries(t *testing.T) {
	home := t.TempDir()
	claudePath := filepath.Join(home, ".claude/settings.json")
	os.MkdirAll(filepath.Dir(claudePath), 0700)
	// An unrelated hook on an event we also use, plus unrelated settings.
	original := []byte(`{"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`)
	os.WriteFile(claudePath, original, 0600)
	cursorPath := filepath.Join(home, ".cursor/hooks.json")
	os.MkdirAll(filepath.Dir(cursorPath), 0700)
	os.WriteFile(cursorPath, []byte(`{"version":1,"hooks":{"stop":[{"command":"echo unrelated"}]}}`), 0600)

	plan, err := Plan(home, "/Applications/agent-archive", []string{"claude", "codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(plan); err != nil {
		t.Fatal(err)
	}

	removal, err := PlanRemoval(home, []string{"claude", "codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(removal) != 3 {
		t.Fatalf("expected one removal per installed harness, got %d", len(removal))
	}
	if err := Apply(removal); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{claudePath, filepath.Join(home, ".codex/hooks.json"), cursorPath} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if strings.Contains(string(b), Owner) || strings.Contains(string(b), "_hook") {
			t.Fatalf("%s still contains our handler:\n%s", path, b)
		}
	}
	claude, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(claude), `"say done"`) || !strings.Contains(string(claude), `"Read"`) {
		t.Fatalf("unrelated claude hook or settings lost:\n%s", claude)
	}
	if strings.Contains(string(claude), `"SessionStart"`) {
		t.Fatalf("an event that only held our handler should be dropped:\n%s", claude)
	}
	cursor, _ := os.ReadFile(cursorPath)
	if !strings.Contains(string(cursor), `"echo unrelated"`) || !strings.Contains(string(cursor), `"version": 1`) {
		t.Fatalf("unrelated cursor hook or version lost:\n%s", cursor)
	}

	// A second removal finds nothing of ours and plans no rewrite at all.
	again, err := PlanRemoval(home, []string{"claude", "codex", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Fatalf("expected no changes once our entries are gone, got %d", len(again))
	}
}

func TestPlanRemovalSkipsMissingAndUnrelatedFiles(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".codex/hooks.json")
	os.MkdirAll(filepath.Dir(path), 0700)
	unrelated := []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`)
	os.WriteFile(path, unrelated, 0600)
	plan, err := PlanRemoval(home, []string{"codex", "claude", "cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("expected nothing to remove, got %d changes", len(plan))
	}
	b, _ := os.ReadFile(path)
	if string(b) != string(unrelated) {
		t.Fatal("planning a removal must not touch the file")
	}
}

func TestInstalledChecksCommandsForEveryHarness(t *testing.T) {
	for _, app := range []string{"codex", "claude", "cursor"} {
		t.Run(app, func(t *testing.T) {
			home := t.TempDir()
			plan, err := Plan(home, "/Applications/agent-archive", []string{app})
			if err != nil {
				t.Fatal(err)
			}
			if err := Apply(plan); err != nil {
				t.Fatal(err)
			}
			ok, err := Installed(home, "/Applications/agent-archive", app)
			if err != nil || !ok {
				t.Fatalf("installed=%v err=%v", ok, err)
			}
			wrong := strings.ReplaceAll(string(plan[0].After), " _hook --harness ", " wrong-command --harness ")
			if err := os.WriteFile(plan[0].Path, []byte(wrong), 0600); err != nil {
				t.Fatal(err)
			}
			ok, err = Installed(home, "/Applications/agent-archive", app)
			if err != nil || ok {
				t.Fatalf("broken installed=%v err=%v", ok, err)
			}
		})
	}
}
