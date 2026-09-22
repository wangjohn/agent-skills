package archive

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	bytes, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return bytes
}

func registration() SessionRegistration {
	return SessionRegistration{
		ArchiveSessionID: "archive-123", NativeSessionID: "native-456", ProjectID: "project-789", ProjectRoot: "/work/widget",
		Harness: Harness{Name: "codex", Version: "observed-build", Mode: "desktop"}, TranscriptPath: "/private/log.jsonl",
		SessionStartedAt: time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC), RegisteredAt: time.Date(2026, 9, 17, 18, 1, 0, 0, time.UTC),
	}
}

func TestCodexAdapterFiltersPrivateFieldsAndUnknownRecords(t *testing.T) {
	filtered, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "codex-safe-and-sensitive.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := filtered.Boundary.RetainedRecords, 2; got != want {
		t.Fatalf("retained records = %d, want %d", got, want)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	for _, forbidden := range []string{"private_debug", "analysis", "sk-this-is-a-synthetic-secret-value", "do not retain"} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("filtered source contains %q: %s", forbidden, joined)
		}
	}
	if !strings.Contains(joined, "[REDACTED]") {
		t.Errorf("secret was not redacted: %s", joined)
	}
	if !hasGap(filtered.Gaps, "unknown_record_type") || !hasGap(filtered.Gaps, "sensitive_or_hidden_field_omitted") || !hasGap(filtered.Gaps, "sensitive_content_redacted") {
		t.Fatalf("expected explicit privacy and format gaps, got %#v", filtered.Gaps)
	}
}

