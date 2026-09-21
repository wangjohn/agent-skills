package cli

import (
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
