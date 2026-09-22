package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// runHookCommand implements the hidden `_hook` entry point hooks.Merge
// installs into each harness's own hook configuration. A hook has a short
// timeout (2s, per hooks.Merge) and must never block the user's turn, so
// this always exits 0; a problem is reported to stderr only, matching the
// spec's failure table ("Hook cannot write a request: task continues,
// diagnostic is available outside model context").
func runHookCommand(args []string, stdin io.Reader, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("_hook", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", "", "harness name (codex, claude, cursor)")
	if err := fs.Parse(args); err != nil {
		return 0
	}
	var payload map[string]any
	// A hook that sends no or malformed JSON is treated as a no-op, not an
	// error: some hook events (per the harness's own docs) carry no useful
	// fields at all, and we must never fail loudly on the harness's input.
	_ = json.NewDecoder(stdin).Decode(&payload)

	home, err := env.home()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: hook: resolve home: %v\n", err)
		return 0
	}
	if err := handleHookEvent(home, *harness, payload, env.now()); err != nil {
		fmt.Fprintf(stderr, "agent-archive: hook: %v\n", err)
	}
	return 0
}

type hookEventKind int

const (
	hookEventIgnored hookEventKind = iota
	hookEventStart
	hookEventStop
	hookEventSubagentStop
	hookEventResponse
)

// classifyHookEvent mirrors, per harness, exactly the event names
// hooks.Merge installs (see internal/hooks/hooks.go's events lists) and the
// spec's Lifecycle integration table. Anything else installed alongside
// these (UserPromptSubmit, beforeSubmitPrompt, PreToolUse, ...) is not an
// archive-relevant event and is ignored here.
func classifyHookEvent(harness, eventName string) hookEventKind {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "codex":
		switch eventName {
		case "SessionStart":
			return hookEventStart
		case "Stop", "Interrupt", "SessionEnd":
			return hookEventStop
		case "SubagentStop":
			return hookEventSubagentStop
		}
	case "claude", "claude-code":
		switch eventName {
		case "SessionStart":
			return hookEventStart
		case "Stop", "StopFailure", "SessionEnd":
			return hookEventStop
		case "SubagentStop":
			return hookEventSubagentStop
		}
	case "cursor":
		switch eventName {
		case "sessionStart":
			return hookEventStart
		case "afterAgentResponse":
			return hookEventResponse
		case "stop", "sessionEnd":
			return hookEventStop
		case "subagentStop":
			return hookEventSubagentStop
		}
	}
	return hookEventIgnored
}

