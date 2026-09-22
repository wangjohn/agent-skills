package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/reader"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func TestHookChildCaptureResumeAndReadBack(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	at := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	setUpTestConfig(t, home, project, at.Add(-time.Hour))
	parentPath, childPath := filepath.Join(project, "parent.jsonl"), filepath.Join(project, "child.jsonl")
	record := func(text, agent string, when time.Time) string {
		t.Helper()
		data, err := json.Marshal(map[string]any{"type": "assistant", "sessionId": "native-parent", "agentId": agent, "timestamp": when.Format(time.RFC3339Nano), "message": map[string]any{"role": "assistant", "content": text}})
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	parentRecord := record("parent", "", at)
	childRecord := record("child-one", "child-agent", at.Add(time.Minute))
	if err := os.WriteFile(parentPath, []byte(parentRecord), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(childRecord), 0600); err != nil {
		t.Fatal(err)
	}
	hook := func(payload map[string]any, when time.Time) {
		t.Helper()
		if err := handleHookEvent(home, "claude", payload, when); err != nil {
			t.Fatal(err)
		}
	}
	hook(map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent", "agent_id": "child-agent", "agent_transcript_path": childPath}, at.Add(-time.Second))
	earlyStore, _ := collector.NewLocalStore(home)
	early, _ := earlyStore.LoadSubagentCandidates()
	if len(early) != 0 {
		t.Fatal("child before accepted parent was staged")
	}
	hook(map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "native-parent", "cwd": project, "transcript_path": parentPath}, at)
	hook(map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent"}, at.Add(15*time.Second))
	stop := map[string]any{"hook_event_name": "SubagentStop", "session_id": "native-parent", "agent_id": "child-agent", "agent_transcript_path": childPath}
	now := at.Add(2 * time.Minute)
	hook(stop, now)
	cfg, _, _ := config.Load(home)
	local, _ := collector.NewLocalStore(home)
	remote := storage.NewMemoryStore()
	collect := func() {
		t.Helper()
		result, err := collector.Run(context.Background(), local, remote, collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return now }, AcceptSession: cfg.AcceptSession})
		if err != nil || len(result.Errors) > 0 {
			t.Fatalf("collection: %+v %v", result, err)
		}
	}
	collect()
	collect()
	regs, _ := local.LoadRegistrations()
	var child, parent archive.SessionRegistration
	for _, reg := range regs {
		if reg.ParentSessionID != "" {
			child = reg
		} else {
			parent = reg
		}
	}
	if child.ArchiveSessionID == "" || !child.SessionStartedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("child registration: %+v", child)
	}
	read := func(reg archive.SessionRegistration) archive.Metadata {
		t.Helper()
		key, _ := archive.MetadataObjectKey("claude", reg.ArchiveSessionID)
		m, b, err := reader.RefreshAndLoad(context.Background(), remote, key, reader.Limits{})
		if err != nil {
			t.Fatal(err)
		}
		if b.ParentSessionID != reg.ParentSessionID {
			t.Fatal("source linkage mismatch")
		}
		return m
	}
	pm, cm := read(parent), read(child)
	foundGap := false
	for _, gap := range pm.CaptureGaps {
		if gap.Code == "subagent_identity_unavailable" {
			foundGap = true
		}
	}
	if !foundGap {
		t.Fatal("known child with missing identity was silently omitted")
	}

	if pm.State == archive.MetadataStateClosed || pm.Counts.Messages == nil || *pm.Counts.Messages != 1 || cm.Counts.Messages == nil || *cm.Counts.Messages != 1 {
		t.Fatalf("wrong states/counts parent=%+v child=%+v", pm, cm)
	}
	if len(pm.LinkedSessions) != 1 || pm.LinkedSessions[0].Status != archive.LinkedSessionPublished {
		t.Fatalf("parent link: %+v", pm.LinkedSessions)
	}
	// A resumed child retains its original start and can publish later messages.
	if err := os.WriteFile(childPath, []byte(childRecord+record("child-two", "child-agent", at.Add(3*time.Minute))), 0600); err != nil {
		t.Fatal(err)
	}
	now = at.Add(4 * time.Minute)
	hook(stop, now)
	hook(stop, now.Add(time.Second))
	now = now.Add(2 * time.Second)
	collect()
	collect()
	cm = read(child)
	if cm.Counts.Messages == nil || *cm.Counts.Messages != 2 {
		t.Fatal(fmt.Sprintf("resumed child messages: %+v", cm.Counts))
	}
	updated, _, _ := local.LoadRegistration(child.ArchiveSessionID)
	if !updated.SessionStartedAt.Equal(child.SessionStartedAt) {
		t.Fatal("child start changed on resume")
	}
	// An unchanged pass does not republish either source.
	unchanged, err := collector.Run(context.Background(), local, remote, collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return now }, AcceptSession: cfg.AcceptSession})
	if err != nil || len(unchanged.Errors) > 0 || len(unchanged.Published) > 0 {
		t.Fatalf("unchanged capture: %+v %v", unchanged, err)
	}
	// Replacing the accepted path with a parent-only transcript must not upload it.
	if err := os.WriteFile(childPath, []byte(parentRecord), 0600); err != nil {
		t.Fatal(err)
	}
	failed, err := collector.Run(context.Background(), local, remote, collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return now }, AcceptSession: cfg.AcceptSession})
	if err != nil || failed.Errors[child.ArchiveSessionID] == nil {
		t.Fatalf("replacement not rejected: %+v %v", failed, err)
	}
	retained := read(child)
	if retained.SourceBundle.SHA256 != cm.SourceBundle.SHA256 {
		t.Fatal("replacement changed published child")
	}

}
