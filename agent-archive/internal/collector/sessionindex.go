package collector

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// A native session ID is harness-controlled input and must never be used
// directly as a file name component (it could contain path separators or
// arbitrary bytes); its hash is a safe, stable index key instead.
func nativeSessionIndexPath(home, nativeSessionID string) string {
	sum := sha256.Sum256([]byte(nativeSessionID))
	return filepath.Join(home, "sessions", hex.EncodeToString(sum[:])+".json")
}

type sessionIndexEntry struct {
	ArchiveSessionID string `json:"archive_session_id"`
}

// ArchiveSessionID returns the persistent archive session ID previously
// associated with a native session ID, if one has been recorded.
func (s *LocalStore) ArchiveSessionID(nativeSessionID string) (string, bool, error) {
	if strings.TrimSpace(nativeSessionID) == "" {
		return "", false, errors.New("native session ID is required")
	}
	var entry sessionIndexEntry
	err := local.Read(nativeSessionIndexPath(s.home, nativeSessionID), &entry)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read session index: %w", err)
	}
	return entry.ArchiveSessionID, true, nil
}

// EnsureArchiveSessionID finds the archive session ID already associated
// with nativeSessionID, or creates and durably records a fresh random one.
// It is the "creates or finds a persistent random archive session ID" step
// the spec assigns to a start/resume hook.
func (s *LocalStore) EnsureArchiveSessionID(nativeSessionID string) (id string, created bool, err error) {
	existing, found, err := s.ArchiveSessionID(nativeSessionID)
	if err != nil {
		return "", false, err
	}
	if found {
		return existing, false, nil
	}
	fresh, err := local.ID()
	if err != nil {
		return "", false, fmt.Errorf("generate archive session ID: %w", err)
	}
	if err := local.Write(nativeSessionIndexPath(s.home, nativeSessionID), sessionIndexEntry{ArchiveSessionID: fresh}); err != nil {
		return "", false, fmt.Errorf("write session index: %w", err)
	}
	return fresh, true, nil
}
