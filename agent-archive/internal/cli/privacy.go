package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

type privacyInspector interface {
	InspectPrivacy(context.Context) storage.PrivacyReport
}

// Bucket privacy evidence: setup inspects the bucket once storage is
// verified, and the background collector re-inspects once the saved report is
// missing, for another storage configuration, or older than
// bucketPrivacyRefreshAfter. Status calls a report stale after
// bucketPrivacyStaleAfter; the refresh interval is shorter so a healthy,
// unpaused install is never reported stale between ticks.
const (
	bucketPrivacyRefreshAfter = 12 * time.Hour
	bucketPrivacyStaleAfter   = 24 * time.Hour
)

func privacyConfigurationID(cfg config.Config) string {
	data, _ := json.Marshal(cfg.Storage)
	return storage.SHA256Hex(data)
}
func inspectBucketPrivacy(cfg config.Config, store storage.ObjectStore, at time.Time) *storage.PrivacyReport {
	report := storage.UnknownPrivacy(cfg.Storage.Provider)
	if inspector, ok := store.(privacyInspector); ok {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		report = inspector.InspectPrivacy(ctx)
	}
	checked := at.UTC()
	report.CheckedAt = &checked
	report.ConfigurationID = privacyConfigurationID(cfg)
	return &report
}

// privacyEvidenceAge reports how old cfg's saved evidence is at the given
// time, and false when there is none for the current storage configuration
// or its check time is unusable (missing or in the future after a clock
// rollback).
func privacyEvidenceAge(cfg config.Config, at time.Time) (time.Duration, bool) {
	report := cfg.BucketPrivacy
	if report == nil || report.ConfigurationID != privacyConfigurationID(cfg) || report.CheckedAt == nil || at.Before(*report.CheckedAt) {
		return 0, false
	}
	return at.Sub(*report.CheckedAt), true
}
func bucketPrivacyNeedsRefresh(cfg config.Config, at time.Time) bool {
	age, ok := privacyEvidenceAge(cfg, at)
	return !ok || age > bucketPrivacyRefreshAfter
}
func currentBucketPrivacy(cfg config.Config, at time.Time) storage.PrivacyReport {
	report := storage.UnknownPrivacy(cfg.Storage.Provider)
	if cfg.BucketPrivacy == nil {
		return report
	}
	report = *cfg.BucketPrivacy
	if report.ConfigurationID != privacyConfigurationID(cfg) {
		report = storage.UnknownPrivacy(cfg.Storage.Provider)
		report.Reason = "storage_configuration_changed"
	} else if age, ok := privacyEvidenceAge(cfg, at); !ok || age > bucketPrivacyStaleAfter {
		report.State = "not_verified"
		report.Reason = "inspection_stale"
	}
	return report
}
func printBucketPrivacy(out io.Writer, report storage.PrivacyReport) {
	switch report.State {
	case "verified_private":
		fmt.Fprintln(out, "Bucket privacy: native public access blocked at the last check.")
	case "public_or_risky":
		fmt.Fprintln(out, "Bucket privacy: public configuration detected; review access before archiving.")
	default:
		fmt.Fprintln(out, "Bucket privacy not verified.")
	}
	checked := "never"
	if report.CheckedAt != nil {
		checked = formatTimeOrNever(*report.CheckedAt)
	}
	fmt.Fprintf(out, "  Checked: %s; %s.\n  Review: %s\n", checked, report.Reason, report.GuidanceURL)
}
