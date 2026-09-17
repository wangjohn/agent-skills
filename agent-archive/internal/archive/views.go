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
	Turns []NormalizedTurn
}

type NormalizedTurn struct {
	RecordIndex int    `json:"record_index"`
	Role        string `json:"role"`
	Text        string `json:"text,omitempty"`
	Model       string `json:"model,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Reasoning   string `json:"reasoning_level,omitempty"`
	ID          string `json:"id,omitempty"`
	ParentID    string `json:"parent_id,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

// ParseNormalized derives a narrow view from already-filtered source. The
// foundation recognizes visible user/assistant/tool messages only; all other
// retained native shapes stay available in SourceBundle for future parsers.
func ParseNormalized(bundle SourceBundle) (NormalizedView, error) {
	if err := validateBundle(bundle); err != nil {
		return NormalizedView{}, &ParseError{Reason: err.Error()}
	}
	view := NormalizedView{}
	for i, record := range bundle.NativeRecords {
		role, text, ok := findVisibleMessage(record)
		if !ok {
			continue
		}
		if isHiddenRole(role) {
			return NormalizedView{}, &ParseError{Reason: "hidden role present in filtered source"}
		}
		view.Turns = append(view.Turns, NormalizedTurn{RecordIndex: i, Role: role, Text: text, Model: firstStringDeep(record, "model"), Provider: firstStringDeep(record, "model_provider"), Reasoning: firstStringDeep(record, "reasoning_effort"), ID: firstStringDeep(record, "id", "uuid"), ParentID: firstStringDeep(record, "parent_id", "parent_uuid", "parentUuid"), Timestamp: firstStringDeep(record, "timestamp", "created_at")})
	}
	return view, nil
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
	turns := 0
	models := map[string]*ModelSummary{}
	for _, turn := range view.Turns {
		if turn.Role != "user" && turn.Role != "assistant" {
			continue
		}
		turns++
		if turn.Model == "" {
			continue
		}
		key := turn.Provider + "\x00" + turn.Model + "\x00" + turn.Reasoning
		model := models[key]
		if model == nil {
			attributes := map[string]string{"gen_ai.request.model": turn.Model}
			if turn.Provider != "" {
				attributes["gen_ai.provider.name"] = turn.Provider
			}
			if turn.Reasoning != "" {
				attributes["gen_ai.request.reasoning.level"] = turn.Reasoning
			}
			model = &ModelSummary{Attributes: attributes, Source: "native_transcript", ResponseModelStatus: "not_exposed"}
			models[key] = model
		}
		if model.TurnCount == nil {
			zero := 0
			model.TurnCount = &zero
		}
		*model.TurnCount++
	}
	metadata.Counts.Turns = &turns
	toolCalls := 0
	for _, record := range bundle.NativeRecords {
		toolCalls += countToolCalls(record)
	}
	metadata.Counts.ToolCalls = &toolCalls
	modelKeys := make([]string, 0, len(models))
	for key := range models {
		modelKeys = append(modelKeys, key)
	}
	sort.Strings(modelKeys)
	for _, key := range modelKeys {
		metadata.Models = append(metadata.Models, *models[key])
	}
	return metadata, nil
}

func countToolCalls(value any) int {
	switch item := value.(type) {
	case map[string]any:
		count := 0
		if kind, _ := item["type"].(string); kind == "tool_use" || kind == "tool_call" || kind == "function_call" {
			count++
		}
		for _, child := range item {
			count += countToolCalls(child)
		}
		return count
	case []any:
		count := 0
		for _, child := range item {
			count += countToolCalls(child)
		}
		return count
	default:
		return 0
	}
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
