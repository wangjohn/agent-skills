package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

// installedFixture runs a real S3 setup into fresh temp homes so uninstall
// tests start from exactly the state setup leaves behind.
func installedFixture(t *testing.T, keychain *fakeKeychain, input string) (home, userHome string, env Env) {
	t.Helper()
	home = t.TempDir()
	userHome = t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env = setupTestEnv(t, home, userHome, keychain, now)
	var stdout, stderr bytes.Buffer
	if code := runSetupCommand(nil, strings.NewReader(input), &stdout, &stderr, env); code != 0 {
		t.Fatalf("setup failed: code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	return home, userHome, env
}

func TestUninstallReversesSetup(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, true, false, "/work/widget"))
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	unloaded := ""
	env.UnloadLaunchAgent = func(p string) error { unloaded = p; return nil }

	var stdout, stderr bytes.Buffer
	code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if unloaded != plistPath {
		t.Fatalf("expected the LaunchAgent to be unloaded via %q, got %q", plistPath, unloaded)
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("expected the plist removed, stat err=%v", err)
	}
	for _, rel := range []string{".codex/hooks.json", ".claude/settings.json"} {
		b, err := os.ReadFile(filepath.Join(userHome, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if strings.Contains(string(b), hooks.Owner) || strings.Contains(string(b), "_hook") {
			t.Fatalf("%s still contains our hook entries:\n%s", rel, b)
		}
	}
	if _, err := os.Stat(filepath.Join(userHome, ".cursor", "hooks.json")); err == nil {
		t.Fatal("cursor was never included; uninstall must not create its hook file")
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("expected local state removed, stat err=%v", err)
	}
	if _, found, _ := config.Load(t.TempDir()); found {
		t.Fatal("sanity: a fresh home must not report a config")
	}
	out := stdout.String()
	for _, want := range []string{"Nothing in your bucket is touched", "Removed:", "background collector LaunchAgent", "hook entries", "local state", "binary was left in place", "Uninstall complete."} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestUninstallDeclineChangesNothing(t *testing.T) {
	home, userHome, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	env.UnloadLaunchAgent = func(string) error {
		t.Fatal("declining must not unload the LaunchAgent")
		return nil
	}
	hookPath := filepath.Join(userHome, ".codex", "hooks.json")
	before, _ := os.ReadFile(hookPath)

	for _, answer := range []string{"n\n", "\n"} { // explicit no, and the default
		var stdout, stderr bytes.Buffer
		code := runUninstallCommand(nil, strings.NewReader(answer), &stdout, &stderr, env)
		if code != 0 {
			t.Fatalf("answer=%q code=%d stderr=%s", answer, code, stderr.String())
		}
		if !strings.Contains(stdout.String(), "Cancelled") {
			t.Fatalf("stdout=%s", stdout.String())
		}
	}
	if _, found, err := config.Load(home); err != nil || !found {
		t.Fatalf("declining must keep the config: found=%v err=%v", found, err)
	}
	after, _ := os.ReadFile(hookPath)
	if string(after) != string(before) {
		t.Fatal("declining must not touch hook configuration")
	}
	if _, err := os.Stat(filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")); err != nil {
		t.Fatalf("declining must keep the plist: %v", err)
	}
}

func TestUninstallRejectsTruncatedInputInsteadOfProceeding(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	var stdout, stderr bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader(""), &stdout, &stderr, env)
	if code != 1 {
		t.Fatalf("expected no input to fail, not take the default: code=%d stdout=%s", code, stdout.String())
	}
	if _, found, _ := config.Load(home); !found {
		t.Fatal("truncated input must not remove anything")
	}
}

func TestUninstallPreservesUnrelatedHooksAndSettings(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), now)
	claudePath := filepath.Join(userHome, ".claude", "settings.json")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0700); err != nil {
		t.Fatal(err)
	}
	original := `{"permissions":{"allow":["Read"]},"hooks":{"Stop":[{"hooks":[{"type":"command","command":"say done"}]}]}}`
	if err := os.WriteFile(claudePath, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := runSetupCommand(nil, strings.NewReader(s3SetupInput("test-bucket", "us-east-1", "test-profile", false, true, false, "/work/widget")), &stdout, &stderr, env); code != 0 {
		t.Fatalf("setup failed: code=%d stderr=%s", code, stderr.String())
	}
	installed, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(installed), hooks.Owner) {
		t.Fatalf("sanity: setup should have installed our handler:\n%s", installed)
	}

	stdout.Reset()
	stderr.Reset()
	if code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	after, err := os.ReadFile(claudePath)
	if err != nil {
		t.Fatalf("the user's settings file must survive: %v", err)
	}
	s := string(after)
	if strings.Contains(s, hooks.Owner) || strings.Contains(s, "_hook") {
		t.Fatalf("our handler must be gone:\n%s", s)
	}
	if !strings.Contains(s, `"say done"`) || !strings.Contains(s, `"Read"`) {
		t.Fatalf("unrelated hook or settings lost:\n%s", s)
	}
}

