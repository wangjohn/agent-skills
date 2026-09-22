package archive

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// Claude Code writes bookkeeping records (summary, file-history-snapshot)
// with no top-level timestamp beside the conversation. Treating those as
// missing provenance would permanently reject every live child transcript.
func TestUntimestampedClaudeBookkeepingRecordsKeepStartProvenanceComplete(t *testing.T) {
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-bookkeeping-records.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	if !filtered.NativeStartComplete {
		t.Fatalf("bookkeeping records without timestamps broke start provenance: gaps=%#v", filtered.Gaps)
	}
	if want := time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC); !filtered.NativeStartAt.Equal(want) {
		t.Fatalf("native start = %v, want %v", filtered.NativeStartAt, want)
	}
	if want := time.Date(2026, 9, 17, 18, 1, 0, 0, time.UTC); !filtered.NativeEndAt.Equal(want) {
		t.Fatalf("native end = %v, want %v", filtered.NativeEndAt, want)
	}
}

// A conversational record without a timestamp is still missing provenance.
func TestUntimestampedMessageRecordStillBreaksStartProvenance(t *testing.T) {
	input := `{"type":"summary","summary":"recap"}` + "\n" +
		`{"type":"assistant","uuid":"a","timestamp":"2026-09-17T18:00:00Z","message":{"role":"assistant","content":"first"}}` + "\n" +
		`{"type":"assistant","uuid":"b","message":{"role":"assistant","content":"untimestamped"}}` + "\n"
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	if filtered.NativeStartComplete {
		t.Fatal("message record without a timestamp was accepted as complete provenance")
	}
}

// `detail` is archive-authored vocabulary for capture-gap evidence. It must
// not become a globally retained native-record field: sanitizeObject recurses
// and keeps strings verbatim, so a native `detail` would leak free text.
func TestNativeDetailFieldIsNotRetainedButCaptureGapEvidenceKeepsIt(t *testing.T) {
	input := `{"type":"assistant","uuid":"a","timestamp":"2026-09-17T18:00:00Z","detail":"native free text","message":{"role":"assistant","content":[{"type":"text","text":"visible","detail":"nested free text"}]}}` + "\n"
	filtered, err := (ClaudeAdapter{}).FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	joined := string(bytes.Join(filtered.Records, []byte("\n")))
	if strings.Contains(joined, "native free text") || strings.Contains(joined, "nested free text") {
		t.Fatalf("native detail field retained: %s", joined)
	}
	if !strings.Contains(joined, "visible") {
		t.Fatalf("conversational content lost: %s", joined)
	}

	evidence, _, err := FilterSupplementalEvidence([]SupplementalEvidence{{
		Kind: EvidenceKindCaptureGap, ObservedAt: time.Date(2026, 9, 17, 18, 0, 0, 0, time.UTC),
		Provenance: "hook:subagent-link",
		Payload:    map[string]any{"code": "subagent_identity_unavailable", "detail": "SubagentStop omitted agent_id"},
	}})
	if err != nil || len(evidence) != 1 {
		t.Fatalf("capture gap evidence dropped: %v %#v", err, evidence)
	}
	if evidence[0].Payload["code"] != "subagent_identity_unavailable" || evidence[0].Payload["detail"] != "SubagentStop omitted agent_id" {
		t.Fatalf("capture gap payload = %#v", evidence[0].Payload)
	}
}

// An older Claude layout inlines a subagent's records in the parent's own
// transcript. The child is archived as its own session, so the parent must
// not count those messages, turns, and tool calls a second time.
func TestParentCountsExcludeInlinedSidechainRecords(t *testing.T) {
	filtered, err := (ClaudeAdapter{}).FilterJSONL(bytes.NewReader(fixture(t, "claude-parent-with-sidechain.jsonl")))
	if err != nil {
		t.Fatal(err)
	}
	reg := registration()
	reg.Harness = Harness{Name: "claude"}
	reg.NativeSessionID = "native-parent"
	capturedAt := time.Date(2026, 9, 17, 18, 25, 0, 0, time.UTC)
	bundle, err := NewSourceBundle(reg, ClaudeAdapter{}, filtered, capturedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := ParseNormalized(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Turns) != 2 || len(view.ToolCalls) != 1 {
		t.Fatalf("parent view counted child records: turns=%d toolCalls=%d", len(view.Turns), len(view.ToolCalls))
	}
	for _, call := range view.ToolCalls {
		if call.CallID == "child-tool-1" {
			t.Fatalf("parent retained the child's tool call: %#v", view.ToolCalls)
		}
	}

	// The same transcript read as the child's own session keeps its records.
	child := reg
	child.ArchiveSessionID = "archive-child"
	child.ParentSessionID = "archive-123"
	child.ParentNativeSessionID = "native-parent"
	child.SubagentID = "agent-1"
	childBundle, err := NewSourceBundle(child, ClaudeAdapter{}, filtered, capturedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	childView, err := ParseNormalized(childBundle)
	if err != nil {
		t.Fatal(err)
	}
	if len(childView.Turns) != 3 || len(childView.ToolCalls) != 2 {
		t.Fatalf("child view dropped its own records: turns=%d toolCalls=%d", len(childView.Turns), len(childView.ToolCalls))
	}
}
