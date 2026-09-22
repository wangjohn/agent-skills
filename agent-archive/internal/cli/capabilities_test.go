package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapabilityProfilesDoNotClaimUnverifiedNativeEvidence(t *testing.T) {
	for _, name := range []string{"codex", "claude", "cursor"} {
		profile := captureCapabilityProfile(name)
		if profile.Transcript.State != "documented" {
			t.Fatalf("%s profile=%#v", name, profile)
		}
		expectedSubagent := "unavailable"
		if name == "claude" {
			expectedSubagent = "fixture_validated"
		}
		if profile.SkillEvidence.State != "unavailable" || profile.SubagentLinkage.State != expectedSubagent {
			t.Fatalf("%s invented native capability: %#v", name, profile)
		}
	}
	if !strings.Contains(captureCapabilityProfile("claude").SubagentLinkage.Evidence, "unverified") {
		t.Fatal("fixture coverage claimed live verification")
	}
	if got := captureCapabilityProfile("cursor").FreshStart.State; got != "unavailable" {
		t.Fatalf("Cursor start=%s", got)
	}
}

func TestInstalledVersionSupportNeedsMatchingVerifiedCapture(t *testing.T) {
	discovery := applicationDiscovery{Installed: true, Version: "agent 1.2.3", VersionState: "observed"}
	if got := installedVersionSupport(discovery, nil); got != "unverified" {
		t.Fatal(got)
	}
	if got := installedVersionSupport(discovery, []string{"1.2.3"}); got != "verified_by_capture" {
		t.Fatal(got)
	}
	if got := installedVersionSupport(discovery, []string{"2.0.0"}); got != "unverified" {
		t.Fatal(got)
	}
}

func TestNormalizedVersionKeepsEveryComponentAndFallsBack(t *testing.T) {
	cases := map[string]string{
		"1.2.3.4":            "1.2.3.4",
		"v1.2.3":             "1.2.3",
		"codex-cli 1.2.3":    "1.2.3",
		"1.2.3-beta.1+build": "1.2.3-beta.1+build",
		"  2.0  ":            "2.0",
		"nightly-abc":        "nightly-abc",
	}
	for input, want := range cases {
		if got := normalizedVersion(input); got != want {
			t.Errorf("normalizedVersion(%q) = %q, want %q", input, got, want)
		}
	}
	if installedVersionSupport(applicationDiscovery{Installed: true, Version: "1.2.3.4", VersionState: "observed"}, []string{"2.3.4"}) != "unverified" {
		t.Fatal("four-component version matched its own suffix")
	}
}

func TestInstalledVersionSupportReportsWhyUnverified(t *testing.T) {
	cli := applicationDiscovery{Installed: true, Version: "1.2.3", VersionKind: versionKindCLI, VersionState: "observed"}
	if state, reason := installedVersionSupportDetail(cli, nil); state != "unverified" || reason != supportReasonNoVerifiedCapture {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(cli, []string{"1.2.4"}); state != "unverified" || reason != supportReasonNoMatchingVersion {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(cli, []string{"1.2.4", "1.2.3"}); state != "verified_by_capture" || reason != "" {
		t.Fatalf("%s %s", state, reason)
	}
	// Cursor: the app bundle's CFBundleShortVersionString against the hook's
	// cursor_version. Same scheme compares; a different scheme is flagged.
	bundle := applicationDiscovery{Installed: true, Version: "1.6.45", VersionKind: versionKindAppBundle, VersionState: "observed"}
	if state, reason := installedVersionSupportDetail(bundle, []string{"1.6.45"}); state != "verified_by_capture" || reason != "" {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(bundle, []string{"2025.09.12-nightly"}); state != "unverified" || reason != supportReasonNoMatchingVersion {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(bundle, []string{"agent-build-7f3a"}); state != "unverified" || reason != supportReasonVersionSourceMismatch {
		t.Fatalf("%s %s", state, reason)
	}
	if state, reason := installedVersionSupportDetail(applicationDiscovery{VersionState: "absent"}, []string{"1.6.45"}); state != "absent" || reason != "" {
		t.Fatalf("%s %s", state, reason)
	}
}

func TestDiscoverCommandVersionTriesEveryPresentCandidate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell scripts")
	}
	dir := t.TempDir()
	script := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	broken := script("broken", "exit 1")
	working := script("working", "echo 'tool 1.2.3'")
	missing := filepath.Join(dir, "missing")
	got := discoverCommandVersion("tool", [][]string{{missing, "--version"}, {broken, "--version"}, {working, "--version"}})
	if !got.Installed || got.Version != "tool 1.2.3" || got.VersionState != "observed" || got.VersionKind != versionKindCLI || got.VersionSource != working+" --version" {
		t.Fatalf("%+v", got)
	}
	got = discoverCommandVersion("tool", [][]string{{broken, "--version"}, {script("broken2", "exit 2"), "--version"}})
	if !got.Installed || got.Version != "" || got.VersionState != "unknown" || got.VersionKind != versionKindCLI {
		t.Fatalf("present-but-failing candidates: %+v", got)
	}
	got = discoverCommandVersion("tool", [][]string{{missing, "--version"}, {dir, "--version"}})
	if got.Installed || got.VersionState != "absent" {
		t.Fatalf("absent candidates: %+v", got)
	}
}
