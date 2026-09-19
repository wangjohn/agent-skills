package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

const defaultPrefix = "agent-archive/"
const defaultRetentionDays = 90

var allHarnesses = []string{"codex", "claude", "cursor"}

// runSetupCommand implements the guided flow from the spec's "Setup on
// each Mac" section: connect a bucket, verify access, select applications
// and projects, review a summary, then install hooks and the LaunchAgent.
// Nothing is written until the final confirmation; on cancellation or any
// failure before that point, a prior configuration (if reconfiguring) is
// left exactly as it was.
func runSetupCommand(_ []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	ctx := context.Background()
	home, err := env.home()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: resolve home: %v\n", err)
		return 1
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: resolve user home: %v\n", err)
		return 1
	}
	executable, err := env.executable()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: resolve executable path: %v\n", err)
		return 1
	}
	now := env.now()
	existing, _, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: load existing config: %v\n", err)
		return 1
	}

	p := newPrompter(stdin, stdout)

	storageCfg, r2Secret, saveSecret, err := promptStorage(p, existing.Storage)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}

	// credentialCommitted guards the deferred cleanup below: it flips to
	// true only once setup actually finishes (after config.Save succeeds).
	// Declared here, outside the "if saveSecret" block, so it stays in
	// scope for that block's defer as well as the success path far below.
	credentialCommitted := false
	if saveSecret {
		keychain, err := env.keychain()
		if err != nil {
			fmt.Fprintf(stderr, "agent-archive: setup: keychain: %v\n", err)
			return 1
		}
		if err := keychain.Save(ctx, storageCfg.R2CredentialRef, r2Secret); err != nil {
			fmt.Fprintf(stderr, "agent-archive: setup: save R2 credentials: %v\n", err)
			return 1
		}
		// VerifyAccess below reads an R2 secret back from Keychain by
		// reference, so it must already be saved before verification can
		// run — the save can't simply move after VerifyAccess. Instead,
		// this docstring's "nothing is written until confirmation"
		// guarantee is restored here: any early return between this save
		// and the final successful config.Save removes the credential
		// again, so a failed or abandoned setup never leaves a fresh R2
		// secret behind. A reused existing credential (saveSecret false)
		// is never touched.
		defer func() {
			if credentialCommitted {
				return
			}
			if delErr := keychain.Delete(ctx, storageCfg.R2CredentialRef); delErr != nil {
				fmt.Fprintf(stderr, "agent-archive: setup: warning: could not remove uncommitted R2 credentials: %v\n", delErr)
			}
		}()
	}

	fmt.Fprintln(stdout, "\nVerifying storage access...")
	probeStore, err := env.openStore(config.Config{Storage: storageCfg})
	if err != nil {
		fmt.Fprintf(stderr, "  failed: %v\n", err)
		return 1
	}
	if err := storage.VerifyAccess(ctx, probeStore, storageCfg.Prefix); err != nil {
		fmt.Fprintf(stderr, "  failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, "  Storage access verified.")
	fmt.Fprintln(stdout, "  Privacy not verified: a successful upload does not prove the bucket is")
	fmt.Fprintln(stdout, "  private. Check your provider's public-access settings yourself.")

	detected := env.detectHarnesses(userHome)
	fmt.Fprintln(stdout)
	if len(detected) > 0 {
		fmt.Fprintf(stdout, "Detected applications: %s\n", strings.Join(detected, ", "))
	} else {
		fmt.Fprintln(stdout, "No applications detected automatically; you can still include any of them.")
	}
	harnesses, err := promptHarnesses(p, detected, existing.Harnesses)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}
	if len(harnesses) == 0 {
		fmt.Fprintln(stderr, "agent-archive: setup: at least one application must be included")
		return 1
	}

	fmt.Fprintln(stdout)
	projects, err := promptProjects(p, existing.Archive.Projects, now)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}
	if len(projects) == 0 {
		fmt.Fprintln(stderr, "agent-archive: setup: at least one project must be included")
		return 1
	}

	fmt.Fprintln(stdout)
	captureNoSkill, err := p.yesNo("Capture sessions without detected skill use?", !existing.RequireSkillUse || existing.MachineID == "")
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}
	retentionDefault := existing.RetentionDays
	if retentionDefault <= 0 {
		retentionDefault = defaultRetentionDays
	}
	retentionDays, err := p.intWithDefault("Retention (days)", retentionDefault)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}

	machineID := existing.MachineID
	if machineID == "" {
		machineID, err = local.ID()
		if err != nil {
			fmt.Fprintf(stderr, "agent-archive: setup: generate machine ID: %v\n", err)
			return 1
		}
	}

	fmt.Fprintln(stdout, "\nStorage:      ", storageCfg.Provider, "/", storageCfg.Bucket, "/", storageCfg.Prefix)
	fmt.Fprintln(stdout, "Applications: ", strings.Join(harnesses, ", "))
	fmt.Fprintf(stdout, "Projects:      %d included\n", len(projects))
	fmt.Fprintln(stdout, "History:       New sessions only")
	fmt.Fprintf(stdout, "Retention:     %d days\n", retentionDays)
	enable, err := p.yesNo("\nEnable automatic capture?", true)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}
	if !enable {
		fmt.Fprintln(stdout, "Cancelled. No changes were made.")
		return 0
	}

	changes, err := hooks.Plan(userHome, executable, harnesses)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: plan hooks: %v\n", err)
		return 1
	}
	if err := hooks.Apply(changes); err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: install hooks: %v\n", err)
		return 1
	}

	plist, err := hooks.LaunchAgent(executable, home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: build LaunchAgent: %v\n", err)
		if rbErr := hooks.Rollback(changes); rbErr != nil {
			fmt.Fprintf(stderr, "agent-archive: setup: hook rollback also failed: %v\n", rbErr)
		}
		return 1
	}
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	if err := local.WriteBytes(plistPath, plist); err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: write LaunchAgent: %v\n", err)
		if rbErr := hooks.Rollback(changes); rbErr != nil {
			fmt.Fprintf(stderr, "agent-archive: setup: hook rollback also failed: %v\n", rbErr)
		}
		return 1
	}
	launchAgentLoaded := false
	if err := env.loadLaunchAgent(plistPath); err != nil {
		fmt.Fprintf(stdout, "\nWarning: could not start the background collector automatically: %v\n", err)
		fmt.Fprintln(stdout, "It will start at your next login; run `agent-archive sync` by hand until then.")
	} else {
		launchAgentLoaded = true
	}

	cfg := config.Config{
		MachineID: machineID,
		Storage:   storageCfg,
		Archive: archive.Config{
			SchemaVersion: 1, MachineID: machineID, Enabled: true, Projects: projects,
		},
		Harnesses:       harnesses,
		RequireSkillUse: !captureNoSkill,
		RetentionDays:   retentionDays,
	}
	if err := config.Save(home, cfg); err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: save config: %v\n", err)
		// Hooks and the LaunchAgent are already live on the host at this
		// point; without this, a config.Save failure (disk full, a
		// permission error) would leave both running with no config.json
		// behind them, so `status`/`sync` would treat the machine as
		// unconfigured while hooks kept firing regardless.
		rollbackHooksAndLaunchAgent(env, stderr, changes, plistPath, launchAgentLoaded)
		return 1
	}
	credentialCommitted = true

	fmt.Fprintln(stdout, "\nSetup complete. Start a session in an included application, then run")
	fmt.Fprintln(stdout, "`agent-archive status` to confirm capture.")
	return 0
}

