package collector

import (
	"context"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
)

func TestChildProvenanceRequiresAgentIdentityAndStableStart(t *testing.T) {
	at := time.Now().UTC()
	reg := archive.SessionRegistration{ParentSessionID: "parent", ParentNativeSessionID: "native-parent", SubagentID: "agent", SessionStartedAt: at, SubagentObservedAt: at.Add(time.Minute)}
	filtered := archive.FilteredTranscript{NativeStartComplete: true, NativeStartAt: at, NativeEndAt: at, SessionIDs: []string{"native-parent"}}
	if err := validateSubagentTranscript(reg, filtered); err == nil {
		t.Fatal("parent transcript without agent identity accepted as child")
	}
	filtered.AgentIDs = []string{"agent"}
	if err := validateSubagentTranscript(reg, filtered); err != nil {
		t.Fatal(err)
	}
	filtered.NativeStartAt = at.Add(-time.Hour)
	if err := validateSubagentTranscript(reg, filtered); err == nil {
		t.Fatal("replacement with old child start accepted")
	}
}

func TestMissingChildTranscriptRemainsRetryable(t *testing.T) {
	home := t.TempDir()
	store, err := NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "native-parent", ProjectID: "p", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "claude"}, SessionStartedAt: at.Add(-time.Hour), RegisteredAt: at.Add(-time.Hour)}
	if err := store.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	candidate := SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-child", ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID, ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness, AgentID: "agent", TranscriptPath: filepath.Join(home, "not-created-yet.jsonl"), ObservedAt: at}
	if err := store.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := materializeSubagentCandidate(store, candidate, Options{}); err == nil {
		t.Fatal("expected pending transcript error")
	}
	candidates, err := store.LoadSubagentCandidates()
	if err != nil || len(candidates) != 1 {
		t.Fatalf("lost delayed child retry: %+v %v", candidates, err)
	}
}

func TestCollectorRepairsParentLinkAfterNotificationFailure(t *testing.T) {
	local := newTestStore(t)
	at := time.Now().UTC()
	parent := registration(t, "")
	parent.ArchiveSessionID = "parent"
	parent.NativeSessionID = "native-parent"
	child := registration(t, "")
	child.ArchiveSessionID = "child"
	child.NativeSessionID = "native-child"
	child.ParentSessionID = "parent"
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRegistration(child); err != nil {
		t.Fatal(err)
	}
	filtered, err := archive.CodexAdapter{}.FilterJSONL(strings.NewReader(`{"type":"turn_context","model":"synthetic"}` + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := archive.NewSourceBundle(child, archive.CodexAdapter{}, filtered, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SavePublished("child", bundle, at, CacheStatusPublished); err != nil {
		t.Fatal(err)
	}
	blocked := local.requestPath("parent")
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := markPublishedSubagent(local, child); err == nil {
		t.Fatal("expected parent notification failure")
	}
	if err := os.Remove(blocked); err != nil {
		t.Fatal(err)
	}
	// Child source cannot be republished in this pass. The durable publication
	// must still repair its parent notification before any live transcript read.
	_, err = Run(context.Background(), local, storage.NewMemoryStore(), Options{MachineID: "machine", Now: func() time.Time { return at.Add(time.Minute) }, AcceptSession: func(reg archive.SessionRegistration) bool { return reg.ArchiveSessionID == "child" }})
	if err != nil {
		t.Fatal(err)
	}
	request, found, err := local.loadRequest("parent")
	if err != nil || !found || len(request.HookEvidence) == 0 {
		t.Fatalf("parent notification lost: %+v %v", request, err)
	}
}

func TestHiddenOldRecordCannotMakeResumedChildLookFresh(t *testing.T) {
	input := `{"type":"unknown_hidden_record","sessionId":"parent","agentId":"agent","timestamp":"2026-09-21T09:00:00Z","content":"not retained"}` + "\n" + `{"type":"assistant","sessionId":"parent","agentId":"agent","timestamp":"2026-09-21T10:00:00Z","message":{"role":"assistant","content":"visible"}}` + "\n"
	filtered, err := archive.ClaudeAdapter{}.FilterJSONL(strings.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	reg := archive.SessionRegistration{ParentSessionID: "parent-archive", ParentNativeSessionID: "parent", SubagentID: "agent", SessionStartedAt: at, SubagentObservedAt: at.Add(time.Minute)}
	if err := validateSubagentTranscript(reg, filtered); err == nil {
		t.Fatal("excluded old native record was ignored for eligibility")
	}
}

func TestLaterChildStopSurvivesEarlierCaptureAcknowledgement(t *testing.T) {
	local := newTestStore(t)
	at := time.Now().UTC()
	first := SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "native-child", ParentArchiveSessionID: "parent", ParentNativeSessionID: "native-parent", ProjectID: "project", ProjectRoot: "/synthetic", Harness: archive.Harness{Name: "claude"}, AgentID: "agent", TranscriptPath: "/synthetic/child.jsonl", ObservedAt: at}
	if err := local.SaveSubagentCandidate(first); err != nil {
		t.Fatal(err)
	}
	newer := first
	newer.ObservedAt = at.Add(time.Second)
	if err := local.SaveSubagentCandidate(newer); err != nil {
		t.Fatal(err)
	}
	if err := local.acknowledgeSubagentCandidate(first); err != nil {
		t.Fatal(err)
	}
	remaining, err := local.LoadSubagentCandidates()
	if err != nil || len(remaining) != 1 || !remaining[0].ObservedAt.Equal(newer.ObservedAt) {
		t.Fatalf("new stop lost: %+v %v", remaining, err)
	}
	if err := local.acknowledgeSubagentCandidate(newer); err != nil {
		t.Fatal(err)
	}
	remaining, err = local.LoadSubagentCandidates()
	if err != nil || len(remaining) != 0 {
		t.Fatalf("processed stop retained: %+v %v", remaining, err)
	}
}
