package cli

import (
	"fmt"
	"io"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
)

// runStatusCommand implements `agent-archive status`. It never prints
// transcript or conversation content — only operational state.
func runStatusCommand(_ []string, stdout, stderr io.Writer, env Env) int {
	home, err := env.home()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: status: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: status: load config: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintln(stdout, "Not set up. Run `agent-archive setup` to get started.")
		return 0
	}

	localStore, err := collector.NewLocalStore(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: status: open local store: %v\n", err)
		return 1
	}
	collectorStatus, err := localStore.LoadStatus()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: status: load collector status: %v\n", err)
		return 1
	}
	registrations, err := localStore.LoadRegistrations()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: status: load registrations: %v\n", err)
		return 1
	}

	included := 0
	for _, project := range cfg.Archive.Projects {
		if project.Included {
			included++
		}
	}

	fmt.Fprintf(stdout, "Storage:        %s (%s)\n", cfg.Storage.Provider, cfg.Storage.Bucket)
	if cfg.Paused {
		fmt.Fprintln(stdout, "Collection:     paused")
	} else {
		fmt.Fprintln(stdout, "Collection:     active")
	}
	fmt.Fprintf(stdout, "Projects:       %d included\n", included)
	fmt.Fprintf(stdout, "Registered:     %d session(s) known on this machine\n", len(registrations))
	fmt.Fprintf(stdout, "Last scan:      %s\n", formatTimeOrNever(collectorStatus.LastScanAt))
	fmt.Fprintf(stdout, "Last publish:   %s\n", formatTimeOrNever(collectorStatus.LastPublishedAt))
	fmt.Fprintf(stdout, "Pending:        %d session(s)\n", collectorStatus.PendingCount)
	if collectorStatus.LastError != "" {
		fmt.Fprintf(stdout, "Last error:     %s\n", collectorStatus.LastError)
	}
	return 0
}

func formatTimeOrNever(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
