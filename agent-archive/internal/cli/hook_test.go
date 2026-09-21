package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
)

func TestAmbiguousStartIsNotRegisteredAndDiagnosticIsContentFree(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		harness string
		payload map[string]any
	}{
		{"codex", map[string]any{"hook_event_name": "SessionStart", "session_id": "private-codex-id", "cwd": "/work/widget", "transcript_path": "/private/transcript.jsonl"}},
		{"cursor", map[string]any{"hook_event_name": "sessionStart", "conversation_id": "private-cursor-id", "workspace_roots": []any{"/work/widget"}, "transcript_path": "/private/cursor.jsonl"}},
	} {
		if err := handleHookEvent(home, tc.harness, tc.payload, now); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("ambiguous starts were registered: %#v", regs)
	}
	diagnostics, err := readCaptureDiagnostics(home)
	if err != nil || len(diagnostics) != 2 {
		t.Fatalf("diagnostics=%#v err=%v", diagnostics, err)
	}
	raw, err := os.ReadFile(captureDiagnosticsPath(home))
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || bytes.Contains(raw, []byte("private-")) || bytes.Contains(raw, []byte("transcript")) {
		t.Fatalf("diagnostic leaked session content or identity: %s", raw)
	}
}

func setUpTestConfig(t *testing.T, home, projectRoot string, activatedAt time.Time) {
	t.Helper()
	cfg := config.Config{
		MachineID: "machine-1",
		Storage:   credentialsTestConfig(),
		Archive: archive.Config{
			SchemaVersion: 1, MachineID: "machine-1", Enabled: true,
			Projects: []archive.ProjectActivation{{ProjectID: archive.ProjectID(projectRoot), Root: projectRoot, Included: true, ActivatedAt: activatedAt}},
		},
	}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyHookEvent(t *testing.T) {
	cases := []struct {
		harness, event string
		want           hookEventKind
	}{
		{"codex", "SessionStart", hookEventStart},
		{"codex", "Interrupt", hookEventStop},
		{"codex", "SubagentStop", hookEventSubagentStop},
		{"codex", "UserPromptSubmit", hookEventIgnored},
		{"claude", "StopFailure", hookEventStop},
		{"claude", "PreToolUse", hookEventIgnored},
		{"cursor", "sessionStart", hookEventStart},
		{"cursor", "afterAgentResponse", hookEventResponse},
		{"cursor", "stop", hookEventStop},
		{"cursor", "SessionStart", hookEventIgnored}, // wrong case for this harness
	}
	for _, c := range cases {
		if got := classifyHookEvent(c.harness, c.event); got != c.want {
			t.Errorf("classifyHookEvent(%q, %q) = %v, want %v", c.harness, c.event, got, c.want)
		}
	}
}

func TestHandleHookEventRegistersEligibleSessionStart(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
	}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 1 {
		t.Fatalf("regs=%#v", regs)
	}
	if regs[0].NativeSessionID != "native-1" || regs[0].ProjectRoot != "/work/widget" || !regs[0].SessionStartedAt.Equal(now) {
		t.Fatalf("reg=%#v", regs[0])
	}
}

func TestHandleHookEventSkipsIneligibleProject(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/somewhere/else"}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("regs=%#v", regs)
	}
}

func TestHandleHookEventSkipsResumeOfUnknownClaudeSession(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{
		"hook_event_name": "SessionStart", "session_id": "native-old", "source": "resume",
		"cwd": "/work/widget",
	}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("a resume of a never-seen session must not be registered: %#v", regs)
	}
}

func TestHandleHookEventSkipsResumeOfUnknownCodexSession(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	for _, source := range []string{"resume", "compact"} {
		payload := map[string]any{
			"hook_event_name": "SessionStart", "session_id": "native-old-" + source, "source": source,
			"cwd": "/work/widget",
		}
		if err := handleHookEvent(home, "codex", payload, now); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("a Codex resume/compact of a never-seen session must not be registered: %#v", regs)
	}
}

func TestHandleHookEventRegistersCodexStartup(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	for _, source := range []string{"startup", "clear"} {
		payload := map[string]any{
			"hook_event_name": "SessionStart", "session_id": "native-" + source, "source": source,
			"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
		}
		if err := handleHookEvent(home, "codex", payload, now); err != nil {
			t.Fatal(err)
		}
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 2 {
		t.Fatalf("a Codex startup/clear must register a fresh session: %#v", regs)
	}
	for _, reg := range regs {
		if reg.Harness.Name != "codex" || !reg.SessionStartedAt.Equal(now) {
			t.Fatalf("reg=%#v", reg)
		}
	}
}

func TestHandleHookEventCodexCompactPreservesOriginalStartTime(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	firstStart := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "source": "startup", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "codex", payload, firstStart); err != nil {
		t.Fatal(err)
	}

	compactAt := firstStart.Add(2 * time.Hour)
	compactPayload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "source": "compact", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "codex", compactPayload, compactAt); err != nil {
		t.Fatal(err)
	}

	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("regs=%#v", regs)
	}
	if !regs[0].SessionStartedAt.Equal(firstStart) {
		t.Fatalf("compact must preserve the original start time: got %s want %s", regs[0].SessionStartedAt, firstStart)
	}
}

