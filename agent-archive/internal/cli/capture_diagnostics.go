package cli

import (
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
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

// includedCaptureDiagnostics keeps only diagnostics for projects that are
// currently included. A diagnostic is recorded only for an included project,
// but the project may be excluded later; its path must then stop appearing
// in status, not linger until newer entries push it out.
func includedCaptureDiagnostics(diagnostics []captureDiagnostic, projects []archive.ProjectActivation) []captureDiagnostic {
	kept := make([]captureDiagnostic, 0, len(diagnostics))
	for _, diagnostic := range diagnostics {
		for _, project := range projects {
			if project.Included && filepath.Clean(project.Root) == filepath.Clean(diagnostic.ProjectRoot) {
				kept = append(kept, diagnostic)
				break
			}
		}
	}
	return kept
}

// pruneCaptureDiagnostics drops stored diagnostics for projects that are no
// longer included, so an excluded path is not kept on disk either.
func pruneCaptureDiagnostics(home string, projects []archive.ProjectActivation) error {
	diagnostics, err := readCaptureDiagnostics(home)
	if err != nil {
		return err
	}
	kept := includedCaptureDiagnostics(diagnostics, projects)
	if len(kept) == len(diagnostics) {
		return nil
	}
	return local.Write(captureDiagnosticsPath(home), kept)
}

func captureDiagnosticMessage(code string) string {
	switch code {
	case diagnosticUnknownSessionStart:
		return "the session start could not be established"
	case diagnosticPreActivationStart:
		return "the session start does not meet the project activation boundary"
	default:
		return "capture evidence was not accepted"
	}
}
