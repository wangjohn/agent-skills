package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
)

type appStatus struct {
	Code            string    `json:"code"`
	Hooks           string    `json:"hooks"`
	Name            string    `json:"name"`
	State           string    `json:"state"`
	Sessions        int       `json:"sessions"`
	LastPublishedAt time.Time `json:"last_published_at,omitempty"`
}
type statusView struct {
	Code              string           `json:"code"`
	Version           int              `json:"schema_version"`
	State             string           `json:"state"`
	Storage           string           `json:"storage,omitempty"`
	StorageVerifiedAt time.Time        `json:"storage_verified_at,omitempty"`
	Privacy           string           `json:"privacy"`
	Background        string           `json:"background"`
	Paused            bool             `json:"paused"`
	Projects          []string         `json:"projects"`
	Apps              []appStatus      `json:"applications"`
	Collector         collector.Status `json:"collector"`
	Next              string           `json:"next_action"`
}

func runStatusCommand(args []string, stdout, stderr io.Writer, env Env) int {
	view, err := readStatus(env)
	if err != nil {
		fmt.Fprintf(stderr, "Cannot read archive status: %v\n", err)
		return 1
	}
	if containsString(args, "--json") {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(view); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Agent Archive — %s\n\n", view.State)
	if view.Storage != "" {
		fmt.Fprintf(stdout, "Storage:       %s\nAccess checked: %s (privacy not verified)\n", view.Storage, formatTimeOrNever(view.StorageVerifiedAt))
	}
	fmt.Fprintf(stdout, "Background:    %s\n", view.Background)
	if view.Paused {
		fmt.Fprintln(stdout, "Collection:    paused")
	}
	fmt.Fprintf(stdout, "Projects:      %d included\nPending:       %d session(s)\nLast scan:     %s\nLast publish:  %s\n", len(view.Projects), view.Collector.PendingCount, formatTimeOrNever(view.Collector.LastScanAt), formatTimeOrNever(view.Collector.LastPublishedAt))
	for _, app := range view.Apps {
		fmt.Fprintf(stdout, "%s: %s (%d session(s)); hooks %s\n", appName(app.Name), app.State, app.Sessions, app.Hooks)
	}
	if view.Collector.LastError != "" {
		fmt.Fprintf(stdout, "Last error:    %s\n", view.Collector.LastError)
	}
	fmt.Fprintf(stdout, "\nNext: %s\n", view.Next)
	return 0
}
func readStatus(env Env) (view statusView, err error) {
	defer func() {
		view.Code = statusCode(view.State)
		for i := range view.Apps {
			view.Apps[i].Code = statusCode(view.Apps[i].State)
		}
	}()
	view = statusView{Version: 1, State: "Not set up", Privacy: "not_verified", Background: "unknown", Projects: []string{}, Apps: []appStatus{}, Next: "Run agent-archive setup to get started."}
	home, err := env.readHome()
	if err != nil {
		return view, err
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return view, err
	}
	if _, err := os.Stat(filepath.Join(home, "setup-draft.json")); err == nil {
		view.State = "Setup saved"
		view.Next = "Run agent-archive setup to continue your saved choices."
	}
	if transactionPending(home) {
		view.State = "Setup needs recovery"
		view.Next = "Run agent-archive setup to recover the interrupted installation."
	}
	if !found {
		return view, nil
	}
	view.Storage = fmt.Sprintf("%s / %s / %s", cfg.Storage.Provider, cfg.Storage.Bucket, cfg.Storage.Prefix)
	view.StorageVerifiedAt = cfg.StorageVerifiedAt
	view.Paused = cfg.Paused
	for _, p := range cfg.Archive.Projects {
		if p.Included {
			view.Projects = append(view.Projects, p.Root)
		}
	}
	store := collector.OpenLocalStoreReadOnly(home)
	view.Collector, err = store.LoadStatus()
	if err != nil {
		return view, err
	}
	queued, err := pendingSessions(home, cfg)
	if err != nil {
		return view, err
	}
	if queued > view.Collector.PendingCount {
		view.Collector.PendingCount = queued
	}
	regs, err := store.LoadRegistrations()
	if err != nil {
		return view, err
	}
	for _, name := range cfg.Harnesses {
		app := appStatus{Name: name, State: "waiting for first session"}
		for _, reg := range regs {
			if reg.Harness.Name != name || !cfg.AcceptSession(reg) {
				continue
			}
			app.Sessions++
			if app.State == "waiting for first session" {
				app.State = "hook observed; waiting for capture"
			}
			_, at, state, found, err := store.LoadPublished(reg.ArchiveSessionID)
			if err != nil {
				return view, err
			}
			if found && app.LastPublishedAt.IsZero() {
				app.State = "captured locally"
			}
			if state == collector.CacheStatusPublished || (state == collector.CacheStatusRateLimited && !at.IsZero()) {
				app.State = "published; source verified"
				if at.After(app.LastPublishedAt) {
					app.LastPublishedAt = at
				}
			}
		}
		view.Apps = append(view.Apps, app)
	}
	userHome, err := env.userHomeDir()
	if err != nil {
		return view, err
	}
	executable, executableErr := env.executable()
	for i := range view.Apps {
		installed, e := hooks.Installed(userHome, executable, view.Apps[i].Name)
		switch {
		case e != nil || executableErr != nil:
			view.Apps[i].Hooks = "unknown"
		case !installed:
			view.Apps[i].Hooks = "missing or incomplete"
		default:
			view.Apps[i].Hooks = "installed"
		}
	}
	plist := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	view.Background = env.jobState(plist)
	view.State = "Ready"
	view.Next = "Keep working. Run agent-archive list to inspect archived sessions."
	for _, app := range view.Apps {
		if app.LastPublishedAt.IsZero() {
			view.State = "Waiting for capture"
			view.Next = "Review hook approval in " + appName(app.Name) + ", then start a new session in an included project."
			break
		}
	}
	for _, app := range view.Apps {
		if app.Hooks != "installed" {
			view.State = "Needs attention"
			view.Next = "Run agent-archive setup to check the hooks for " + appName(app.Name) + "."
			break
		}
	}
	if view.Background != "running" && view.Background != "loaded" {
		view.State = "Needs attention"
		view.Next = "Run agent-archive setup to restore the background collector."
	}
	if !view.Collector.LastScanAt.IsZero() && env.now().Sub(view.Collector.LastScanAt) > 5*time.Minute {
		view.State = "Needs attention"
		view.Next = "The last scan is over 5 minutes old. Run agent-archive sync to check collection."
	}
	if view.Collector.LastError != "" {
		view.State = "Needs attention"
		view.Next = "Check storage access and run agent-archive sync. To change credentials, run agent-archive setup and choose storage."
	}
	if cfg.Paused {
		view.State = "Paused"
		view.Next = "Run agent-archive resume when ready. Registered sessions can catch up after resume."
	}
	if !cfg.Archive.Enabled {
		view.State = "Not installed"
		view.Next = "Local data is kept. Run agent-archive setup to reinstall."
	}
	if transactionPending(home) {
		view.State = "Setup needs recovery"
		view.Next = "Run agent-archive setup to recover the interrupted installation."
	}
	return view, nil
}
func formatTimeOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}

// Codes are an API; changing display wording must not change them.
func statusCode(label string) string {
	codes := map[string]string{
		"Not set up": "not_configured", "Setup saved": "setup_pending",
		"Ready": "ready", "Waiting for capture": "awaiting_capture",
		"Needs attention": "needs_attention", "Paused": "paused",
		"Not installed": "not_installed", "Setup needs recovery": "recovery_required",
		"waiting for first session":          "awaiting_session",
		"hook observed; waiting for capture": "hook_observed",
		"captured locally":                   "captured_local", "published; source verified": "published_source_verified",
	}
	if code, ok := codes[label]; ok {
		return code
	}
	return "unknown"
}
