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

	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
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
	env.AWSProfiles = func() ([]AWSProfile, error) { return nil, nil }
	env.WorkingDir = func() (string, error) { return "", errors.New("no current project") }
	env.UserHomeDir = func() (string, error) { return userHome, nil }
	env.Executable = func() (string, error) { return "/opt/agent-archive/bin/agent-archive", nil }
	env.DetectHarnesses = func(string) []string { return nil }
	env.DiscoverApplications = func(string) map[string]applicationDiscovery { return map[string]applicationDiscovery{} }
	state := "missing"
	env.JobState = func(string) string { return state }
	env.LoadLaunchAgent = func(string) error { state = "loaded"; return nil }
	env.UnloadLaunchAgent = func(string) error { state = "missing"; return nil }
	env.Keychain = func() (credentials.CredentialStore, error) { return keychain, nil }
	return env
}

func s3SetupInput(bucket, region, profile string, codex, claude, cursor bool, project string) string {
	yn := func(b bool) string {
		if b {
			return "y"
		}
		return "n"
	}
	return strings.Join([]string{yn(codex), yn(claude), yn(cursor), project, "", "s3", bucket, profile, region, "y"}, "\n") + "\n"
}
func r2SetupInput(project, secret string) string {
	return strings.Join([]string{"y", "n", "n", project, "", "r2", "test-bucket", "0123456789abcdef0123456789abcdef", "ACCESS", secret, "y"}, "\n") + "\n"
}
func setupRun(t *testing.T, env Env, input string, want int) string {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run([]string{"setup"}, strings.NewReader(input), &out, &errOut, env)
	if code != want {
		t.Fatalf("setup exit %d want %d\n%s\n%s", code, want, &out, &errOut)
	}
	return out.String() + errOut.String()
}
func TestSetupFirstTimeProviders(t *testing.T) {
	for _, provider := range []string{"s3", "r2"} {
		t.Run(provider, func(t *testing.T) {
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			kc := newFakeKeychain()
			now := time.Now().UTC()
			env := setupTestEnv(t, home, userHome, kc, now)
			input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
			if provider == "r2" {
				input = r2SetupInput(project, "NEVER_PRINT_THIS")
			}
			output := setupRun(t, env, input, 0)
			if strings.Contains(output, "NEVER_PRINT_THIS") {
				t.Fatal("secret leaked")
			}
			cfg, found, err := config.Load(home)
			if err != nil || !found || cfg.Storage.Provider != provider || cfg.RequireSkillUse || cfg.RetentionDays != 90 {
				t.Fatalf("config: %+v %v", cfg, err)
			}
			if cfg.MachineID == "" || !cfg.Archive.Projects[0].ActivatedAt.Equal(now) {
				t.Fatal("missing activation or identity")
			}
			if _, err := os.Stat(filepath.Join(userHome, ".codex/hooks.json")); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(home, "setup-draft.json")); !os.IsNotExist(err) {
				t.Fatal("draft should be removed")
			}
			if provider == "r2" {
				if _, err := kc.Load(context.Background(), cfg.Storage.R2CredentialRef); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
func TestSetupCancelAndResumeDraft(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
	setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0)
	if _, found, _ := config.Load(home); found {
		t.Fatal("cancel activated config")
	}
	if _, err := os.Stat(filepath.Join(userHome, ".codex/hooks.json")); !os.IsNotExist(err) {
		t.Fatal("cancel installed hooks")
	}
	setupRun(t, env, "continue\ny\n", 0)
	if _, found, _ := config.Load(home); !found {
		t.Fatal("resume did not install")
	}
}
func TestSetupTruncatedInputDoesNotEnable(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
	setupRun(t, env, strings.TrimSuffix(input, "y\n"), 1)
	if _, found, _ := config.Load(home); found {
		t.Fatal("EOF enabled capture")
	}
}
func TestSetupStorageFailureKeepsDraftAndOldSecret(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	setupRun(t, env, r2SetupInput(project, "old-private-value"), 0)
	old, _, _ := config.Load(home)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return nil, errors.New("offline") }
	input := "storage\nr2\ntest-bucket\n0123456789abcdef0123456789abcdef\nn\nACCESS2\nnew-private-value\ny\n"
	output := setupRun(t, env, input, 1)
	secret, err := kc.Load(context.Background(), old.Storage.R2CredentialRef)
	if err != nil || secret.SecretAccessKey != "old-private-value" {
		t.Fatal("old credential replaced")
	}
	for _, value := range []string{"old-private-value", "new-private-value"} {
		if strings.Contains(output, value) {
			t.Fatal("output leaked secret")
		}
		b, _ := os.ReadFile(filepath.Join(home, "setup-draft.json"))
		if strings.Contains(string(b), value) {
			t.Fatal("draft leaked secret")
		}
	}
	current, _, _ := config.Load(home)
	if current.Storage.R2CredentialRef != old.Storage.R2CredentialRef {
		t.Fatal("active config changed")
	}
}
func TestSetupReconfigurePreservesPauseIdentityActivationAndRemovesHooks(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, true, false, project), 0)
	old, _, _ := config.Load(home)
	old.Paused = true
	config.Save(home, old)
	env.Now = func() time.Time { return time.Now().Add(time.Hour) }
	setupRun(t, env, "capture\nn\ny\nn\nn\ny\n\ny\n", 0)
	next, _, _ := config.Load(home)
	if !next.Paused || next.MachineID != old.MachineID || !next.Archive.Projects[0].ActivatedAt.Equal(old.Archive.Projects[0].ActivatedAt) {
		t.Fatal("reconfigure reset stable state")
	}
	b, _ := os.ReadFile(filepath.Join(userHome, ".claude/settings.json"))
	if strings.Contains(string(b), hooks.Owner) {
		t.Fatal("deselected app hooks remain")
	}
}
func TestSetupSchedulerFailureRestoresExistingFiles(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	paths := []string{filepath.Join(home, "config.json"), filepath.Join(userHome, ".codex/hooks.json"), filepath.Join(userHome, "Library/LaunchAgents", hooks.LaunchLabel+".plist")}
	before := map[string]string{}
	for _, p := range paths {
		b, _ := os.ReadFile(p)
		before[p] = string(b)
	}
	originalLoad := env.LoadLaunchAgent
	calls := 0
	env.LoadLaunchAgent = func(p string) error {
		calls++
		if calls == 1 {
			return errors.New("cannot load new job")
		}
		return originalLoad(p)
	}
	output := setupRun(t, env, "retention\n120\ny\n", 1)
	if !strings.Contains(output, "restored") {
		t.Fatal(output)
	}
	for p, want := range before {
		b, _ := os.ReadFile(p)
		if string(b) != want {
			t.Fatalf("did not restore %s", p)
		}
	}
	if transactionPending(home) {
		t.Fatal("successful rollback left journal")
	}
}
func TestSetupCrashRecoveryPreservesConcurrentEdits(t *testing.T) {
	home := t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	path := filepath.Join(home, "config.json")
	c := hooks.Change{Path: path, Before: []byte("before"), After: []byte("after"), Existed: true, Mode: 0600}
	journal := setupJournal{Changes: []hooks.Change{c}, Plist: "/synthetic/job"}
	local.Write(journalPath(home), journal)
	os.WriteFile(path, []byte("user edit"), 0600)
	if err := recoverSetup(home, env); err == nil {
		t.Fatal("must refuse concurrent edit")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "user edit" {
		t.Fatal("overwrote user edit")
	}
	os.WriteFile(path, []byte("after"), 0600)
	if err := recoverSetup(home, env); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(path)
	if string(b) != "before" {
		t.Fatal("did not restore")
	}
}
func TestSetupDestinationRejectsPendingAndRetiresPublishedSessions(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	now := env.now().Add(time.Second)
	payload := map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "one", "cwd": project, "transcript_path": writeCodexTranscript(t, project)}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	input := "storage\ns3\nother-bucket\nprofile\ny\n"
	output := setupRun(t, env, input, 1)
	if !strings.Contains(output, "pending") {
		t.Fatal(output)
	}
	var out, errOut bytes.Buffer
	env.Now = func() time.Time { return now.Add(time.Minute) }
	if code := runSyncCommand(nil, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	env.Now = func() time.Time { return now.Add(2 * time.Minute) }
	setupRun(t, env, "continue\ny\n", 0)
	cfg, _, _ := config.Load(home)
	store, _ := collector.NewLocalStore(home)
	regs, _ := store.LoadRegistrations()
	if cfg.AcceptSession(regs[0]) || len(cfg.PreviousDestinations) != 1 {
		t.Fatal("old sessions followed destination switch")
	}
}
func TestPromptsRetryInvalidValuesAndDeduplicatePaths(t *testing.T) {
	var out bytes.Buffer
	p := newPrompter(strings.NewReader("maybe\ny\n0\n-1\n30\n"), &out)
	if yes, err := p.yesNo("Enable?", false); err != nil || !yes {
		t.Fatal(err)
	}
	if n, err := p.intWithDefault("Days", 90); err != nil || n != 30 {
		t.Fatal(n, err)
	}
	root := t.TempDir()
	p = newPrompter(strings.NewReader("/does/not/exist\n"+root+"\n"+root+"/./\n\n"), &out)
	projects, err := promptProjects(p, nil, time.Time{})
	if err != nil || len(projects) != 1 {
		t.Fatal(projects, err)
	}
}
