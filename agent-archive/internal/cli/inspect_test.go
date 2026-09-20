package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// publishedFixture registers one Codex session through the hook path and
// publishes it with `sync` into a single in-memory store that the returned
// Env keeps handing back, so `list`/`show` read what `sync` wrote. It
// returns the archive session ID `list`/`show` address the session by.
func publishedFixture(t *testing.T) (Env, *storage.MemoryStore, string) {
	t.Helper()
	home := t.TempDir()
	dir := t.TempDir()
	setUpTestConfig(t, home, dir, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	transcript := writeCodexTranscript(t, dir)
	now := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	payload := map[string]any{"hook_event_name": "SessionStart", "session_id": "native-1", "cwd": dir, "transcript_path": transcript}
	if err := handleHookEvent(home, "codex", payload, now); err != nil {
		t.Fatal(err)
	}
	mem := storage.NewMemoryStore()
	env := testEnv(t, home, now)
	env.OpenStore = func(config.Config) (storage.ObjectStore, error) { return mem, nil }
	var stdout, stderr bytes.Buffer
	if code := runSyncCommand(nil, &stdout, &stderr, env); code != 0 {
		t.Fatalf("sync code=%d stderr=%s", code, stderr.String())
	}
	local, err := collector.NewLocalStore(home)
	if err != nil {
		t.Fatal(err)
	}
	regs, err := local.LoadRegistrations()
	if err != nil || len(regs) != 1 {
		t.Fatalf("regs=%#v err=%v", regs, err)
	}
	return env, mem, regs[0].ArchiveSessionID
}

func TestListAndShowReportNotSetUp(t *testing.T) {
	env := testEnv(t, t.TempDir(), time.Now())
	for _, args := range [][]string{{"list"}, {"show", "session-1"}} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut.String())
		}
		if !strings.Contains(out.String(), "Not set up") {
			t.Fatalf("%v: out=%s", args, out.String())
		}
	}
}

func TestListShowsPublishedSessionMetadataOnly(t *testing.T) {
	env, _, id := publishedFixture(t)
	var out, errOut bytes.Buffer
	if code := Run([]string{"list"}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	for _, want := range []string{id, "codex", "gpt-test", "2026-01-02T00:00:00Z", "1 session(s)."} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("list output missing %q:\n%s", want, out.String())
		}
	}
	// "visible" is the assistant message text in writeCodexTranscript; a
	// listing must never surface transcript content.
	if strings.Contains(out.String(), "visible") {
		t.Fatalf("list printed transcript content:\n%s", out.String())
	}
}

func TestListFilters(t *testing.T) {
	env, _, id := publishedFixture(t)
	cases := []struct {
		args  []string
		match bool
	}{
		{[]string{"list", "--harness", "codex"}, true},
		{[]string{"list", "--harness", "cursor"}, false},
		{[]string{"list", "--model", "gpt-test"}, true},
		{[]string{"list", "--model", "other-model"}, false},
		{[]string{"list", "--skill", "review"}, false},
		{[]string{"list", "--since", "2026-01-02"}, true},
		{[]string{"list", "--since", "2026-01-03"}, false},
		{[]string{"list", "--since", "1d"}, true},
		{[]string{"list", "--since", "2026-01-02T00:00:01Z"}, false},
		// The fixture's parser status is "partial" (BuildMetadata's default),
		// so requiring complete coverage excludes it.
		{[]string{"list", "--complete"}, false},
	}
	for _, c := range cases {
		var out, errOut bytes.Buffer
		if code := Run(c.args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", c.args, code, errOut.String())
		}
		if got := strings.Contains(out.String(), id); got != c.match {
			t.Fatalf("%v: matched=%v want=%v\n%s", c.args, got, c.match, out.String())
		}
		if !c.match && !strings.Contains(out.String(), "No archived sessions match.") {
			t.Fatalf("%v: expected the no-match line:\n%s", c.args, out.String())
		}
	}
}

func TestListRejectsBadArguments(t *testing.T) {
	env, _, _ := publishedFixture(t)
	for _, args := range [][]string{
		{"list", "--since", "yesterday"},
		{"list", "--skill-usage", "sometimes"},
		{"list", "extra"},
		{"list", "--bogus"},
		{"show"},
		{"show", "id", "extra"},
	} {
		var out, errOut bytes.Buffer
		if code := Run(args, nil, &out, &errOut, env); code != 2 {
			t.Fatalf("%v: code=%d stdout=%s stderr=%s", args, code, out.String(), errOut.String())
		}
	}
}