func handleHookEvent(home, harness string, payload map[string]any, now time.Time) error {
	if payload == nil {
		return nil
	}
	eventName, _ := payload["hook_event_name"].(string)
	kind := classifyHookEvent(harness, eventName)
	if kind == hookEventIgnored {
		return nil
	}
	if transactionPending(home) {
		return nil
	}
	unlock, lockErr := local.NamedLockWait(home, "hooks.lock", time.Second)
	if lockErr != nil {
		return fmt.Errorf("capture registration busy; this hook was not recorded: %w", lockErr)
	}
	defer unlock()
	// Setup may have started while this hook was waiting for the lock.
	if transactionPending(home) {
		return nil
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	if !found || !cfg.Archive.Enabled || cfg.Paused {
		return nil
	}
	store, err := collector.NewLocalStore(home)
	if err != nil {
		return fmt.Errorf("open local store: %w", err)
	}
	nativeSessionID := firstNonEmptyString(payload, "session_id", "conversation_id")
	if nativeSessionID == "" {
		return fmt.Errorf("hook payload for %s has no session identifier", eventName)
	}

	switch kind {
	case hookEventStart:
		return handleSessionStart(store, cfg, harness, nativeSessionID, payload, now)
	case hookEventStop, hookEventSubagentStop, hookEventResponse:
		return handleSessionStop(store, harness, nativeSessionID, eventName, payload, now)
	}
	return nil
}

func handleSessionStart(store *collector.LocalStore, cfg config.Config, harness, nativeSessionID string, payload map[string]any, now time.Time) error {
	transcriptPath, _ := payload["transcript_path"].(string)
	root := projectRoot(payload)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		for _, project := range cfg.Archive.Projects {
			if configured, err := filepath.EvalSymlinks(project.Root); err == nil && configured == resolved {
				root = project.Root
				break
			}
		}
	}

	existingID, found, err := store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return fmt.Errorf("look up archive session ID: %w", err)
	}
	if found {
		existing, regFound, err := store.LoadRegistration(existingID)
		if err != nil {
			return fmt.Errorf("load existing registration: %w", err)
		}
		if !regFound {
			// The session index points at a registration we no longer have
			// (e.g. it was never eligible). Fall through to re-evaluate as
			// if this were the first time we've seen this native session.
		} else {
			// A continuation of a session we already registered: keep its
			// original start time and just refresh what may have changed.
			if !cfg.AcceptSession(existing) {
				return nil
			}
			existing.TranscriptPath = transcriptPath
			existing.RegisteredAt = now
			applyHarnessObservation(&existing.Harness, harness, payload)
			if err := store.SaveRegistration(existing); err != nil {
				return err
			}
			return saveLifecycleEvidence(store, existing.ArchiveSessionID, harness, "sessionstart", payload, now)
		}
	}

	// This native session ID has never been registered locally. Claude Code
	// and Codex both document a "source" field on SessionStart that
	// distinguishes a fresh conversation from a continuation of an earlier
	// one (Codex: https://learn.chatgpt.com/docs/hooks.md, "Common input
	// fields" and the SessionStart section; values "startup", "resume",
	// "clear", "compact"). "resume" and "compact" both continue a
	// conversation whose true start time we cannot establish from this
	// event, so for a never-seen session the spec's guidance on older
	// resumed sessions applies: leave it uncollected rather than guess. A
	// "compact" of a session we already registered never reaches here; the
	// found branch above keeps its original start time. "startup" and
	// "clear" both begin a new conversation and are treated as fresh.
	// Cursor does not document an equivalent signal, so a first-seen start
	// for it is treated as fresh; this is a known simplification pending
	// live verification against that harness (tracked in
	// docs/agent-archive-implementation.md).
	if harnessReportsSessionSource(harness) {
		if source, _ := payload["source"].(string); sessionSourceContinuesEarlierConversation(source) {
			return nil
		}
	}
	if root == "" {
		return fmt.Errorf("hook payload for SessionStart has no project root")
	}
	if !cfg.Archive.Eligible(root, now) {
		return nil
	}
	archiveID, _, err := store.EnsureArchiveSessionID(nativeSessionID)
	if err != nil {
		return fmt.Errorf("assign archive session ID: %w", err)
	}
	observedHarness := archive.Harness{Name: strings.ToLower(strings.TrimSpace(harness))}
	applyHarnessObservation(&observedHarness, harness, payload)
	reg := archive.SessionRegistration{
		ArchiveSessionID: archiveID,
		NativeSessionID:  nativeSessionID,
		ProjectID:        archive.ProjectID(root),
		ProjectRoot:      root,
		Harness:          observedHarness,
		TranscriptPath:   transcriptPath,
		SessionStartedAt: now,
		RegisteredAt:     now,
	}
	if err := store.SaveRegistration(reg); err != nil {
		return err
	}
	return saveLifecycleEvidence(store, archiveID, harness, "sessionstart", payload, now)
}

func applyHarnessObservation(target *archive.Harness, harness string, payload map[string]any) {
	if target == nil {
		return
	}
	if strings.EqualFold(strings.TrimSpace(harness), "cursor") {
		if version := firstNonEmptyString(payload, "cursor_version"); version != "" {
			target.Version = version
		}
		if mode := firstNonEmptyString(payload, "composer_mode"); mode != "" {
			target.Mode = mode
		}
	}
}

func saveLifecycleEvidence(store *collector.LocalStore, archiveID, harness, reason string, payload map[string]any, now time.Time) error {
	evidence, err := filteredHookEvidence(archive.EvidenceKindLifecycleHook, harness, reason, payload, false, now)
	if err != nil || evidence == nil {
		return err
	}
	return store.SaveRequest(archiveID, reason, now, *evidence)
}

