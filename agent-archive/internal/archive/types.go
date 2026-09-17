// Package archive contains the privacy-first, storage-independent foundation
// for an Agent Archive collector. It deliberately has no filesystem, network,
// CLI, or credential dependencies.
package archive

import (
	"errors"
	"path/filepath"
	"strings"
	"time"
)

const (
	SourceSchemaVersion   = 1
	MetadataSchemaVersion = 1
	FilterVersion         = "1"
)

// Config is the durable, non-secret configuration required to decide whether a
// session is eligible for capture. Credentials and runtime state do not belong
// in this type.
type Config struct {
	SchemaVersion int                 `json:"schema_version"`
	MachineID     string              `json:"machine_id"`
	Enabled       bool                `json:"enabled"`
	Projects      []ProjectActivation `json:"projects"`
}

// ProjectActivation explicitly opts a project into capture. Sessions which
// started before ActivatedAt are intentionally ineligible.
type ProjectActivation struct {
	ProjectID   string    `json:"project_id"`
	Root        string    `json:"root"`
	ActivatedAt time.Time `json:"activated_at"`
	Included    bool      `json:"included"`
}

// Eligible reports whether a project is explicitly included and the session
// began at or after its activation time. It performs lexical path matching; a
// caller is responsible for resolving symlinks before constructing Config.
func (c Config) Eligible(projectRoot string, sessionStartedAt time.Time) bool {
	if !c.Enabled || sessionStartedAt.IsZero() {
		return false
	}
	clean := filepath.Clean(projectRoot)
	for _, project := range c.Projects {
		if project.Included && filepath.Clean(project.Root) == clean && !project.ActivatedAt.IsZero() && !sessionStartedAt.Before(project.ActivatedAt) {
			return true
		}
	}
	return false
}

// Harness identifies the application which owns the native transcript.
type Harness struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
	Mode    string `json:"mode,omitempty"`
}

// SessionRegistration is the small hook-produced observation a later collector
// needs. TranscriptPath is local operational data and is never placed in a
// SourceBundle or Metadata document.
type SessionRegistration struct {
	ArchiveSessionID string    `json:"archive_session_id"`
	NativeSessionID  string    `json:"native_session_id"`
	ProjectID        string    `json:"project_id"`
	ProjectRoot      string    `json:"project_root"`
	Harness          Harness   `json:"harness"`
	TranscriptPath   string    `json:"transcript_path"`
	SessionStartedAt time.Time `json:"session_started_at"`
	RegisteredAt     time.Time `json:"registered_at"`
}

func (r SessionRegistration) Validate() error {
	if strings.TrimSpace(r.ArchiveSessionID) == "" || strings.TrimSpace(r.NativeSessionID) == "" {
		return errors.New("archive and native session IDs are required")
	}
	if strings.TrimSpace(r.ProjectID) == "" || strings.TrimSpace(r.ProjectRoot) == "" {
		return errors.New("project ID and project root are required")
	}
	if strings.TrimSpace(r.Harness.Name) == "" {
		return errors.New("harness name is required")
	}
	if r.SessionStartedAt.IsZero() {
		return errors.New("session start time is required; older sessions cannot be inferred safely")
	}
	return nil
}

