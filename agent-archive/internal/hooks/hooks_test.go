package hooks

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestMergePreservesAndIsIdempotent(t *testing.T) {
	for _, app := range []string{"codex", "claude", "cursor"} {
		t.Run(app, func(t *testing.T) {
			original := []byte(`{"unrelated":true,"hooks":{}}`)
			first, e := Merge(original, app, "/Applications/Agent Archive/bin/agent-archive")
			if e != nil {
				t.Fatal(e)
			}
			second, e := Merge(first, app, "/Applications/Agent Archive/bin/agent-archive")
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("duplicate hooks on setup rerun")
			}
			var value map[string]any
			json.Unmarshal(first, &value)
			if value["unrelated"] != true {
				t.Fatal("lost setting")
			}
			if bytes.Contains(first, []byte("PostToolUse")) {
				t.Fatal("per-tool hook installed")
			}
		})
	}
}
func TestPreserveUnrelatedHandler(t *testing.T) {
	data, e := Merge([]byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"other"}]}]}}`), "codex", "/bin/agent-archive")
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Contains(data, []byte(`"command": "other"`)) {
		t.Fatal("removed unrelated handler")
	}
}
func TestInvalidConfigIsNotOverwritten(t *testing.T) {
	for _, s := range []string{`null`, `[]`, `{"hooks":42}`, `{"hooks":{"Stop":[{}]}}`} {
		if _, e := Merge([]byte(s), "codex", "/bin/archive"); e == nil {
			t.Fatalf("accepted %s", s)
		}
	}
}

func TestMigratePrototypeOnly(t *testing.T) {
	raw := []byte(`{"hooks":{"PostToolUse":[{"hooks":[{"type":"command","command":"python old.py hook","statusMessage":"Recording private skill-run evidence"},{"type":"command","command":"echo keep"}]}]}}`)
	result, e := Merge(raw, "codex", "/tmp/agent-archive")
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(result), "old.py") || !strings.Contains(string(result), "echo keep") {
		t.Fatal(string(result))
	}
}
