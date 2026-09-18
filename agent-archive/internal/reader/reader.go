// Package reader lists private metadata and reads selected source bundles into
// memory. It intentionally creates no normalized persistence or local cache.
package reader

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

var ErrRefreshRequired = errors.New("source changed or was deleted; refresh metadata and retry")

// SkillUsage narrows a Filter's Skill/SkillSHA256 match to a specific
// relationship between a session and the named skill. The zero value means
// "the skill was used".
type SkillUsage string

const (
	// SkillUsageUsed is the default (zero-value) behavior: match sessions
	// that actually used the skill.
	SkillUsageUsed SkillUsage = "used"
	// SkillUsageAvailable matches sessions where the skill was available
	// (eligible or discovered coverage), regardless of whether it was used.
	SkillUsageAvailable SkillUsage = "available"
	// SkillUsageEligibleNoUse matches sessions where the skill was eligible
	// but never used. It requires the metadata's SkillDetection to be
	// archive.SkillDetectionObservedNone; anything else (including
	// archive.SkillDetectionUnavailable) is treated as unknown, not "no use".
	SkillUsageEligibleNoUse SkillUsage = "eligible_no_use"
)

type Filter struct {
	Harness, Model, Skill, SkillSHA256 string
	From, To                           time.Time
	RequireCompleteCoverage            bool
	SkillUsage                         SkillUsage
}
type Limits struct{ MaxCompressedBytes, MaxUncompressedBytes int }

func (l Limits) compressed() int {
	if l.MaxCompressedBytes > 0 {
		return l.MaxCompressedBytes
	}
	return 32 << 20
}
func (l Limits) uncompressed() int {
	if l.MaxUncompressedBytes > 0 {
		return l.MaxUncompressedBytes
	}
	return 128 << 20
}

// ListMetadata reads only metadata sidecars and applies filters without
// downloading transcript bundles.
func ListMetadata(ctx context.Context, store storage.ObjectStore, prefix string, filter Filter) ([]archive.Metadata, error) {
	objects, err := store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	var results []archive.Metadata
	for _, object := range objects {
		if !strings.HasSuffix(object.Key, "/metadata.json") {
			continue
		}
		data, err := store.Get(ctx, object.Key)
		if err != nil {
			return nil, fmt.Errorf("read metadata %q: %w", object.Key, err)
		}
		var metadata archive.Metadata
		if err := json.Unmarshal(data, &metadata); err != nil {
			return nil, fmt.Errorf("decode metadata %q: %w", object.Key, err)
		}
		if err := metadata.ValidateSourceReference(); err != nil {
			return nil, fmt.Errorf("invalid metadata %q: %w", object.Key, err)
		}
		if matches(metadata, filter) {
			results = append(results, metadata)
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].CapturedAt.After(results[j].CapturedAt) })
	return results, nil
}