// CaptureGap tells readers why source coverage is incomplete without including
// an omitted value or transcript content.
type CaptureGap struct {
	Code   string `json:"code"`
	Record int    `json:"record,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// CaptureBoundary describes retained evidence represented by a source snapshot.
// Excluded native records deliberately do not affect it or a source hash.
type CaptureBoundary struct {
	RetainedRecords int `json:"retained_records"`
	RetainedBytes   int `json:"retained_bytes"`
}

// FilteredTranscript is the only adapter output accepted by NewSourceBundle.
// Records retain their allowed native JSON shape and source ordering.
type FilteredTranscript struct {
	Format       string          `json:"format"`
	Records      [][]byte        `json:"-"`
	Boundary     CaptureBoundary `json:"boundary"`
	Gaps         []CaptureGap    `json:"gaps,omitempty"`
	FirstEventAt time.Time       `json:"-"`
}

// SupplementalEvidence is hook-only evidence. Payload must already be
// privacy-filtered by the producer; this package preserves it separately from
// native records and does not reconcile it into a transcript.
type SupplementalEvidence struct {
	Kind       string         `json:"kind"`
	ObservedAt time.Time      `json:"observed_at"`
	Provenance string         `json:"provenance"`
	Payload    map[string]any `json:"payload"`
}

// SourceCapture describes the provenance shared by all records in a snapshot.
type SourceCapture struct {
	Harness        Harness         `json:"harness"`
	AdapterName    string          `json:"adapter_name"`
	AdapterVersion string          `json:"adapter_version"`
	SourceFormat   string          `json:"source_format"`
	Boundary       CaptureBoundary `json:"boundary"`
	FilterVersion  string          `json:"filter_version"`
	CapturedAt     time.Time       `json:"captured_at"`
	Gaps           []CaptureGap    `json:"gaps,omitempty"`
}

// SourceBundle is the durable filtered source envelope. It intentionally has
// no normalized transcript or local filesystem path.
type SourceBundle struct {
	SchemaVersion        int                    `json:"schema_version"`
	ArchiveSessionID     string                 `json:"archive_session_id"`
	NativeSessionID      string                 `json:"native_session_id"`
	ProjectID            string                 `json:"project_id"`
	Capture              SourceCapture          `json:"capture"`
	NativeRecords        []map[string]any       `json:"native_records"`
	SupplementalEvidence []SupplementalEvidence `json:"supplemental_evidence,omitempty"`
}

// SourceReference is the content-addressed pointer carried by metadata.
type SourceReference struct {
	Key             string `json:"key"`
	SHA256          string `json:"sha256"`
	CompressedBytes int    `json:"compressed_bytes"`
}

// AdapterInfo records which code filtered and summarized the source.
type AdapterInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ParserInfo identifies a metadata derivation. Status is partial, failed, or
// complete only when the particular adapter has demonstrated complete coverage.
type ParserInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Status  string `json:"status"`
}

// Counts intentionally uses pointers: nil means unavailable, rather than an
// invented zero after a partial or failed parse.
type Counts struct {
	Turns            *int `json:"turns,omitempty"`
	ToolCalls        *int `json:"tool_calls,omitempty"`
	ExplicitFeedback *int `json:"explicit_feedback,omitempty"`
}

type ModelSummary struct {
	Attributes          map[string]string `json:"attributes"`
	Source              string            `json:"source"`
	ResponseModelStatus string            `json:"response_model_status"`
	TurnCount           *int              `json:"turn_count,omitempty"`
}

type SkillSnapshot struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256,omitempty"`
}

type SkillUse struct {
	Name      string `json:"name"`
	SHA256    string `json:"sha256,omitempty"`
	TurnCount *int   `json:"turn_count,omitempty"`
	Evidence  string `json:"evidence"`
}

// Metadata is the replaceable, source-first reader index. It contains no
// transcript text or tool payloads.
type Metadata struct {
	SchemaVersion     int             `json:"schema_version"`
	SessionID         string          `json:"session_id"`
	NativeSessionID   string          `json:"native_session_id"`
	MachineID         string          `json:"machine_id"`
	ProjectID         string          `json:"project_id"`
	StartedAt         time.Time       `json:"started_at"`
	CapturedAt        time.Time       `json:"captured_at"`
	MetadataDerivedAt time.Time       `json:"metadata_derived_at"`
	Harness           Harness         `json:"harness"`
	Adapter           AdapterInfo     `json:"adapter"`
	Parser            ParserInfo      `json:"parser"`
	FilterVersion     string          `json:"filter_version"`
	State             string          `json:"state"`
	Models            []ModelSummary  `json:"models,omitempty"`
	SkillsAvailable   []SkillSnapshot `json:"skills_available,omitempty"`
	SkillsUsed        []SkillUse      `json:"skills_used,omitempty"`
	SkillDetection    string          `json:"skill_detection"`
	Counts            Counts          `json:"counts"`
	CaptureGaps       []CaptureGap    `json:"capture_gaps,omitempty"`
	SourceBundle      SourceReference `json:"source_bundle"`
}