func TestAdaptersRetainVisibleSiblingBlocksAndDeriveModelToolMetadata(t *testing.T) {
	filtered, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "codex-function-call.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "hidden thought") || !strings.Contains(joined, "I will inspect the file.") || !strings.Contains(joined, "call-1") || !strings.Contains(joined, "parentUuid") {
		t.Fatalf("unexpected Codex filtered source: %s", joined)
	}
	bundle, err := NewSourceBundle(registration(), CodexAdapter{}, filtered, time.Date(2026, 9, 17, 18, 25, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := BuildMetadata(bundle, "machine", registration().SessionStartedAt, time.Date(2026, 9, 17, 18, 26, 0, 0, time.UTC), SourceReference{Key: "sessions/codex/archive-123/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Counts.Turns != nil || metadata.Counts.Messages == nil || *metadata.Counts.Messages != 1 || metadata.Counts.ToolCalls == nil || *metadata.Counts.ToolCalls != 1 {
		t.Fatalf("metadata counts = %#v", metadata.Counts)
	}
	if len(metadata.Models) != 1 || metadata.Models[0].Attributes["gen_ai.request.model"] != "gpt-6-astra" {
		t.Fatalf("metadata models = %#v", metadata.Models)
	}
	claude, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-tool-use.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	claudeJSON := string(bytes.Join(claude.Records, []byte("\n")))
	if strings.Contains(claudeJSON, "hidden thought") || !strings.Contains(claudeJSON, "I will check it.") || !strings.Contains(claudeJSON, "tool-1") {
		t.Fatalf("unexpected Claude filtered source: %s", claudeJSON)
	}
}

func TestExcludedRecordsDoNotChangeBoundaryOrHash(t *testing.T) {
	first, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "codex-known-plus-unknown.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	second, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "codex-known-plus-two-unknown.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if first.Boundary != second.Boundary {
		t.Fatalf("excluded record changed retained boundary: %#v vs %#v", first.Boundary, second.Boundary)
	}
	captured := time.Date(2026, 9, 17, 18, 25, 0, 0, time.UTC)
	left, err := NewSourceBundle(registration(), CodexAdapter{}, first, captured, nil)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewSourceBundle(registration(), CodexAdapter{}, second, captured, nil)
	if err != nil {
		t.Fatal(err)
	}
	leftSource, err := BuildCompressedSource(left)
	if err != nil {
		t.Fatal(err)
	}
	rightSource, err := BuildCompressedSource(right)
	if err != nil {
		t.Fatal(err)
	}
	if leftSource.SHA256 != rightSource.SHA256 || !bytes.Equal(leftSource.Bytes, rightSource.Bytes) {
		t.Fatal("excluded input changed deterministic source artifact")
	}
}

func TestSourceBundleIsDeterministicAndGzipTimestampIsFixed(t *testing.T) {
	filtered, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "codex-safe-and-sensitive.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := NewSourceBundle(registration(), CodexAdapter{}, filtered, time.Date(2026, 9, 17, 18, 25, 0, 0, time.UTC), []SupplementalEvidence{{
		Kind: EvidenceKindExplicitFeedback, Provenance: "hook", ObservedAt: time.Date(2026, 9, 17, 18, 26, 0, 0, time.UTC), Payload: map[string]any{"text": "fixed password=synthetic-value"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	second, err := BuildCompressedSource(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if first.SHA256 != second.SHA256 || !bytes.Equal(first.Bytes, second.Bytes) {
		t.Fatal("same source bundle did not produce exact same gzip bytes")
	}
	reader, err := gzip.NewReader(bytes.NewReader(first.Bytes))
	if err != nil {
		t.Fatal(err)
	}
	if !reader.ModTime.Equal(time.Unix(1, 0).UTC()) {
		t.Fatalf("gzip mtime = %s", reader.ModTime)
	}
	plain, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "synthetic-value") {
		t.Fatal("supplemental secret survived filtering")
	}
	if _, err := SourceObjectKey(bundle, first.SHA256); err != nil {
		t.Fatal(err)
	}
	bundle.ArchiveSessionID = "../../escape"
	if _, err := SourceObjectKey(bundle, first.SHA256); err == nil {
		t.Fatal("unsafe archive session ID accepted in object key")
	}
	if _, err := SourceObjectKey(registrationBundle(t), strings.ToUpper(first.SHA256)); err == nil {
		t.Fatal("uppercase source hash accepted in object key")
	}
	if key, err := MetadataObjectKey("codex", "archive-123"); err != nil || key != "sessions/codex/archive-123/metadata.json" {
		t.Fatalf("key=%q err=%v", key, err)
	}
	if _, err := MetadataObjectKey("codex", "../../escape"); err == nil {
		t.Fatal("unsafe archive session ID accepted in metadata object key")
	}
}

func registrationBundle(t *testing.T) SourceBundle {
	t.Helper()
	filtered, err := (CodexAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "codex-known-plus-unknown.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := NewSourceBundle(registration(), CodexAdapter{}, filtered, time.Date(2026, 9, 17, 18, 25, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	return bundle
}

func TestMetadataParserFailureLeavesMinimalSourceFirstMetadata(t *testing.T) {
	bundle := SourceBundle{SchemaVersion: SourceSchemaVersion, ArchiveSessionID: "archive", NativeSessionID: "native", ProjectID: "project", Capture: SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "0.1.0", SourceFormat: "codex-jsonl", FilterVersion: FilterVersion, CapturedAt: time.Date(2026, 9, 17, 18, 25, 0, 0, time.UTC)}, NativeRecords: []map[string]any{{"role": "system", "content": "must not be normalized"}}}
	metadata, err := BuildMetadata(bundle, "machine", time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC), time.Date(2026, 9, 17, 18, 26, 0, 0, time.UTC), SourceReference{Key: "sessions/codex/archive/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, ParserInfo{})
	if !IsParseError(err) {
		t.Fatalf("expected ParseError, got %v", err)
	}
	if metadata.Parser.Status != ParserStatusFailed || metadata.Counts.Turns != nil || metadata.SourceBundle.SHA256 == "" {
		t.Fatalf("unexpected failed metadata: %#v", metadata)
	}
}

func TestConfigEligibilityRequiresExplicitActivation(t *testing.T) {
	activation := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC)
	config := Config{Enabled: true, Projects: []ProjectActivation{{ProjectID: "p", Root: "/work/widget", Included: true, ActivatedAt: activation}}}
	if config.Eligible("/work/widget", activation.Add(-time.Second)) {
		t.Fatal("older session was eligible")
	}
	if !config.Eligible("/work/widget", activation) {
		t.Fatal("activated session was not eligible")
	}
}

func TestSchemasAreValidJSON(t *testing.T) {
	for _, name := range []string{"../../schemas/source-bundle.schema.json", "../../schemas/metadata.schema.json"} {
		contents, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var value any
		if err := json.Unmarshal(contents, &value); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func hasGap(gaps []CaptureGap, code string) bool {
	for _, gap := range gaps {
		if gap.Code == code {
			return true
		}
	}
	return false
}

func TestCursorTextFailsClosedAndTypelessJSONLWorks(t *testing.T) {
	if _, err := (CursorAdapter{}).FilterText(strings.NewReader("system: hidden\nunknown raw"), time.Now()); !IsFilterError(err) {
		t.Fatalf("err=%v", err)
	}
	filtered, err := (CursorAdapter{}).FilterJSONL(strings.NewReader(`{"role":"assistant","content":"visible"}`))
	if err != nil || len(filtered.Records) != 1 {
		t.Fatalf("%v %#v", err, filtered)
	}
}

func TestCursorTextRetainsMultilineMessageBodies(t *testing.T) {
	input := "User: hello\nAssistant: Let me help.\nHere is more detail on a second line.\n"
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(input), time.Now())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if len(filtered.Text) != 1 || !strings.Contains(filtered.Text[0], "second line") {
		t.Fatalf("continuation line lost: %#v", filtered.Text)
	}
}

func TestCursorTextOmitsHiddenSectionContinuationLines(t *testing.T) {
	input := "User: hello\nSystem: hidden instructions\nmore hidden continuation\nAssistant: ok\n"
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader(input), time.Now())
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if strings.Contains(strings.ToLower(filtered.Text[0]), "hidden") {
		t.Fatalf("hidden continuation leaked: %#v", filtered.Text)
	}
}

func TestCursorTextMetadataLeavesStructuredCountsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	filtered, err := (CursorAdapter{}).FilterText(strings.NewReader("User: hello\nAssistant: hi\n"), now)
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "cursor"}
	bundle, err := NewSourceBundle(reg, CursorAdapter{}, filtered, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := BuildMetadata(bundle, "machine", reg.SessionStartedAt, now.Add(time.Minute), SourceReference{Key: "sessions/cursor/archive-123/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Counts.Messages != nil || metadata.Counts.Turns != nil || metadata.Counts.ToolCalls != nil {
		t.Fatalf("unparsed text produced structured counts: %#v", metadata.Counts)
	}
	if metadata.Counts.ExplicitFeedback == nil || *metadata.Counts.ExplicitFeedback != 0 {
		t.Fatalf("independently observed feedback count should remain known: %#v", metadata.Counts)
	}
}

func TestLifecycleDerivationIsOrderedConservativeAndDeterministic(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	event := func(at time.Time, name string, fields map[string]any) SupplementalEvidence {
		payload := map[string]any{"event_name": name}
		for key, value := range fields {
			payload[key] = value
		}
		return SupplementalEvidence{Kind: EvidenceKindLifecycleHook, ObservedAt: at, Provenance: "hook:fixture:" + strings.ToLower(name), Payload: payload}
	}
	// Deliberately append out of order and repeat an exact event. The delayed
	// event must not overwrite the later state, and retries must be inert.
	evidence := []SupplementalEvidence{
		event(now.Add(2*time.Minute), "Stop", nil),
		event(now, "SessionStart", nil),
		event(now.Add(time.Minute), "Stop", nil),
		event(now.Add(2*time.Minute), "Stop", nil),
		event(now.Add(3*time.Minute), "UserPromptSubmit", nil),
	}
	state, outcome := deriveLifecycle(evidence)
	if state != MetadataStateActive || outcome != TurnOutcomeUnknown {
		t.Fatalf("active transition = %q/%q", state, outcome)
	}
	state, outcome = deriveLifecycle(append(evidence, event(now.Add(4*time.Minute), "Stop", nil)))
	if state != MetadataStateIdle || outcome != TurnOutcomeUnknown {
		t.Fatalf("generic stop must be idle with unknown outcome, got %q/%q", state, outcome)
	}
	state, outcome = deriveLifecycle(append(evidence, event(now.Add(4*time.Minute), "Stop", map[string]any{"status": "completed"})))
	if state != MetadataStateIdle || outcome != TurnOutcomeUnknown {
		t.Fatalf("generic stop status must not manufacture completion, got %q/%q", state, outcome)
	}
	cursorStop := event(now.Add(4*time.Minute), "Stop", map[string]any{"status": "completed"})
	cursorStop.Provenance = "hook:cursor:stop"
	state, outcome = deriveLifecycle(append(evidence, cursorStop))
	if state != MetadataStateIdle || outcome != TurnOutcomeCompleted {
		t.Fatalf("documented Cursor stop completion = %q/%q", state, outcome)
	}
	// A subagent finishing does not close, idle, or complete the parent
	// session, whatever status it reports and from whichever provenance.
	for _, status := range []string{"incomplete", "completed"} {
		state, outcome = deriveLifecycle(append(evidence, event(now.Add(4*time.Minute), "SubagentStop", map[string]any{"status": status})))
		if state != MetadataStateActive || outcome != TurnOutcomeUnknown {
			t.Fatalf("subagent stop with status %q changed the parent session to %q/%q", status, state, outcome)
		}
	}
	cursorSubagent := event(now.Add(4*time.Minute), "SubagentStop", map[string]any{"status": "completed"})
	cursorSubagent.Provenance = "hook:cursor:subagentstop"
	state, outcome = deriveLifecycle(append(evidence, cursorStop, cursorSubagent))
	if state != MetadataStateIdle || outcome != TurnOutcomeCompleted {
		t.Fatalf("subagent stop after a documented stop must leave it as observed, got %q/%q", state, outcome)
	}
	for name, want := range map[string]TurnOutcome{"Interrupt": TurnOutcomeInterrupted, "StopFailure": TurnOutcomeError} {
		state, outcome = deriveLifecycle(append(evidence, event(now.Add(5*time.Minute), name, nil)))
		if state != MetadataStateIdle || outcome != want {
			t.Fatalf("%s = %q/%q", name, state, outcome)
		}
	}
	state, outcome = deriveLifecycle(append(evidence, event(now.Add(6*time.Minute), "SessionEnd", nil)))
	if state != MetadataStateClosed || outcome != TurnOutcomeUnknown {
		t.Fatalf("closure = %q/%q", state, outcome)
	}
}

func TestMetadataPinsSemanticConventionAndOldMetadataStillLoads(t *testing.T) {
	now := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	bundle := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "cursor"}, AdapterName: "cursor", AdapterVersion: "1", SourceFormat: "jsonl", FilterVersion: FilterVersion, CapturedAt: now}, NativeRecords: []map[string]any{{"role": "assistant", "model": "gpt-x", "reasoning_effort": "high", "content": "done"}}}
	ref := SourceReference{Key: "sessions/cursor/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64), CompressedBytes: 1}
	metadata, err := BuildMetadata(bundle, "machine", now, now, ref, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if metadata.SemanticConventions == nil || metadata.SemanticConventions.Revision != OpenTelemetryGenAIRevision {
		t.Fatalf("semantic conventions = %#v", metadata.SemanticConventions)
	}
	attrs := metadata.Models[0].Attributes
	if attrs["gen_ai.request.model"] != "gpt-x" || attrs["agent_archive.request.reasoning_level"] != "high" || attrs["gen_ai.request.reasoning.level"] != "" {
		t.Fatalf("attribute mapping = %#v", attrs)
	}

	oldJSON := `{"schema_version":1,"source_bundle":{"key":"sessions/codex/a/source.` + strings.Repeat("a", 64) + `.json.gz","sha256":"` + strings.Repeat("a", 64) + `","compressed_bytes":1}}`
	var old Metadata
	if err := json.Unmarshal([]byte(oldJSON), &old); err != nil {
		t.Fatal(err)
	}
	if old.TurnOutcome != "" || old.SemanticConventions != nil || old.ValidateSourceReference() != nil {
		t.Fatalf("old metadata compatibility failed: %#v", old)
	}
	reencoded, err := json.Marshal(old)
	if err != nil || bytes.Contains(reencoded, []byte(`"turn_outcome":""`)) {
		t.Fatalf("old metadata re-encoded invalid outcome: %s err=%v", reencoded, err)
	}
}

func TestPreciseNativeSkillReadInference(t *testing.T) {
	bundle := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: time.Now()}, NativeRecords: []map[string]any{{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/skills/review-pr/SKILL.md"}}, {"type": "function_call", "command": "cat /skills/create/SKILL.md"}, {"type": "message", "role": "user", "content": "echo SKILL.md"}}}
	m, err := BuildMetadata(bundle, "m", time.Now(), time.Now(), SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.SkillsUsed) != 2 || m.SkillsUsed[0].Name != "create" || m.SkillsUsed[1].Name != "review-pr" {
		t.Fatalf("%#v", m.SkillsUsed)
	}
}

// TestCodexFunctionCallArgumentsSkillReadInference guards against a
// regression where Codex's real function_call shape — arguments as a
// JSON-encoded string, e.g. `{"path":"..."}`, rather than a nested "input"
// object like Claude's tool_use — was never parsed, so a genuine Codex
// SKILL.md read was silently missed. See the codex-function-call.jsonl
// fixture for the real record shape this models.
func TestCodexFunctionCallArgumentsSkillReadInference(t *testing.T) {
	now := time.Now()
	bundle := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: now}, NativeRecords: []map[string]any{
		// A leading turn_context record, as every real Codex transcript has:
		// ParseNormalized skips it before calling toolCalls, so this also
		// guards that skill detection (now derived from toolCalls' own walk,
		// not a separate one) still fires for the record right after it.
		{"type": "turn_context", "model": "gpt-6-astra"},
		{"type": "function_call", "call_id": "call-1", "name": "read_file", "arguments": `{"path":"/skills/review-pr/SKILL.md"}`},
	}}
	m, err := BuildMetadata(bundle, "m", now, now, SourceReference{Key: "sessions/codex/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.SkillsUsed) != 1 || m.SkillsUsed[0].Name != "review-pr" || m.SkillsUsed[0].Evidence != SkillUseEvidenceReadInference {
		t.Fatalf("expected Codex's real function_call/arguments shape to be recognized as a skill read: %#v", m.SkillsUsed)
	}
	if m.SkillDetection != SkillDetectionObserved {
		t.Fatalf("SkillDetection=%q, want observed", m.SkillDetection)
	}
}

func TestNativeAndSupplementalSkillUseDedupeByName(t *testing.T) {
	now := time.Now()
	b := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: now},
		NativeRecords:        []map[string]any{{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/skills/review/SKILL.md"}}},
		SupplementalEvidence: []SupplementalEvidence{{Kind: EvidenceKindSkillRead, ObservedAt: now, Provenance: "hook", Payload: map[string]any{"name": "review", "sha256": "aaaaaaaa"}}},
	}
	ref := SourceReference{Key: "sessions/claude/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}
	m, err := BuildMetadata(b, "m", now, now, ref, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.SkillsUsed) != 1 || m.SkillsUsed[0].SHA256 != "aaaaaaaa" {
		t.Fatalf("expected one deduplicated, hash-bearing entry: %#v", m.SkillsUsed)
	}
}

func TestEligibleSkillWithoutUseCoverageRemainsPartial(t *testing.T) {
	now := time.Now()
	b := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: now}, SupplementalEvidence: []SupplementalEvidence{{Kind: EvidenceKindSkillInventory, ObservedAt: now, Provenance: "fs", Payload: map[string]any{"coverage": "eligible", "skills": []any{map[string]any{"name": "review", "sha256": "aaa"}}}}}}
	ref := SourceReference{Key: "sessions/codex/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}
	m, err := BuildMetadata(b, "m", now, now, ref, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if m.SkillDetection != SkillDetectionPartial {
		t.Fatalf("SkillDetection=%q, want partial without complete use observation", m.SkillDetection)
	}
}

func TestModelSummaryTracksTurnCount(t *testing.T) {
	now := time.Now()
	b := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "cursor"}, AdapterName: "cursor", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: now}, NativeRecords: []map[string]any{
		{"role": "assistant", "model": "gpt-x", "content": "one"},
		{"role": "assistant", "model": "gpt-x", "content": "two"},
		{"role": "assistant", "model": "gpt-y", "content": "three"},
	}}
	ref := SourceReference{Key: "sessions/cursor/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}
	m, err := BuildMetadata(b, "m", now, now, ref, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, model := range m.Models {
		if model.TurnCount == nil {
			t.Fatalf("nil turn count for %#v", model)
		}
		counts[model.Attributes["gen_ai.request.model"]] = *model.TurnCount
	}
	if counts["gpt-x"] != 2 || counts["gpt-y"] != 1 {
		t.Fatalf("turn counts=%#v", counts)
	}
}

func TestHookModelAndAssistantOnlyFinalReconciliation(t *testing.T) {
	now := time.Now()
	bundle := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "cursor"}, AdapterName: "cursor", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: now}, NativeRecords: []map[string]any{{"role": "user", "id": "u", "turn_id": "t", "content": "q"}, {"role": "assistant", "id": "a", "turn_id": "t", "content": "a"}}, SupplementalEvidence: []SupplementalEvidence{{Kind: EvidenceKindLifecycleHook, ObservedAt: now, Provenance: "hook", Payload: map[string]any{"model_id": "canonical", "model": "label", "model_params": []any{map[string]any{"id": "effort", "value": "high"}}}}, {Kind: EvidenceKindFinalResponse, ObservedAt: now, Provenance: "hook", Payload: map[string]any{"turn_id": "t"}}, {Kind: EvidenceKindExplicitFeedback, ObservedAt: now, Provenance: "hook", Payload: map[string]any{"text": "ok"}}}}
	m, err := BuildMetadata(bundle, "m", now, now, SourceReference{Key: "sessions/cursor/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Models) != 1 || m.Models[0].Attributes["gen_ai.request.model"] != "canonical" || m.Models[0].ResponseModelStatus != ResponseModelStatusNotExposed || *m.Counts.ExplicitFeedback != 1 {
		t.Fatalf("%#v", m)
	}
	view, err := ParseNormalized(bundle)
	if err != nil || view.HookFinals[0].Status != HookFinalStatusMatchedTurnID {
		t.Fatalf("%v %#v", err, view.HookFinals)
	}
}

