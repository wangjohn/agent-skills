package archive

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ParseError means a parser could not derive a complete summary. The filtered
// source remains valid and should be retained with minimal failed metadata.
type ParseError struct{ Reason string }

func (e *ParseError) Error() string { return "metadata parse failed: " + e.Reason }

// NormalizedView is an in-memory analysis aid. It must never be uploaded as a
// second transcript representation.
type NormalizedView struct {
	Turns      []NormalizedTurn
	ToolCalls  []NormalizedToolCall
	HookFinals []HookFinalReconciliation
	// NativeSkillUses is collected in the same per-record walk as ToolCalls
	// (see toolCalls), rather than a second, separate traversal, so
	// deriveSkills only has to fold this in with supplemental evidence.
	NativeSkillUses []SkillUse
}

// TurnModelSource names where a NormalizedTurn's model attribution came from.
type TurnModelSource string

const (
	TurnModelSourceTurnContext      TurnModelSource = "turn_context"
	TurnModelSourceNativeResponse   TurnModelSource = "native_response"
	TurnModelSourceNativeTranscript TurnModelSource = "native_transcript"
)

type NormalizedTurn struct {
	RecordIndex   int             `json:"record_index"`
	Role          string          `json:"role"`
	Text          string          `json:"text,omitempty"`
	Model         string          `json:"model,omitempty"`
	ResponseModel string          `json:"response_model,omitempty"`
	ModelSource   TurnModelSource `json:"model_source,omitempty"`
	Provider      string          `json:"provider,omitempty"`
	Reasoning     string          `json:"reasoning_level,omitempty"`
	ID            string          `json:"id,omitempty"`
	ParentID      string          `json:"parent_id,omitempty"`
	TurnID        string          `json:"turn_id,omitempty"`
	Timestamp     string          `json:"timestamp,omitempty"`
}

// HookFinalStatus reports how a hook-reported final response was reconciled
// against the native transcript's turns.
type HookFinalStatus string

const (
	HookFinalStatusUnreconciledIdentity HookFinalStatus = "unreconciled_identity"
	HookFinalStatusSeparateSubagent     HookFinalStatus = "separate_subagent"
	HookFinalStatusMatchedMessageID     HookFinalStatus = "matched_message_id"
	HookFinalStatusMatchedTurnID        HookFinalStatus = "matched_turn_id"
)

// HookFinalReconciliation keeps hook-only finals separate from native source.
// It never uses identical text as a deduplication signal.
type HookFinalReconciliation struct {
	EvidenceIndex int             `json:"evidence_index"`
	Status        HookFinalStatus `json:"status"`
	MessageID     string          `json:"message_id,omitempty"`
	TurnID        string          `json:"turn_id,omitempty"`
	AgentID       string          `json:"agent_id,omitempty"`
}

