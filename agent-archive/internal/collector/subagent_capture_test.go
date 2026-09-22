package collector

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func TestRunMaterializesAndPublishesSeparateClaudeSubagent(t *testing.T) {
	home := t.TempDir()
	local, err := NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	parentStart := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	parentPath := filepath.Join(home, "parent.jsonl")
	childPath := filepath.Join(home, "child.jsonl")
	if err := os.WriteFile(parentPath, []byte(`{"type":"assistant","sessionId":"parent-native","timestamp":"2026-09-21T10:01:00Z","message":{"role":"assistant","content":"parent"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(`{"type":"assistant","sessionId":"parent-native","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"child"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: parentPath, SessionStartedAt: parentStart, RegisteredAt: parentStart}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	stopAt := parentStart.Add(3 * time.Minute)
	if err := local.SaveSubagentCandidate(SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "parent-native:subagent:agent-1", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: childPath, ObservedAt: stopAt}); err != nil {
		t.Fatal(err)
	}
	link, err := archive.NewLinkedSessionEvidence("child", archive.LinkedSessionPending, stopAt)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SaveRequest("parent", "subagent-link", stopAt, link); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	now := stopAt.Add(time.Minute)
	result, err := Run(context.Background(), local, remote, Options{MachineID: "machine", Now: func() time.Time { return now }, AcceptSession: func(archive.SessionRegistration) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Errors) != 0 || len(result.Published) != 2 {
		t.Fatalf("result=%#v", result)
	}
	child, found, err := local.LoadRegistration("child")
	if err != nil || !found || child.ParentSessionID != "parent" || !child.SessionStartedAt.Equal(parentStart.Add(2*time.Minute)) {
		t.Fatalf("child=%#v found=%v err=%v", child, found, err)
	}
	parentMetadata := fetchMetadata(t, remote, "claude", "parent")
	childMetadata := fetchMetadata(t, remote, "claude", "child")
	if parentMetadata.Counts.Messages == nil || *parentMetadata.Counts.Messages != 1 || childMetadata.Counts.Messages == nil || *childMetadata.Counts.Messages != 1 {
		t.Fatalf("counts parent=%#v child=%#v", parentMetadata.Counts, childMetadata.Counts)
	}
	if childMetadata.ParentSessionID != "parent" {
		t.Fatalf("child parent=%q", childMetadata.ParentSessionID)
	}
}

func TestMaterializeRejectsMismatchedSubagentOwnership(t *testing.T) {
	home := t.TempDir()
	local, _ := NewLocalStore(home)
	start := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	path := filepath.Join(home, "wrong.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"assistant","sessionId":"other-parent","agentId":"agent-1","timestamp":"2026-09-21T10:02:00Z","message":{"role":"assistant","content":"wrong"}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent := archive.SessionRegistration{ArchiveSessionID: "parent", NativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: start, RegisteredAt: start}
	if err := local.SaveRegistration(parent); err != nil {
		t.Fatal(err)
	}
	candidate := SubagentCandidate{ArchiveSessionID: "child", NativeSessionID: "child-native", ParentArchiveSessionID: "parent", ParentNativeSessionID: "parent-native", ProjectID: "project", ProjectRoot: "/project", Harness: archive.Harness{Name: "claude"}, AgentID: "agent-1", TranscriptPath: path, ObservedAt: start.Add(3 * time.Minute)}
	if err := local.SaveSubagentCandidate(candidate); err != nil {
		t.Fatal(err)
	}
	if err := materializeSubagentCandidate(local, candidate, Options{}); err == nil {
		t.Fatal("mismatched transcript was accepted")
	}
	if _, found, _ := local.LoadRegistration("child"); found {
		t.Fatal("mismatched child registration persisted")
	}
	requests, err := local.LoadRequests()
	if err != nil || len(requests) != 1 || requests[0].ArchiveSessionID != "parent" {
		t.Fatalf("requests=%#v err=%v", requests, err)
	}
}
