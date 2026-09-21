package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAppSelectionSuggestionsAndManualFallback(t *testing.T) {
	tests := []struct {
		name               string
		detected, existing []string
		input              string
		want               []string
		prompt             string
		manual             bool
	}{
		{"accept detected", []string{"claude", "codex", "codex"}, nil, "\n", []string{"codex", "claude"}, "Include Codex and Claude Code? [Y/n]", false},
		{"single app", []string{"cursor"}, nil, "y\n", []string{"cursor"}, "Include Cursor? [Y/n]", false},
		{"choose another app", []string{"codex", "claude"}, nil, "n\nn\nn\ny\n", []string{"cursor"}, "Include Codex and Claude Code?", true},
		{"nothing detected", nil, nil, "n\ny\nn\n", []string{"claude"}, "No apps found automatically.", true},
		{"keep prior selection", []string{"codex", "cursor"}, []string{"claude"}, "\n", []string{"claude"}, "Keep Claude Code?", false},
		{"retry empty selection", nil, nil, "n\nn\nn\ny\nn\nn\n", []string{"codex"}, "Choose at least one app to continue.", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			got, err := promptHarnesses(newPrompter(strings.NewReader(tt.input), &out), tt.detected, tt.existing)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
			if !strings.Contains(out.String(), tt.prompt) {
				t.Fatalf("missing prompt: %s", &out)
			}
			if strings.Contains(out.String(), "Choose which apps to include:") != tt.manual {
				t.Fatalf("incorrect manual fallback: %s", &out)
			}
		})
	}
}

func TestDetectedAppsSetupSkipsIndividualQuestions(t *testing.T) {
	home, project := t.TempDir(), t.TempDir()
	env := setupTestEnv(t, home, t.TempDir(), newFakeKeychain(), time.Now())
	env.DetectHarnesses = func(string) []string { return []string{"codex", "claude"} }
	input := strings.Join([]string{"y", project, "", "s3", "test-bucket", "profile", "us-east-1", "y"}, "\n") + "\n"
	output := setupRun(t, env, input, 0)
	for _, unwanted := range []string{"Detected settings", "capture policy", "Include Cursor?", "Include Codex?"} {
		if strings.Contains(output, unwanted) {
			t.Fatalf("unexpected %q in %s", unwanted, output)
		}
	}
	for _, want := range []string{"Include Codex and Claude Code?", "Save new sessions, with or without skills.", "Automatically delete archived sessions after 90 days.", "Start archiving? [Y/n/edit]"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in %s", want, output)
		}
	}
}