func matches(m archive.Metadata, f Filter) bool {
	if f.Harness != "" && m.Harness.Name != f.Harness {
		return false
	}
	if !f.From.IsZero() && m.CapturedAt.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && m.CapturedAt.After(f.To) {
		return false
	}
	if f.RequireCompleteCoverage && (m.Parser.Status != archive.ParserStatusComplete || len(m.CaptureGaps) != 0) {
		return false
	}
	if f.Model != "" {
		found := false
		for _, x := range m.Models {
			if x.Attributes["gen_ai.request.model"] == f.Model || x.Attributes["gen_ai.response.model"] == f.Model {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if f.Skill != "" || f.SkillSHA256 != "" {
		used, available := false, false
		for _, x := range m.SkillsUsed {
			if (f.Skill == "" || x.Name == f.Skill) && (f.SkillSHA256 == "" || x.SHA256 == f.SkillSHA256) {
				used = true
			}
		}
		for _, x := range m.SkillsAvailable {
			if (f.Skill == "" || x.Name == f.Skill) && (f.SkillSHA256 == "" || x.SHA256 == f.SkillSHA256) {
				if x.Coverage == archive.SkillCoverageEligible || x.Coverage == archive.SkillCoverageDiscovered {
					available = true
				}
			}
		}
		eligibleNoUse := available && !used && m.SkillDetection == archive.SkillDetectionObservedNone
		switch f.SkillUsage {
		case SkillUsageAvailable:
			if !available {
				return false
			}
		case SkillUsageEligibleNoUse:
			if !eligibleNoUse {
				return false
			}
		default: // SkillUsageUsed, or the zero value
			if !used {
				return false
			}
		}
	}
	return true
}

// LoadSource verifies the compressed SHA-256 before bounded decompression and
// validates that source identity matches the selected metadata pointer.
func LoadSource(ctx context.Context, store storage.ObjectStore, metadata archive.Metadata, limits Limits) (archive.SourceBundle, error) {
	if err := metadata.ValidateSourceReference(); err != nil {
		return archive.SourceBundle{}, err
	}
	data, err := store.Get(ctx, metadata.SourceBundle.Key)
	if err != nil {
		if errors.Is(err, storage.ErrNotFound) {
			return archive.SourceBundle{}, ErrRefreshRequired
		}
		return archive.SourceBundle{}, err
	}
	if len(data) > limits.compressed() {
		return archive.SourceBundle{}, errors.New("source exceeds compressed read limit")
	}
	if len(data) != metadata.SourceBundle.CompressedBytes {
		return archive.SourceBundle{}, errors.New("source compressed size does not match metadata")
	}
	if !storage.VerifySHA256(data, metadata.SourceBundle.SHA256) {
		return archive.SourceBundle{}, errors.New("source checksum mismatch")
	}
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return archive.SourceBundle{}, fmt.Errorf("open source gzip: %w", err)
	}
	defer gz.Close()
	plain, err := io.ReadAll(io.LimitReader(gz, int64(limits.uncompressed()+1)))
	if err != nil {
		return archive.SourceBundle{}, err
	}
	if len(plain) > limits.uncompressed() {
		return archive.SourceBundle{}, errors.New("source exceeds uncompressed read limit")
	}
	var bundle archive.SourceBundle
	if err := json.Unmarshal(plain, &bundle); err != nil {
		return archive.SourceBundle{}, fmt.Errorf("decode source: %w", err)
	}
	if bundle.SchemaVersion != archive.SourceSchemaVersion || bundle.ArchiveSessionID != metadata.SessionID || bundle.NativeSessionID != metadata.NativeSessionID || bundle.ProjectID != metadata.ProjectID || bundle.Capture.Harness != metadata.Harness || !bundle.Capture.CapturedAt.Equal(metadata.CapturedAt) || bundle.Capture.FilterVersion != metadata.FilterVersion {
		return archive.SourceBundle{}, errors.New("source identity does not match metadata")
	}
	key, err := archive.SourceObjectKey(bundle, metadata.SourceBundle.SHA256)
	if err != nil || key != metadata.SourceBundle.Key {
		return archive.SourceBundle{}, errors.New("source key does not match metadata identity")
	}
	return bundle, nil
}

// RefreshAndLoad retries once after rereading metadata, covering the normal
// metadata-pointer refresh race after old source cleanup.
func RefreshAndLoad(ctx context.Context, store storage.ObjectStore, metadataKey string, limits Limits) (archive.Metadata, archive.SourceBundle, error) {
	for attempt := 0; attempt < 2; attempt++ {
		data, err := store.Get(ctx, metadataKey)
		if err != nil {
			return archive.Metadata{}, archive.SourceBundle{}, err
		}
		var m archive.Metadata
		if err = json.Unmarshal(data, &m); err != nil {
			return archive.Metadata{}, archive.SourceBundle{}, err
		}
		b, err := LoadSource(ctx, store, m, limits)
		if !errors.Is(err, ErrRefreshRequired) || attempt == 1 {
			return m, b, err
		}
	}
	return archive.Metadata{}, archive.SourceBundle{}, ErrRefreshRequired
}
