package cli

import (
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"testing"
	"time"
)

func TestCursorVersionDoesNotProveSessionStart(t *testing.T) {
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := handleHookEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/work/widget"}, "cursor_version": "99.0.0"}, at); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatal("arbitrary version enrolled session")
	}
	ds, _ := readCaptureDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != diagnosticUnknownSessionStart {
		t.Fatalf("diagnostics %+v", ds)
	}
}
func TestResumeCannotReplaceIdentityOrEraseTranscript(t *testing.T) {
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s", "cwd": "/work/widget", "transcript_path": "/synthetic/transcript.jsonl"}
	if err := handleHookEvent(home, "claude", payload, at); err != nil {
		t.Fatal(err)
	}
	delete(payload, "transcript_path")
	payload["source"] = "resume"
	if err := handleHookEvent(home, "claude", payload, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := handleHookEvent(home, "codex", payload, at.Add(time.Minute)); err == nil {
		t.Fatal("cross-harness identity accepted")
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].TranscriptPath != "/synthetic/transcript.jsonl" || !regs[0].SessionStartedAt.Equal(at) {
		t.Fatalf("registration %+v", regs)
	}
}
func TestExcludedProjectLeavesNoDiagnostic(t *testing.T) {
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := handleHookEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/private/excluded"}}, at); err != nil {
		t.Fatal(err)
	}
	ds, _ := readCaptureDiagnostics(home)
	if len(ds) != 0 {
		t.Fatalf("excluded paths recorded %+v", ds)
	}
}
