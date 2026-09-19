// Package collector runs the local scan/build/publish loop that turns a
// hook-registered session into a published metadata sidecar and source
// bundle. It owns publication cadence and change detection; it builds on
// internal/local for the private home directory, atomic file I/O, and the
// machine-level lock, and does not own transcript reading (archive
// adapters), privacy filtering (archive adapters), or storage upload
// mechanics (storage.PutSourceThenMetadata). A caller runs Run under
// local.Lock(home) so only one collector process acts on a given home at a
// time; Run itself does not take that lock.
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

// LocalStore persists small operational files under a private home
// directory (see local.Home): registrations, upload requests, and a
// per-session cache of the last published source bundle, used to detect
// unchanged input without redownloading or reparsing published history. It
// never stores credentials or a second copy of conversation content beyond
// what the published source bundle itself already contains.
type LocalStore struct {
	home string
}

// NewLocalStore creates (if needed) the local store's directory layout under
// home — ordinarily the result of local.Home() — and returns a handle to it.
// home is caller-owned; this package never deletes it.
func NewLocalStore(home string) (*LocalStore, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("local store home is required")
	}
	for _, dir := range []string{"registrations", "requests", "published", "sessions"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o700); err != nil {
			return nil, fmt.Errorf("create local store directory %q: %w", dir, err)
		}
	}
	return &LocalStore{home: home}, nil
}

func safeFileComponent(value string) bool {
	if value == "" || value == "." || value == ".." || strings.ContainsAny(value, "/\\") {
		return false
	}
	return true
}

