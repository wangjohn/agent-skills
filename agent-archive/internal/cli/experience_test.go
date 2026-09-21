package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

func TestEveryPublicHelpIsReadOnly(t *testing.T) {
	env := Env{Home: func() (string, error) { t.Fatal("help accessed runtime"); return "", nil }}
	for cmd := range commandHelp {
		for _, args := range [][]string{{cmd, "--help"}, {"help", cmd}, {cmd, "-h"}} {
			var out, errOut bytes.Buffer
			if code := Run(args, nil, &out, &errOut, env); code != 0 || !strings.Contains(out.String(), "Usage:") {
				t.Fatalf("%v: %d %s %s", args, code, &out, &errOut)
			}
		}
	}
	for _, args := range [][]string{{"pause", "--unknown"}, {"resume", "oops"}, {"setup", "--json"}, {"status", "extra"}, {"uninstall", "--force"}, {"sync", "extra"}, {"list", "--bad"}, {"show", "id", "--bad"}} {
		if code := Run(args, nil, nil, nil, env); code != 2 {
			t.Fatalf("%v code %d", args, code)
		}
	}
}
func TestStatusJSONAndTextUseObservedEvidence(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	view, err := readStatus(env)
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "Waiting for capture" || view.Apps[0].State != "waiting for first session" {
		t.Fatalf("invented capture: %+v", view)
	}
	var out bytes.Buffer
	if code := Run([]string{"status", "--json"}, nil, &out, nil, env); code != 0 {
		t.Fatal(code)
	}
	var decoded statusView
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.State != view.State || decoded.Next != view.Next {
		t.Fatal("JSON does not match model")
	}
	env.OpenStore = nil // status must not resolve storage at all.
	cfg, _, _ := config.Load(home)
	cfg.Paused = true
	config.Save(home, cfg)
	view, err = readStatus(env)
	if err != nil || view.State != "Paused" {
		t.Fatal(view, err)
	}
	cfg.Paused = false
	config.Save(home, cfg)
	local.Write(journalPath(home), setupJournal{})
	view, err = readStatus(env)
	if err != nil || view.State != "Setup needs recovery" {
		t.Fatal(view, err)
	}
}
func TestPauseBusyMakesNoFalseClaimAndDoesNotLoseConfig(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/project", time.Now())
	env := testEnv(t, home, time.Now())
	unlock, err := local.Lock(home)
	if err != nil {
		t.Fatal(err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"pause"}, nil, &out, &errOut, env); code != 1 {
		t.Fatal(code)
	}
	cfg, _, _ := config.Load(home)
	if cfg.Paused {
		t.Fatal("busy pause changed config")
	}
	unlock()
	if code := Run([]string{"pause"}, nil, &out, &errOut, env); code != 0 {
		t.Fatal(code)
	}
	cfg, _, _ = config.Load(home)
	if !cfg.Paused {
		t.Fatal("pause not persisted")
	}
}
func TestUninstallPurgeRequiresSecondConfirmation(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()))
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--delete-local-data"}, strings.NewReader("y\nn\n"), &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	cfg, found, _ := config.Load(home)
	if !found || !cfg.Archive.Enabled {
		t.Fatal("declining purge altered installation")
	}
}
func TestSetupFailureBeforeCommitRecoversFreshInstall(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.LoadLaunchAgent = func(string) error { return errors.New("bootstrap failed") }
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 1)
	if _, found, _ := config.Load(home); found {
		t.Fatal("failed fresh setup left active config")
	}
	if _, err := os.Stat(filepath.Join(userHome, ".codex/hooks.json")); !os.IsNotExist(err) {
		t.Fatal("failed setup left hooks")
	}
	if transactionPending(home) {
		t.Fatal("journal not cleaned after restoration")
	}
}
func TestStatusReadDoesNotCreateCollectorLayout(t *testing.T) {
	home := t.TempDir()
	setUpTestConfig(t, home, "/project", time.Now())
	env := testEnv(t, home, time.Now())
	env.JobState = func(string) string { return "unknown" }
	if _, err := readStatus(env); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"registrations", "requests", "published", "sessions"} {
		if _, err := os.Stat(filepath.Join(home, dir)); !os.IsNotExist(err) {
			t.Fatal("status created", dir)
		}
	}
}
func TestCollectorKeepsLastPublicationOnUnchangedPass(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	now := time.Now()
	setUpTestConfig(t, home, project, now.Add(-time.Hour))
	env := testEnv(t, home, now)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "one", "cwd": project, "transcript_path": writeCodexTranscript(t, project)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	store := collector.OpenLocalStoreReadOnly(home)
	first, _ := store.LoadStatus()
	env.Now = func() time.Time { return now.Add(time.Minute) }
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	next, _ := store.LoadStatus()
	if next.LastPublishedAt.IsZero() || !next.LastPublishedAt.Equal(first.LastPublishedAt) {
		t.Fatal("lost last publication")
	}
}

