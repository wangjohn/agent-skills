package cli

import (
	"context"
	"errors"
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
		// R2CredentialRef is deterministic per bucket ("r2-"+bucket), so
		// reconfiguring the same bucket with a freshly-entered secret
		// overwrites, not creates, when a working credential already lives
		// under that reference. Capture whatever is there now, before the
		// overwrite, so a failed or abandoned setup can restore it instead
		// of deleting it outright — deleting it would destroy a previously
		// working credential the docstring's "nothing is written until
		// confirmation" guarantee never intended to touch.
		priorSecret, priorErr := keychain.Load(ctx, storageCfg.R2CredentialRef)
		hadPrior := priorErr == nil
		priorLoadFailed := priorErr != nil && !errors.Is(priorErr, credentials.ErrMissingCredential)

		if err := keychain.Save(ctx, storageCfg.R2CredentialRef, r2Secret); err != nil {
			fmt.Fprintf(stderr, "agent-archive: setup: save R2 credentials: %v\n", err)
			return 1
		}
		// VerifyAccess below reads an R2 secret back from Keychain by
		// reference, so it must already be saved before verification can
		// run — the save can't simply move after VerifyAccess. Instead,
		// this docstring's "nothing is written until confirmation"
		// guarantee is restored here: any early return between this save
		// and the final successful config.Save undoes it, so a failed or
		// abandoned setup never leaves a fresh R2 secret behind, and never
		// destroys a reused reference's prior working value either.
		defer func() {
			if credentialCommitted {
				return
			}
			switch {
			case hadPrior:
				if err := keychain.Save(ctx, storageCfg.R2CredentialRef, priorSecret); err != nil {
					fmt.Fprintf(stderr, "agent-archive: setup: warning: could not restore prior R2 credentials: %v\n", err)
				}
			case priorLoadFailed:
				// Could not confirm whether this reference already held a
				// working credential; leaving it as-is (the just-written,
				// unconfirmed secret) is safer than guessing and possibly
				// deleting a credential that was actually there before.
				fmt.Fprintln(stderr, "agent-archive: setup: warning: could not confirm prior R2 credential state; leaving Keychain unchanged")
			default:
				if delErr := keychain.Delete(ctx, storageCfg.R2CredentialRef); delErr != nil {
					fmt.Fprintf(stderr, "agent-archive: setup: warning: could not remove uncommitted R2 credentials: %v\n", delErr)
				}
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
	fmt.Fprintln(stdout, "\nWhich applications should sessions be captured from?")
	if len(detected) > 0 {
		p.help(fmt.Sprintf("Detected: %s. Including one adds hooks to its config; nothing else changes.", strings.Join(harnessDisplayNames(detected), ", ")))
	} else {
		p.help("None detected. Including one adds hooks to its config; nothing else changes.")
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

	fmt.Fprintln(stdout, "\nWhich project directories should be captured?")
	p.help("Absolute paths, one per line. Only sessions started in exactly these directories are archived.")
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
	p.help("Yes keeps every session; No keeps only sessions where a skill was used.")
	captureNoSkill, err := p.yesNo("Capture sessions without detected skill use?", !existing.RequireSkillUse || existing.MachineID == "")
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}
	retentionDefault := existing.RetentionDays
	if retentionDefault <= 0 {
		retentionDefault = defaultRetentionDays
	}
	p.help("How long should sessions stay in the bucket? Older ones are deleted.")
	retentionDays, err := p.intWithDefault("Retention (days)", retentionDefault)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: setup: %v\n", err)
		return 1
	}
	if retentionDays <= 0 {
		// retention.Options.SessionMaxAge treats zero (or, from a negative
		// duration, anything non-positive) as "disabled" — never expire —
		// which is the opposite of what a user typing "0" here, expecting
		// aggressive cleanup, would want. This guided flow requires a real
		// number of days rather than silently accepting that surprise.
		fmt.Fprintln(stderr, "agent-archive: setup: retention days must be a positive number")
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
	fmt.Fprintln(stdout)
	p.help("Ready to enable? This installs the hooks and a login LaunchAgent that publishes about once a minute.")
	enable, err := p.yesNo("Enable automatic capture?", true)
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
	fmt.Fprintln(p.out, "Where should sessions be stored?")
	p.options("1) Cloudflare R2", "2) Amazon S3")
	choice, err := p.withDefault("Choice", defaultProviderChoice(existing.Provider))
	if err != nil {
		return credentials.Config{}, credentials.R2Credentials{}, false, err
	}
	var cfg credentials.Config
	switch strings.ToLower(choice) {
	case "1", "r2":
		cfg.Provider = credentials.ProviderR2
		fmt.Fprintln(p.out)
		p.help("Which existing R2 bucket? Listed under R2 Object Storage > Overview in the Cloudflare dashboard.")
		if cfg.Bucket, err = p.withDefault("Bucket name", existing.Bucket); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.Bucket == "" {
			return cfg, credentials.R2Credentials{}, false, fmt.Errorf("bucket name is required")
		}
		p.help("What is your Cloudflare account ID? Shown at the right of the R2 Overview page. Blank if you give the endpoint instead.")
		if cfg.R2AccountID, err = p.withDefault("R2 account ID", existing.R2AccountID); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		p.help("What is the bucket's S3 endpoint? Shown under the bucket's Settings > S3 API. Blank to derive it from the account ID.")
		if cfg.R2Endpoint, err = p.withDefault("R2 endpoint", existing.R2Endpoint); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.R2AccountID == "" && cfg.R2Endpoint == "" {
			return cfg, credentials.R2Credentials{}, false, fmt.Errorf("either the R2 account ID or the R2 endpoint is required")
		}
		p.help("Where in the bucket should sessions go? A folder-style prefix.")
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
		p.help(
			"Which R2 API token? Create one under R2 > Manage R2 API Tokens with \"Object Read & Write\" scoped to this bucket.",
			"The secret is stored in macOS Keychain. It is visible on screen as you type it.",
		)
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
		fmt.Fprintln(p.out)
		p.help("Which existing S3 bucket? Listed under S3 > Buckets in the AWS console, or by `aws s3 ls --profile <name>`.")
		if cfg.Bucket, err = p.withDefault("Bucket name", existing.Bucket); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.Bucket == "" {
			return cfg, credentials.R2Credentials{}, false, fmt.Errorf("bucket name is required")
		}
		p.help("Which region is the bucket in? For example us-east-1.")
		if cfg.Region, err = p.withDefault("AWS region", existing.Region); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.Region == "" {
			return cfg, credentials.R2Credentials{}, false, fmt.Errorf("AWS region is required")
		}
		p.help("Which AWS profile can read, write, list, and delete in the bucket? Only its name is stored.")
		if cfg.AWSProfile, err = p.withDefault("AWS profile", existing.AWSProfile); err != nil {
			return cfg, credentials.R2Credentials{}, false, err
		}
		if cfg.AWSProfile == "" {
			return cfg, credentials.R2Credentials{}, false, fmt.Errorf("AWS profile is required")
		}
		p.help("Where in the bucket should sessions go? A folder-style prefix.")
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
		include, err := p.yesNo(fmt.Sprintf("Include %s?", harnessDisplayName(h)), def)
		if err != nil {
			return nil, err
		}
		if include {
			included = append(included, h)
		}
	}
	return included, nil
}

// harnessDisplayName maps an internal harness name to what its users call it.
func harnessDisplayName(h string) string {
	switch h {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude Code"
	case "cursor":
		return "Cursor"
	}
	return h
}

func harnessDisplayNames(hs []string) []string {
	out := make([]string, 0, len(hs))
	for _, h := range hs {
		out = append(out, harnessDisplayName(h))
	}
	return out
}

// normalizeProjectRoot expands a leading ~ and cleans the path so the stored
// root matches the working directory a hook later reports. It rejects
// relative paths: a root that depends on where setup was run would silently
// never match anything.
func normalizeProjectRoot(root string) (string, error) {
	if root == "~" || strings.HasPrefix(root, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~ in %q: %w", root, err)
		}
		root = filepath.Join(home, strings.TrimPrefix(root, "~"))
	}
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("project directory %q must be an absolute path", root)
	}
	return filepath.Clean(root), nil
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
	added, err := p.lines("Add project directories (blank line to finish):")
	if err != nil {
		return nil, err
	}
	for _, root := range added {
		root, err = normalizeProjectRoot(root)
		if err != nil {
			return nil, err
		}
		kept = append(kept, archive.ProjectActivation{
			ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: now,
		})
	}
	return kept, nil
}
