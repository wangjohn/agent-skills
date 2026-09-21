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
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

const defaultPrefix = "agent-archive/"
const defaultRetentionDays = 90

var allHarnesses = []string{"codex", "claude", "cursor"}

type setupDraft struct {
	StagedRefs    []string      `json:"staged_credential_refs,omitempty"`
	Version       int           `json:"version"`
	Config        config.Config `json:"config"`
	Step          int           `json:"step"`
	CredentialRef string        `json:"staged_credential_ref,omitempty"`
}

func runSetupCommand(_ []string, stdin io.Reader, stdout, stderr io.Writer, env Env) int {
	if err := setup(stdin, stdout, stderr, env); err != nil {
		fmt.Fprintf(stderr, "Setup incomplete: %v\nRun agent-archive setup to continue.\n", err)
		return 1
	}
	return 0
}

func setup(stdin io.Reader, out, errOut io.Writer, env Env) error {
	home, err := env.home()
	if err != nil {
		return err
	}
	if err = os.MkdirAll(home, 0700); err != nil {
		return err
	}
	// Only another wizard is excluded while prompting; the old collector keeps working.
	release, err := local.NamedLock(home, "setup.lock")
	if err != nil {
		return err
	}
	defer release()
	userHome, err := env.userHomeDir()
	if err != nil {
		return err
	}
	exe, err := env.executable()
	if err != nil {
		return err
	}
	if err = recoverSetup(home, env); err != nil {
		return err
	}
	existing, found, err := config.Load(home)
	if err != nil {
		return err
	}
	discoveries := env.discoverApplications(userHome)
	discoveredAt := env.now()
	p := newPrompter(stdin, out)
	if !found {
		fmt.Fprintln(out, "You’ll need a private Cloudflare R2 or Amazon S3 bucket. Type help at the storage prompt for instructions.")
	}
	draft := setupDraft{Version: 1, Config: existing}
	draftPath := filepath.Join(home, "setup-draft.json")
	var saved setupDraft
	readErr := local.Read(draftPath, &saved)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read saved setup: %w", readErr)
	}
	if readErr == nil {
		if saved.Version != 1 || saved.Step < 0 || saved.Step > 2 {
			return fmt.Errorf("saved setup has an unsupported version")
		}
		choice, e := promptChoice(p, "Saved setup: continue, capture, storage, retention, or restart", "continue", "continue", "capture", "storage", "retention", "restart")
		if e != nil {
			return e
		}
		if choice != "restart" {
			draft = saved
			if choice == "storage" {
				draft.Step = 1
			}
			if choice == "capture" {
				if e = chooseCapture(p, &draft.Config, userHome, env); e != nil {
					return e
				}
				draft.Step = 2
				if draft.Config.Storage.Provider == "" {
					draft.Step = 1
				}
			}
			if choice == "retention" {
				days := draft.Config.RetentionDays
				if days <= 0 {
					days = defaultRetentionDays
				}
				draft.Config.RetentionDays, e = p.intWithDefault("Keep sessions for how many days?", days)
				if e != nil {
					return e
				}
			}
		} else {
			if e = discardDraft(home, saved, existing, env); e != nil {
				return e
			}
		}
	} else if found {
		choice, e := promptChoice(p, "Edit capture, storage, retention, or all settings", "capture", "capture", "storage", "retention", "all")
		if e != nil {
			return e
		}
		switch choice {
		case "storage":
			draft.Step = 1
		case "retention":
			draft.Step = 2
			draft.Config.RetentionDays, err = p.intWithDefault("Keep sessions for how many days?", existing.RetentionDays)
			if err != nil {
				return err
			}
		}
		if choice == "capture" { // Storage is still verified, but its prompts are skipped.
			if err = chooseCapture(p, &draft.Config, userHome, env); err != nil {
				return err
			}
			draft.Step = 2
		}
	}
	save := func() error { return local.Write(draftPath, draft) }
	var verifiedStorage credentials.Config
	for {
		if draft.Step == 0 {
			if err = chooseCapture(p, &draft.Config, userHome, env); err != nil {
				return err
			}
			draft.Step = 1
			if err = save(); err != nil {
				return err
			}
		}
		if draft.Step == 1 {
			fmt.Fprintln(out, "\n2 of 3 — Connect storage")
			cfg, secret, saveSecret, e := promptStorage(p, draft.Config.Storage, env)
			if e != nil {
				return e
			}
			if saveSecret {
				keychain, e := env.keychain()
				if e != nil {
					return fmt.Errorf("open Keychain: %w", e)
				}
				id, e := local.ID()
				if e != nil {
					return e
				}
				cfg.R2CredentialRef = "setup-" + id
				draft.CredentialRef = cfg.R2CredentialRef
				draft.StagedRefs = append(draft.StagedRefs, cfg.R2CredentialRef)
				// Journal the opaque reference before storing, so cancellation/crash is recoverable.
				draft.Config.Storage = cfg
				if e = save(); e != nil {
					return e
				}
				if e = keychain.Save(context.Background(), cfg.R2CredentialRef, secret); e != nil {
					return fmt.Errorf("save staged credential: %w", e)
				}

			}
			draft.Config.Storage = cfg
			draft.Step = 2
			if err = save(); err != nil {
				return err
			}
		}
		if err = save(); err != nil {
			return err
		}
		if draft.Config.Storage.Provider == credentials.ProviderR2 {
			kc, e := env.keychain()
			if e != nil {
				return e
			}
			if _, e = kc.Load(context.Background(), draft.Config.Storage.R2CredentialRef); e != nil {
				draft.Step = 1
				draft.Config.Storage.R2CredentialRef = ""
				_ = save()
				return fmt.Errorf("stored R2 credential is unavailable; enter it again during setup")
			}
		}
		if draft.Config.Storage != verifiedStorage {
			fmt.Fprintln(out, "\nChecking your storage connection…")
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			store, e := env.openStore(draft.Config)
			if e != nil {
				cancel()
				draft.Step = 1
				_ = save()
				return fmt.Errorf("connect storage: %w", e)
			}
			e = storage.VerifyAccess(ctx, store)
			cancel()
			if e != nil {
				failure := fmt.Errorf("storage test failed: %w (check access and retry; saved choices are kept)", e)
				fmt.Fprintln(out, failure)
				choice, promptErr := promptChoice(p, "Edit storage settings, retry, or cancel", "cancel", "edit", "retry", "cancel")
				if promptErr != nil || choice == "cancel" {
					return failure
				}
				if choice == "edit" {
					if err = editSetupReview(p, &draft, userHome); err != nil {
						return err
					}
					if err = save(); err != nil {
						return err
					}
				}
				continue
			}
			draft.Config.BucketPrivacy = inspectBucketPrivacy(draft.Config, store, env.now())
			draft.Config.StorageVerifiedAt = env.now().UTC()
			verifiedStorage = draft.Config.Storage
			fmt.Fprintln(out, "Connected.")
		}

		if draft.Config.RetentionDays <= 0 {
			draft.Config.RetentionDays = defaultRetentionDays
		}
		showSetupReview(p, draft.Config, found, discoveries)
		if existing.Paused {
			fmt.Fprintln(out, "Capture stays paused until you run agent-archive resume.")
		}
		if err = reviewChanges(home, existing, draft.Config, p, env); err != nil {
			return err
		}
		label := "Start archiving?"
		if found {
			label = "Save these changes?"
		}
		action, e := reviewAction(p, label)
		if e != nil {
			return e
		}
		if action == "cancel" {
			fmt.Fprintln(out, "Cancelled. Active settings are unchanged; your setup draft is saved.")
			return nil
		}
		if action == "edit" {
			if err = editSetupReview(p, &draft, userHome); err != nil {
				return err
			}
			if err = save(); err != nil {
				return err
			}
			continue
		}

		for _, ref := range draft.StagedRefs {
			if ref != draft.Config.Storage.R2CredentialRef && !containsString(draft.Config.RetiredCredentialRefs, ref) {
				draft.Config.RetiredCredentialRefs = append(draft.Config.RetiredCredentialRefs, ref)
			}
		}
		// Re-read under the machine lock in applySetup; it rejects concurrent config changes.
		if err = applySetup(home, userHome, exe, existing, &draft.Config, env); err != nil {
			return err
		}
		if err = recordApplicationDiscoveries(home, discoveries, discoveredAt); err != nil {
			fmt.Fprintf(out, "Warning: installed application versions could not be recorded: %v\n", err)
		}
		if err = os.Remove(draftPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		fmt.Fprintln(out, "\nConfiguration saved.")
		if existing.Paused {
			fmt.Fprintln(out, "Next: run agent-archive resume when you’re ready to start archiving.")
		} else if containsString(draft.Config.Harnesses, "codex") {
			fmt.Fprintln(out, "Next: in Codex CLI, open /hooks to approve the archive hooks, then start a new session in an included project.")
			if len(draft.Config.Harnesses) > 1 {
				fmt.Fprintln(out, "Repeat hook approval and a new session in your other selected apps.")
			}
		} else {
			fmt.Fprintln(out, "Next: approve the archive hooks in your selected apps, then start a new session in an included project.")
		}
		fmt.Fprintln(out, "Check progress with agent-archive status.")
		return nil
	}
}

