package archive

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Adapter filters one application's hook-provided JSONL transcript. Adapters
// are deliberately readers only; the collector owns paths, retries, and I/O.
type Adapter interface {
	Name() string
	Version() string
	FilterJSONL(io.Reader) (FilteredTranscript, error)
}

// FilterError means no new source bundle may be made from this input. It is
// intentionally distinct from ParseError, where a filtered source is still
// safe and valuable to retain.
type FilterError struct{ Reason string }

func (e *FilterError) Error() string { return "unsafe source format: " + e.Reason }

var ErrUnsafeSourceFormat = &FilterError{Reason: "no recognized safe records"}

const adapterVersion = "0.2.0"

// DefaultParserVersion is the source parser version reported by this bounded
// foundation. The parser is intentionally partial until fixture coverage proves
// a given native format more completely.
const DefaultParserVersion = "0.5.0"

// NewAdapter returns a privacy-first adapter by canonical harness name.
func NewAdapter(name string) (Adapter, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "codex":
		return CodexAdapter{}, nil
	case "claude", "claude-code":
		return ClaudeAdapter{}, nil
	case "cursor":
		return CursorAdapter{}, nil
	default:
		return nil, fmt.Errorf("unsupported archive adapter %q", name)
	}
}

// CodexAdapter supports the conservative JSONL shapes observed by this
// foundation. Unsupported Codex record types are gaps, never pass-through.
type CodexAdapter struct{}

func (CodexAdapter) Name() string    { return "codex" }
func (CodexAdapter) Version() string { return adapterVersion }
func (CodexAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "codex-jsonl", map[string]bool{
		"session_meta": true, "turn_context": true, "response_item": true,
		"event_msg": true, "message": true,
	})
}

// ClaudeAdapter handles a small, explicit subset of Claude Code JSONL event
// types. It does not claim schema coverage for every installed version.
type ClaudeAdapter struct{}

func (ClaudeAdapter) Name() string    { return "claude" }
func (ClaudeAdapter) Version() string { return adapterVersion }
func (ClaudeAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "claude-jsonl", map[string]bool{
		"user": true, "assistant": true, "tool_use": true, "tool_result": true,
		"message": true, "summary": true,
	})
}

// CursorAdapter is intentionally limited to hook-provided JSONL records. Text
// paths and undocumented formats remain unsupported capture gaps upstream.
type CursorAdapter struct{}

func (CursorAdapter) Name() string    { return "cursor" }
func (CursorAdapter) Version() string { return adapterVersion }
func (CursorAdapter) FilterJSONL(r io.Reader) (FilteredTranscript, error) {
	return filterJSONL(r, "cursor-jsonl", map[string]bool{
		"session": true, "message": true, "tool_call": true, "tool_result": true,
		"event": true,
	})
}

// FilterText retains a hook-provided Cursor text transcript only when the hook
// has established that this is a fresh eligible session. It labels the source
// as text rather than fabricating message events from unstructured content.
func (CursorAdapter) FilterText(r io.Reader, freshStartedAt time.Time) (FilteredTranscript, error) {
	if freshStartedAt.IsZero() {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no reliable fresh-session start"}
	}
	const maxText = 2 * 1024 * 1024
	content, err := io.ReadAll(io.LimitReader(r, maxText+1))
	if err != nil {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript cannot be read"}
	}
	if len(content) > maxText {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript exceeds safe size limit"}
	}
	result := FilteredTranscript{Format: "cursor-text", FirstEventAt: freshStartedAt.UTC(), Gaps: []CaptureGap{{Code: "text_structure_partial", Detail: "Cursor role sections retained without manufactured events"}}}
	var retained []string
	section := ""
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		lower := strings.ToLower(trimmed)
		switch {
		case strings.HasPrefix(lower, "system:") || strings.HasPrefix(lower, "developer:") || strings.HasPrefix(lower, "thinking:") || strings.HasPrefix(lower, "analysis:"):
			section = "hidden"
			result.Gaps = append(result.Gaps, CaptureGap{Code: "hidden_instruction_omitted", Detail: "text section omitted"})
		case strings.HasPrefix(lower, "user:") || strings.HasPrefix(lower, "assistant:") || strings.HasPrefix(lower, "tool:"):
			section = "visible"
			retained = append(retained, line)
		case section == "hidden":
			// continuation line of an already-hidden section; omit.
		case section == "visible":
			// continuation line of the current visible section's message body.
			retained = append(retained, line)
		default:
			return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has unrecognized role section"}
		}
	}
	if len(retained) == 0 {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable visible sections"}
	}
	state := sanitizeState{addGap: func(code string, _ int, detail string) {
		result.Gaps = append(result.Gaps, CaptureGap{Code: code, Detail: detail})
	}}
	safe, keep := sanitizeValue(strings.Join(retained, "\n"), &state)
	if !keep {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript has no retainable content"}
	}
	text, ok := safe.(string)
	if !ok {
		return FilteredTranscript{}, &FilterError{Reason: "cursor text transcript is not text"}
	}
	result.Text = []string{text}
	result.Boundary.RetainedBytes = len(text)
	return result, nil
}

