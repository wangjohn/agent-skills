package archive

import (
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
}

type NormalizedTurn struct {
	RecordIndex   int    `json:"record_index"`
	Role          string `json:"role"`
	Text          string `json:"text,omitempty"`
	Model         string `json:"model,omitempty"`
	ResponseModel string `json:"response_model,omitempty"`
	ModelSource   string `json:"model_source,omitempty"`
	Provider      string `json:"provider,omitempty"`
	Reasoning     string `json:"reasoning_level,omitempty"`
	ID            string `json:"id,omitempty"`
	ParentID      string `json:"parent_id,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	Timestamp     string `json:"timestamp,omitempty"`
}

// HookFinalReconciliation keeps hook-only finals separate from native source.
// It never uses identical text as a deduplication signal.
type HookFinalReconciliation struct {
	EvidenceIndex int    `json:"evidence_index"`
	Status        string `json:"status"`
	MessageID     string `json:"message_id,omitempty"`
	TurnID        string `json:"turn_id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
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
	var codexModel, codexReasoning string
	for i, record := range bundle.NativeRecords {
		if bundle.Capture.Harness.Name == "codex" && firstString(record, "type") == "turn_context" {
			codexModel, codexReasoning = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort")
			continue
		}
		view.ToolCalls = append(view.ToolCalls, toolCalls(record, i, codexModel, codexReasoning)...)
		role, text, ok := findVisibleMessage(record)
		if !ok {
			continue
		}
		if isHiddenRole(role) {
			return NormalizedView{}, &ParseError{Reason: "hidden role present in filtered source"}
		}
		turn := NormalizedTurn{RecordIndex: i, Role: role, Text: text, Provider: firstStringDeep(record, "model_provider"), ID: firstStringDeep(record, "id", "uuid"), ParentID: firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"), TurnID: firstStringDeep(record, "turn_id"), Timestamp: firstStringDeep(record, "timestamp", "created_at")}
		if bundle.Capture.Harness.Name == "codex" {
			turn.Model, turn.Reasoning, turn.ModelSource = codexModel, codexReasoning, "turn_context"
		} else if bundle.Capture.Harness.Name == "claude" {
			turn.ResponseModel, turn.ModelSource = firstStringDeep(record, "model", "model_id"), "native_response"
		} else {
			turn.Model, turn.Reasoning, turn.ModelSource = firstStringDeep(record, "model", "model_id"), firstStringDeep(record, "reasoning_effort"), "native_transcript"
		}
		view.Turns = append(view.Turns, turn)
	}
	view.HookFinals = reconcileHookFinals(bundle, view.Turns)
	return view, nil
}

func reconcileHookFinals(bundle SourceBundle, turns []NormalizedTurn) []HookFinalReconciliation {
	var out []HookFinalReconciliation
	for index, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != "final_response" {
			continue
		}
		final := HookFinalReconciliation{EvidenceIndex: index, MessageID: firstString(evidence.Payload, "message_id"), TurnID: firstString(evidence.Payload, "turn_id"), AgentID: firstString(evidence.Payload, "agent_id"), Status: "unreconciled_identity"}
		if final.AgentID != "" {
			final.Status = "separate_subagent"
		} else {
			for _, turn := range turns {
				if final.MessageID != "" && final.MessageID == turn.ID {
					final.Status = "matched_message_id"
					break
				}
				if final.TurnID != "" && turn.Role == "assistant" && final.TurnID == turn.TurnID {
					final.Status = "matched_turn_id"
					break
				}
			}
		}
		out = append(out, final)
	}
	return out
}