func TestShowPrintsMetadataAndOnlyPrintsContentWithNormalizedFlag(t *testing.T) {
	env, _, id := publishedFixture(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var metadata archive.Metadata
	if err := json.Unmarshal(out.Bytes(), &metadata); err != nil {
		t.Fatalf("show output is not one metadata document: %v\n%s", err, out.String())
	}
	if metadata.SessionID != id || metadata.SourceBundle.Key == "" {
		t.Fatalf("metadata=%#v", metadata)
	}
	if strings.Contains(out.String(), "visible") {
		t.Fatalf("show without --normalized printed transcript content:\n%s", out.String())
	}

	for _, args := range [][]string{{"show", id, "--normalized"}, {"show", "--normalized", "--harness", "codex", id}} {
		out.Reset()
		errOut.Reset()
		if code := Run(args, nil, &out, &errOut, env); code != 0 {
			t.Fatalf("%v: code=%d stderr=%s", args, code, errOut.String())
		}
		decoder := json.NewDecoder(bytes.NewReader(out.Bytes()))
		var first archive.Metadata
		var second normalizedOutput
		if err := decoder.Decode(&first); err != nil {
			t.Fatalf("%v: first document: %v\n%s", args, err, out.String())
		}
		if err := decoder.Decode(&second); err != nil {
			t.Fatalf("%v: second document: %v\n%s", args, err, out.String())
		}
		if first.SessionID != id {
			t.Fatalf("%v: metadata=%#v", args, first)
		}
		if len(second.Turns) != 1 || second.Turns[0].Role != "assistant" || second.Turns[0].Text != "visible" {
			t.Fatalf("%v: normalized view=%#v", args, second)
		}
	}
}

func TestShowReportsUnknownAndUnavailableSessions(t *testing.T) {
	env, mem, id := publishedFixture(t)

	var out, errOut bytes.Buffer
	if code := Run([]string{"show", "no-such-session"}, nil, &out, &errOut, env); code != 1 {
		t.Fatalf("code=%d stdout=%s", code, out.String())
	}
	if !strings.Contains(errOut.String(), `no archived session "no-such-session"`) {
		t.Fatalf("stderr=%s", errOut.String())
	}

	// A wrong --harness derives a key that does not exist.
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", "--harness", "cursor", id}, nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}

	// Metadata stays readable after its source is gone, but --normalized
	// must fail closed with an actionable message and print no content.
	metadata, err := readSingleMetadata(t, mem)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Delete(context.Background(), metadata.SourceBundle.Key); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", id}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("metadata-only show should still work: code=%d stderr=%s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", id, "--normalized"}, nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "source bundle is not available") {
		t.Fatalf("stderr=%s", errOut.String())
	}
}

func TestShowRefusesAmbiguousSessionWithoutHarness(t *testing.T) {
	env, mem, id := publishedFixture(t)
	metadata, err := readSingleMetadata(t, mem)
	if err != nil {
		t.Fatal(err)
	}
	// Publish a copy of the same session ID under a second harness.
	metadata.Harness.Name = "cursor"
	data, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	key, err := archive.MetadataObjectKey("cursor", id)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Put(context.Background(), key, data); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"show", id}, nil, &out, &errOut, env); code != 1 || out.Len() != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, out.String(), errOut.String())
	}
	if !strings.Contains(errOut.String(), "codex, cursor") || !strings.Contains(errOut.String(), "--harness") {
		t.Fatalf("stderr=%s", errOut.String())
	}
	out.Reset()
	errOut.Reset()
	if code := Run([]string{"show", "--harness", "cursor", id}, nil, &out, &errOut, env); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	if !strings.Contains(out.String(), `"name": "cursor"`) {
		t.Fatalf("expected the cursor copy: %s", out.String())
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"2026-03-01":           time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		"2026-03-01T06:30:00Z": time.Date(2026, 3, 1, 6, 30, 0, 0, time.UTC),
		"7d":                   now.Add(-7 * 24 * time.Hour),
		"12h":                  now.Add(-12 * time.Hour),
	}
	for input, want := range cases {
		got, err := parseSince(input, now)
		if err != nil || !got.Equal(want) {
			t.Fatalf("%q: got %v err=%v want %v", input, got, err, want)
		}
	}
	for _, input := range []string{"", "yesterday", "-7d", "-1h", "3 days"} {
		if _, err := parseSince(input, now); err == nil {
			t.Fatalf("%q: expected an error", input)
		}
	}
}

// readSingleMetadata returns the one metadata sidecar publishedFixture wrote.
func readSingleMetadata(t *testing.T, mem *storage.MemoryStore) (archive.Metadata, error) {
	t.Helper()
	objects, err := mem.List(context.Background(), "sessions")
	if err != nil {
		return archive.Metadata{}, err
	}
	for _, object := range objects {
		if strings.HasSuffix(object.Key, "/metadata.json") {
			data, err := mem.Get(context.Background(), object.Key)
			if err != nil {
				return archive.Metadata{}, err
			}
			var metadata archive.Metadata
			return metadata, json.Unmarshal(data, &metadata)
		}
	}
	t.Fatal("no metadata sidecar in store")
	return archive.Metadata{}, nil
}