var sensitiveValue = regexp.MustCompile(`(?i)(?:\bauthorization\b\s*:\s*bearer\s+[^\s,;]+|\b(?:api[_-]?key|access[_-]?key|secret|password|authorization|bearer|token)\b\s*[=:]\s*[^\s,;]+|\bAKIA[0-9A-Z]{16}\b|\bsk-[A-Za-z0-9_-]{12,}\b)`)

var allowedKeys = map[string]bool{
	"type": true, "id": true, "uuid": true, "session_id": true, "parent_id": true,
	"parent_uuid": true, "parentuuid": true, "timestamp": true, "created_at": true, "updated_at": true,
	"cwd": true, "model": true, "model_provider": true, "role": true, "content": true,
	"channel": true,
	"text":    true, "message": true, "item": true, "event": true, "payload": true,
	"tool_name": true, "tool_input": true, "tool_output": true, "tool_use": true,
	"tool_result": true, "call_id": true, "input": true, "output": true, "result": true, "arguments": true,
	"command": true, "path": true, "query": true, "url": true, "description": true,
	"status": true, "event_name": true, "turn_id": true, "reasoning_effort": true, "name": true, "items": true, "data": true,
	"sha256": true, "message_id": true, "settings": true, "model_id": true, "discovered": true, "installed": true, "snapshot": true, "source": true,
	"coverage": true, "skills": true, "redacted": true, "observed_at": true,
	"model_params": true, "value": true, "cli_version": true, "agent_id": true,
	// "version" is the per-record Claude Code build stamp; it attributes a
	// published capture to an installed version. Values still pass sanitizeValue.
	"version":     true,
	"uncertainty": true, "scope": true, "original_bytes": true, "event_id": true,
	"truncated": true, "omitted_count": true, "snapshot_omitted_count": true, "inventory_complete": true, "root_status": true,
	"gaps":  true,
	"skill": true,
	// Claude Code marks a subagent's records with is_sidechain. Retaining the
	// flag lets a parent's normalized view exclude any inlined child records
	// from its own counts; the child is archived as its own session.
	"is_sidechain": true, "issidechain": true,
	"file_path":          true,
	"archive_session_id": true, "relationship": true,
}

// captureGapKeys are additionally allowed inside a capture_gap evidence
// payload, whose whole content is an archive-authored code and its fixed
// description. They are deliberately not in allowedKeys: `detail` is a
// common free-text field name in native transcripts, and sanitizeObject
// recurses, so allowing it globally would retain arbitrary nested prose.
var captureGapKeys = map[string]bool{"code": true, "detail": true}

var blockedKeys = map[string]bool{
	"api_key": true, "apikey": true, "access_key": true, "secret": true,
	"secret_key": true, "password": true, "authorization": true, "token": true,
	"cookie": true, "set_cookie": true, "system": true, "developer": true,
	"instructions": true, "reasoning": true, "analysis": true, "encrypted_content": true,
	"image": true, "images": true, "audio": true, "binary": true, "attachment": true,
}