func toolCalls(record map[string]any, index int, model, reasoning string) []NormalizedToolCall {
	var out []NormalizedToolCall
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			kind, _ := item["type"].(string)
			if kind == "tool_use" || kind == "tool_call" || kind == "function_call" {
				out = append(out, NormalizedToolCall{RecordIndex: index, CallID: firstString(item, "call_id", "id"), ParentID: firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"), Model: model, Reasoning: reasoning})
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
	return out
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
		parser.Version = bundle.Capture.AdapterVersion
	}
	if parser.Status == "" {
		parser.Status = "partial"
	}
	metadata := Metadata{
		SchemaVersion: MetadataSchemaVersion, SessionID: bundle.ArchiveSessionID, NativeSessionID: bundle.NativeSessionID,
		MachineID: machineID, ProjectID: bundle.ProjectID, StartedAt: startedAt.UTC(), CapturedAt: bundle.Capture.CapturedAt.UTC(),
		MetadataDerivedAt: derivedAt.UTC(), Harness: bundle.Capture.Harness,
		Adapter: AdapterInfo{Name: bundle.Capture.AdapterName, Version: bundle.Capture.AdapterVersion}, Parser: parser,
		FilterVersion: bundle.Capture.FilterVersion, State: "unknown", SkillDetection: "unavailable",
		CaptureGaps: append([]CaptureGap(nil), bundle.Capture.Gaps...), SourceBundle: reference,
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		metadata.Parser.Status = "failed"
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
		modelName, attribute, responseStatus := turn.Model, "gen_ai.request.model", "not_exposed"
		if modelName == "" && turn.ResponseModel != "" {
			modelName, attribute, responseStatus = turn.ResponseModel, "gen_ai.response.model", "observed"
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
				attributes["gen_ai.request.reasoning.level"] = turn.Reasoning
			}
			model = &ModelSummary{Attributes: attributes, Source: "native_transcript", ResponseModelStatus: responseStatus}
			models[key] = model
		}
		if model.TurnCount == nil {
			zero := 0
			model.TurnCount = &zero
		}
		*model.TurnCount++
	}
	metadata.Counts.Messages = &messages
	if len(turnIDs) > 0 {
		turns := len(turnIDs)
		metadata.Counts.Turns = &turns
	}
	toolCalls := len(view.ToolCalls)
	metadata.Counts.ToolCalls = &toolCalls
	modelKeys := make([]string, 0, len(models))
	for key := range models {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		metadata.Models = append(metadata.Models, *models[key])
	}
	deriveHookModels(bundle, &metadata)
	deriveSkills(bundle, &metadata)
	feedback := 0
	for _, e := range bundle.SupplementalEvidence {
		if e.Kind == "explicit_feedback" {
			feedback++
		}
	}
	metadata.Counts.ExplicitFeedback = &feedback
	return metadata, nil
}

func deriveHookModels(bundle SourceBundle, metadata *Metadata) {
	seen := map[string]bool{}
	for _, evidence := range bundle.SupplementalEvidence {
		if evidence.Kind != "lifecycle_hook" && evidence.Kind != "final_response" {
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
			attrs["gen_ai.request.model.label"] = label
		}
		if params, ok := evidence.Payload["model_params"].([]any); ok {
			for _, raw := range params {
				if p, ok := raw.(map[string]any); ok {
					if name, value := firstString(p, "id"), firstString(p, "value"); name != "" {
						attrs["gen_ai.request.setting."+name] = value
					}
				}
			}
		}
		metadata.Models = append(metadata.Models, ModelSummary{Attributes: attrs, Source: "hook", ResponseModelStatus: "not_exposed"})
	}
}

func deriveSkills(bundle SourceBundle, metadata *Metadata) {
	available := map[string]SkillSnapshot{}
	used := map[string]SkillUse{}
	for _, evidence := range bundle.SupplementalEvidence {
		name := firstString(evidence.Payload, "name")
		if name == "" && evidence.Kind != "skill_inventory" {
			continue
		}
		hash := firstString(evidence.Payload, "sha256")
		switch evidence.Kind {
		case "skill_inventory", "skill_discovered", "skill_snapshot":
			coverage := firstString(evidence.Payload, "coverage")
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
		case "skill_invocation":
			used[name+"\x00"+hash] = SkillUse{Name: name, SHA256: hash, Evidence: "native_invocation"}
		case "skill_read":
			used[name+"\x00"+hash] = SkillUse{Name: name, SHA256: hash, Evidence: "skill_read_inference"}
		}
	}
	var walk func(any)
	walk = func(value any) {
		switch item := value.(type) {
		case map[string]any:
			kind, _ := item["type"].(string)
			tool := firstString(item, "name", "tool_name")
			if kind == "tool_use" && strings.EqualFold(tool, "skill") {
				if input, ok := item["input"].(map[string]any); ok {
					if name := firstString(input, "skill", "name"); name != "" {
						used[name+"\x00"] = SkillUse{Name: name, Evidence: "native_invocation"}
					}
				}
			}
			path := firstString(item, "file_path", "path")
			if input, ok := item["input"].(map[string]any); ok && path == "" {
				path = firstString(input, "file_path", "path")
			}
			command := firstString(item, "command", "arguments")
			readTool := strings.EqualFold(tool, "read") || strings.EqualFold(tool, "read_file")
			catRead := strings.HasPrefix(strings.TrimSpace(command), "cat ")
			if readTool || catRead {
				if name := skillNameFromPath(path); name != "" {
					used[name+"\x00"] = SkillUse{Name: name, Evidence: "skill_read_inference"}
				} else if catRead {
					for _, field := range []string{command} {
						if name := skillNameFromPath(field); name != "" {
							used[name+"\x00"] = SkillUse{Name: name, Evidence: "skill_read_inference"}
						}
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
	for _, record := range bundle.NativeRecords {
		walk(record)
	}
	for _, entry := range available {
		metadata.SkillsAvailable = append(metadata.SkillsAvailable, entry)
	}
	for _, entry := range used {
		metadata.SkillsUsed = append(metadata.SkillsUsed, entry)
	}
	if len(metadata.SkillsUsed) > 0 {
		metadata.SkillDetection = "observed"
	} else if len(metadata.SkillsAvailable) > 0 {
		metadata.SkillDetection = "observed_none"
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
