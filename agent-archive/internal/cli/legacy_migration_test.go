package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