func TestHandleHookEventResumePreservesOriginalStartTime(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	firstStart := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl"}
	if err := handleHookEvent(home, "claude", payload, firstStart); err != nil {
		t.Fatal(err)
	}

	resumeAt := firstStart.Add(48 * time.Hour)
	resumePayload := map[string]any{
		"hook_event_name": "SessionStart", "session_id": "native-1", "source": "resume",
		"cwd": "/work/widget", "transcript_path": "/tmp/t2.jsonl",
	}
	if err := handleHookEvent(home, "claude", resumePayload, resumeAt); err != nil {
		t.Fatal(err)
	}

	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 {
		t.Fatalf("regs=%#v", regs)
	}
	if !regs[0].SessionStartedAt.Equal(firstStart) {
		t.Fatalf("resume must preserve the original start time: got %s want %s", regs[0].SessionStartedAt, firstStart)
	}
	if regs[0].TranscriptPath != "/tmp/t2.jsonl" {
		t.Fatalf("resume should refresh the transcript path: %#v", regs[0])
	}
}

func TestHandleHookEventStopWritesRequestWithEvidence(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	startPayload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", startPayload, start); err != nil {
		t.Fatal(err)
	}

	stopAt := start.Add(time.Minute)
	stopPayload := map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "turn_id": "t1", "model": "claude-opus-5"}
	if err := handleHookEvent(home, "claude", stopPayload, stopAt); err != nil {
		t.Fatal(err)
	}

	store, _ := collector.NewLocalStore(home)
	requests, err := store.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 || requests[0].Reasons[0] != "stop" {
		t.Fatalf("requests=%#v", requests)
	}
	if len(requests[0].HookEvidence) != 1 || requests[0].HookEvidence[0].Payload["turn_id"] != "t1" {
		t.Fatalf("evidence=%#v", requests[0].HookEvidence)
	}
}

func TestHandleHookEventCapturesSupportedFinalTextAfterFiltering(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	start := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}, start); err != nil {
		t.Fatal(err)
	}
	stop := map[string]any{"hook_event_name": "Stop", "session_id": "native-1", "turn_id": "t1", "model": "gpt-x", "last_assistant_message": "done token=synthetic-secret-value"}
	if err := handleHookEvent(home, "codex", stop, start.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	requests, err := store.LoadRequests()
	if err != nil {
		t.Fatal(err)
	}
	var final archive.SupplementalEvidence
	for _, item := range requests[0].HookEvidence {
		if item.Kind == archive.EvidenceKindFinalResponse {
			final = item
		}
	}
	if text, _ := final.Payload["text"].(string); final.Payload["redacted"] != true || strings.Contains(text, "synthetic-secret-value") || !strings.Contains(text, "[REDACTED]") {
		t.Fatalf("final evidence was not filtered before persistence: %#v", final)
	}
}

func TestHandleCursorHookCapturesVersionModeModelParamsAndResponse(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	start := map[string]any{"hook_event_name": "sessionStart", "conversation_id": "native-1", "workspace_roots": []any{"/work/widget"}, "cursor_version": "1.7.2", "composer_mode": "agent", "model": "label", "model_id": "model-x", "model_params": []any{map[string]any{"id": "effort", "value": "high"}}}
	if err := handleHookEvent(home, "cursor", start, now); err != nil {
		t.Fatal(err)
	}
	response := map[string]any{"hook_event_name": "afterAgentResponse", "conversation_id": "native-1", "generation_id": "generation-1", "text": "finished", "model": "label", "model_id": "model-x", "model_params": []any{map[string]any{"id": "effort", "value": "high"}}}
	if err := handleHookEvent(home, "cursor", response, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 1 || regs[0].Harness.Version != "1.7.2" || regs[0].Harness.Mode != "agent" {
		t.Fatalf("registration=%#v", regs)
	}
	requests, _ := store.LoadRequests()
	if len(requests) != 1 || len(requests[0].HookEvidence) != 2 {
		t.Fatalf("requests=%#v", requests)
	}
	final := requests[0].HookEvidence[1]
	if final.Payload["text"] != "finished" || final.Payload["turn_id"] != "generation-1" || final.Payload["model_id"] != "model-x" {
		t.Fatalf("final=%#v", final)
	}
}

func TestHandleHookEventStopForUnregisteredSessionIsNoop(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "Stop", "session_id": "never-registered"}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	requests, _ := store.LoadRequests()
	if len(requests) != 0 {
		t.Fatalf("requests=%#v", requests)
	}
}

func TestHandleHookEventIgnoresUnrelatedEvent(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "native-1", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if len(regs) != 0 {
		t.Fatalf("regs=%#v", regs)
	}
}

func TestHandleHookEventNoopWhenNotConfigured(t *testing.T) {
	home := t.TempDir() // no config.Save call
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1", "cwd": "/work/widget"}
	if err := handleHookEvent(home, "claude", payload, now); err != nil {
		t.Fatal(err)
	}
}

// TestHandleHookEventNoopWhilePaused guards the spec's "Hooks perform no new
// registrations while paused" requirement: pause must stop new tracking at
// the hook, not just at collection/upload time.
func TestHandleHookEventNoopWhilePaused(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/work/widget", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if _, err := config.SetPaused(home, true); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	payload := map[string]any{
		"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-1",
		"cwd": "/work/widget", "transcript_path": "/tmp/t.jsonl",
	}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}

	store, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(regs) != 0 {
		t.Fatalf("a paused hook must register nothing new: regs=%#v", regs)
	}
}

func TestRunHookCommandNeverFailsOnMalformedInput(t *testing.T) {
	home := t.TempDir()
	env := testEnv(t, home, time.Now())
	var errOut bytes.Buffer
	code := runHookCommand([]string{"--harness", "codex"}, strings.NewReader("not json"), &errOut, env)
	if code != 0 {
		t.Fatalf("hook must never fail the harness's turn: code=%d stderr=%s", code, errOut.String())
	}
}