func TestUninstallDeletesStoredR2CredentialsOnly(t *testing.T) {
	keychain := newFakeKeychain()
	// A credential under some other reference stands in for anything else
	// stored under our Keychain service; uninstall must leave it alone.
	keychain.Save(context.Background(), "r2-other-bucket", credentials.R2Credentials{AccessKeyID: "OTHER", SecretAccessKey: "othersecret"})
	r2Input := strings.Join([]string{
		"1", "r2-bucket", "account123", "", "",
		"AKIAEXAMPLE", "supersecret",
		"y", "n", "n",
		"/work/widget", "",
		"y", "",
		"y",
	}, "\n") + "\n"
	_, _, env := installedFixture(t, keychain, r2Input)
	if _, err := keychain.Load(context.Background(), "r2-r2-bucket"); err != nil {
		t.Fatalf("sanity: setup should have stored the secret: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if _, err := keychain.Load(context.Background(), "r2-r2-bucket"); !errors.Is(err, credentials.ErrMissingCredential) {
		t.Fatalf("expected the stored R2 secret deleted, got err=%v", err)
	}
	if _, err := keychain.Load(context.Background(), "r2-other-bucket"); err != nil {
		t.Fatalf("an unrelated credential must survive: %v", err)
	}
	combined := stdout.String() + stderr.String()
	for _, secret := range []string{"supersecret", "AKIAEXAMPLE"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("uninstall output must never contain a secret:\n%s", combined)
		}
	}
	if !strings.Contains(stdout.String(), "stored R2 credentials") {
		t.Fatalf("expected the plan and summary to mention the Keychain item:\n%s", stdout.String())
	}
}

func TestUninstallWithS3NeverOpensKeychain(t *testing.T) {
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	env.Keychain = func() (credentials.CredentialStore, error) {
		t.Fatal("an S3 configuration references no Keychain item; uninstall must not open Keychain")
		return nil, nil
	}
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "stored R2 credentials") {
		t.Fatalf("plan should not mention a Keychain item for S3:\n%s", stdout.String())
	}
}

func TestUninstallRemovesLeftoversWithoutAConfig(t *testing.T) {
	home := t.TempDir()
	userHome := t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	// The state a setup that failed at config.Save, and whose rollback also
	// failed, would leave: a plist and hooks but no config.
	executable := "/opt/agent-archive/bin/agent-archive"
	changes, err := hooks.Plan(userHome, executable, []string{"cursor"})
	if err != nil {
		t.Fatal(err)
	}
	if err := hooks.Apply(changes); err != nil {
		t.Fatal(err)
	}
	plist, err := hooks.LaunchAgent(executable, home)
	if err != nil {
		t.Fatal(err)
	}
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	if err := local.WriteBytes(plistPath, plist); err != nil {
		t.Fatal(err)
	}
	// A launchd that never had this plist loaded reports an error; that
	// must be a warning, not a failure.
	env.UnloadLaunchAgent = func(string) error { return errors.New("not loaded") }

	var stdout, stderr bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env)
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "No agent-archive configuration found") {
		t.Fatalf("stdout=%s", stdout.String())
	}
	if _, err := os.Stat(plistPath); !os.IsNotExist(err) {
		t.Fatalf("expected the plist removed, stat err=%v", err)
	}
	b, _ := os.ReadFile(filepath.Join(userHome, ".cursor", "hooks.json"))
	if strings.Contains(string(b), hooks.Owner) {
		t.Fatalf("cursor hook entry should be removed even without a config:\n%s", b)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("expected local state removed, stat err=%v", err)
	}
}

func TestUninstallRefusesToRemoveTheUserHome(t *testing.T) {
	userHome := t.TempDir()
	env := setupTestEnv(t, userHome, userHome, newFakeKeychain(), time.Now())
	var stdout, stderr bytes.Buffer
	if code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env); code != 1 {
		t.Fatalf("code=%d stdout=%s", code, stdout.String())
	}
	if !strings.Contains(stderr.String(), "refusing to remove") {
		t.Fatalf("stderr=%s", stderr.String())
	}
	if _, err := os.Stat(userHome); err != nil {
		t.Fatalf("user home must survive: %v", err)
	}
}

func TestUninstallReportsBusyCollectorAndKeepsLocalState(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "test-profile", true, false, false, "/work/widget"))
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var stdout, stderr bytes.Buffer
	code := runUninstallCommand(nil, strings.NewReader("y\n"), &stdout, &stderr, env)
	if code != 1 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "collector pass is still running") {
		t.Fatalf("stderr=%s", stderr.String())
	}
	if _, found, _ := config.Load(home); !found {
		t.Fatal("local state must survive while a collector holds the lock")
	}
}

func TestUsageListsUninstall(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"--help"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "agent-archive uninstall") {
		t.Fatalf("help output missing uninstall: %s", out.String())
	}
}
