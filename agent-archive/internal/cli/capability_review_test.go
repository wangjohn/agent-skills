package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func TestSetupShowsVersionsBeforeActivationAndDiscoversOnce(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now().UTC())
	calls := 0
	env.DiscoverApplications = func(string) map[string]applicationDiscovery {
		calls++
		return map[string]applicationDiscovery{"codex": {Installed: true, Version: "1.2.3", VersionState: "observed"}}
	}
	input := s3SetupInput("test", "us-east-1", "profile", true, false, false, project)
	output := setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0)
	if calls != 1 || !strings.Contains(output, "installed version 1.2.3") {
		t.Fatalf("calls %d output %s", calls, output)
	}
	if !strings.Contains(output, "Checking installed applications...") {
		t.Fatalf("discovery ran without notice: %s", output)
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("cancel activated configuration")
	}
	observations, err := readApplicationDiscoveries(home)
	if err != nil || len(observations) != 0 {
		t.Fatalf("cancel persisted discovery: %v %v", observations, err)
	}
}
func TestVersionSupportUsesPublishedVersionNotResumedRegistration(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	localStore, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "synthetic.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"turn_context","model":"synthetic"}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	reg := archive.SessionRegistration{ArchiveSessionID: "s", NativeSessionID: "n", ProjectID: "p", ProjectRoot: project, Harness: archive.Harness{Name: "codex", Version: "1.2.3"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	result, err := collector.Run(context.Background(), localStore, remote, collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	if summary, err := verifyPublications(home, cfg, env, localStore, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("%+v %v", summary, err)
	}
	reg.Harness.Version = "2.0.0"
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	if err := recordApplicationDiscoveries(home, map[string]applicationDiscovery{"codex": {Installed: true, Version: "2.0.0", VersionState: "observed"}}, at); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].VersionSupport != "unverified" {
		t.Fatalf("unpublished resumed version verified: %+v", view.Apps[0])
	}
}
func TestMissingVersionDiscoveryIsUnknown(t *testing.T) {
	if got := installedVersionSupport(applicationDiscovery{}, nil); got != "unknown" {
		t.Fatal(got)
	}
	if got := installedVersionSupport(applicationDiscovery{Installed: true, Version: "1.0.0"}, []string{"11.0.0"}); got != "unverified" {
		t.Fatal(got)
	}
	var output cappedBuffer
	if _, err := output.Write([]byte(strings.Repeat("x", 5000))); err == nil || output.Len() > 4096 {
		t.Fatal("version output not bounded")
	}
}

func TestStatusAttributesClaudeRecordVersionToVerifiedCapture(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"claude"}, Archive: archive.Config{Enabled: true, Projects: []archive.ProjectActivation{{Root: project, Included: true, ActivatedAt: at.Add(-time.Hour)}}}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	localStore, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(project, "claude.jsonl")
	record := `{"version":"1.0.83","type":"user","uuid":"u1","timestamp":"2026-09-17T18:00:00Z","message":{"role":"user","content":"hello"}}` + "\n"
	if err := os.WriteFile(path, []byte(record), 0600); err != nil {
		t.Fatal(err)
	}
	// The hook has no version source for Claude Code; the transcript does.
	reg := archive.SessionRegistration{ArchiveSessionID: "s", NativeSessionID: "n", ProjectID: "p", ProjectRoot: project, Harness: archive.Harness{Name: "claude"}, TranscriptPath: path, SessionStartedAt: at, RegisteredAt: at}
	if err := localStore.SaveRegistration(reg); err != nil {
		t.Fatal(err)
	}
	remote := storage.NewMemoryStore()
	result, err := collector.Run(context.Background(), localStore, remote, collector.Options{MachineID: cfg.MachineID, Now: func() time.Time { return at }})
	if err != nil || len(result.Errors) > 0 {
		t.Fatalf("%+v %v", result, err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	if summary, err := verifyPublications(home, cfg, env, localStore, remote); err != nil || summary.Verified != 1 {
		t.Fatalf("%+v %v", summary, err)
	}
	if err := recordApplicationDiscoveries(home, map[string]applicationDiscovery{"claude": {Installed: true, Version: "1.0.83 (Claude Code)", VersionKind: versionKindCLI, VersionState: "observed"}}, at); err != nil {
		t.Fatal(err)
	}
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.Apps[0].VersionSupport != "verified_by_capture" || view.Apps[0].VersionSupportReason != "" {
		t.Fatalf("claude version not attributed: %+v", view.Apps[0])
	}
	if err := recordApplicationDiscoveries(home, map[string]applicationDiscovery{"claude": {Installed: true, Version: "1.0.90", VersionKind: versionKindCLI, VersionState: "observed"}}, at); err != nil {
		t.Fatal(err)
	}
	if view, err = readStatus(env); err != nil || view.Apps[0].VersionSupport != "unverified" || view.Apps[0].VersionSupportReason != "no_matching_verified_version" {
		t.Fatalf("%+v %v", view.Apps[0], err)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, &out, env); code != 0 || !strings.Contains(out.String(), "support unverified (verified sessions came from a different version)") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}

func TestStatusSurvivesCorruptApplicationVersions(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	at := time.Now().UTC()
	cfg := config.Config{MachineID: "machine", Storage: credentialsTestConfig(), Harnesses: []string{"codex"}, Archive: archive.Config{Enabled: true}}
	if err := config.Save(home, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(applicationDiscoveriesPath(home), []byte("{not json"), 0600); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), at)
	view, err := readStatus(env)
	if err != nil {
		t.Fatalf("corrupt advisory file failed status: %v", err)
	}
	if len(view.Warnings) != 1 || !strings.Contains(view.Warnings[0], "application-versions.json") {
		t.Fatalf("warnings %v", view.Warnings)
	}
	if app := view.Apps[0]; app.VersionState != "unknown" || app.VersionSupport != "unknown" || app.Installed {
		t.Fatalf("%+v", app)
	}
	var out strings.Builder
	if code := runStatusCommand(nil, &out, &out, env); code != 0 || !strings.Contains(out.String(), "Warning:       Installed versions could not be read") {
		t.Fatalf("exit %d: %s", code, out.String())
	}
}