// rollbackHooksAndLaunchAgent undoes hook installation and a written (and
// possibly already-loaded) LaunchAgent plist. It is used only after
// config.Save fails partway through setup, once hooks and the LaunchAgent
// are already live on the host: without this, that failure would leave both
// running with no config.json behind them. Each step is best-effort and
// independent, so one failing does not stop the others from being tried.
func rollbackHooksAndLaunchAgent(env Env, stderr io.Writer, changes []hooks.Change, plistPath string, launchAgentLoaded bool) {
	if launchAgentLoaded {
		if err := env.unloadLaunchAgent(plistPath); err != nil {
			fmt.Fprintf(stderr, "agent-archive: setup: warning: could not unload LaunchAgent during rollback: %v\n", err)
		}
	}
	if err := os.Remove(plistPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(stderr, "agent-archive: setup: warning: could not remove LaunchAgent plist during rollback: %v\n", err)
	}
	if err := hooks.Rollback(changes); err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: hook rollback also failed: %v\n", err)
	}
}

// promptStorage collects a storage destination, defaulting every field to
// an existing configuration when reconfiguring. It returns the R2 secret
// (zero value if not applicable) and whether it needs saving: a blank
// answer when existing R2 credentials are already on file keeps them
// rather than overwriting Keychain with an empty secret.
func promptStorage(p *prompter, existing credentials.Config) (credentials.Config, credentials.R2Credentials, bool, error) {
	choice, err := p.withDefault("Where should sessions be stored? (1=Cloudflare R2, 2=Amazon S3)", defaultProviderChoice(existing.Provider))
	if err != nil {
		return credentials.Config{}, credentials.R2Credentials{}, false, err
	}
	var cfg credentials.Config
	switch strings.ToLower(choice) {
	case "1", "r2":
		cfg.Provider = credentials.ProviderR2
		if cfg.Bucket, err = p.withDefault("Bucket name", existing.Bucket); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.R2AccountID, err = p.withDefault("R2 account ID (blank if supplying the full endpoint)", existing.R2AccountID); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.R2Endpoint, err = p.withDefault("R2 endpoint (blank to derive from account ID)", existing.R2Endpoint); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.Prefix, err = p.withDefault("Prefix", firstNonEmpty(existing.Prefix, defaultPrefix)); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		reuse := false
		if existing.Provider == credentials.ProviderR2 && existing.R2CredentialRef != "" {
			if reuse, err = p.yesNo("Keep the currently stored R2 credentials?", true); err != nil {
				return cfg, credentials.R2Credentials{}, false, err
			}
		}
		if reuse {
			cfg.R2CredentialRef = existing.R2CredentialRef
			return cfg, credentials.R2Credentials{}, false, nil
		}
		accessKeyID, err := p.line("Access key ID: ")
		if err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		secretKey, err := p.line("Secret access key: ")
		if err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if accessKeyID == "" || secretKey == "" {
			return cfg, credentials.R2Credentials{}, false, fmt.Errorf("access key ID and secret access key are required")
		}
		cfg.R2CredentialRef = "r2-" + cfg.Bucket
		return cfg, credentials.R2Credentials{AccessKeyID: accessKeyID, SecretAccessKey: secretKey}, true, nil
	case "2", "s3":
		cfg.Provider = credentials.ProviderS3
		if cfg.Bucket, err = p.withDefault("Bucket name", existing.Bucket); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.Region, err = p.withDefault("AWS region", existing.Region); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.AWSProfile, err = p.withDefault("AWS profile", existing.AWSProfile); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.Prefix, err = p.withDefault("Prefix", firstNonEmpty(existing.Prefix, defaultPrefix)); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		return cfg, credentials.R2Credentials{}, false, nil
	default:
		return cfg, credentials.R2Credentials{}, false, fmt.Errorf("unrecognized storage choice %q", choice)
	}
}