func filterJSONL(r io.Reader, format string, knownTypes map[string]bool) (FilteredTranscript, error) {
	result := FilteredTranscript{Format: format, NativeStartComplete: true}
	scanner := bufio.NewScanner(r)
	// Individual native JSONL records can contain tool output. A hard limit keeps
	// filtering bounded; exceeding it is unsafe rather than silently truncated.
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	lineNo, recognized := 0, 0
	gapSet := map[string]bool{}
	addGap := func(code string, record int, detail string) {
		key := fmt.Sprintf("%s:%s", code, detail)
		if !gapSet[key] {
			gapSet[key] = true
			// Do not retain a source line number: an excluded preceding line must
			// not alter an otherwise identical retained snapshot.
			result.Gaps = append(result.Gaps, CaptureGap{Code: code, Detail: detail})
		}
	}
	for scanner.Scan() {
		lineNo++
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal(line, &raw); err != nil {
			result.NativeStartComplete = false
			addGap("incomplete_or_invalid_record", lineNo, "jsonl record omitted")
			continue
		}
		observed := parseNativeTimestamp(raw)
		if result.FirstEventAt.IsZero() {
			result.FirstEventAt = observed
		}
		if observed.IsZero() {
			if recordCarriesConversation(raw) {
				result.NativeStartComplete = false
			}
		} else if result.NativeStartAt.IsZero() || observed.Before(result.NativeStartAt) {
			result.NativeStartAt = observed
		}
		if !observed.IsZero() && (result.NativeEndAt.IsZero() || observed.After(result.NativeEndAt)) {
			result.NativeEndAt = observed
		}
		result.SessionIDs = appendUniqueString(result.SessionIDs, firstString(raw, "session_id", "sessionId"))
		result.AgentIDs = appendUniqueString(result.AgentIDs, firstString(raw, "agent_id", "agentId"))
		kind, _ := raw["type"].(string)
		cursorRoleContent := format == "cursor-jsonl" && kind == "" && firstString(raw, "role") != ""
		if !knownTypes[kind] && !cursorRoleContent {
			addGap("unknown_record_type", lineNo, "record omitted")
			continue
		}
		recognized++
		state := sanitizeState{record: lineNo, addGap: addGap}
		safe, keep := sanitizeObject(raw, &state)
		if !keep {
			continue
		}
		encoded, err := json.Marshal(safe)
		if err != nil {
			return FilteredTranscript{}, &FilterError{Reason: "safe record cannot be encoded"}
		}
		result.Records = append(result.Records, encoded)
		result.Boundary.RetainedRecords++
		result.Boundary.RetainedBytes += len(encoded)
	}
	if err := scanner.Err(); err != nil {
		return FilteredTranscript{}, &FilterError{Reason: "record exceeds safe size limit or transcript cannot be read"}
	}
	if lineNo > 0 && recognized == 0 {
		return FilteredTranscript{}, ErrUnsafeSourceFormat
	}
	sort.SliceStable(result.Gaps, func(i, j int) bool { return result.Gaps[i].Code < result.Gaps[j].Code })
	return result, nil
}

func appendUniqueString(values []string, candidate string) []string {
	if candidate == "" {
		return values
	}
	for _, existing := range values {
		if existing == candidate {
			return values
		}
	}
	return append(values, candidate)
}

// conversationRecordTypes are the record types whose start time is part of a
// session's timestamp provenance. Harnesses also write bookkeeping entries
// beside the conversation — Claude Code's `summary` and `file-history-snapshot`
// records are the observed examples — which carry no top-level timestamp and
// no conversational content. Treating those as missing provenance would make
// an otherwise fully timestamped transcript permanently ineligible for child
// capture, so only conversation-bearing records are required to be stamped.
var conversationRecordTypes = map[string]bool{
	"user": true, "assistant": true, "system": true, "message": true,
	"tool_use": true, "tool_result": true, "tool_call": true,
	"session_meta": true, "turn_context": true, "response_item": true,
	"event_msg": true, "session": true, "event": true,
}