// harnessReportsSessionSource reports whether the harness documents a
// "source" field on its SessionStart payload. Claude Code and Codex do;
// Cursor does not.
func harnessReportsSessionSource(harness string) bool {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude", "claude-code", "codex":
		return true
	}
	return false
}

// sessionSourceContinuesEarlierConversation reports whether a documented
// SessionStart "source" value means the event continues a conversation
// that began earlier ("resume", "compact") rather than starting a new one
// ("startup", "clear", or absent).
func sessionSourceContinuesEarlierConversation(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "resume", "compact":
		return true
	}
	return false
}

func handleSessionStop(store *collector.LocalStore, harness, nativeSessionID, eventName string, payload map[string]any, now time.Time) error {
	archiveID, found, err := store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return fmt.Errorf("look up archive session ID: %w", err)
	}
	if !found {
		// Never registered (ineligible project, or a resume we declined to
		// track): nothing to request.
		return nil
	}
	reason := strings.ToLower(eventName)
	var evidence []archive.SupplementalEvidence
	filtered, err := filteredHookEvidence(archive.EvidenceKindFinalResponse, harness, eventName, payload, true, now)
	if err != nil {
		return err
	}
	if filtered != nil {
		evidence = append(evidence, *filtered)
	}
	return store.SaveRequest(archiveID, reason, now, evidence...)
}

// extractHookEvidencePayload passes through only documented or stable identity
// fields. Cursor's generation_id is normalized to turn_id for reconciliation;
// every other value remains exactly as observed.
func extractHookEvidencePayload(payload map[string]any, includeFinalText bool) map[string]any {
	out := map[string]any{}
	for _, key := range []string{"message_id", "turn_id", "agent_id", "model", "model_id"} {
		if value, ok := payload[key].(string); ok && value != "" {
			out[key] = value
		}
	}
	if out["turn_id"] == nil {
		if generation := firstNonEmptyString(payload, "generation_id"); generation != "" {
			out["turn_id"] = generation
		}
	}
	if raw, ok := payload["model_params"].([]any); ok {
		params := make([]any, 0, len(raw))
		for _, item := range raw {
			param, ok := item.(map[string]any)
			if !ok {
				continue
			}
			id, value := firstNonEmptyString(param, "id"), firstNonEmptyString(param, "value")
			if id != "" && value != "" {
				params = append(params, map[string]any{"id": id, "value": value})
			}
		}
		if len(params) > 0 {
			out["model_params"] = params
		}
	}
	if includeFinalText {
		if text := firstNonEmptyString(payload, "last_assistant_message", "text"); text != "" {
			out["text"] = text
		}
	}
	return out
}

func filteredHookEvidence(kind archive.SupplementalEvidenceKind, harness, event string, payload map[string]any, includeFinalText bool, now time.Time) (*archive.SupplementalEvidence, error) {
	hookPayload := extractHookEvidencePayload(payload, includeFinalText)
	if len(hookPayload) == 0 {
		return nil, nil
	}
	candidate := archive.SupplementalEvidence{
		Kind: kind, ObservedAt: now, Provenance: "hook:" + strings.ToLower(strings.TrimSpace(harness)) + ":" + strings.ToLower(event), Payload: hookPayload,
	}
	filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{candidate})
	if err != nil {
		return nil, fmt.Errorf("filter hook evidence: %w", err)
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	archive.AnnotateSupplementalGaps(filtered[0].Payload, gaps)
	return &filtered[0], nil
}

func firstNonEmptyString(payload map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := payload[key].(string); ok && value != "" {
			return value
		}
	}
	return ""
}

// projectRoot reads cwd, falling back to the first entry of workspace_roots
// (Cursor's base hook schema uses that instead of a single cwd).
func projectRoot(payload map[string]any) string {
	if cwd := firstNonEmptyString(payload, "cwd"); cwd != "" {
		return cwd
	}
	if roots, ok := payload["workspace_roots"].([]any); ok {
		for _, raw := range roots {
			if root, ok := raw.(string); ok && root != "" {
				return root
			}
		}
	}
	return ""
}
