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
	p := newPrompter(stdin, out)
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
		cfg, secret, saveSecret, e := promptStorage(p, draft.Config.Storage)
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
	fmt.Fprintln(out, "\nChecking storage with a temporary test object (write, read, list, delete)…")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	store, err := env.openStore(draft.Config)
	if err != nil {
		draft.Step = 1
		_ = save()
		return fmt.Errorf("connect storage: %w", err)
	}
	if err = storage.VerifyAccess(ctx, store, draft.Config.Storage.Prefix); err != nil {
		return fmt.Errorf("storage test failed: %w (check access and retry; saved choices are kept)", err)
	}
	draft.Config.StorageVerifiedAt = env.now().UTC()
	fmt.Fprintln(out, "Storage access verified. Bucket privacy is not verified.")
	if draft.Config.Storage.Provider == credentials.ProviderR2 {
		fmt.Fprintln(out, "Check public access: https://developers.cloudflare.com/r2/buckets/public-buckets/")
	} else {
		fmt.Fprintln(out, "Check public access: https://docs.aws.amazon.com/AmazonS3/latest/userguide/access-control-block-public-access.html")
	}

	if draft.Config.RetentionDays <= 0 {
		draft.Config.RetentionDays = defaultRetentionDays
	}
	fmt.Fprintln(out, "\n3 of 3 — Review and enable")
	fmt.Fprintf(out, "Storage:      %s / %s / %s\nApplications: %s\n", draft.Config.Storage.Provider, draft.Config.Storage.Bucket, draft.Config.Storage.Prefix, friendlyApps(draft.Config.Harnesses))
	for _, project := range draft.Config.Archive.Projects {
		if project.Included {
			fmt.Fprintf(out, "Project:      %s\n", project.Root)
		}
	}
	fmt.Fprintln(out, "History:      New sessions only")
	fmt.Fprintf(out, "Without skills: %t\nRetention:    %d days; older sessions are deleted automatically\n", !draft.Config.RequireSkillUse, draft.Config.RetentionDays)
	fmt.Fprintln(out, "Filtering is best effort. Private code and sensitive text may remain in the archive.")
	if existing.Paused {
		fmt.Fprintln(out, "Capture remains paused until you run agent-archive resume.")
	}
	if err = reviewChanges(home, existing, draft.Config, p, env); err != nil {
		return err
	}
	enabled, err := p.yesNo("Apply these settings?", true)
	if err != nil {
		return err
	}
	if !enabled {
		fmt.Fprintln(out, "Cancelled. Active settings are unchanged; your setup draft is saved.")
		return nil
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
	if err = os.Remove(draftPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	fmt.Fprintln(out, "\nSetup complete. Settings saved; background collector installed.")
	for _, app := range draft.Config.Harnesses {
		fmt.Fprintf(out, "%s: waiting for a new session.\n", appName(app))
	}
	fmt.Fprintln(out, "Next: approve the archive hooks in each app, then start a harmless new session in an included project.")
	if containsString(draft.Config.Harnesses, "codex") {
		fmt.Fprintln(out, "Codex CLI: open /hooks to review and trust the installed hooks.")
	}
	fmt.Fprintln(out, "Run agent-archive status to check capture. Storage access alone does not verify capture.")
	return nil
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
	if len(cfg.Archive.Projects) == 0 {
		dir, e := os.Getwd()
		if env.WorkingDir != nil {
			dir, e = env.WorkingDir()
		}
		if e == nil {
			if root := suggestedProject(dir); root != "" {
				fmt.Fprintf(p.out, "Current project: %s (enter this path below to include it).\n", root)
			}
		}
	}
	cfg.Archive.Projects, err = promptProjects(p, cfg.Archive.Projects, time.Time{})
	if err != nil {
		return err
	}
	if len(cfg.Archive.Projects) == 0 {
		return fmt.Errorf("choose at least one project")
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	if cfg.RequireSkillUse {
		fmt.Fprintln(p.out, "Sessions: New sessions with detected skill use")
	} else {
		fmt.Fprintln(p.out, "Sessions: All new sessions, with or without skills")
	}
	fmt.Fprintf(p.out, "Keep for: %d days\n", cfg.RetentionDays)
	advanced, err := p.yesNo("Change these settings?", false)
	if err != nil {
		return err
	}
	if advanced {
		all, e := p.yesNo("Save sessions even when no skills are used?", !cfg.RequireSkillUse)
		if e != nil {
			return e
		}
		cfg.RequireSkillUse = !all
		cfg.RetentionDays, err = p.intWithDefault("Keep sessions for how many days?", cfg.RetentionDays)
	}
	return err
}

func promptStorage(p *prompter, existing credentials.Config) (credentials.Config, credentials.R2Credentials, bool, error) {
	cfg := existing
	var secret credentials.R2Credentials
	choice, err := promptChoice(p, "Storage provider: r2 (Cloudflare) or s3 (Amazon)", firstNonEmpty(existing.Provider, "r2"), "r2", "s3")
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
		cfg.Region, err = p.required("AWS region", cfg.Region)
		if err != nil {
			return cfg, secret, false, err
		}
		cfg.AWSProfile, err = p.required("Existing AWS profile", cfg.AWSProfile)
		if err != nil {
			return cfg, secret, false, err
		}
	}
	cfg.Prefix = firstNonEmpty(cfg.Prefix, defaultPrefix)
	advanced, err := p.yesNo("Change the storage prefix ("+cfg.Prefix+")?", false)
	if err != nil {
		return cfg, secret, false, err
	}
	if advanced {
		cfg.Prefix, err = p.required("Storage prefix", cfg.Prefix)
	}
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
func promptProjects(p *prompter, existing []archive.ProjectActivation, now time.Time) ([]archive.ProjectActivation, error) {
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
