package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// fakeKeychain is an in-memory credentials.CredentialStore for tests, since
// the real one is only available on a darwin+cgo build and must never be
// exercised in automated tests regardless of platform.
type fakeKeychain struct {
	mu    sync.Mutex
	items map[string]credentials.R2Credentials
}

func newFakeKeychain() *fakeKeychain {
	return &fakeKeychain{items: map[string]credentials.R2Credentials{}}
}

func (f *fakeKeychain) Save(_ context.Context, reference string, value credentials.R2Credentials) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items[reference] = value
	return nil
}
func (f *fakeKeychain) Load(_ context.Context, reference string) (credentials.R2Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.items[reference]
	if !ok {
		return credentials.R2Credentials{}, credentials.ErrMissingCredential
	}
	return v, nil
}
func (f *fakeKeychain) Delete(_ context.Context, reference string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, reference)
	return nil
}

func setupTestEnv(t *testing.T, home, userHome string, keychain *fakeKeychain, now time.Time) Env {
	t.Helper()
	env := testEnv(t, home, now)
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	env.Executable = func() (string, error) { return "/opt/agent-archive/bin/agent-archive", nil }
	env.DetectHarnesses = func(string) []string { return nil }
	env.LoadLaunchAgent = func(string) error { return nil }
	env.Keychain = func() (credentials.CredentialStore, error) { return keychain, nil }
	return env
}

func s3SetupInput(bucket, region, profile string, includeCodex, includeClaude, includeCursor bool, projectRoot string) string {
	yn := func(b bool) string {
		if b {
			return "y"
		}
		return "n"
	}
	lines := []string{
		"2", bucket, region, profile, "", // storage: provider, bucket, region, profile, prefix(default)
		yn(includeCodex), yn(includeClaude), yn(includeCursor), // harnesses
		projectRoot, "", // one project, then blank to finish
		"y", "", // capture-without-skill-use=yes, retention=default
		"y", // enable
	}
	return strings.Join(lines, "\n") + "\n"
}

func TestSetupFirstTimeS3EndToEnd(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)

	stdin := strings.NewReader(s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	var stdout, stderr bytes.Buffer
	code := runSetupCommand(nil, stdin, &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("cfg=%#v found=%v err=%v", cfg, found, err)
	}
	if cfg.Storage.Provider != credentials.ProviderS3 || cfg.Storage.Bucket != "test-bucket" || cfg.Storage.Prefix != defaultPrefix {
		t.Fatalf("storage=%#v", cfg.Storage)
	}
	if len(cfg.Harnesses) != 1 || cfg.Harnesses[0] != "codex" {
		t.Fatalf("harnesses=%#v", cfg.Harnesses)
	}
	if len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != "/work/widget" || !cfg.Archive.Projects[0].ActivatedAt.Equal(now) {
		t.Fatalf("projects=%#v", cfg.Archive.Projects)
	}
	if cfg.RequireSkillUse {
		t.Fatalf("expected capture-without-skill-use to be honored: RequireSkillUse=%v", cfg.RequireSkillUse)
	}
	if cfg.RetentionDays != defaultRetentionDays {
		t.Fatalf("retention=%d", cfg.RetentionDays)
	}
	if cfg.MachineID == "" {
		t.Fatal("expected a generated machine ID")
	}

	hookPath := filepath.Join(userHome, ".codex", "hooks.json")
	if _, err := os.Stat(hookPath); err != nil {
		t.Fatalf("expected codex hooks installed: %v", err)
	}
	claudeHookPath := filepath.Join(userHome, ".claude", "settings.json")
	if _, err := os.Stat(claudeHookPath); err == nil {
		t.Fatal("claude was not included and must not have hooks installed")
	}
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", "com.agent-archive.collector.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Fatalf("expected LaunchAgent plist written: %v", err)
	}
}

func TestSetupCancelAtConfirmationChangesNothing(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)

	input := s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget")
	input = strings.TrimSuffix(input, "y\n") + "n\n" // decline the final confirmation
	var stdout, stderr bytes.Buffer
	code := runSetupCommand(nil, strings.NewReader(input), &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Cancelled") {
		t.Fatalf("stdout=%s", stdout.String())
	}
	if _, found, err := config.Load(home); err != nil || found {
		t.Fatalf("cancelling must not write a config: found=%v err=%v", found, err)
	}
	if _, err := os.Stat(filepath.Join(userHome, ".codex", "hooks.json")); err == nil {
		t.Fatal("cancelling must not install hooks")
	}
}

func TestSetupAbortsOnStorageVerificationFailure(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) {
		return nil, errors.New("storage unavailable")
	}

	stdin := strings.NewReader(s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	var stdout, stderr bytes.Buffer
	code := runSetupCommand(nil, stdin, &stdout, &stderr, env)
	if code != 1 {
		t.Fatalf("code=%d stdout=%s", code, stdout.String())
	}
	if _, found, _ := config.Load(home); found {
		t.Fatal("a failed verification must not leave a config behind")
	}
}