type NormalizedToolCall struct {
	RecordIndex int    `json:"record_index"`
	CallID      string `json:"call_id,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	Model       string `json:"model,omitempty"`
	Reasoning   string `json:"reasoning_level,omitempty"`
}

// ParseNormalized derives a narrow view from already-filtered source. The
// foundation recognizes visible user/assistant/tool messages only; all other
// retained native shapes stay available in SourceBundle for future parsers.
func ParseNormalized(bundle SourceBundle) (NormalizedView, error) {
	if err := validateBundle(bundle); err != nil {
		return NormalizedView{}, &ParseError{Reason: err.Error()}
	}
	view := NormalizedView{}
	isParentBundle := bundle.ParentSessionID == ""
	var codexModel, codexReasoning string
	for i, record := range bundle.NativeRecords {
		if isParentBundle && isSidechainRecord(record) {
			// A subagent's records are archived as the child's own session.
			// Older Claude layouts inline them in the parent transcript; the
			// parent must not count the same messages, turns, and tool calls
			// a second time.
			continue
		}
		if bundle.Capture.Harness.Name == "codex" && firstString(record, "type") == "turn_context" {
			codexModel, codexReasoning = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort")
			continue
		}
		calls, skillUses := toolCalls(record, i, codexModel, codexReasoning)
		view.ToolCalls = append(view.ToolCalls, calls...)
		view.NativeSkillUses = append(view.NativeSkillUses, skillUses...)
		role, text, ok := findVisibleMessage(record)
		if !ok {
			continue
		}
		if isHiddenRole(role) {
			return NormalizedView{}, &ParseError{Reason: "hidden role present in filtered source"}
		}
		turn := NormalizedTurn{RecordIndex: i, Role: role, Text: text, Provider: firstStringDeep(record, "model_provider"), ID: firstStringDeep(record, "id", "uuid"), ParentID: firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"), TurnID: firstStringDeep(record, "turn_id"), Timestamp: firstStringDeep(record, "timestamp", "created_at")}
		if bundle.Capture.Harness.Name == "codex" {
			turn.Model, turn.Reasoning, turn.ModelSource = codexModel, codexReasoning, TurnModelSourceTurnContext
		} else if bundle.Capture.Harness.Name == "claude" {
			turn.ResponseModel, turn.ModelSource = firstStringDeep(record, "model", "model_id"), TurnModelSourceNativeResponse
		} else {
			turn.Model, turn.Reasoning, turn.ModelSource = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort"), TurnModelSourceNativeTranscript
		}
		view.Turns = append(view.Turns, turn)
	}
	view.HookFinals = reconcileHookFinals(bundle, view.Turns)
	return view, nil
}

func reconcileHookFinals(bundle SourceBundle, turns []NormalizedTurn) []HookFinalReconciliation {
	var out []HookFinalReconciliation
	for index, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != EvidenceKindFinalResponse {
			continue
		}
		final := HookFinalReconciliation{EvidenceIndex: index, MessageID: firstString(evidence.Payload, "message_id"), TurnID: firstString(evidence.Payload, "turn_id"), AgentID: firstString(evidence.Payload, "agent_id"), Status: HookFinalStatusUnreconciledIdentity}
		if final.AgentID != "" {
			final.Status = HookFinalStatusSeparateSubagent
		} else {
			for _, turn := range turns {
				if final.MessageID != "" && final.MessageID == turn.ID {
					final.Status = HookFinalStatusMatchedMessageID
					break
				}
				if final.TurnID != "" && turn.Role == "assistant" && final.TurnID == turn.TurnID {
					final.Status = HookFinalStatusMatchedTurnID
					break
				}
			}
		}
		out = append(out, final)
	}
	return out
}

// toolCalls walks one native record once, extracting both its normalized
// tool-call entries and any native skill invocation/read-inference signal
// found along the way — a single pass shared by ParseNormalized's ToolCalls
// and deriveSkills' native-record evidence, rather than each doing its own
// separate recursive walk over the same structure.
func toolCalls(record map[string]any, index int, model, reasoning string) ([]NormalizedToolCall, []SkillUse) {
	var calls []NormalizedToolCall
	var skillUses []SkillUse
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			kind, _ := item["type"].(string)
			// "function_call"/"tool_call" cover Codex's and other harnesses'
			// shapes alongside Claude's "tool_use".
			isToolInvocation := kind == "tool_use" || kind == "tool_call" || kind == "function_call"
			if isToolInvocation {
				calls = append(calls, NormalizedToolCall{RecordIndex: index, CallID: firstString(item, "call_id", "id"), ParentID: firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"), Model: model, Reasoning: reasoning})
			}
			tool := firstString(item, "name", "tool_name")
			if isToolInvocation && strings.EqualFold(tool, "skill") {
				if input, ok := item["input"].(map[string]any); ok {
					if name := firstString(input, "skill", "name"); name != "" {
						skillUses = append(skillUses, SkillUse{Name: name, Evidence: SkillUseEvidenceNativeInvocation})
					}
				}
			}
			path := firstString(item, "file_path", "path")
			if input, ok := item["input"].(map[string]any); ok && path == "" {
				path = firstString(input, "file_path", "path")
			}
			if path == "" {
				// Codex's function_call records carry their arguments as a
				// JSON-encoded string (e.g. `{"path":"..."}`), not a nested
				// object like "input" above — parse it the same way before
				// giving up on finding a path in this record.
				if raw, ok := item["arguments"].(string); ok && raw != "" {
					var args map[string]any
					if json.Unmarshal([]byte(raw), &args) == nil {
						path = firstString(args, "file_path", "path")
					}
				}
			}
			command := firstString(item, "command", "arguments")
			readTool := strings.EqualFold(tool, "read") || strings.EqualFold(tool, "read_file")
			catRead := strings.HasPrefix(strings.TrimSpace(command), "cat ")
			if readTool || catRead {
				if name := skillNameFromPath(path); name != "" {
					skillUses = append(skillUses, SkillUse{Name: name, Evidence: SkillUseEvidenceReadInference})
				} else if catRead {
					if name := skillNameFromPath(command); name != "" {
						skillUses = append(skillUses, SkillUse{Name: name, Evidence: SkillUseEvidenceReadInference})
					}
				}
			}
			for _, child := range item {
				walk(child)
			}
		case []any:
			for _, child := range item {
				walk(child)
			}
		}
	}
	walk(record)
	return calls, skillUses
}

// isSidechainRecord reports whether a native record belongs to a subagent
// rather than to the session that owns the transcript. Only an explicit true
// counts: an absent or non-boolean flag leaves the record in place.
func isSidechainRecord(record map[string]any) bool {
	for _, key := range []string{"isSidechain", "is_sidechain"} {
		if flag, ok := record[key].(bool); ok && flag {
			return true
		}
	}
	return false
}

func findVisibleMessage(record map[string]any) (string, string, bool) {
	if role, _ := record["role"].(string); role != "" {
		if text := contentText(record["content"]); text != "" || role == "tool" {
			return role, text, true
		}
	}
	for _, key := range []string{"message", "payload", "item", "event"} {
		if nested, ok := record[key].(map[string]any); ok {
			if role, text, found := findVisibleMessage(nested); found {
				return role, text, true
			}
		}
	}
	return "", "", false
}

func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			if text := contentText(item); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text, _ := v["text"].(string); text != "" {
			return text
		}
		if content, ok := v["content"]; ok {
			return contentText(content)
		}
	}
	return ""
}

func firstString(record map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, _ := record[key].(string); value != "" {
			return value
		}
	}
	return ""
}

func firstStringDeep(record map[string]any, keys ...string) string {
	if value := firstString(record, keys...); value != "" {
		return value
	}
	for _, key := range []string{"message", "payload", "item", "event"} {
		if nested, ok := record[key].(map[string]any); ok {
			if value := firstStringDeep(nested, keys...); value != "" {
				return value
			}
		}
	}
	return ""
}

// BuildMetadata derives a replaceable metadata sidecar. If parsing fails, it
// returns failed minimal metadata together with a ParseError; callers should
// still publish the verified filtered source and retry a parser upgrade later.
func BuildMetadata(bundle SourceBundle, machineID string, startedAt, derivedAt time.Time, reference SourceReference, parser ParserInfo) (Metadata, error) {
	if err := validateBundle(bundle); err != nil {
		return Metadata{}, err
	}
	if strings.TrimSpace(machineID) == "" || startedAt.IsZero() || derivedAt.IsZero() {
		return Metadata{}, errors.New("machine ID, start time, and derivation time are required")
	}
	if strings.TrimSpace(reference.Key) == "" || len(reference.SHA256) != 64 || reference.CompressedBytes < 0 {
		return Metadata{}, errors.New("verified source reference is required")
	}
	if parser.Name == "" {
		parser.Name = bundle.Capture.AdapterName
	}
	if parser.Version == "" {
		parser.Version = DefaultParserVersion
	}
	if parser.Status == "" {
		parser.Status = ParserStatusPartial
	}
	metadata := Metadata{
		SchemaVersion: MetadataSchemaVersion, SessionID: bundle.ArchiveSessionID, NativeSessionID: bundle.NativeSessionID,
		MachineID: machineID, ProjectID: bundle.ProjectID, StartedAt: startedAt.UTC(), CapturedAt: bundle.Capture.CapturedAt.UTC(),
		MetadataDerivedAt: derivedAt.UTC(), Harness: bundle.Capture.Harness,
		Adapter: AdapterInfo{Name: bundle.Capture.AdapterName, Version: bundle.Capture.AdapterVersion}, Parser: parser,
		FilterVersion: bundle.Capture.FilterVersion, State: MetadataStateUnknown, TurnOutcome: TurnOutcomeUnknown,
		SemanticConventions: &SemanticConventionsInfo{Name: "OpenTelemetry GenAI semantic conventions", Revision: OpenTelemetryGenAIRevision},
		SkillDetection:      SkillDetectionUnavailable,
		CaptureGaps:         append([]CaptureGap(nil), bundle.Capture.Gaps...), SourceBundle: reference,
		ParentSessionID: bundle.ParentSessionID,
		LinkedSessions:  append([]LinkedSessionReference(nil), bundle.LinkedSessions...),
	}
	metadata.State, metadata.TurnOutcome = deriveLifecycle(bundle.SupplementalEvidence)
	view, err := ParseNormalized(bundle)
	if err != nil {
		metadata.Parser.Status = ParserStatusFailed
		return metadata, err
	}
	messages := 0
	turnIDs := map[string]bool{}
	models := map[string]*ModelSummary{}
	for _, turn := range view.Turns {
		if turn.Role != "user" && turn.Role != "assistant" {
			continue
		}
		messages++
		if turn.Role == "user" && turn.ID != "" {
			turnIDs[turn.ID] = true
		}
		modelName, attribute, responseStatus := turn.Model, "gen_ai.request.model", ResponseModelStatusNotExposed
		if modelName == "" && turn.ResponseModel != "" {
			modelName, attribute, responseStatus = turn.ResponseModel, "gen_ai.response.model", ResponseModelStatusObserved
		}
		if modelName == "" {
			continue
		}
		key := attribute + "\x00" + turn.Provider + "\x00" + modelName + "\x00" + turn.Reasoning
		model := models[key]
		if model == nil {
			attributes := map[string]string{attribute: modelName}
			if turn.Provider != "" {
				attributes["gen_ai.provider.name"] = turn.Provider
			}
			if turn.Reasoning != "" {
				attributes["agent_archive.request.reasoning_level"] = turn.Reasoning
			}
			model = &ModelSummary{Attributes: attributes, Source: ModelSummarySourceNativeTranscript, ResponseModelStatus: responseStatus}
			models[key] = model
		}
		if model.TurnCount == nil {
			zero := 0
			model.TurnCount = &zero
		}
		*model.TurnCount++
	}
	// Native text is retained precisely because its structure is not proven.
	// Counts derived only from the structured subset would look complete, so
	// leave all structure-dependent totals unknown whenever text is present.
	if len(bundle.NativeText) == 0 {
		metadata.Counts.Messages = &messages
		if len(turnIDs) > 0 {
			turns := len(turnIDs)
			metadata.Counts.Turns = &turns
		}
		toolCalls := len(view.ToolCalls)
		metadata.Counts.ToolCalls = &toolCalls
	}
	modelKeys := make([]string, 0, len(models))
	for key := range models {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		metadata.Models = append(metadata.Models, *models[key])
	}
	deriveHookModels(bundle, &metadata)
	deriveSkills(bundle, view.NativeSkillUses, &metadata)
	feedback := 0
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind == EvidenceKindExplicitFeedback {
			feedback++
		}
	}
	metadata.Counts.ExplicitFeedback = &feedback
	return metadata, nil
}

type lifecycleObservation struct {
	at         time.Time
	event      string
	status     string
	provenance string
	payloadKey string
}

// deriveLifecycle applies conservative rules to filtered hook evidence.
// Sorting makes duplicate, delayed, and out-of-order delivery deterministic.
// Evidence captured before event_name was retained stays unknown rather than
// being reconstructed from local operational request state.
func deriveLifecycle(evidence []SupplementalEvidence) (MetadataState, TurnOutcome) {
	var observations []lifecycleObservation
	for _, item := range evidence {
		if item.Kind != EvidenceKindLifecycleHook {
			continue
		}
		event := strings.ToLower(strings.ReplaceAll(firstString(item.Payload, "event_name"), "_", ""))
		if event == "" {
			continue
		}
		payload, _ := json.Marshal(item.Payload)
		observations = append(observations, lifecycleObservation{
			at: item.ObservedAt, event: event, status: strings.ToLower(firstString(item.Payload, "status")),
			provenance: item.Provenance, payloadKey: string(payload),
		})
	}
	sort.Slice(observations, func(i, j int) bool {
		if !observations[i].at.Equal(observations[j].at) {
			return observations[i].at.Before(observations[j].at)
		}
		if lifecycleRank(observations[i].event) != lifecycleRank(observations[j].event) {
			return lifecycleRank(observations[i].event) < lifecycleRank(observations[j].event)
		}
		left := observations[i].event + "\x00" + observations[i].status + "\x00" + observations[i].provenance + "\x00" + observations[i].payloadKey
		right := observations[j].event + "\x00" + observations[j].status + "\x00" + observations[j].provenance + "\x00" + observations[j].payloadKey
		return left < right
	})
	state, outcome := MetadataStateUnknown, TurnOutcomeUnknown
	for _, observation := range observations {
		switch observation.event {
		case "sessionstart", "userpromptsubmit", "beforesubmitprompt":
			state, outcome = MetadataStateActive, TurnOutcomeUnknown
		case "stop":
			state, outcome = MetadataStateIdle, documentedLifecycleOutcome(observation.event, observation.status, observation.provenance)
		case "interrupt":
			state, outcome = MetadataStateIdle, TurnOutcomeInterrupted
		case "stopfailure":
			state, outcome = MetadataStateIdle, TurnOutcomeError
		case "sessionend":
			state = MetadataStateClosed
			// Closing the session does not undo a previously observed turn outcome.
			if observed := documentedLifecycleOutcome(observation.event, observation.status, observation.provenance); observed != TurnOutcomeUnknown {
				outcome = observed
			}
		}
		// A subagent finishing says nothing about the parent session, which
		// is still running: its state and outcome are left as observed.
	}
	return state, outcome
}

func lifecycleRank(event string) int {
	switch event {
	case "sessionstart", "userpromptsubmit", "beforesubmitprompt":
		return 1
	case "stop", "interrupt", "stopfailure":
		return 2
	case "sessionend", "subagentstop":
		return 3
	default:
		return 0
	}
}

func documentedLifecycleOutcome(event, status, provenance string) TurnOutcome {
	if event == "stop" && provenance != "hook:cursor:stop" {
		return TurnOutcomeUnknown
	}
	if event == "sessionend" && provenance != "hook:cursor:sessionend" {
		return TurnOutcomeUnknown
	}
	switch status {
	case "completed":
		return TurnOutcomeCompleted
	case "aborted":
		return TurnOutcomeInterrupted
	case "error":
		return TurnOutcomeError
	default:
		return TurnOutcomeUnknown
	}
}

func deriveHookModels(bundle SourceBundle, metadata *Metadata) {
	seen := map[string]bool{}
	for _, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != EvidenceKindLifecycleHook && evidence.Kind != EvidenceKindFinalResponse {
			continue
		}
		id, label := firstString(evidence.Payload, "model_id"), firstString(evidence.Payload, "model")
		if id == "" {
			continue
		}
		key := id + "\x00" + label
		if seen[key] {
			continue
		}
		seen[key] = true
		attrs := map[string]string{"gen_ai.request.model": id}
		if label != "" {
			attrs["agent_archive.request.model_label"] = label
		}
		if params, ok := evidence.Payload["model_params"].([]any); ok {
			for _, raw := range params {
				if p, ok := raw.(map[string]any); ok {
					if name, value := firstString(p, "id"), firstString(p, "value"); name != "" {
						attrs["agent_archive.request.setting."+name] = value
					}
				}
			}
		}
		metadata.Models = append(metadata.Models, ModelSummary{Attributes: attrs, Source: ModelSummarySourceHook, ResponseModelStatus: ResponseModelStatusNotExposed})
	}
}

func deriveSkills(bundle SourceBundle, nativeSkillUses []SkillUse, metadata *Metadata) {
	available := map[string]SkillSnapshot{}
	used := map[string]SkillUse{}
	recordUse := func(entry SkillUse) {
		if entry.SHA256 == "" {
			for _, existing := range used {
				if existing.Name == entry.Name && existing.SHA256 != "" {
					return
				}
			}
		} else {
			delete(used, entry.Name+"\x00")
		}
		key := entry.Name + "\x00" + entry.SHA256
		if existing, ok := used[key]; ok && existing.Evidence == SkillUseEvidenceNativeInvocation {
			return
		}
		used[key] = entry
	}
	for _, evidence := range bundle.SupplementalEvidence {
		name := firstString(evidence.Payload, "name")
		if name == "" && evidence.Kind != EvidenceKindSkillInventory {
			continue
		}
		hash := firstString(evidence.Payload, "sha256")
		switch evidence.Kind {
		case EvidenceKindSkillInventory, EvidenceKindSkillDiscovered, EvidenceKindSkillSnapshot:
			coverage := SkillCoverage(firstString(evidence.Payload, "coverage"))
			if skills, ok := evidence.Payload["skills"].([]any); ok {
				for _, raw := range skills {
					if skill, ok := raw.(map[string]any); ok {
						n, h := firstString(skill, "name"), firstString(skill, "sha256")
						if n != "" {
							available[n+"\x00"+h] = SkillSnapshot{Name: n, SHA256: h, Coverage: coverage}
						}
					}
				}
			} else {
				available[name+"\x00"+hash] = SkillSnapshot{Name: name, SHA256: hash, Coverage: coverage}
			}
		case EvidenceKindSkillInvocation:
			recordUse(SkillUse{Name: name, SHA256: hash, Evidence: SkillUseEvidenceNativeInvocation})
		case EvidenceKindSkillRead:
			recordUse(SkillUse{Name: name, SHA256: hash, Evidence: SkillUseEvidenceReadInference})
		}
	}
	// Native invocation/read-inference evidence is collected once, in
	// toolCalls()'s per-record walk (see ParseNormalized), rather than a
	// second traversal of bundle.NativeRecords here.
	for _, entry := range nativeSkillUses {
		recordUse(entry)
	}
	for _, entry := range available {
		metadata.SkillsAvailable = append(metadata.SkillsAvailable, entry)
	}
	for _, entry := range used {
		metadata.SkillsUsed = append(metadata.SkillsUsed, entry)
	}
	if len(metadata.SkillsUsed) > 0 {
		metadata.SkillDetection = SkillDetectionObserved
	} else {
		for _, entry := range metadata.SkillsAvailable {
			if entry.Coverage == SkillCoverageEligible || entry.Coverage == SkillCoverageDiscovered {
				metadata.SkillDetection = SkillDetectionPartial
				break
			}
		}
	}
	sort.Slice(metadata.SkillsAvailable, func(i, j int) bool {
		return metadata.SkillsAvailable[i].Name+"\x00"+metadata.SkillsAvailable[i].SHA256 < metadata.SkillsAvailable[j].Name+"\x00"+metadata.SkillsAvailable[j].SHA256
	})
	sort.Slice(metadata.SkillsUsed, func(i, j int) bool {
		return metadata.SkillsUsed[i].Name+"\x00"+metadata.SkillsUsed[i].SHA256 < metadata.SkillsUsed[j].Name+"\x00"+metadata.SkillsUsed[j].SHA256
	})
}

func skillNameFromPath(value string) string {
	value = strings.ReplaceAll(value, "\\", "/")
	marker := "/SKILL.md"
	index := strings.Index(value, marker)
	if index < 1 {
		return ""
	}
	before := strings.TrimSuffix(value[:index], "/")
	parts := strings.Split(before, "/")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return ""
	}
	return parts[len(parts)-1]
}

// IsParseError supports the source-first publication flow.
func IsParseError(err error) bool {
	var target *ParseError
	return errors.As(err, &target)
}

func (m Metadata) ValidateSourceReference() error {
	if m.SchemaVersion != MetadataSchemaVersion {
		return fmt.Errorf("unsupported metadata schema version %d", m.SchemaVersion)
	}
	if m.SourceBundle.Key == "" || len(m.SourceBundle.SHA256) != 64 {
		return errors.New("metadata has no verified source reference")
	}
	return nil
}
