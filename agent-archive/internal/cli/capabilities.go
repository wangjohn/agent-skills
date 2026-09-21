package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

type capabilityEvidence struct {
	State      string `json:"state"`
	Evidence   string `json:"evidence"`
	NextAction string `json:"next_action,omitempty"`
}

type captureCapabilities struct {
	FreshStart      capabilityEvidence `json:"fresh_start"`
	Transcript      capabilityEvidence `json:"transcript"`
	Lifecycle       capabilityEvidence `json:"lifecycle"`
	SkillEvidence   capabilityEvidence `json:"skill_evidence"`
	SubagentLinkage capabilityEvidence `json:"subagent_linkage"`
	AdapterFixtures capabilityEvidence `json:"adapter_fixtures"`
}

type applicationDiscovery struct {
	Installed     bool      `json:"installed"`
	Version       string    `json:"version,omitempty"`
	VersionSource string    `json:"version_source,omitempty"`
	VersionState  string    `json:"version_state"`
	ObservedAt    time.Time `json:"observed_at"`
}

func applicationDiscoveriesPath(home string) string {
	return filepath.Join(home, "application-versions.json")
}

func recordApplicationDiscoveries(home string, discoveries map[string]applicationDiscovery, now time.Time) error {
	for name, discovery := range discoveries {
		discovery.ObservedAt = now.UTC()
		discoveries[name] = discovery
	}
	return local.Write(applicationDiscoveriesPath(home), discoveries)
}

func readApplicationDiscoveries(home string) (map[string]applicationDiscovery, error) {
	result := map[string]applicationDiscovery{}
	if err := local.Read(applicationDiscoveriesPath(home), &result); err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}
	return result, nil
}

func captureCapabilityProfile(name string) captureCapabilities {
	documented := func(evidence string) capabilityEvidence {
		return capabilityEvidence{State: "documented", Evidence: evidence}
	}
	unavailable := func(evidence, next string) capabilityEvidence {
		return capabilityEvidence{State: "unavailable", Evidence: evidence, NextAction: next}
	}
	profile := captureCapabilities{
		SkillEvidence:   unavailable("No supported native eligibility/use contract has been verified.", "Treat eligibility comparisons as unavailable."),
		SubagentLinkage: unavailable("A lifecycle event alone does not provide a verified child transcript and parent link.", "Validate child identity, parent identity, and transcript path for the installed version."),
		AdapterFixtures: documented("Synthetic fixtures exercise the bounded adapter; they do not prove an installed version."),
	}
	switch canonicalHarness(name) {
	case "codex":
		profile.FreshStart = documented("SessionStart.source distinguishes startup/clear from resume/compact.")
		profile.Transcript = documented("Hooks provide transcript_path; official documentation says its format is not stable.")
		profile.Lifecycle = documented("SessionStart, Stop, Interrupt, SessionEnd, SubagentStart, and SubagentStop are documented.")
	case "claude":
		profile.SubagentLinkage = capabilityEvidence{State: "fixture_validated", Evidence: "Documented SubagentStop identity/path plus synthetic JSONL ownership and native timestamp fixtures. Actual installed-version capture is unverified.", NextAction: "Run a synthetic parent/child capture and read-back for the installed version."}
		profile.FreshStart = documented("SessionStart.source distinguishes startup/clear from resume/compact.")
		profile.Transcript = documented("Hooks provide transcript_path to the native JSONL transcript.")
		profile.Lifecycle = documented("SessionStart, Stop, SessionEnd, and SubagentStop are documented.")
	case "cursor":
		profile.FreshStart = unavailable("sessionStart is documented for composer creation, but resume behavior has not been verified for an installed version.", "Run a version-specific fresh and resumed synthetic session before enabling new capture.")
		profile.Transcript = documented("Common hook input includes transcript_path, which may be null when transcripts are disabled.")
		profile.Lifecycle = documented("sessionStart, stop, sessionEnd, afterAgentResponse, subagentStart, and subagentStop are documented.")
	default:
		unknown := capabilityEvidence{State: "unknown", Evidence: "No capability contract is registered."}
		return captureCapabilities{unknown, unknown, unknown, unknown, unknown, unknown}
	}
	return profile
}

