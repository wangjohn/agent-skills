package cli

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
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
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "Stop", "session_id": "s"}, at.Add(time.Minute)); err == nil {
		t.Fatal("cross-harness stop accepted")
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

func TestCompactFromSubdirectoryKeepsProjectIdentity(t *testing.T) {
	home := t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{
		MachineID: "machine-1",
		Storage:   credentialsTestConfig(),
		Archive: archive.Config{
			SchemaVersion: 1, MachineID: "machine-1", Enabled: true,
			Projects: []archive.ProjectActivation{
				{ProjectID: archive.ProjectID("/work/widget"), Root: "/work/widget", Included: true, ActivatedAt: at.Add(-time.Hour)},
				{ProjectID: archive.ProjectID("/work/other"), Root: "/work/other", Included: true, ActivatedAt: at.Add(-time.Hour)},
			},
		},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	start := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "s", "cwd": "/work/widget", "transcript_path": "/synthetic/t1.jsonl"}
	if err := handleHookEvent(home, "claude", start, at); err != nil {
		t.Fatal(err)
	}
	// The agent `cd`'d into a subdirectory; Claude Code's hook cwd follows it.
	compact := map[string]any{"hook_event_name": "SessionStart", "source": "compact", "session_id": "s", "cwd": "/work/widget/internal/cli", "transcript_path": "/synthetic/t2.jsonl", "model": "model-x"}
	if err := handleHookEvent(home, "claude", compact, at.Add(time.Minute)); err != nil {
		t.Fatalf("compact from a subdirectory of the registered project was rejected: %v", err)
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].ProjectRoot != "/work/widget" || regs[0].TranscriptPath != "/synthetic/t2.jsonl" || !regs[0].SessionStartedAt.Equal(at) {
		t.Fatalf("registration %+v", regs)
	}
	// Both the fresh start and the compact continuation are lifecycle
	// evidence for the same session; neither asks for an upload.
	requests, err := store.LoadRequests()
	if err != nil || len(requests) != 1 || len(requests[0].HookEvidence) != 2 || !requests[0].Deferred {
		t.Fatalf("compact lifecycle evidence was not recorded: %+v err=%v", requests, err)
	}
	if last := requests[0].HookEvidence[1]; last.Provenance != "hook:claude:sessionstart" || last.Payload["model"] != "model-x" {
		t.Fatalf("compact lifecycle evidence was not recorded: %+v", last)
	}
	// A continuation reported from a different configured project is still a conflict.
	compact["cwd"] = "/work/other/sub"
	if err := handleHookEvent(home, "claude", compact, at.Add(2*time.Minute)); err == nil {
		t.Fatal("cross-project identity accepted")
	}
	regs, _ = store.LoadRegistrations()
	if len(regs) != 1 || regs[0].ProjectRoot != "/work/widget" || regs[0].TranscriptPath != "/synthetic/t2.jsonl" {
		t.Fatalf("registration %+v", regs)
	}
}

func TestResumeBeforeActivationRecordsActivationDiagnostic(t *testing.T) {
	home := t.TempDir()
	at := time.Now().UTC()
	setUpTestConfig(t, home, "/work/widget", at.Add(time.Hour))
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "old", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", payload, at); err != nil {
		t.Fatal(err)
	}
	ds, _ := readCaptureDiagnostics(home)
	if len(ds) != 1 || ds[0].Code != diagnosticPreActivationStart {
		t.Fatalf("diagnostics %+v", ds)
	}
}

func TestExcludedProjectDiagnosticLeavesStatus(t *testing.T) {
	home := t.TempDir()
	at := time.Now().UTC()
	env := testEnv(t, home, at)
	setUpTestConfig(t, home, "/work/widget", at.Add(-time.Hour))
	if err := handleHookEvent(home, "cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "old", "workspace_roots": []any{"/work/widget"}}, at); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || !strings.Contains(out.String(), "Capture skipped in /work/widget") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	// The project is excluded afterwards: its path must stop appearing.
	setUpTestConfig(t, home, "/work/kept", at.Add(-time.Hour))
	out.Reset()
	if code := runStatusCommand(nil, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "/work/widget") {
		t.Fatalf("status exit=%d output=%s", code, out.String())
	}
	out.Reset()
	if code := runStatusCommand([]string{"--json"}, &out, os.Stderr, env); code != 0 || strings.Contains(out.String(), "/work/widget") || strings.Contains(out.String(), "capture_diagnostics") {
		t.Fatalf("status --json exit=%d output=%s", code, out.String())
	}
}

func TestSetupExcludingProjectPrunesStoredDiagnostic(t *testing.T) {
	home, first, second := t.TempDir(), t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, first), 0)
	cfg, _, _ := config.Load(home)
	root := cfg.Archive.Projects[0].Root
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "resume", "session_id": "old", "cwd": root}, now); err != nil {
		t.Fatal(err)
	}
	if ds, _ := readCaptureDiagnostics(home); len(ds) != 1 || ds[0].ProjectRoot != root {
		t.Fatalf("diagnostics %+v", ds)
	}
	// Reconfigure capture: drop the first project and include the second.
	setupRun(t, env, "capture\ny\nn\nn\nn\n"+second+"\n\ny\n", 0)
	cfg, _, _ = config.Load(home)
	if len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root == root {
		t.Fatalf("projects %+v", cfg.Archive.Projects)
	}
	if ds, err := readCaptureDiagnostics(home); err != nil || len(ds) != 0 {
		t.Fatalf("excluded project diagnostic kept on disk: %+v err=%v", ds, err)
	}
}
