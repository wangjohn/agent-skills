package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/smithy-go"
	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/reader"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

type verificationEvidence struct {
	ConfigurationID string    `json:"configuration_id"`
	PublishedAt     time.Time `json:"published_at"`
	VerifiedAt      time.Time `json:"verified_at"`
	SourceSHA256    string    `json:"source_sha256"`
}
type storageHealth struct {
	ConfigurationID string    `json:"configuration_id"`
	State           string    `json:"state"`
	CheckedAt       time.Time `json:"checked_at"`
	Context         string    `json:"context"`
}

func configurationID(cfg config.Config) string {
	// These fields contain references, never credentials. Pausing, retention,
	// and unrelated historical settings do not invalidate capture verification.
	data, _ := json.Marshal(struct {
		Storage            any
		Harnesses          []string
		Machine            string
		Since              time.Time
		CapabilityContract int
	}{cfg.Storage, cfg.Harnesses, cfg.MachineID, cfg.DestinationSince, 1})
	return storage.SHA256Hex(data)
}

func sessionVerificationConfigurationID(cfg config.Config, reg archive.SessionRegistration) string {
	var activation time.Time
	for _, project := range cfg.Archive.Projects {
		if project.Included && project.Root == reg.ProjectRoot {
			activation = project.ActivatedAt
			break
		}
	}
	data, _ := json.Marshal(struct {
		Storage            any
		Machine            string
		DestinationSince   time.Time
		Harness            string
		ProjectRoot        string
		ProjectActivatedAt time.Time
		CapabilityContract int
	}{cfg.Storage, cfg.MachineID, cfg.DestinationSince, reg.Harness.Name, reg.ProjectRoot, activation, 1})
	return storage.SHA256Hex(data)
}
func verificationPath(home, id string) string {
	return filepath.Join(home, "sessions", id, "verification.json")
}
func readVerification(home, id string) (verificationEvidence, error) {
	var v verificationEvidence
	err := local.Read(verificationPath(home, id), &v)
	if os.IsNotExist(err) {
		return v, nil
	}
	return v, err
}
func recordStorageHealth(home string, cfg config.Config, env Env, background bool, state string) error {
	contextName := "manual_sync"
	if background {
		contextName = "background_collector"
	}
	return local.Write(filepath.Join(home, "storage-health.json"), storageHealth{configurationID(cfg), state, env.now().UTC(), contextName})
}
func storageFailureState(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "ExpiredToken", "ExpiredTokenException", "RequestExpired":
			return "credentials_expired"
		case "AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch", "Unauthorized":
			return "authentication_failed"
		}
	}
	return "storage_unavailable"
}

// Verify only new publications or evidence invalidated by configuration changes.
// Status reads this local result; it never downloads conversation content.
func verifyPublications(home string, cfg config.Config, env Env, store *collector.LocalStore, remote storage.ObjectStore, result *collector.Result) error {
	regs, err := store.LoadRegistrations()
	if err != nil {
		return err
	}
	for _, reg := range regs {
		if !cfg.AcceptSession(reg) {
			continue
		}
		bundle, at, _, err := store.LoadLastPublished(reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		if at.IsZero() {
			continue
		}
		old, err := readVerification(home, reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		verificationConfigurationID := sessionVerificationConfigurationID(cfg, reg)
		if old.ConfigurationID == verificationConfigurationID && old.PublishedAt.Equal(at) && !old.VerifiedAt.IsZero() {
			continue
		}
		key, err := archive.MetadataObjectKey(reg.Harness.Name, reg.ArchiveSessionID)
		if err != nil {
			return err
		}
		metadata, err := reader.ReadMetadata(context.Background(), remote, key)
		expected, buildErr := archive.BuildCompressedSource(bundle)
		if buildErr != nil {
			return buildErr
		}
		if err == nil && metadata.SourceBundle.SHA256 != expected.SHA256 {
			err = fmt.Errorf("remote metadata does not identify the last local publication")
		}
		if err == nil && metadata.MachineID != cfg.MachineID {
			err = fmt.Errorf("metadata ownership does not match this machine")
		}
		if err == nil {
			_, err = reader.LoadSource(context.Background(), remote, metadata, reader.Limits{})
		}
		if err == nil {
			err = local.Write(verificationPath(home, reg.ArchiveSessionID), verificationEvidence{verificationConfigurationID, at, env.now().UTC(), metadata.SourceBundle.SHA256})
		}
		if err != nil {
			if result.Errors == nil {
				result.Errors = map[string]error{}
			}
			result.Errors[reg.ArchiveSessionID] = fmt.Errorf("read-back verification: %w", err)
		}
	}
	return nil
}
