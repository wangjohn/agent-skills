package collector

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
)

func materializeSubagentCandidates(local *LocalStore, opts Options) map[string]error {
	issues := map[string]error{}
	candidates, err := local.LoadSubagentCandidates()
	if err != nil {
		issues["subagent-candidates"] = err
		return issues
	}
	for _, candidate := range candidates {
		if err := materializeSubagentCandidate(local, candidate, opts); err != nil {
			issues[candidate.ArchiveSessionID] = err
		}
	}
	return issues
}

func materializeSubagentCandidate(local *LocalStore, candidate SubagentCandidate, opts Options) error {
	parent, found, err := local.LoadRegistration(candidate.ParentArchiveSessionID)
	if err != nil {
		return err
	}
	if !found || parent.NativeSessionID != candidate.ParentNativeSessionID || parent.ProjectID != candidate.ProjectID || parent.ProjectRoot != candidate.ProjectRoot || !strings.EqualFold(parent.Harness.Name, candidate.Harness.Name) || (opts.AcceptSession != nil && !opts.AcceptSession(parent)) {
		return rejectSubagentCandidate(local, candidate, "subagent_parent_ownership_unavailable")
	}
	reg := archive.SessionRegistration{
		ArchiveSessionID: candidate.ArchiveSessionID, NativeSessionID: candidate.NativeSessionID,
		ProjectID: parent.ProjectID, ProjectRoot: parent.ProjectRoot, Harness: parent.Harness,
		TranscriptPath: candidate.TranscriptPath, RegisteredAt: candidate.ObservedAt,
		ParentSessionID: parent.ArchiveSessionID, ParentNativeSessionID: parent.NativeSessionID,
		SubagentID: candidate.AgentID, SubagentObservedAt: candidate.ObservedAt,
	}
	adapter, err := archive.NewAdapter(reg.Harness.Name)
	if err != nil {
		return rejectSubagentCandidate(local, candidate, "subagent_format_unavailable")
	}
	filtered, err := filterTranscript(adapter, reg)
	if err != nil {
		return rejectSubagentCandidate(local, candidate, "subagent_transcript_unavailable")
	}
	reg.SessionStartedAt = filtered.NativeStartAt
	if err := validateSubagentTranscript(reg, filtered); err != nil {
		return rejectSubagentCandidate(local, candidate, "subagent_provenance_unavailable")
	}
	if reg.SessionStartedAt.Before(parent.SessionStartedAt) || reg.SessionStartedAt.After(candidate.ObservedAt) || (opts.AcceptSession != nil && !opts.AcceptSession(reg)) {
		return rejectSubagentCandidate(local, candidate, "subagent_start_ineligible")
	}
	if existing, found, err := local.LoadRegistration(reg.ArchiveSessionID); err != nil {
		return err
	} else if found && (existing.ParentSessionID != reg.ParentSessionID || existing.ParentNativeSessionID != reg.ParentNativeSessionID || existing.ProjectID != reg.ProjectID || existing.ProjectRoot != reg.ProjectRoot || !strings.EqualFold(existing.Harness.Name, reg.Harness.Name) || existing.SubagentID != reg.SubagentID) {
		return rejectSubagentCandidate(local, candidate, "subagent_registration_conflict")
	}
	if err := local.SaveRegistration(reg); err != nil {
		return err
	}
	lifecycle, _, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindLifecycleHook, ObservedAt: candidate.ObservedAt,
		Provenance: "hook:" + strings.ToLower(reg.Harness.Name) + ":subagentstop",
		Payload:    map[string]any{"event_name": "SubagentStop", "agent_id": candidate.AgentID},
	}})
	if err != nil {
		return err
	}
	if len(lifecycle) > 0 {
		if err := local.SaveRequest(reg.ArchiveSessionID, "subagentstop", candidate.ObservedAt, lifecycle[0]); err != nil {
			return err
		}
	}
	return local.RemoveSubagentCandidate(candidate.ArchiveSessionID)
}

// validateSubagentTranscript is called before registration and on every later
// scan because the hook-provided path is mutable local state.
func validateSubagentTranscript(reg archive.SessionRegistration, filtered archive.FilteredTranscript) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	if !filtered.NativeStartComplete || filtered.NativeStartAt.IsZero() || filtered.NativeEndAt.IsZero() || reg.SubagentObservedAt.IsZero() || filtered.NativeEndAt.After(reg.SubagentObservedAt) {
		return errors.New("subagent transcript has incomplete native timestamp provenance")
	}
	if len(filtered.SessionIDs) == 0 {
		return errors.New("subagent transcript has no parent session identity")
	}
	for _, id := range filtered.SessionIDs {
		if id != reg.ParentNativeSessionID {
			return errors.New("subagent transcript parent session identity mismatch")
		}
	}
	for _, id := range filtered.AgentIDs {
		if id != reg.SubagentID {
			return errors.New("subagent transcript agent identity mismatch")
		}
	}
	return nil
}

func rejectSubagentCandidate(local *LocalStore, candidate SubagentCandidate, code string) error {
	evidence, err := archive.NewLinkedSessionEvidence(candidate.ArchiveSessionID, archive.LinkedSessionUnavailable, candidate.ObservedAt)
	if err != nil {
		return err
	}
	if err := local.SaveRequest(candidate.ParentArchiveSessionID, code, candidate.ObservedAt, evidence); err != nil {
		return err
	}
	if err := local.RemoveSubagentCandidate(candidate.ArchiveSessionID); err != nil {
		return err
	}
	return fmt.Errorf("%s", code)
}

func markPublishedSubagent(local *LocalStore, reg archive.SessionRegistration, observedAt time.Time) error {
	if reg.ParentSessionID == "" {
		return nil
	}
	evidence, err := archive.NewLinkedSessionEvidence(reg.ArchiveSessionID, archive.LinkedSessionPublished, observedAt)
	if err != nil {
		return err
	}
	return local.SaveRequest(reg.ParentSessionID, "subagent-published", observedAt, evidence)
}