func TestSetupReconfigurePreservesActivationTimeAndAllowsRemoval(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	firstRun := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), firstRun)

	stdin := strings.NewReader(s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	var stdout, stderr bytes.Buffer
	if code := runSetupCommand(nil, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("first setup failed: code=%d stderr=%s", code, stderr.String())
	}

	// Reconfigure a month later: keep the existing project, add a second one.
	secondRun := firstRun.Add(30 * 24 * time.Hour)
	env2 := setupTestEnv(t, home, userHome, newFakeKeychain(), secondRun)
	reconfigureInput := strings.Join([]string{
		"2", "test-bucket", "us-east-1", "test-profile", "", // storage, same bucket
		"y", "n", "n", // harnesses
		"y",                // keep /work/widget
		"/work/second", "", // add a second project, then finish
		"y", "", // capture-without-skill-use, retention default
		"y", // enable
	}, "\n") + "\n"
	stdout.Reset()
	stderr.Reset()
	if code := runSetupCommand(nil, strings.NewReader(reconfigureInput), &stdout, &stderr, env2); code != 0 {
		t.Fatalf("reconfigure failed: code=%d stderr=%s", code, stderr.String())
	}

	cfg, found, err := config.Load(home)
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if len(cfg.Archive.Projects) != 2 {
		t.Fatalf("projects=%#v", cfg.Archive.Projects)
	}
	var widget, second *time.Time
	for i := range cfg.Archive.Projects {
		p := cfg.Archive.Projects[i]
		switch p.Root {
		case "/work/widget":
			widget = &p.ActivatedAt
		case "/work/second":
			second = &p.ActivatedAt
		}
	}
	if widget == nil || !widget.Equal(firstRun) {
		t.Fatalf("kept project must preserve its original activation time: got %v want %v", widget, firstRun)
	}
	if second == nil || !second.Equal(secondRun) {
		t.Fatalf("newly added project must activate now: got %v want %v", second, secondRun)
	}
}

func TestSetupReconfigureCanRemoveAProject(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	stdin := strings.NewReader(s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	var stdout, stderr bytes.Buffer
	if code := runSetupCommand(nil, stdin, &stdout, &stderr, env); code != 0 {
		t.Fatalf("first setup failed: code=%d", code)
	}

	removeInput := strings.Join([]string{
		"2", "test-bucket", "us-east-1", "test-profile", "",
		"y", "n", "n",
		"n", // do not keep /work/widget
		"",  // no new projects
	}, "\n") + "\n"
	stdout.Reset()
	stderr.Reset()
	code := runSetupCommand(nil, strings.NewReader(removeInput), &stdout, &stderr, env)
	if code != 1 {
		t.Fatalf("removing the only project should fail cleanly (at least one required): code=%d stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "at least one project") {
		t.Fatalf("stderr=%s", stderr.String())
	}
}

func TestSetupR2SavesSecretAndReusesOnReconfigure(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	keychain := newFakeKeychain()
	env := setupTestEnv(t, home, userHome, keychain, now)

	r2Input := strings.Join([]string{
		"1", "r2-bucket", "account123", "", "", // provider, bucket, account id, endpoint(blank), prefix(default)
		"AKIAEXAMPLE", "supersecret", // access key id, secret
		"y", "n", "n",
		"/work/widget", "",
		"y", "",
		"y",
	}, "\n") + "\n"
	var stdout, stderr bytes.Buffer
	if code := runSetupCommand(nil, strings.NewReader(r2Input), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	cfg, _, err := config.Load(home)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := keychain.Load(context.Background(), cfg.Storage.R2CredentialRef)
	if err != nil || saved.AccessKeyID != "AKIAEXAMPLE" || saved.SecretAccessKey != "supersecret" {
		t.Fatalf("saved=%#v err=%v", saved, err)
	}

	// Reconfigure, keeping the existing R2 credentials.
	reconfigureInput := strings.Join([]string{
		"1", "r2-bucket", "account123", "", "",
		"y", // keep existing R2 credentials
		"y", "n", "n",
		"y", // keep /work/widget
		"",  // no new projects
		"y", "",
		"y",
	}, "\n") + "\n"
	stdout.Reset()
	stderr.Reset()
	if code := runSetupCommand(nil, strings.NewReader(reconfigureInput), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	stillSaved, err := keychain.Load(context.Background(), cfg.Storage.R2CredentialRef)
	if err != nil || stillSaved.SecretAccessKey != "supersecret" {
		t.Fatalf("expected the R2 secret to survive reconfiguration unchanged: %#v err=%v", stillSaved, err)
	}
}
