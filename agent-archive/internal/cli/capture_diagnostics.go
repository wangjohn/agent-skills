package cli

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

const (
	diagnosticUnknownSessionStart = "session_start_unknown"
	diagnosticPreActivationStart  = "session_started_before_activation"
)

// captureDiagnostic is deliberately content-free. It records only the
// integration boundary that prevented capture; native session identifiers,
// transcript paths, hook payloads, and conversation content never belong here.
type captureDiagnostic struct {
	Code        string    `json:"code"`
	Harness     string    `json:"harness"`
	ProjectRoot string    `json:"project_root,omitempty"`
	ObservedAt  time.Time `json:"observed_at"`
}

func captureDiagnosticsPath(home string) string {
	return filepath.Join(home, "capture-diagnostics.json")
}

func readCaptureDiagnostics(home string) ([]captureDiagnostic, error) {
	var diagnostics []captureDiagnostic
	if err := local.Read(captureDiagnosticsPath(home), &diagnostics); err != nil {
		if os.IsNotExist(err) {
			return []captureDiagnostic{}, nil
		}
		return nil, err
	}
	return diagnostics, nil
}

func recordCaptureDiagnostic(home string, diagnostic captureDiagnostic) error {
	diagnostics, err := readCaptureDiagnostics(home)
	if err != nil {
		return err
	}
	diagnostic.ObservedAt = diagnostic.ObservedAt.UTC()
	// Keep only the latest instance of a reason for an app/project pair. This
	// bounds local status data even when a harness repeats the same hook.
	kept := diagnostics[:0]
	for _, existing := range diagnostics {
		if existing.Code == diagnostic.Code && existing.Harness == diagnostic.Harness && existing.ProjectRoot == diagnostic.ProjectRoot {
			continue
		}
		kept = append(kept, existing)
	}
	kept = append(kept, diagnostic)
	sort.Slice(kept, func(i, j int) bool { return kept[i].ObservedAt.Before(kept[j].ObservedAt) })
	if len(kept) > 50 {
		kept = kept[len(kept)-50:]
	}
	return local.Write(captureDiagnosticsPath(home), kept)
}