// SaveRegistration durably records a hook-observed session start. Re-saving
// the same archive session ID overwrites its prior registration.
func (s *LocalStore) SaveRegistration(reg archive.SessionRegistration) error {
	if err := reg.Validate(); err != nil {
		return err
	}
	if !safeFileComponent(reg.ArchiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	return local.Write(s.registrationPath(reg.ArchiveSessionID), reg)
}

func (s *LocalStore) registrationPath(archiveSessionID string) string {
	return filepath.Join(s.home, "registrations", archiveSessionID+".json")
}

// LoadRegistrations returns every registered session, sorted by archive
// session ID for deterministic scan order.
func (s *LocalStore) LoadRegistrations() ([]archive.SessionRegistration, error) {
	dir := filepath.Join(s.home, "registrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list registrations: %w", err)
	}
	out := make([]archive.SessionRegistration, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var reg archive.SessionRegistration
		if err := local.Read(filepath.Join(dir, entry.Name()), &reg); err != nil {
			return nil, fmt.Errorf("read registration %q: %w", entry.Name(), err)
		}
		out = append(out, reg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

// LoadRegistration returns one session's registration, if it has been saved.
func (s *LocalStore) LoadRegistration(archiveSessionID string) (archive.SessionRegistration, bool, error) {
	var reg archive.SessionRegistration
	err := local.Read(s.registrationPath(archiveSessionID), &reg)
	if errors.Is(err, os.ErrNotExist) {
		return archive.SessionRegistration{}, false, nil
	}
	if err != nil {
		return archive.SessionRegistration{}, false, fmt.Errorf("read registration %q: %w", archiveSessionID, err)
	}
	return reg, true, nil
}

// Request is a small durable marker left by a stop, failure, or end hook. It
// exists to carry hook-only supplemental evidence (a final response, model
// selection) through to the next scan; periodic scanning already covers
// every registered session regardless of whether a request is pending, so a
// request with no new evidence is a no-op beyond confirming the session was
// checked.
type Request struct {
	ArchiveSessionID string                         `json:"archive_session_id"`
	Reasons          []string                       `json:"reasons"`
	RequestedAt      time.Time                      `json:"requested_at"`
	HookEvidence     []archive.SupplementalEvidence `json:"hook_evidence,omitempty"`
}

// SaveRequest merges a new hook event into any already-pending request for
// the same session: reasons accumulate, evidence appends, and RequestedAt
// advances to the latest event. This is the coalescing the spec asks for
// when a stop and a session-end hook both fire for the same session.
func (s *LocalStore) SaveRequest(archiveSessionID, reason string, requestedAt time.Time, evidence ...archive.SupplementalEvidence) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if requestedAt.IsZero() {
		return errors.New("requested_at is required")
	}
	existing, found, err := s.loadRequest(archiveSessionID)
	if err != nil {
		return err
	}
	merged := Request{ArchiveSessionID: archiveSessionID, RequestedAt: requestedAt}
	if found {
		merged = existing
		if requestedAt.After(merged.RequestedAt) {
			merged.RequestedAt = requestedAt
		}
	}
	if reason != "" {
		have := false
		for _, r := range merged.Reasons {
			if r == reason {
				have = true
				break
			}
		}
		if !have {
			merged.Reasons = append(merged.Reasons, reason)
		}
	}
	merged.HookEvidence = append(merged.HookEvidence, evidence...)
	return local.Write(s.requestPath(archiveSessionID), merged)
}

func (s *LocalStore) requestPath(archiveSessionID string) string {
	return filepath.Join(s.home, "requests", archiveSessionID+".json")
}

func (s *LocalStore) loadRequest(archiveSessionID string) (Request, bool, error) {
	var req Request
	err := local.Read(s.requestPath(archiveSessionID), &req)
	if errors.Is(err, os.ErrNotExist) {
		return Request{}, false, nil
	}
	if err != nil {
		return Request{}, false, fmt.Errorf("read request %q: %w", archiveSessionID, err)
	}
	return req, true, nil
}

// LoadRequests returns every pending request, sorted by archive session ID.
func (s *LocalStore) LoadRequests() ([]Request, error) {
	dir := filepath.Join(s.home, "requests")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("list requests: %w", err)
	}
	out := make([]Request, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		req, found, err := s.loadRequest(id)
		if err != nil {
			return nil, err
		}
		if found {
			out = append(out, req)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ArchiveSessionID < out[j].ArchiveSessionID })
	return out, nil
}

// CompleteRequest removes a session's pending request. The collector calls
// this only after it has scanned that session in the current pass, whether
// or not that scan produced a publish.
func (s *LocalStore) CompleteRequest(archiveSessionID string) error {
	err := os.Remove(s.requestPath(archiveSessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove request %q: %w", archiveSessionID, err)
	}
	return nil
}

// publishedState is the small local cache of what was last successfully
// published for a session: the exact source bundle (so a later scan can
// detect "no meaningful change" without redownloading or reparsing
// published history) and when it was published (so the collector can
// enforce its minimum upload interval per session).
type publishedState struct {
	Bundle        archive.SourceBundle `json:"bundle"`
	PublishedAt   time.Time            `json:"published_at"`
	CandidateOnly bool                 `json:"candidate_only,omitempty"`
}

func (s *LocalStore) publishedPath(archiveSessionID string) string {
	return filepath.Join(s.home, "published", archiveSessionID+".json")
}

// SavePublished records the bundle that was just successfully published (or,
// with candidateOnly set, a bundle that was built but withheld by the
// minimum upload interval — so the next scan compares against it instead of
// rebuilding from scratch, without treating it as actually published).
func (s *LocalStore) SavePublished(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, candidateOnly bool) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	return local.Write(s.publishedPath(archiveSessionID), publishedState{Bundle: bundle, PublishedAt: publishedAt, CandidateOnly: candidateOnly})
}

// LoadPublished returns the last cached bundle for a session, if any.
func (s *LocalStore) LoadPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, candidateOnly, found bool, err error) {
	var state publishedState
	readErr := local.Read(s.publishedPath(archiveSessionID), &state)
	if errors.Is(readErr, os.ErrNotExist) {
		return archive.SourceBundle{}, time.Time{}, false, false, nil
	}
	if readErr != nil {
		return archive.SourceBundle{}, time.Time{}, false, false, fmt.Errorf("read published state %q: %w", archiveSessionID, readErr)
	}
	return state.Bundle, state.PublishedAt, state.CandidateOnly, true, nil
}

// Status summarizes the collector's local state for a future `status`
// command. It never includes transcript content.
type Status struct {
	LastScanAt      time.Time `json:"last_scan_at"`
	LastPublishedAt time.Time `json:"last_published_at,omitempty"`
	PendingCount    int       `json:"pending_count"`
	LastError       string    `json:"last_error,omitempty"`
}

func (s *LocalStore) statusPath() string { return filepath.Join(s.home, "status.json") }

// SaveStatus durably records the latest Status.
func (s *LocalStore) SaveStatus(status Status) error {
	return local.Write(s.statusPath(), status)
}

// LoadStatus returns the last saved Status, or the zero value if none exists
// yet.
func (s *LocalStore) LoadStatus() (Status, error) {
	var status Status
	err := local.Read(s.statusPath(), &status)
	if errors.Is(err, os.ErrNotExist) {
		return Status{}, nil
	}
	if err != nil {
		return Status{}, fmt.Errorf("read status: %w", err)
	}
	return status, nil
}
