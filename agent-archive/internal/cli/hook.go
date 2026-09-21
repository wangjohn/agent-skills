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
	hookEventTurnStart
	hookEventStop
	hookEventSubagentStop
	hookEventResponse
)

// classifyHookEvent mirrors, per harness, exactly the event names
// hooks.Merge installs (see internal/hooks/hooks.go's events lists) and the
// spec's Lifecycle integration table. Anything else installed alongside
// these (PreToolUse, PostToolUse, ...) is not an
// archive-relevant event and is ignored here.
func classifyHookEvent(harness, eventName string) hookEventKind {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "codex":
		switch eventName {
		case "SessionStart":
			return hookEventStart
		case "UserPromptSubmit":
			return hookEventTurnStart
		case "Stop", "Interrupt", "SessionEnd":
			return hookEventStop
		case "SubagentStop":
			return hookEventSubagentStop
		}
	case "claude", "claude-code":
		switch eventName {
		case "SessionStart":
			return hookEventStart
		case "UserPromptSubmit":
			return hookEventTurnStart
		case "Stop", "StopFailure", "SessionEnd":
			return hookEventStop
		case "SubagentStop":
			return hookEventSubagentStop
		}
	case "cursor":
		switch eventName {
		case "sessionStart":
			return hookEventStart
		case "beforeSubmitPrompt":
			return hookEventTurnStart
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
		return handleSessionStart(home, store, cfg, harness, nativeSessionID, payload, now)
	case hookEventTurnStart:
		return handleSessionActivity(store, harness, nativeSessionID, eventName, payload, now)
	case hookEventSubagentStop:
		return handleSubagentStop(store, cfg, harness, nativeSessionID, eventName, payload, now)
	case hookEventStop, hookEventResponse:
		return handleSessionStop(store, harness, nativeSessionID, eventName, payload, now)
	}
	return nil
}

func handleSessionActivity(store *collector.LocalStore, harness, nativeSessionID, eventName string, payload map[string]any, now time.Time) error {
	archiveID, found, err := store.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return fmt.Errorf("look up archive session ID: %w", err)
	}
	if !found {
		return nil
	}
	reg, found, err := store.LoadRegistration(archiveID)
	if err != nil {
		return err
	}
	if !found || canonicalHarness(reg.Harness.Name) != canonicalHarness(harness) {
		return nil
	}
	return saveLifecycleEvidence(store, archiveID, harness, strings.ToLower(eventName), payload, now)
}

func handleSessionStart(home string, store *collector.LocalStore, cfg config.Config, harness, nativeSessionID string, payload map[string]any, now time.Time) error {
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
			if canonicalHarness(existing.Harness.Name) != canonicalHarness(harness) || (root != "" && root != existing.ProjectRoot) {
				return fmt.Errorf("session identity conflicts with the accepted registration")
			}
			if transcriptPath != "" {
				existing.TranscriptPath = transcriptPath
			}
			existing.RegisteredAt = now
			applyHarnessObservation(&existing.Harness, harness, payload)
			if err := store.SaveRegistration(existing); err != nil {
				return err
			}
			return saveLifecycleEvidence(store, existing.ArchiveSessionID, harness, "sessionstart", payload, now)
		}
	}

	// Ignore excluded projects without persisting their paths in diagnostics.
	included := false
	for _, project := range cfg.Archive.Projects {
		if project.Included && filepath.Clean(project.Root) == filepath.Clean(root) {
			included = true
			break
		}
	}
	if !included {
		return nil
	}
	// Codex and Claude document an explicit fresh-start source. Cursor's
	// version field does not prove that an unknown session began after
	// activation; until native start provenance is verified, leave it out.
	if !provesFreshSessionStart(harness, payload) {
		return recordCaptureDiagnostic(home, captureDiagnostic{
			Code: diagnosticUnknownSessionStart, Harness: canonicalHarness(harness),
			ProjectRoot: root, ObservedAt: now,
		})
	}
	if root == "" {
		return fmt.Errorf("hook payload for SessionStart has no project root")
	}
	if !cfg.Archive.Eligible(root, now) {
		return recordCaptureDiagnostic(home, captureDiagnostic{
			Code: diagnosticPreActivationStart, Harness: canonicalHarness(harness),
			ProjectRoot: root, ObservedAt: now,
		})
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

func canonicalHarness(harness string) string {
	harness = strings.ToLower(strings.TrimSpace(harness))
	if harness == "claude-code" {
		return "claude"
	}
	return harness
}

func provesFreshSessionStart(harness string, payload map[string]any) bool {
	switch canonicalHarness(harness) {
	case "codex", "claude":
		source, _ := payload["source"].(string)
		switch strings.ToLower(strings.TrimSpace(source)) {
		case "startup", "clear":
			return true
		}

	}
	return false
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
	registration, registered, err := store.LoadRegistration(archiveID)
	if err != nil {
		return err
	}
	if !registered {
		return nil
	}
	if canonicalHarness(registration.Harness.Name) != canonicalHarness(harness) {
		return fmt.Errorf("session event does not match the accepted harness")
	}
	reason := strings.ToLower(eventName)
	var evidence []archive.SupplementalEvidence
	if isSessionLifecycleEvent(eventName) {
		lifecycle, err := filteredHookEvidence(archive.EvidenceKindLifecycleHook, harness, eventName, payload, false, now)
		if err != nil {
			return err
		}
		if lifecycle != nil {
			evidence = append(evidence, *lifecycle)
		}
	}
	filtered, err := filteredHookEvidence(archive.EvidenceKindFinalResponse, harness, eventName, payload, true, now)
	if err != nil {
		return err
	}
	if filtered != nil {
		evidence = append(evidence, *filtered)
	}
	return store.SaveRequest(archiveID, reason, now, evidence...)
}

func isSessionLifecycleEvent(eventName string) bool {
	switch eventName {
	case "Stop", "Interrupt", "SessionEnd", "StopFailure", "stop", "sessionEnd":
		return true
	default:
		return false
	}
}

// extractHookEvidencePayload passes through only documented or stable identity
// fields. Cursor's generation_id is normalized to turn_id for reconciliation;
// every other value remains exactly as observed.
func extractHookEvidencePayload(payload map[string]any, includeFinalText bool) map[string]any {
	out := map[string]any{}
	if eventName := firstNonEmptyString(payload, "hook_event_name"); eventName != "" {
		out["event_name"] = eventName
	}
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
	if kind == archive.EvidenceKindLifecycleHook {
		if status := documentedLifecycleStatus(harness, event, payload); status != "" {
			hookPayload["status"] = status
		}
	}
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
	if len(gaps) > 0 {
		filtered[0].Payload["redacted"] = true
	}
	return &filtered[0], nil
}

// documentedLifecycleStatus retains only closed native enums. Free-form
// reason/status text is never archived as lifecycle metadata.
func documentedLifecycleStatus(harness, event string, payload map[string]any) string {
	if !strings.EqualFold(strings.TrimSpace(harness), "cursor") {
		return ""
	}
	field := ""
	switch event {
	case "stop":
		field = firstNonEmptyString(payload, "status")
	case "sessionEnd":
		field = firstNonEmptyString(payload, "reason")
	default:
		return ""
	}
	switch field {
	case "completed", "aborted", "error":
		return field
	case "window_close", "user_close":
		if event == "sessionEnd" {
			return field
		}
	}
	return ""
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
