package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/hooks"
	"github.com/wangjohn/agent-skills/agent-archive/internal/local"
)

const legacyPlist = `<?xml version="1.0"?><plist><dict><key>Label</key><string>com.agent-skills.skill-runs-upload</string><key>ProgramArguments</key><array><string>/usr/bin/python3</string><string>/private/runtime/skill_runs.py</string><string>--home</string><string>/private/records</string><string>upload</string></array></dict></plist>`

func TestSetupMigratesLegacyJobAndRestoresOnFailure(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[fail], func(t *testing.T) {
			home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
			env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
			path := filepath.Join(userHome, "Library", "LaunchAgents", legacyLaunchLabel+".plist")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(legacyPlist), 0600); err != nil {
				t.Fatal(err)
			}
			states := map[string]string{path: "loaded"}
			unloaded := false
			env.JobState = func(p string) string {
				if s := states[p]; s != "" {
					return s
				}
				return "missing"
			}
			env.UnloadLaunchAgent = func(p string) error {
				states[p] = "missing"
				if p == path {
					unloaded = true
				}
				return nil
			}
			env.LoadLaunchAgent = func(p string) error {
				if fail && p != path {
					return errors.New("start failed")
				}
				states[p] = "loaded"
				return nil
			}
			want := 0
			if fail {
				want = 1
			}
			setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), want)
			if !unloaded {
				t.Fatal("legacy job not stopped")
			}
			data, err := os.ReadFile(path)
			if fail {
				if err != nil || string(data) != legacyPlist || states[path] != "loaded" {
					t.Fatalf("not restored: %s %v %v", data, err, states)
				}
			} else if !os.IsNotExist(err) || states[path] != "missing" {
				t.Fatalf("not retired: %v %v", err, states)
			}
		})
	}
}

func TestLegacyMigrationRejectsUnownedJob(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "Library", "LaunchAgents", legacyLaunchLabel+".plist")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data := strings.Replace(legacyPlist, "skill_runs.py", "unrelated.py", 1)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := planLegacyMigration(home, Env{JobState: func(string) string { return "loaded" }}); err == nil {
		t.Fatal("accepted unowned job")
	}
	after, _ := os.ReadFile(path)
	if string(after) != data {
		t.Fatal("unowned job changed")
	}
}

func TestFreshSetupToleratesUnknownLaunchctlWithoutLegacyJob(t *testing.T) {
	home, userHome, project := t.TempDir(), t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	env.JobState = func(string) string { return "unknown" }
	if job, err := planLegacyMigration(userHome, env); err != nil || job != nil {
		t.Fatalf("no legacy plist must not need launchctl: job=%+v err=%v", job, err)
	}
	setupRun(t, env, s3SetupInput("bucket", "us-east-1", "profile", true, false, false, project), 0)
	if _, found, err := config.Load(home); err != nil || !found {
		t.Fatalf("fresh setup blocked by unknown launchctl state: found=%v err=%v", found, err)
	}
	if transactionPending(home) {
		t.Fatal("journal left behind")
	}
}

// A crash after the legacy job was retired and the new collector's files
// were written, but before the collector was started, leaves a journal with
// a Legacy entry. Recovery must restore and reload the prototype job and
// remove the half-installed collector.
func TestRecoverSetupReplaysLegacyJournal(t *testing.T) {
	home, userHome := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, userHome, newFakeKeychain(), time.Now())
	legacyPath := filepath.Join(userHome, "Library", "LaunchAgents", legacyLaunchLabel+".plist")
	plistPath := filepath.Join(userHome, "Library", "LaunchAgents", hooks.LaunchLabel+".plist")
	states := map[string]string{}
	var loaded []string
	env.JobState = func(p string) string {
		if s := states[p]; s != "" {
			return s
		}
		return "missing"
	}
	env.LoadLaunchAgent = func(p string) error { states[p] = "loaded"; loaded = append(loaded, p); return nil }
	env.UnloadLaunchAgent = func(p string) error { states[p] = "missing"; return nil }
	// Simulated crash state: legacy plist already removed and unloaded, the
	// new plist and config written but the collector never started.
	if err := os.MkdirAll(filepath.Dir(plistPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("new plist"), 0600); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "config.json")
	if err := os.WriteFile(configPath, []byte("new config"), 0600); err != nil {
		t.Fatal(err)
	}
	journal := setupJournal{
		Legacy:  &legacyJob{Change: hooks.Change{Path: legacyPath, Before: []byte(legacyPlist), Existed: true, Mode: 0600}, WasLoaded: true},
		Changes: []hooks.Change{{Path: plistPath, After: []byte("new plist"), Mode: 0600}, {Path: configPath, After: []byte("new config"), Mode: 0600}},
		Plist:   plistPath,
	}
	if err := local.Write(journalPath(home), journal); err != nil {
		t.Fatal(err)
	}
	if err := recoverSetup(home, env); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(legacyPath); err != nil || string(data) != legacyPlist {
		t.Fatalf("legacy job not restored: %q %v", data, err)
	}
	if states[legacyPath] != "loaded" || len(loaded) != 1 || loaded[0] != legacyPath {
		t.Fatalf("legacy job not reloaded (and nothing else started): states=%v loaded=%v", states, loaded)
	}
	for _, p := range []string{plistPath, configPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", p, err)
		}
	}
	if transactionPending(home) {
		t.Fatal("journal not removed")
	}
	// Recovery is idempotent once the journal is gone.
	if err := recoverSetup(home, env); err != nil {
		t.Fatal(err)
	}
}
