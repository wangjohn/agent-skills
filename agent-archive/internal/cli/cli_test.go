package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/credentials"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

func testEnv(t *testing.T, home string, now time.Time) Env {
	t.Helper()
	userHome := t.TempDir()
	return Env{
		UserHomeDir: func() (string, error) { return userHome, nil },
		Home:        func() (string, error) { return home, nil },
		Now:         func() time.Time { return now },
		OpenStore: func(config.Config) (storage.ObjectStore, error) {
			return storage.NewMemoryStore(), nil
		},
	}
}

// credentialsTestConfig is a syntactically valid storage destination for
// tests that never actually touch storage (they use OpenStore above, or
// test hook logic that writes only local files).
func credentialsTestConfig() credentials.Config {
	return credentials.Config{Provider: credentials.ProviderS3, Bucket: "test-bucket", Region: "us-east-1", AWSProfile: "test", Prefix: "agent-archive/"}
}

func TestHelpAndVersion(t *testing.T) {
	var out bytes.Buffer
	if code := Run([]string{"--help"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(out.String(), "agent-archive setup") {
		t.Fatalf("help output missing usage: %s", out.String())
	}

	out.Reset()
	if code := Run([]string{"--version"}, nil, &out, nil, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	if strings.TrimSpace(out.String()) != Version {
		t.Fatalf("version output=%q want=%q", out.String(), Version)
	}
}

func TestUnknownCommandAndNoArgs(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"bogus"}, nil, &out, &errOut, Env{}); code != 2 {
		t.Fatalf("code=%d", code)
	}
	if !strings.Contains(errOut.String(), `unknown command "bogus"`) {
		t.Fatalf("stderr=%q", errOut.String())
	}

	errOut.Reset()
	if code := Run(nil, nil, &out, &errOut, Env{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
}
