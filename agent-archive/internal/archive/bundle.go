package archive

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// CompressedSource is the exact immutable byte sequence to persist and retry.
// SHA256 is over Bytes, never an object-store ETag.
type CompressedSource struct {
	Bytes  []byte
	SHA256 string
}

// NewSourceBundle converts only adapter-filtered records into the durable
// envelope. A caller cannot accidentally add a raw transcript through this API.
func NewSourceBundle(reg SessionRegistration, adapter Adapter, transcript FilteredTranscript, capturedAt time.Time, supplemental []SupplementalEvidence) (SourceBundle, error) {
	if err := reg.Validate(); err != nil {
		return SourceBundle{}, err
	}
	if adapter == nil {
		return SourceBundle{}, errors.New("adapter is required")
	}
	if capturedAt.IsZero() {
		return SourceBundle{}, errors.New("captured_at is required and must be reused for retries")
	}
	if transcript.Format == "" {
		return SourceBundle{}, errors.New("filtered transcript format is required")
	}
	records := make([]map[string]any, 0, len(transcript.Records))
	for index, raw := range transcript.Records {
		var record map[string]any
		if err := json.Unmarshal(raw, &record); err != nil {
			return SourceBundle{}, fmt.Errorf("filtered record %d is invalid: %w", index, err)
		}
		if len(record) == 0 {
			return SourceBundle{}, fmt.Errorf("filtered record %d is empty", index)
		}
		records = append(records, record)
	}
	harness := observedHarness(reg.Harness, records)
	filteredSupplemental, gaps, err := FilterSupplementalEvidence(supplemental)
	if err != nil {
		return SourceBundle{}, err
	}
	allGaps := append(append([]CaptureGap(nil), transcript.Gaps...), gaps...)
	return SourceBundle{
		SchemaVersion:    SourceSchemaVersion,
		ArchiveSessionID: reg.ArchiveSessionID,
		NativeSessionID:  reg.NativeSessionID,
		ProjectID:        reg.ProjectID,
		Capture: SourceCapture{
			Harness: harness, AdapterName: adapter.Name(), AdapterVersion: adapter.Version(),
			SourceFormat: transcript.Format, Boundary: transcript.Boundary,
			FilterVersion: FilterVersion, CapturedAt: capturedAt.UTC(), Gaps: allGaps,
		},
		NativeRecords: records, SupplementalEvidence: filteredSupplemental,
	}, nil
}

func observedHarness(base Harness, records []map[string]any) Harness {
	for _, record := range records {
		if firstString(record, "type") != "session_meta" {
			continue
		}
		if version := firstStringDeep(record, "cli_version"); version != "" {
			base.Version = version
		}
		if mode := firstStringDeep(record, "source"); mode != "" {
			base.Mode = mode
		}
	}
	return base
}

// FilterSupplementalEvidence applies the same strict allowlist and secret
// redaction policy used for native records. Hooks must call it (or use
// NewSourceBundle, which calls it) before persisting upload-ready evidence.
func FilterSupplementalEvidence(in []SupplementalEvidence) ([]SupplementalEvidence, []CaptureGap, error) {
	out := make([]SupplementalEvidence, 0, len(in))
	var gaps []CaptureGap
	for _, evidence := range in {
		if strings.TrimSpace(evidence.Kind) == "" || strings.TrimSpace(evidence.Provenance) == "" || evidence.ObservedAt.IsZero() {
			return nil, nil, errors.New("supplemental evidence requires kind, provenance, and observation time")
		}
		state := sanitizeState{addGap: func(code string, _ int, detail string) {
			gaps = append(gaps, CaptureGap{Code: code, Detail: "supplemental " + detail})
		}}
		payload, keep := sanitizeObject(evidence.Payload, &state)
		if !keep {
			gaps = append(gaps, CaptureGap{Code: "supplemental_evidence_omitted", Detail: "no allowed fields"})
			continue
		}
		if evidence.Kind == "final_response" && firstString(payload, "agent_id") != "" {
			gaps = append(gaps, CaptureGap{Code: "subagent_final_not_reconciled", Detail: "separate subagent source required"})
		}
		out = append(out, SupplementalEvidence{Kind: evidence.Kind, ObservedAt: evidence.ObservedAt.UTC(), Provenance: evidence.Provenance, Payload: payload})
	}
	return out, gaps, nil
}

// BuildCompressedSource serializes a bundle canonically and uses gzip headers
// that are independent of the wall clock and host platform.
func BuildCompressedSource(bundle SourceBundle) (CompressedSource, error) {
	if err := validateBundle(bundle); err != nil {
		return CompressedSource{}, err
	}
	plain, err := json.Marshal(bundle)
	if err != nil {
		return CompressedSource{}, fmt.Errorf("marshal source bundle: %w", err)
	}
	var output bytes.Buffer
	writer, err := gzip.NewWriterLevel(&output, gzip.DefaultCompression)
	if err != nil {
		return CompressedSource{}, err
	}
	// A non-zero epoch avoids gzip's special "unknown time" representation
	// while remaining independent of capture and wall-clock time.
	writer.Header.ModTime = time.Unix(1, 0).UTC()
	writer.Header.OS = 255
	if _, err = writer.Write(plain); err != nil {
		return CompressedSource{}, err
	}
	if err = writer.Close(); err != nil {
		return CompressedSource{}, err
	}
	bytes := output.Bytes()
	digest := sha256.Sum256(bytes)
	return CompressedSource{Bytes: append([]byte(nil), bytes...), SHA256: hex.EncodeToString(digest[:])}, nil
}

func validateBundle(bundle SourceBundle) error {
	if bundle.SchemaVersion != SourceSchemaVersion {
		return fmt.Errorf("unsupported source schema version %d", bundle.SchemaVersion)
	}
	if strings.TrimSpace(bundle.ArchiveSessionID) == "" || strings.TrimSpace(bundle.NativeSessionID) == "" || strings.TrimSpace(bundle.ProjectID) == "" {
		return errors.New("source bundle IDs are required")
	}
	if strings.TrimSpace(bundle.Capture.AdapterName) == "" || bundle.Capture.CapturedAt.IsZero() {
		return errors.New("source capture provenance is required")
	}
	return nil
}

// SourceObjectKey returns the provider-relative content-addressed object key.
func SourceObjectKey(bundle SourceBundle, sha256 string) (string, error) {
	if err := validateBundle(bundle); err != nil {
		return "", err
	}
	if !safeObjectComponent(bundle.Capture.Harness.Name) || !safeObjectComponent(bundle.ArchiveSessionID) {
		return "", errors.New("harness name and archive session ID must be safe object-key components")
	}
	if !isLowerHexSHA256(sha256) {
		return "", errors.New("source sha256 must be 64 lowercase hexadecimal characters")
	}
	return fmt.Sprintf("sessions/%s/%s/source.%s.json.gz", bundle.Capture.Harness.Name, bundle.ArchiveSessionID, sha256), nil
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if !(character >= '0' && character <= '9') && !(character >= 'a' && character <= 'f') {
			return false
		}
	}
	return true
}

func safeObjectComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