func discoverApplications(userHome string) map[string]applicationDiscovery {
	return map[string]applicationDiscovery{
		"codex": discoverCommandVersion("codex", [][]string{
			{"/Applications/Codex.app/Contents/Resources/codex", "--version"},
			{filepath.Join(userHome, "Applications/Codex.app/Contents/Resources/codex"), "--version"},
			{"codex", "--version"},
		}),
		"claude": discoverCommandVersion("claude", [][]string{{"claude", "--version"}}),
		"cursor": discoverCursorVersion(userHome),
	}
}

func discoverCommandVersion(name string, candidates [][]string) applicationDiscovery {
	for _, original := range candidates {
		candidate := append([]string(nil), original...)
		path := candidate[0]
		if filepath.IsAbs(path) {
			if info, err := os.Stat(path); err != nil || info.IsDir() {
				continue
			}
		} else if resolved, err := exec.LookPath(path); err == nil {
			candidate[0] = resolved
		} else {
			continue
		}
		version, ok := boundedVersionCommand(candidate...)
		if ok {
			return applicationDiscovery{Installed: true, Version: version, VersionSource: candidate[0] + " --version", VersionState: "observed"}
		}
		return applicationDiscovery{Installed: true, VersionSource: name + " --version", VersionState: "unknown"}
	}
	return applicationDiscovery{VersionState: "absent"}
}

func discoverCursorVersion(userHome string) applicationDiscovery {
	for _, bundle := range []string{"/Applications/Cursor.app", filepath.Join(userHome, "Applications/Cursor.app")} {
		if info, err := os.Stat(bundle); err != nil || !info.IsDir() {
			continue
		}
		plist := filepath.Join(bundle, "Contents", "Info.plist")
		version, ok := boundedVersionCommand("/usr/bin/plutil", "-extract", "CFBundleShortVersionString", "raw", "-o", "-", plist)
		if ok {
			return applicationDiscovery{Installed: true, Version: version, VersionSource: plist + ":CFBundleShortVersionString", VersionState: "observed"}
		}
		return applicationDiscovery{Installed: true, VersionSource: plist, VersionState: "unknown"}
	}
	return applicationDiscovery{VersionState: "absent"}
}

func boundedVersionCommand(argv ...string) (string, bool) {
	if len(argv) == 0 {
		return "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.WaitDelay = 250 * time.Millisecond
	var output cappedBuffer
	cmd.Stdout = &output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false
	}
	value := strings.TrimSpace(output.String())
	if value == "" || strings.ContainsAny(value, "\x00\r\n") {
		return "", false
	}
	return value, true
}

func installedVersionSupport(discovery applicationDiscovery, verifiedVersions []string) string {
	if !discovery.Installed {
		if discovery.VersionState != "absent" {
			return "unknown"
		}
		return "absent"
	}
	if discovery.Version == "" || discovery.VersionState == "stale" {
		return "unknown"
	}
	installed := normalizedVersion(discovery.Version)
	for _, version := range verifiedVersions {
		if installed != "" && installed == normalizedVersion(version) {
			return "verified_by_capture"
		}
	}
	return "unverified"
}

var versionPattern = regexp.MustCompile(`(?:^|[^0-9])([0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?)(?:$|[^0-9A-Za-z.+-])`)

func normalizedVersion(value string) string {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(value))
	if len(match) == 2 {
		return match[1]
	}
	return ""
}

type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	const limit = 4096
	remaining := limit - b.Len()
	if remaining <= 0 {
		return 0, errors.New("version output exceeded limit")
	}
	if len(p) > remaining {
		_, _ = b.Buffer.Write(p[:remaining])
		return remaining, errors.New("version output exceeded limit")
	}
	return b.Buffer.Write(p)
}