func defaultProviderChoice(provider string) string {
	if provider == credentials.ProviderS3 {
		return "2"
	}
	return "1"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// promptHarnesses asks once per supported application, defaulting to
// whichever were detected or already included.
func promptHarnesses(p *prompter, detected, existing []string) ([]string, error) {
	var included []string
	for _, h := range allHarnesses {
		def := containsString(detected, h) || containsString(existing, h)
		include, err := p.yesNo(fmt.Sprintf("Include %s?", h), def)
		if err != nil {
			return nil, err
		}
		if include {
			included = append(included, h)
		}
	}
	return included, nil
}

func containsString(values []string, target string) bool {
	for _, v := range values {
		if v == target {
			return true
		}
	}
	return false
}

// promptProjects asks whether to keep each currently included project, then
// collects new project roots to add. Kept projects retain their original
// ActivatedAt, satisfying the spec's "reruns preserve existing activation
// times"; added ones activate now.
func promptProjects(p *prompter, existing []archive.ProjectActivation, now time.Time) ([]archive.ProjectActivation, error) {
	var kept []archive.ProjectActivation
	for _, project := range existing {
		if !project.Included {
			continue
		}
		keep, err := p.yesNo(fmt.Sprintf("Keep project %s?", project.Root), true)
		if err != nil {
			return nil, err
		}
		if keep {
			kept = append(kept, project)
		}
	}
	added, err := p.lines("Add project roots to include (one per line, blank line to finish):")
	if err != nil {
		return nil, err
	}
	for _, root := range added {
		kept = append(kept, archive.ProjectActivation{
			ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: now,
		})
	}
	return kept, nil
}
