package collector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// SubagentCandidate is local operational state written by a bounded hook.
// TranscriptPath is never copied into an archive bundle or metadata sidecar.
type SubagentCandidate struct {
	ArchiveSessionID       string          `json:"archive_session_id"`
	NativeSessionID        string          `json:"native_session_id"`
	ParentArchiveSessionID string          `json:"parent_archive_session_id"`
	ParentNativeSessionID  string          `json:"parent_native_session_id"`
	ProjectID              string          `json:"project_id"`
	ProjectRoot            string          `json:"project_root"`
	Harness                archive.Harness `json:"harness"`
	AgentID                string          `json:"agent_id"`
	TranscriptPath         string          `json:"transcript_path"`
	ObservedAt             time.Time       `json:"observed_at"`
}

func (s *LocalStore) subagentCandidatePath(id string) string {
	return filepath.Join(s.home, "subagent-candidates", id+".json")
}

// SaveSubagentCandidate coalesces duplicate stop deliveries without allowing
// a later event to replace the path or ownership established by the first.
func (s *LocalStore) SaveSubagentCandidate(candidate SubagentCandidate) error {
	if !safeFileComponent(candidate.ArchiveSessionID) || candidate.NativeSessionID == "" || candidate.ParentArchiveSessionID == "" || candidate.ParentNativeSessionID == "" || candidate.ProjectID == "" || candidate.ProjectRoot == "" || candidate.Harness.Name == "" || candidate.AgentID == "" || candidate.TranscriptPath == "" || candidate.ObservedAt.IsZero() {
		return errors.New("subagent candidate is incomplete")
	}
	unlock, err := s.lockSubagentCandidate(candidate.ArchiveSessionID)
	if err != nil {
		return err
	}
	defer unlock()
	path := s.subagentCandidatePath(candidate.ArchiveSessionID)
	var prior SubagentCandidate
	if err := local.Read(path, &prior); err == nil {
		if prior.NativeSessionID != candidate.NativeSessionID || prior.ParentArchiveSessionID != candidate.ParentArchiveSessionID || prior.ParentNativeSessionID != candidate.ParentNativeSessionID || prior.ProjectID != candidate.ProjectID || prior.ProjectRoot != candidate.ProjectRoot || !strings.EqualFold(prior.Harness.Name, candidate.Harness.Name) || prior.AgentID != candidate.AgentID || prior.TranscriptPath != candidate.TranscriptPath {
			return errors.New("subagent candidate ownership changed")
		}
		if prior.ObservedAt.After(candidate.ObservedAt) {
			candidate.ObservedAt = prior.ObservedAt
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read subagent candidate: %w", err)
	}
	return local.Write(path, candidate)
}

func (s *LocalStore) LoadSubagentCandidates() ([]SubagentCandidate, error) {
	dir := filepath.Join(s.home, "subagent-candidates")
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list subagent candidates: %w", err)
	}
	var out []SubagentCandidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var candidate SubagentCandidate
		if err := local.Read(filepath.Join(dir, entry.Name()), &candidate); err != nil {
			return nil, fmt.Errorf("read subagent candidate %q: %w", entry.Name(), err)
		}
		out = append(out, candidate)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

func (s *LocalStore) lockSubagentCandidate(id string) (func(), error) {
	if !safeFileComponent(id) {
		return nil, errors.New("invalid subagent candidate ID")
	}
	return local.NamedLockWait(s.home, filepath.Join("request-locks", "subagent-"+id+".lock"), time.Second)
}

func (s *LocalStore) RemoveSubagentCandidate(id string) error {
	unlock, err := s.lockSubagentCandidate(id)
	if err != nil {
		return err
	}
	defer unlock()
	return s.removeSubagentCandidate(id)
}

// A later stop may arrive while background transcript validation is running.
// Acknowledge only the exact observed generation, under the writer's short lock.
func (s *LocalStore) acknowledgeSubagentCandidate(expected SubagentCandidate) error {
	unlock, err := s.lockSubagentCandidate(expected.ArchiveSessionID)
	if err != nil {
		return err
	}
	defer unlock()
	var current SubagentCandidate
	if err := local.Read(s.subagentCandidatePath(expected.ArchiveSessionID), &current); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !current.ObservedAt.Equal(expected.ObservedAt) || current.TranscriptPath != expected.TranscriptPath || current.NativeSessionID != expected.NativeSessionID || current.ParentArchiveSessionID != expected.ParentArchiveSessionID {
		return nil
	}
	return s.removeSubagentCandidate(expected.ArchiveSessionID)
}

func (s *LocalStore) removeSubagentCandidate(id string) error {
	err := os.Remove(s.subagentCandidatePath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove subagent candidate: %w", err)
	}
	return nil
}

func (s *LocalStore) removeSubagentCandidatesForSession(id string) error {
	candidates, err := s.LoadSubagentCandidates()
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		if candidate.ArchiveSessionID == id || candidate.ParentArchiveSessionID == id {
			if err := s.RemoveSubagentCandidate(candidate.ArchiveSessionID); err != nil {
				return err
			}
		}
	}
	return nil
}