func TestSameNameDifferentHashInventoryMetadataIsDeterministic(t *testing.T) {
	now := time.Now()
	b := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: now}, SupplementalEvidence: []SupplementalEvidence{{Kind: EvidenceKindSkillInventory, ObservedAt: now, Provenance: "fs", Payload: map[string]any{"coverage": "installed_only", "skills": []any{map[string]any{"name": "same", "sha256": "bbb"}, map[string]any{"name": "same", "sha256": "aaa"}}}}}}
	ref := SourceReference{Key: "sessions/codex/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}
	one, e := BuildMetadata(b, "m", now, now, ref, ParserInfo{})
	if e != nil {
		t.Fatal(e)
	}
	two, e := BuildMetadata(b, "m", now, now, ref, ParserInfo{})
	if e != nil {
		t.Fatal(e)
	}
	x, _ := json.Marshal(one)
	y, _ := json.Marshal(two)
	if !bytes.Equal(x, y) || one.SkillsAvailable[0].SHA256 != "aaa" {
		t.Fatalf("%s %s", x, y)
	}
	if one.SkillDetection != SkillDetectionUnavailable {
		t.Fatalf("installed-only inventory must leave use detection unknown, got %q", one.SkillDetection)
	}
}

func TestMergeSupplementalEvidenceRetainsChangedInventoryHistory(t *testing.T) {
	first := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	later := first.Add(time.Hour)
	payload := map[string]any{"coverage": "installed_only", "scope": "user", "skills": []any{map[string]any{"name": "review", "sha256": "abc"}}}
	previous := []SupplementalEvidence{{Kind: EvidenceKindSkillInventory, ObservedAt: first, Provenance: "filesystem:codex", Payload: payload}}
	fresh := []SupplementalEvidence{{Kind: EvidenceKindSkillInventory, ObservedAt: later, Provenance: "filesystem:codex", Payload: payload}}
	merged := MergeSupplementalEvidence(previous, fresh)
	if len(merged) != 1 || !merged[0].ObservedAt.Equal(first) {
		t.Fatalf("merged=%#v", merged)
	}
	fresh[0].Payload = map[string]any{"coverage": "installed_only", "scope": "user", "skills": []any{map[string]any{"name": "review", "sha256": "def"}}}
	merged = MergeSupplementalEvidence(previous, fresh)
	if len(merged) != 2 || !merged[1].ObservedAt.Equal(later) || firstString(merged[0].Payload["skills"].([]any)[0].(map[string]any), "sha256") != "abc" || firstString(merged[1].Payload["skills"].([]any)[0].(map[string]any), "sha256") != "def" {
		t.Fatalf("changed merged=%#v", merged)
	}
	back := previous[0]
	back.ObservedAt = later.Add(time.Hour)
	merged = MergeSupplementalEvidence(merged, []SupplementalEvidence{back})
	if len(merged) != 3 || !merged[2].ObservedAt.Equal(back.ObservedAt) {
		t.Fatalf("changed-back merged=%#v", merged)
	}
}

