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
func OpenLocalStoreReadOnly(home string) *LocalStore { return &LocalStore{home: home} }

func NewLocalStore(home string) (*LocalStore, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("local store home is required")
	}
	for _, dir := range []string{"registrations", "requests", "request-locks", "published", "pending", "sessions", "pending-scans"} {
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
	if os.IsNotExist(err) {
		return nil, nil
	}
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
	Token            string                         `json:"token"`
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
	unlock, err := local.NamedLockWait(s.home, filepath.Join("request-locks", archiveSessionID+".lock"), time.Second)
	if err != nil {
		return fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
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
	merged.Token, err = local.ID()
	if err != nil {
		return fmt.Errorf("generate request token: %w", err)
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

// ensureRequestToken upgrades a request written by an older collector. The
// token is assigned under the same lock used by hooks and acknowledgements so
// migration cannot overwrite a concurrent hook update.
func (s *LocalStore) ensureRequestToken(archiveSessionID string) (Request, error) {
	unlock, err := local.NamedLockWait(s.home, filepath.Join("request-locks", archiveSessionID+".lock"), time.Second)
	if err != nil {
		return Request{}, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
	request, found, err := s.loadRequest(archiveSessionID)
	if err != nil || !found || request.Token != "" {
		return request, err
	}
	request.Token, err = local.ID()
	if err != nil {
		return Request{}, fmt.Errorf("generate request token: %w", err)
	}
	if err := local.Write(s.requestPath(archiveSessionID), request); err != nil {
		return Request{}, fmt.Errorf("upgrade request %q: %w", archiveSessionID, err)
	}
	return request, nil
}

// LoadRequests returns every pending request, sorted by archive session ID.
func (s *LocalStore) LoadRequests() ([]Request, error) {
	dir := filepath.Join(s.home, "requests")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
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

// CompleteRequest removes a request only if it still has the random token
// covered by a durable publish or policy decision. A hook that arrives while
// a scan is in progress assigns a new token under the same lock and therefore
// remains pending for the next pass. Tokens do not repeat when a request file
// was removed between events, unlike a per-file revision counter.
func (s *LocalStore) CompleteRequest(archiveSessionID, coveredToken string) (bool, error) {
	if !safeFileComponent(archiveSessionID) {
		return false, errors.New("archive session ID is not a safe file name component")
	}
	unlock, err := local.NamedLockWait(s.home, filepath.Join("request-locks", archiveSessionID+".lock"), time.Second)
	if err != nil {
		return false, fmt.Errorf("lock request %q: %w", archiveSessionID, err)
	}
	defer unlock()
	current, found, err := s.loadRequest(archiveSessionID)
	if err != nil || !found {
		return false, err
	}
	if coveredToken == "" || current.Token != coveredToken {
		return false, nil
	}
	err = os.Remove(s.requestPath(archiveSessionID))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("remove request %q: %w", archiveSessionID, err)
	}
	return true, nil
}

// CacheStatus distinguishes why a bundle sits in the local published cache,
// since only some of those reasons should be auto-retried once time passes.
type CacheStatus string

const (
	// CacheStatusPublished means this exact bundle was actually published.
	CacheStatusPublished CacheStatus = "published"
	// CacheStatusRateLimited means this bundle was built and differs from
	// what's published, but was withheld by the minimum upload interval; it
	// is eligible to auto-publish once that interval elapses.
	CacheStatusRateLimited CacheStatus = "rate_limited"
	// CacheStatusDeclined means this bundle was deliberately not published
	// by policy (see Options.RequireSkillUse), not by cadence. Unlike
	// CacheStatusRateLimited, it must never auto-publish just because time
	// passed — only a genuine further content change reconsiders it.
	CacheStatusDeclined CacheStatus = "declined"
)

// publishedState is the small local cache of what was last built for a
// session: the exact source bundle (so a later scan can detect "no
// meaningful change" without redownloading or reparsing published history),
// when that happened, and why the bundle is in the state it's in.
type publishedState struct {
	MetadataBytes []byte               `json:"metadata_bytes,omitempty"`
	Bundle        archive.SourceBundle `json:"bundle"`
	PublishedAt   time.Time            `json:"published_at"`
	Status        CacheStatus          `json:"status"`
	// LastPublished survives a newer rate-limited or declined candidate so
	// compaction checks and retention always have the actual remote baseline.
	LastPublished *publishedSnapshot `json:"last_published,omitempty"`
}

type publishedSnapshot struct {
	Bundle      archive.SourceBundle `json:"bundle"`
	PublishedAt time.Time            `json:"published_at"`
}

func (s *LocalStore) publishedPath(archiveSessionID string) string {
	return filepath.Join(s.home, "published", archiveSessionID+".json")
}

// SavePublished records the outcome of a build/publish decision for a
// session, so the next scan can compare against it instead of rebuilding
// from scratch. See CacheStatus for what each status means for retry.
func (s *LocalStore) SavePublished(archiveSessionID string, bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, metadata ...[]byte) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	var last *publishedSnapshot
	var existing publishedState
	if err := local.Read(s.publishedPath(archiveSessionID), &existing); err == nil {
		last = existing.LastPublished
		if last == nil && existing.Status == CacheStatusPublished {
			copy := publishedSnapshot{Bundle: existing.Bundle, PublishedAt: existing.PublishedAt}
			last = &copy
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read published state %q: %w", archiveSessionID, err)
	}
	if status == CacheStatusPublished {
		last = &publishedSnapshot{Bundle: bundle, PublishedAt: publishedAt}
	}
	return local.Write(s.publishedPath(archiveSessionID), publishedState{Bundle: bundle, PublishedAt: publishedAt, Status: status, LastPublished: last, MetadataBytes: publicationMetadata(existing.MetadataBytes, metadata)})
}

// LoadPublished returns the last cached bundle for a session, if any.
func (s *LocalStore) LoadPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, status CacheStatus, found bool, err error) {
	var state publishedState
	readErr := local.Read(s.publishedPath(archiveSessionID), &state)
	if errors.Is(readErr, os.ErrNotExist) {
		return archive.SourceBundle{}, time.Time{}, "", false, nil
	}
	if readErr != nil {
		return archive.SourceBundle{}, time.Time{}, "", false, fmt.Errorf("read published state %q: %w", archiveSessionID, readErr)
	}
	return state.Bundle, state.PublishedAt, state.Status, true, nil
}

// LoadLastPublished returns the most recent bundle actually made discoverable
// by remote metadata. It deliberately ignores a newer local-only candidate.
func (s *LocalStore) LoadLastPublished(archiveSessionID string) (bundle archive.SourceBundle, publishedAt time.Time, found bool, err error) {
	var state publishedState
	readErr := local.Read(s.publishedPath(archiveSessionID), &state)
	if errors.Is(readErr, os.ErrNotExist) {
		return archive.SourceBundle{}, time.Time{}, false, nil
	}
	if readErr != nil {
		return archive.SourceBundle{}, time.Time{}, false, fmt.Errorf("read published state %q: %w", archiveSessionID, readErr)
	}
	if state.LastPublished != nil {
		return state.LastPublished.Bundle, state.LastPublished.PublishedAt, true, nil
	}
	// Backward compatibility with state written before the separate ledger.
	if state.Status == CacheStatusPublished {
		return state.Bundle, state.PublishedAt, true, nil
	}
	return archive.SourceBundle{}, time.Time{}, false, nil
}

// PendingPublication is one fully rendered publication transaction. Source
// and metadata bytes are persisted together before the first remote write, so
// every retry uses the same hash and timestamps even after process restart.
// Bundle remains available for change detection and future parser-only rebuilds.
type PendingPublication struct {
	Bundle        archive.SourceBundle `json:"bundle"`
	SourceKey     string               `json:"source_key"`
	MetadataKey   string               `json:"metadata_key"`
	SourceSHA256  string               `json:"source_sha256"`
	SourceBytes   []byte               `json:"source_bytes"`
	MetadataBytes []byte               `json:"metadata_bytes"`
	RequestToken  string               `json:"request_token,omitempty"`
	ReadyAt       time.Time            `json:"ready_at"`
	Attempted     bool                 `json:"attempted,omitempty"`
}

func (s *LocalStore) pendingPath(id string) string {
	return filepath.Join(s.home, "pending", id+".json")
}

func (s *LocalStore) SavePending(id string, pending PendingPublication) error {
	if !safeFileComponent(id) {
		return errors.New("archive session ID is not a safe file name component")
	}
	if pending.SourceKey == "" || pending.MetadataKey == "" || pending.SourceSHA256 == "" || len(pending.SourceBytes) == 0 || len(pending.MetadataBytes) == 0 {
		return errors.New("pending publication is incomplete")
	}
	return local.Write(s.pendingPath(id), pending)
}

func (s *LocalStore) LoadPending(id string) (PendingPublication, bool, error) {
	var pending PendingPublication
	err := local.Read(s.pendingPath(id), &pending)
	if errors.Is(err, os.ErrNotExist) {
		return PendingPublication{}, false, nil
	}
	if err != nil {
		return PendingPublication{}, false, fmt.Errorf("read pending publication %q: %w", id, err)
	}
	return pending, true, nil
}

func (s *LocalStore) RemovePending(id string) error {
	err := os.Remove(s.pendingPath(id))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove pending publication %q: %w", id, err)
	}
	return nil
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

// SetScanPending journals work before scanning. A failed or interrupted update
// remains pending even when the previous published cache is still valid.
func (s *LocalStore) SetScanPending(id string, pending bool) error {
	if !safeFileComponent(id) {
		return errors.New("invalid session ID")
	}
	path := filepath.Join(s.home, "pending-scans", id+".json")
	if pending {
		return local.Write(path, true)
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *LocalStore) ScanPending(id string) (bool, error) {
	if !safeFileComponent(id) {
		return false, errors.New("invalid session ID")
	}
	var pending bool
	err := local.Read(filepath.Join(s.home, "pending-scans", id+".json"), &pending)
	if os.IsNotExist(err) {
		return false, nil
	}
	return pending, err
}

func publicationMetadata(previous []byte, supplied [][]byte) []byte {
	if len(supplied) > 0 {
		return supplied[0]
	}
	return previous
}
