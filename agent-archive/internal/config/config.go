// Package config is the single local, durable record of how this machine's
// agent-archive is set up: which storage destination it publishes to, which
// projects are included, and whether collection is currently paused. It is
// the one file `agent-archive setup` writes and every other command reads.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// SchemaVersion is bumped only when Config's on-disk shape changes
// incompatibly.
const SchemaVersion = 1

// Config is this machine's complete archive configuration. It contains no
// secrets: R2 secrets live in Keychain (see credentials.Config.R2CredentialRef)
// and S3 credentials are resolved through the named AWS profile.
type Config struct {
	RetiredCredentialRefs []string             `json:"retired_credential_refs,omitempty"`
	StorageVerifiedAt     time.Time            `json:"storage_verified_at,omitempty"`
	DestinationSince      time.Time            `json:"destination_since,omitempty"`
	PreviousDestinations  []credentials.Config `json:"previous_destinations,omitempty"`

	SchemaVersion int                `json:"schema_version"`
	MachineID     string             `json:"machine_id"`
	Storage       credentials.Config `json:"storage"`
	Archive       archive.Config     `json:"archive"`
	// Paused persistently suspends collection, uploads, and remote cleanup
	// without deleting data or existing configuration.
	Paused bool `json:"paused"`
	// Harnesses lists which applications setup installed hooks for
	// (values match archive.Harness.Name: "codex", "claude", "cursor").
	Harnesses []string `json:"harnesses,omitempty"`
	// RequireSkillUse opts out of the spec's default (capture sessions with
	// no detected skill use too, to preserve comparison evidence). The zero
	// value (false) matches that default, so a config that predates this
	// field, or one built without setting it, behaves correctly rather than
	// silently declining everything.
	RequireSkillUse bool `json:"require_skill_use"`
	// RetentionDays is whole-session retention, proposed as 90 by setup.
	// Enforcing it is the Retention slice's job, not this package's.
	RetentionDays int `json:"retention_days"`
}

func path(home string) string { return filepath.Join(home, "config.json") }

// Load reads this machine's configuration. found is false, with a nil error,
// when setup has never run.
func Load(home string) (cfg Config, found bool, err error) {
	err = local.Read(path(home), &cfg)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, false, nil
	}
	if err != nil {
		return Config{}, false, err
	}
	return cfg, true, nil
}

// Save durably writes cfg, replacing any prior configuration atomically.
func Save(home string, cfg Config) error {
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = SchemaVersion
	}
	return local.Write(path(home), cfg)
}

// SetPaused updates only the Paused flag, preserving the rest of an existing
// configuration. It fails if setup has not run yet: pausing before there is
// anything to pause is not a meaningful state.
func SetPaused(home string, paused bool) (Config, error) {
	cfg, found, err := Load(home)
	if err != nil {
		return Config{}, err
	}
	if !found {
		return Config{}, errors.New("not set up yet; run `agent-archive setup` first")
	}
	cfg.Paused = paused
	if err := Save(home, cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// AcceptSession prevents excluded apps/projects and previous destinations from
// continuing to publish or delete sessions after reconfiguration.
func (c Config) AcceptSession(r archive.SessionRegistration) bool {
	if !c.DestinationSince.IsZero() && r.SessionStartedAt.Before(c.DestinationSince) {
		return false
	}
	if len(c.Harnesses) > 0 {
		found := false
		for _, h := range c.Harnesses {
			if h == r.Harness.Name {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	for _, p := range c.Archive.Projects {
		if p.Included && p.Root == r.ProjectRoot {
			return true
		}
	}
	return len(c.Archive.Projects) == 0 // older programmatic configurations
}