func TestDeriveSkillsKeepsInventoryHistoryWithoutClaimingUse(t *testing.T) {
	now := time.Date(2026, 9, 20, 1, 0, 0, 0, time.UTC)
	bundle := SourceBundle{
		SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p",
		Capture: SourceCapture{Harness: Harness{Name: "codex"}, AdapterName: "codex", AdapterVersion: "1", SourceFormat: "jsonl", FilterVersion: FilterVersion, CapturedAt: now},
		SupplementalEvidence: []SupplementalEvidence{
			{Kind: EvidenceKindSkillInventory, ObservedAt: now, Provenance: "filesystem:codex", Payload: map[string]any{"coverage": "installed_only", "scope": "project_agents", "root_status": "present", "skills": []any{map[string]any{"name": "review", "sha256": "old"}}}},
			{Kind: EvidenceKindSkillInventory, ObservedAt: now.Add(time.Hour), Provenance: "filesystem:codex", Payload: map[string]any{"coverage": "installed_only", "scope": "project_agents", "root_status": "absent", "skills": []any{}}},
		},
	}
	metadata, err := BuildMetadata(bundle, "machine", now, now.Add(2*time.Hour), SourceReference{Key: "sessions/codex/a/source." + strings.Repeat("a", 64) + ".json.gz", SHA256: strings.Repeat("a", 64)}, ParserInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata.SkillsAvailable) != 1 || metadata.SkillsAvailable[0].Name != "review" || len(metadata.SkillsUsed) != 0 || metadata.SkillDetection != SkillDetectionUnavailable {
		t.Fatalf("metadata=%#v", metadata)
	}
}

