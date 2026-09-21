package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
)

func handleSubagentStop(store *collector.LocalStore, cfg config.Config, harness, parentNativeID, eventName string, payload map[string]any, now time.Time) error {
	parentID, found, err := store.ArchiveSessionID(parentNativeID)
	if err != nil {
		return fmt.Errorf("look up parent archive session ID: %w", err)
	}
	if !found {
		return nil
	}
	parent, found, err := store.LoadRegistration(parentID)
	if err != nil {
		return err
	}
	if !found || !strings.EqualFold(parent.Harness.Name, harness) || !cfg.AcceptSession(parent) {
		return nil
	}

	agentID := firstNonEmptyString(payload, "agent_id")
	if agentID == "" {
		return saveSubagentCaptureGap(store, parent.ArchiveSessionID, "subagent_identity_unavailable", "SubagentStop omitted agent_id", now)
	}
	childNativeID := parent.NativeSessionID + ":subagent:" + agentID
	childID, _, err := store.EnsureArchiveSessionID(childNativeID)
	if err != nil {
		return fmt.Errorf("assign subagent archive session ID: %w", err)
	}
	status := archive.LinkedSessionUnavailable
	path := firstNonEmptyString(payload, "agent_transcript_path")
	if supportsSubagentTranscript(harness) && path != "" {
		status = archive.LinkedSessionPending
	}
	if existing, childFound, err := store.LoadRegistration(childID); err != nil {
		return err
	} else if childFound {
		if existing.ParentSessionID != parent.ArchiveSessionID || existing.ParentNativeSessionID != parent.NativeSessionID || existing.ProjectID != parent.ProjectID || existing.ProjectRoot != parent.ProjectRoot || !strings.EqualFold(existing.Harness.Name, parent.Harness.Name) || existing.SubagentID != agentID {
			return nil
		}
		if _, _, published, err := store.LoadLastPublished(childID); err != nil {
			return err
		} else if published {
			status = archive.LinkedSessionPublished
		}
	}
	if err := saveLinkedSessionEvidence(store, parent.ArchiveSessionID, childID, status, now); err != nil {
		return err
	}
	if !supportsSubagentTranscript(harness) || path == "" {
		return nil
	}
	return store.SaveSubagentCandidate(collector.SubagentCandidate{
		ArchiveSessionID: childID, NativeSessionID: childNativeID,
		ParentArchiveSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID,
		ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness,
		AgentID: agentID, TranscriptPath: path, ObservedAt: now,
	})
}

func supportsSubagentTranscript(harness string) bool {
	switch strings.ToLower(strings.TrimSpace(harness)) {
	case "claude", "claude-code":
		return true
	default:
		return false
	}
}

func saveLinkedSessionEvidence(store *collector.LocalStore, parentID, childID string, status archive.LinkedSessionStatus, observedAt time.Time) error {
	evidence, err := archive.NewLinkedSessionEvidence(childID, status, observedAt)
	if err != nil {
		return fmt.Errorf("filter subagent link: %w", err)
	}
	return store.SaveRequest(parentID, "subagent-link", observedAt, evidence)
}

func saveSubagentCaptureGap(store *collector.LocalStore, parentID, code, detail string, observedAt time.Time) error {
	filtered, _, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindCaptureGap, ObservedAt: observedAt, Provenance: "hook:subagent-link",
		Payload: map[string]any{"code": code, "detail": detail},
	}})
	if err != nil || len(filtered) == 0 {
		return err
	}
	return store.SaveRequest(parentID, "subagent-link-unavailable", observedAt, filtered[0])
}
