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