func TestMergeSupplementalEvidenceDeduplicatesRetriesButKeepsIntentionalFeedback(t *testing.T) {
	now := time.Now().UTC()
	retried := SupplementalEvidence{Kind: EvidenceKindFinalResponse, ObservedAt: now, Provenance: "hook:codex:stop", Payload: map[string]any{"turn_id": "t1", "text": "done"}}
	one := SupplementalEvidence{Kind: EvidenceKindExplicitFeedback, ObservedAt: now, Provenance: "user:agent-archive-feedback-file", Payload: map[string]any{"event_id": "one", "text": "same"}}
	two := SupplementalEvidence{Kind: EvidenceKindExplicitFeedback, ObservedAt: now, Provenance: "user:agent-archive-feedback-file", Payload: map[string]any{"event_id": "two", "text": "same"}}
	merged := MergeSupplementalEvidence([]SupplementalEvidence{retried, one}, []SupplementalEvidence{retried, two})
	if len(merged) != 3 {
		t.Fatalf("merged=%#v", merged)
	}
}

func TestMergeSupplementalEvidenceRetainsHistoricalSkillVersions(t *testing.T) {
	now := time.Now().UTC()
	previous := []SupplementalEvidence{
		{Kind: EvidenceKindSkillInventory, ObservedAt: now, Provenance: "filesystem:codex", Payload: map[string]any{"coverage": "installed_only", "scope": "project", "skills": []any{map[string]any{"name": "old"}}}},
		{Kind: EvidenceKindSkillSnapshot, ObservedAt: now, Provenance: "filesystem:codex", Payload: map[string]any{"name": "old", "scope": "project", "snapshot": "old"}},
	}
	fresh := []SupplementalEvidence{
		{Kind: EvidenceKindSkillInventory, ObservedAt: now.Add(time.Minute), Provenance: "filesystem:codex", Payload: map[string]any{"coverage": "installed_only", "scope": "project", "skills": []any{map[string]any{"name": "old", "sha256": "new-hash"}}}},
		{Kind: EvidenceKindSkillSnapshot, ObservedAt: now.Add(time.Minute), Provenance: "filesystem:codex", Payload: map[string]any{"name": "old", "scope": "project", "sha256": "new-hash", "snapshot": "new"}},
	}
	merged := MergeSupplementalEvidence(previous, fresh)
	if len(merged) != 4 || firstString(merged[1].Payload, "snapshot") != "old" || firstString(merged[3].Payload, "snapshot") != "new" {
		t.Fatalf("merged=%#v", merged)
	}
	if repeated := MergeSupplementalEvidence(merged, fresh[1:]); len(repeated) != len(merged) {
		t.Fatalf("identical historical snapshot repeated: %#v", repeated)
	}
}

