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
	if got, want := filtered.Boundary.RetainedRecords, 3; got != want {
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
	if metadata.Counts.Turns == nil || *metadata.Counts.Turns != 1 || metadata.Counts.ToolCalls == nil || *metadata.Counts.ToolCalls != 1 {
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
		Kind: "explicit_feedback", Provenance: "hook", ObservedAt: time.Date(2026, 9, 17, 18, 26, 0, 0, time.UTC), Payload: map[string]any{"text": "fixed password=synthetic-value"},
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
	if metadata.Parser.Status != "failed" || metadata.Counts.Turns != nil || metadata.SourceBundle.SHA256 == "" {
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
