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
	report.CheckedAt = at.UTC()
	report.ConfigurationID = privacyConfigurationID(cfg)
	return &report
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
	} else if report.CheckedAt.IsZero() || at.Before(report.CheckedAt) || at.Sub(report.CheckedAt) > 24*time.Hour {
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
	fmt.Fprintf(out, "  Checked: %s; %s.\n  Review: %s\n", formatTimeOrNever(report.CheckedAt), report.Reason, report.GuidanceURL)
}
