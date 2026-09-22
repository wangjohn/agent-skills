package collector

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// SupersededSource is a source object that was once the current snapshot
// for a session and no longer is. It stays eligible for deletion, not
// deleted immediately, so the spec's grace period can protect a reader
// mid-download of what was, until a moment ago, the current pointer.
type SupersededSource struct {
	Key          string    `json:"key"`
	SupersededAt time.Time `json:"superseded_at"`
}

func (s *LocalStore) supersededPath(archiveSessionID string) string {
	return filepath.Join(s.home, "superseded", archiveSessionID+".json")
}

// RecordSuperseded appends key to a session's superseded-source ledger, so
// a later retention sweep can delete it once its grace period elapses. A
// session republished more than once before a sweep ever runs must not
// lose track of an earlier supersession, so each key is tracked
// individually rather than only the most recent one. If key is already
// recorded (content reverted to an earlier snapshot and was then
// superseded again), the entry moves to the end of the ledger with a fresh
// SupersededAt: append order is the supersession order retention relies
// on to identify the immediate predecessor of the current snapshot, so
// the most recently superseded key must always be last.
func (s *LocalStore) RecordSuperseded(archiveSessionID, key string, at time.Time) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	existing, err := s.LoadSuperseded(archiveSessionID)
	if err != nil {
		return err
	}
	out := make([]SupersededSource, 0, len(existing)+1)
	for _, e := range existing {
		if e.Key != key {
			out = append(out, e)
		}
	}
	out = append(out, SupersededSource{Key: key, SupersededAt: at})
	return local.Write(s.supersededPath(archiveSessionID), out)
}

// LoadSuperseded returns a session's superseded-source ledger.
func (s *LocalStore) LoadSuperseded(archiveSessionID string) ([]SupersededSource, error) {
	var out []SupersededSource
	err := local.Read(s.supersededPath(archiveSessionID), &out)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read superseded sources %q: %w", archiveSessionID, err)
	}
	return out, nil
}

// RemoveSuperseded drops one entry from a session's ledger, after its
// object has actually been deleted from storage.
func (s *LocalStore) RemoveSuperseded(archiveSessionID, key string) error {
	existing, err := s.LoadSuperseded(archiveSessionID)
	if err != nil {
		return err
	}
	out := existing[:0]
	for _, e := range existing {
		if e.Key != key {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		err := os.Remove(s.supersededPath(archiveSessionID))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove superseded ledger %q: %w", archiveSessionID, err)
		}
		return nil
	}
	return local.Write(s.supersededPath(archiveSessionID), out)
}

// ForgetSession removes every local record of a session: its registration,
// request, request lock, published-bundle cache, pending publication and
// scan markers, superseded-source ledger, and native-session index entry. A
// caller uses this only after successfully deleting that session's metadata
// and every source object from storage (whole-session retention); it never
// touches storage itself.
func (s *LocalStore) ForgetSession(archiveSessionID, nativeSessionID string) error {
	if !safeFileComponent(archiveSessionID) {
		return errors.New("archive session ID is not a safe file name component")
	}
	paths := []string{
		s.registrationPath(archiveSessionID),
		s.requestPath(archiveSessionID),
		s.publishedPath(archiveSessionID),
		s.pendingPath(archiveSessionID),
		filepath.Join(s.home, "pending-scans", archiveSessionID+".json"),
		filepath.Join(s.home, "request-locks", archiveSessionID+".lock"),
		s.supersededPath(archiveSessionID),
	}
	if nativeSessionID != "" {
		paths = append(paths, nativeSessionIndexPath(s.home, nativeSessionID))
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %q: %w", path, err)
		}
	}
	return nil
}