func TestClaudeMultipleToolUseEntriesHaveResponseAttribution(t *testing.T) {
	now := time.Now()
	b := SourceBundle{SchemaVersion: 1, ArchiveSessionID: "a", NativeSessionID: "n", ProjectID: "p", Capture: SourceCapture{Harness: Harness{Name: "claude"}, AdapterName: "claude", AdapterVersion: "1", SourceFormat: "x", FilterVersion: FilterVersion, CapturedAt: now}, NativeRecords: []map[string]any{{"type": "assistant", "model": "claude-response", "message": map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": "x"}, map[string]any{"type": "tool_use", "id": "one"}, map[string]any{"type": "tool_use", "id": "two"}}}}}}
	v, e := ParseNormalized(b)
	if e != nil || len(v.ToolCalls) != 2 || v.Turns[0].ResponseModel != "claude-response" {
		t.Fatalf("%v %#v", e, v)
	}
}

func TestHiddenChannelAndBearerCredentialAreRemoved(t *testing.T) {
	input := `{"type":"response_item","payload":{"type":"message","role":"assistant","channel":"analysis","content":"hidden"}}` + "\n" + `{"type":"response_item","payload":{"type":"message","role":"assistant","channel":"final","content":"Authorization: Bearer token-secret-value visible"}}`
	f, e := (CodexAdapter{}).FilterJSONL(strings.NewReader(input))
	if e != nil {
		t.Fatal(e)
	}
	joined := string(bytes.Join(f.Records, []byte("\n")))
	if strings.Contains(joined, "hidden") || strings.Contains(joined, "token-secret-value") || !strings.Contains(joined, "final") || !strings.Contains(joined, "[REDACTED]") {
		t.Fatalf("%s", joined)
	}
}

func TestAllHiddenContentArrayOmitsFieldInsteadOfEmptyPlaceholder(t *testing.T) {
	input := `{"type":"response_item","id":"x","payload":{"type":"message","role":"assistant","content":[{"type":"reasoning","text":"hidden thought only"}]}}`
	f, e := (CodexAdapter{}).FilterJSONL(strings.NewReader(input))
	if e != nil {
		t.Fatal(e)
	}
	if len(f.Records) != 1 {
		t.Fatalf("records=%d", len(f.Records))
	}
	if strings.Contains(string(f.Records[0]), `"content":[]`) {
		t.Fatalf("empty content placeholder still present: %s", f.Records[0])
	}
}

func TestClaudeBundleAttributesRecordVersionToHarness(t *testing.T) {
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-tool-use.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if hasGap(filtered.Gaps, "unknown_field_omitted") {
		t.Fatalf("version field was dropped: %#v", filtered.Gaps)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, time.Date(2026, 9, 17, 18, 25, 0, 0, time.UTC), nil)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Capture.Harness.Version != "1.0.83" {
		t.Fatalf("claude harness version = %q, want 1.0.83", bundle.Capture.Harness.Version)
	}
	// A Codex transcript must not pick up a stray version key the same way.
	codex := observedHarness(Harness{Name: "codex", Version: "keep"}, "codex-jsonl", []map[string]any{{"type": "message", "version": "9.9.9"}})
	if codex.Version != "keep" {
		t.Fatalf("codex harness version = %q", codex.Version)
	}
}