// This child is only launched under a pseudo-terminal by the test below.
func TestSecretTerminalChild(t *testing.T) {
	if os.Getenv("ARCHIVE_SECRET_TEST_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	p := newPrompter(os.Stdin, os.Stdout)
	value, err := p.secret("Secret (hidden): ")
	if err != nil || value != "synthetic-terminal-secret" {
		t.Fatal("hidden input failed")
	}
}
func TestSecretInputDisablesTerminalEcho(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := `import os, pty, select, subprocess, sys, termios, time
master, slave = pty.openpty()
env = dict(os.environ, ARCHIVE_SECRET_TEST_CHILD='1')
p = subprocess.Popen([sys.argv[1], '-test.run=^TestSecretTerminalChild$'], stdin=slave, stdout=slave, stderr=slave, env=env)
try:
    deadline = time.monotonic() + 10
    output = b''
    while b'Secret (hidden): ' not in output:
        if time.monotonic() > deadline: raise RuntimeError('prompt timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    while termios.tcgetattr(slave)[3] & termios.ECHO:
        if time.monotonic() > deadline: raise RuntimeError('echo was not disabled')
        time.sleep(.01)
    os.write(master, b'synthetic-terminal-secret\n')
    while p.poll() is None:
        if time.monotonic() > deadline: raise RuntimeError('child timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    assert p.returncode == 0, output
    assert b'synthetic-terminal-secret' not in output, 'secret echoed'
    assert termios.tcgetattr(slave)[3] & termios.ECHO, 'terminal echo not restored'
finally:
    if p.poll() is None: p.kill(); p.wait()
    os.close(master); os.close(slave)
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", script, binary)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY test: %v %s", err, out)
	}
}

func TestPurgeConfirmationDoesNotBlockCapture(t *testing.T) {
	home, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()))
	input := &checkingReader{Reader: strings.NewReader("y\nn\n"), check: func() {
		// Prompts may wait indefinitely; only the wizard lock may be held here.
		unlock, err := local.Lock(home)
		if err != nil {
			t.Fatal("prompt held collector lock")
		}
		unlock()
	}}
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall", "--delete-local-data"}, input, &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
}

type checkingReader struct {
	*strings.Reader
	check func()
}

func (r *checkingReader) Read(b []byte) (int, error) { r.check(); return r.Reader.Read(b[:1]) }

func TestDraftStorageEditKeepsCaptureChoices(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	input := s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project)
	setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0)
	setupRun(t, env, "storage\ns3\nother-bucket\nprofile\ny\n", 0)
	cfg, _, _ := config.Load(home)
	if cfg.Storage.Bucket != "other-bucket" || len(cfg.Archive.Projects) != 1 || len(cfg.Harnesses) != 1 {
		t.Fatal("edit lost capture choices")
	}
}
func TestRestartRemovesOnlyStagedCredentials(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	kc := newFakeKeychain()
	env := setupTestEnv(t, home, t.TempDir(), kc, time.Now())
	input := r2SetupInput(project, "staged-secret")
	setupRun(t, env, strings.TrimSuffix(input, "y\n")+"n\n", 0)
	if len(kc.items) != 1 {
		t.Fatal("missing staged credential")
	}
	setupRun(t, env, "restart\n", 1) // cancel at the first capture prompt after discarding
	if len(kc.items) != 0 {
		t.Fatal("discarded draft leaked credential")
	}
}
func TestRetentionReductionShowsImpactBeforeConfirmation(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	now := time.Now().UTC()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), now)
	setupRun(t, env, s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, project), 0)
	if err := handleHookEvent(home, "codex", map[string]any{"hook_event_name": "SessionStart", "source": "startup", "session_id": "one", "cwd": project, "transcript_path": writeCodexTranscript(t, project)}, now); err != nil {
		t.Fatal(err)
	}
	if _, err := runOnePass(env, false); err != nil {
		t.Fatal(err)
	}
	env.Now = func() time.Time { return now.Add(45 * 24 * time.Hour) }
	output := setupRun(t, env, "retention\n30\nn\n", 0)
	if !strings.Contains(output, "1 currently captured session(s)") {
		t.Fatal(output)
	}
	cfg, _, _ := config.Load(home)
	if cfg.RetentionDays != 90 {
		t.Fatal("decline applied shorter retention")
	}
}

func TestUninstallCannotResumeRemovedIntegrations(t *testing.T) {
	_, _, env := installedFixture(t, newFakeKeychain(), s3SetupInput("test-bucket", "us-east-1", "profile", true, false, false, t.TempDir()))
	var out, errOut bytes.Buffer
	if code := Run([]string{"uninstall"}, strings.NewReader("y\n"), &out, &errOut, env); code != 0 {
		t.Fatal(errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"resume"}, nil, &out, &errOut, env); code != 1 || !strings.Contains(errOut.String(), "setup") {
		t.Fatal("resume claimed removed integrations were active")
	}
}

func TestSecretInterruptRestoresTerminalEcho(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("PTY harness requires Python 3")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := `import signal, os, pty, select, subprocess, sys, termios, time
master, slave = pty.openpty()
env = dict(os.environ, ARCHIVE_SECRET_TEST_CHILD='1')
p = subprocess.Popen([sys.argv[1], '-test.run=^TestSecretTerminalChild$'], stdin=slave, stdout=slave, stderr=slave, env=env)
try:
    deadline = time.monotonic() + 10
    output = b''
    while b'Secret (hidden): ' not in output:
        if time.monotonic() > deadline: raise RuntimeError('prompt timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    while termios.tcgetattr(slave)[3] & termios.ECHO:
        if time.monotonic() > deadline: raise RuntimeError('echo was not disabled')
        time.sleep(.01)
    p.send_signal(signal.SIGINT)
    while p.poll() is None:
        if time.monotonic() > deadline: raise RuntimeError('child timeout')
        if select.select([master], [], [], .1)[0]: output += os.read(master, 4096)
    assert p.returncode == 130, (p.returncode, output)
    assert b'synthetic-terminal-secret' not in output, 'secret echoed'
    assert termios.tcgetattr(slave)[3] & termios.ECHO, 'terminal echo not restored'
finally:
    if p.poll() is None: p.kill(); p.wait()
    os.close(master); os.close(slave)
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", script, binary)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY test: %v %s", err, out)
	}
}