func recordCarriesConversation(record map[string]any) bool {
	if _, present := record["message"]; present {
		return true
	}
	if firstString(record, "role") != "" {
		return true
	}
	kind, _ := record["type"].(string)
	return conversationRecordTypes[strings.ToLower(strings.TrimSpace(kind))]
}

func parseNativeTimestamp(record map[string]any) time.Time {
	for _, key := range []string{"timestamp", "created_at"} {
		value, _ := record[key].(string)
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

type sanitizeState struct {
	record int
	addGap func(string, int, string)
	// extraAllowed widens the key allowlist for one archive-authored payload
	// shape. It applies at every depth of that payload, which is safe only
	// because such payloads are flat maps this repository writes itself.
	extraAllowed map[string]bool
}

func sanitizeObject(in map[string]any, state *sanitizeState) (map[string]any, bool) {
	if role, _ := in["role"].(string); isHiddenRole(role) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	if channel, _ := in["channel"].(string); isHiddenChannel(channel) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	if kind, _ := in["type"].(string); isHiddenRole(kind) {
		state.addGap("hidden_instruction_omitted", state.record, "record omitted")
		return nil, false
	}
	out := make(map[string]any)
	keys := make([]string, 0, len(in))
	for key := range in {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		value := in[key]
		lower := strings.ToLower(key)
		if blockedKeys[lower] {
			state.addGap("sensitive_or_hidden_field_omitted", state.record, "field omitted")
			continue
		}
		if !allowedKeys[lower] && !state.extraAllowed[lower] {
			state.addGap("unknown_field_omitted", state.record, "field omitted")
			continue
		}
		safe, keep := sanitizeValue(value, state)
		if keep {
			out[key] = safe
		}
	}
	if len(out) == 0 {
		state.addGap("record_without_allowed_fields_omitted", state.record, "record omitted")
		return nil, false
	}
	for _, key := range []string{"payload", "message", "item", "event"} {
		if _, had := in[key]; had {
			if _, kept := out[key]; !kept {
				state.addGap("hidden_or_unknown_nested_content_omitted", state.record, "record omitted")
				return nil, false
			}
		}
	}
	return out, true
}

func isHiddenChannel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "analysis", "reasoning", "thinking", "chain_of_thought":
		return true
	}
	return false
}

func sanitizeValue(value any, state *sanitizeState) (any, bool) {
	switch v := value.(type) {
	case nil, bool, float64:
		return v, true
	case string:
		if sensitiveValue.MatchString(v) {
			state.addGap("sensitive_content_redacted", state.record, "content redacted")
			v = sensitiveValue.ReplaceAllString(v, "[REDACTED]")
		}
		const maxTextBytes = 64 * 1024
		if len(v) > maxTextBytes {
			state.addGap("content_truncated", state.record, "content truncated")
			v = v[:maxTextBytes]
		}
		return v, true
	case map[string]any:
		return sanitizeObject(v, state)
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			safe, keep := sanitizeValue(item, state)
			if keep {
				out = append(out, safe)
			}
		}
		if len(v) > 0 && len(out) == 0 {
			state.addGap("hidden_or_unknown_nested_content_omitted", state.record, "field omitted")
			return nil, false
		}
		return out, true
	default:
		state.addGap("unsupported_value_omitted", state.record, "value omitted")
		return nil, false
	}
}

func isHiddenRole(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "system", "developer", "reasoning", "analysis", "thinking", "chain_of_thought", "agent_reasoning", "agent_reasoning_delta", "raw_agent_reasoning":
		return true
	default:
		return false
	}
}

// IsFilterError supports callers which need to retain a previous source bundle
// when a new native format cannot be safely filtered.
func IsFilterError(err error) bool {
	var target *FilterError
	return errors.As(err, &target)
}
