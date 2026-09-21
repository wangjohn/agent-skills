package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func TestShortSetupAndReviewEdits(t *testing.T) {
	for _, tc := range []struct {
		name, edits  string
		probes, days int
		prefix       string
	}{
		{"accept", "y\n", 1, 90, defaultPrefix},
		{"retention", "edit\nretention\n30\ny\n", 1, 30, defaultPrefix},
		{"folder", "edit\nprefix\n../invalid\narchive/\ny\n", 2, 90, "archive/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, project := t.TempDir(), t.TempDir()
			project, _ = filepath.EvalSymlinks(project)
			if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
			env.WorkingDir = func() (string, error) { return project, nil }
			env.AWSProfiles = func() ([]AWSProfile, error) { return []AWSProfile{{"personal", "us-west-2"}}, nil }
			probes := 0
			env.OpenStore = func(config.Config) (storage.ObjectStore, error) { probes++; return storage.NewMemoryStore(), nil }
			out := setupRun(t, env, "\n\ns3\ntest-bucket\n\n"+tc.edits, 0)
			cfg, found, err := config.Load(home)
			if err != nil || !found {
				t.Fatalf("load: %v", err)
			}
			if probes != tc.probes || cfg.RetentionDays != tc.days || cfg.Storage.Prefix != tc.prefix {
				t.Fatalf("probes=%d config=%+v", probes, cfg)
			}
			if cfg.Storage.Region != "us-west-2" || cfg.Storage.AWSProfile != "personal" || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != project {
				t.Fatalf("unexpected config: %+v", cfg)
			}
			for _, unwanted := range []string{"Change these settings?", "Bucket region (", "Project path"} {
				if strings.Contains(out, unwanted) {
					t.Fatalf("unexpected %q: %s", unwanted, out)
				}
			}
		})
	}
}

func TestDecliningSuggestedProjectUsesManualSelection(t *testing.T) {
	project, other := t.TempDir(), t.TempDir()
	other, _ = filepath.EvalSymlinks(other)
	if err := os.Mkdir(filepath.Join(project, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	env := setupTestEnv(t, t.TempDir(), t.TempDir(), newFakeKeychain(), time.Now())
	env.WorkingDir = func() (string, error) { return project, nil }
	env.DetectHarnesses = func(string) []string { return []string{"codex"} }
	var cfg config.Config
	var out bytes.Buffer
	err := chooseCapture(newPrompter(strings.NewReader("y\nn\n"+other+"\n\n"), &out), &cfg, t.TempDir(), env)
	if err != nil || len(cfg.Archive.Projects) != 1 || cfg.Archive.Projects[0].Root != other {
		t.Fatalf("config=%+v err=%v", cfg, err)
	}
}

func TestAWSProfileDiscoveryReadsSettingsWithoutRunningCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config")
	credsPath := filepath.Join(dir, "credentials")
	marker := filepath.Join(dir, "should-not-exist")
	data := "[default]\nregion = us-east-1\n[profile work]\nregion = eu-west-1\ncredential_process = touch " + marker + "\n[sso-session company]\nsso_region = us-west-2\n"
	if err := os.WriteFile(configPath, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credsPath, []byte("[legacy]\naws_access_key_id = synthetic\naws_secret_access_key = synthetic\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AWS_CONFIG_FILE", configPath)
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", credsPath)
	profiles, err := (Env{}).awsProfiles()
	want := []AWSProfile{{"default", "us-east-1"}, {"legacy", ""}, {"work", "eu-west-1"}}
	if err != nil || !reflect.DeepEqual(profiles, want) {
		t.Fatalf("profiles=%+v err=%v", profiles, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("credential process executed")
	}
}

func TestAWSProfileSwitchDoesNotReuseOldRegion(t *testing.T) {
	cfg := credentials.Config{AWSProfile: "old", Region: "us-east-1"}
	env := Env{AWSProfiles: func() ([]AWSProfile, error) { return []AWSProfile{{"new", ""}}, nil }}
	var out bytes.Buffer
	err := promptAWSProfile(newPrompter(strings.NewReader("new\neu-west-1\n"), &out), &cfg, env)
	if err != nil || cfg.Region != "eu-west-1" || cfg.AWSProfile != "new" {
		t.Fatalf("cfg=%+v err=%v", cfg, err)
	}
}

func TestStorageHelpReturnsToSelection(t *testing.T) {
	var out bytes.Buffer
	cfg, _, _, err := promptStorage(newPrompter(strings.NewReader("help\ns3\nbucket\nprofile\nus-east-1\n"), &out), credentials.Config{}, Env{AWSProfiles: func() ([]AWSProfile, error) { return nil, nil }})
	if err != nil || cfg.Provider != "s3" || !strings.Contains(out.String(), "https://developers.cloudflare.com/") {
		t.Fatalf("cfg=%+v err=%v output=%s", cfg, err, &out)
	}
}

func TestReviewEditCancellationDoesNotInstall(t *testing.T) {
	for _, tc := range []struct {
		name, ending string
		code         int
	}{
		{"cancel", "n\n", 0}, {"EOF", "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
			input := s3SetupInput("bucket", "us-east-1", "profile", true, false, false, t.TempDir())
			input = strings.TrimSuffix(input, "y\n") + "edit\nretention\n30\n" + tc.ending
			setupRun(t, env, input, tc.code)
			if _, found, err := config.Load(home); err != nil || found {
				t.Fatalf("installed without confirmation: found=%v err=%v", found, err)
			}
		})
	}
}

func TestManualProjectsExpandInjectedHomeAndDeduplicateSymlinks(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(home, "project")
	alias := filepath.Join(home, "alias")
	if err := os.Mkdir(project, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(project, alias); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	projects, err := promptProjects(newPrompter(strings.NewReader("~/project\n~/alias\n\n"), &out), nil, time.Time{}, home)
	canonical, _ := filepath.EvalSymlinks(project)
	if err != nil || len(projects) != 1 || projects[0].Root != canonical {
		t.Fatalf("projects=%+v err=%v", projects, err)
	}
}