func chooseCapture(p *prompter, cfg *config.Config, userHome string, env Env) error {
	fmt.Fprintln(p.out, "\n1 of 3 — Choose what to capture")
	detected := env.detectHarnesses(userHome)
	var err error
	cfg.Harnesses, err = promptHarnesses(p, detected, cfg.Harnesses)
	if err != nil {
		return err
	}
	if len(cfg.Harnesses) == 0 {
		return fmt.Errorf("choose at least one application")
	}
	acceptedProject := false
	if len(cfg.Archive.Projects) == 0 {
		dir, e := os.Getwd()
		if env.WorkingDir != nil {
			dir, e = env.WorkingDir()
		}
		if e == nil {
			if root := suggestedProject(dir); root != "" {
				fmt.Fprintf(p.out, "Project: %s\n", root)
				acceptedProject, err = p.yesNo("Archive sessions in this project?", true)
				if err != nil {
					return err
				}
				if acceptedProject {
					cfg.Archive.Projects = []archive.ProjectActivation{{ProjectID: archive.ProjectID(root), Root: root, Included: true}}
				}
			}
		}
	}
	if !acceptedProject {
		cfg.Archive.Projects, err = promptProjects(p, cfg.Archive.Projects, time.Time{}, userHome)
		if err != nil {
			return err
		}
	}

	if len(cfg.Archive.Projects) == 0 {
		return fmt.Errorf("choose at least one project")
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	return nil
}

func promptStorage(p *prompter, existing credentials.Config, env Env) (credentials.Config, credentials.R2Credentials, bool, error) {
	cfg := existing
	var secret credentials.R2Credentials
	choice, err := promptChoice(p, "Storage provider: r2 (Cloudflare) or s3 (Amazon); help for instructions", firstNonEmpty(existing.Provider, "r2"), "r2", "s3", "help")
	for err == nil && choice == "help" {
		fmt.Fprintln(p.out, "Cloudflare R2: create a private bucket and bucket-scoped Object Read & Write credentials. Keep public access disabled.")
		fmt.Fprintln(p.out, "https://developers.cloudflare.com/r2/get-started/s3/")
		fmt.Fprintln(p.out, "Amazon S3: create a private bucket and configure an AWS profile with access to it.")
		fmt.Fprintln(p.out, "https://docs.aws.amazon.com/AmazonS3/latest/userguide/create-bucket-overview.html")
		fmt.Fprintln(p.out, "https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-files.html")
		choice, err = promptChoice(p, "Storage provider", firstNonEmpty(existing.Provider, "r2"), "r2", "s3", "help")
	}
	if err != nil {
		return cfg, secret, false, err
	}
	if cfg.Provider != choice {
		cfg = credentials.Config{Provider: choice}
	}
	cfg.Bucket, err = p.required("Bucket name", cfg.Bucket)
	if err != nil {
		return cfg, secret, false, err
	}
	if choice == "r2" {
		for {
			endpoint, e := p.required("R2 account ID or full S3 endpoint", firstNonEmpty(cfg.R2Endpoint, cfg.R2AccountID))
			if e != nil {
				return cfg, secret, false, e
			}
			if strings.Contains(endpoint, "://") {
				cfg.R2Endpoint = endpoint
				cfg.R2AccountID = ""
			} else {
				cfg.R2AccountID = endpoint
				cfg.R2Endpoint = ""
			}
			normalized, e := credentials.R2Endpoint(cfg.R2Endpoint, cfg.R2AccountID)
			if e == nil {
				cfg.R2Endpoint = normalized
				break
			}
			fmt.Fprintln(p.out, "Enter the Cloudflare R2 S3 endpoint or account ID from your dashboard.")
		}
		reuse := false
		if cfg.R2CredentialRef != "" {
			reuse, err = p.yesNo("Keep stored R2 credentials?", true)
			if err != nil {
				return cfg, secret, false, err
			}
		}
		if !reuse {
			secret.AccessKeyID, err = p.required("Access key ID", "")
			if err != nil {
				return cfg, secret, false, err
			}
			for secret.SecretAccessKey == "" {
				secret.SecretAccessKey, err = p.secret("Secret access key (hidden): ")
				if err != nil {
					return cfg, secret, false, err
				}
			}
		}
	} else {
		if err = promptAWSProfile(p, &cfg, env); err != nil {
			return cfg, secret, false, err
		}
	}
	cfg.Prefix = firstNonEmpty(cfg.Prefix, defaultPrefix)

	return cfg, secret, secret.SecretAccessKey != "", err
}

func promptChoice(p *prompter, label, def string, choices ...string) (string, error) {
	for {
		value, err := p.withDefault(label, def)
		if err != nil {
			return "", err
		}
		value = strings.ToLower(value)
		if containsString(choices, value) {
			return value, nil
		}
		fmt.Fprintf(p.out, "Choose %s.\n", strings.Join(choices, " or "))
	}
}
func promptHarnesses(p *prompter, detected, existing []string) ([]string, error) {
	// Preserve an existing selection on reconfiguration. Detection supplies
	// defaults only for first-time setup; it never proves capture is working.
	defaults := detected
	verb := "Include "
	if len(existing) > 0 {
		defaults = existing
		verb = "Keep "
	}
	var suggested []string
	for _, app := range allHarnesses {
		if containsString(defaults, app) {
			suggested = append(suggested, app)
		}
	}
	if len(suggested) > 0 {
		names := make([]string, len(suggested))
		for i, app := range suggested {
			names[i] = appName(app)
		}
		label := names[0]
		if len(names) == 2 {
			label = names[0] + " and " + names[1]
		}
		if len(names) > 2 {
			label = strings.Join(names[:len(names)-1], ", ") + ", and " + names[len(names)-1]
		}
		yes, err := p.yesNo(verb+label+"?", true)
		if err != nil {
			return nil, err
		}
		if yes {
			return suggested, nil
		}
	} else {
		fmt.Fprintln(p.out, "No apps found automatically.")
	}
	fmt.Fprintln(p.out, "Choose which apps to include:")
	for {
		var result []string
		for _, app := range allHarnesses {
			yes, err := p.yesNo("Include "+appName(app)+"?", containsString(suggested, app))
			if err != nil {
				return nil, err
			}
			if yes {
				result = append(result, app)
			}
		}
		if len(result) > 0 {
			return result, nil
		}
		fmt.Fprintln(p.out, "Choose at least one app to continue.")
	}
}
func promptProjects(p *prompter, existing []archive.ProjectActivation, now time.Time, userHomes ...string) ([]archive.ProjectActivation, error) {
	result := []archive.ProjectActivation{}
	seen := map[string]bool{}
	for _, project := range existing {
		if !project.Included {
			continue
		}
		keep, err := p.yesNo("Keep project "+project.Root+"?", true)
		if err != nil {
			return nil, err
		}
		if keep {
			result = append(result, project)
			seen[project.Root] = true
		}
	}
	fmt.Fprintln(p.out, "Add project directories, one per line. Enter a blank line when finished.")
	for {
		root, err := p.line("Project path: ")
		if err != nil {
			return nil, err
		}
		if root == "" {
			break
		}
		if root == "~" || strings.HasPrefix(root, "~/") {
			home, e := os.UserHomeDir()
			if len(userHomes) > 0 {
				home = userHomes[0]
				e = nil
			}
			if e != nil {
				return nil, e
			}
			root = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(root, "~"), "/"))
		}
		root, err = filepath.Abs(root)
		if err == nil {
			root, err = filepath.EvalSymlinks(root)
		}
		if err != nil {
			fmt.Fprintln(p.out, "That directory does not exist. Enter an existing project path.")
			continue
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			fmt.Fprintln(p.out, "Enter a directory, not a file.")
			continue
		}
		if seen[root] {
			fmt.Fprintln(p.out, "That project is already included.")
			continue
		}
		seen[root] = true
		project := archive.ProjectActivation{ProjectID: archive.ProjectID(root), Root: root, Included: true, ActivatedAt: now}
		for _, old := range existing {
			if old.Root == root {
				project.ActivatedAt = old.ActivatedAt
			}
		}
		result = append(result, project)
	}
	return result, nil
}
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
func appName(app string) string {
	switch app {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude Code"
	case "cursor":
		return "Cursor"
	}
	return app
}
func friendlyApps(apps []string) string {
	if len(apps) == 0 {
		return "none"
	}
	names := []string{}
	for _, a := range apps {
		names = append(names, appName(a))
	}
	return strings.Join(names, ", ")
}

func suggestedProject(dir string) string {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return ""
	}
	for p := root; ; p = filepath.Dir(p) {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p
		}
		if filepath.Dir(p) == p {
			return ""
		}
	}
}
