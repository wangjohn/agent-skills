package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/collector"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
)

const maxFeedbackBytes = 64 << 10

// runFeedbackCommand records a user's explicit assessment as supplemental
// evidence. The input path is local operational data and is never archived;
// only privacy-filtered file content and fixed provenance reach the durable
// request consumed by the next collector pass.
func runFeedbackCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("feedback", flag.ContinueOnError)
	fs.SetOutput(stderr)
	file := fs.String("file", "", "read explicit feedback from this local UTF-8 text file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "agent-archive: feedback: an archive session ID is required")
		return 2
	}
	sessionID := fs.Arg(0)
	// Accept the documented `feedback ID --file PATH` form as well as flags
	// before ID; flag.FlagSet otherwise stops at the first positional value.
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "agent-archive: feedback: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *file == "" {
		fmt.Fprintln(stderr, "agent-archive: feedback: --file PATH is required")
		return 2
	}
	if sessionID == "" || sessionID == "." || sessionID == ".." || strings.ContainsAny(sessionID, "/\\") {
		fmt.Fprintln(stderr, "agent-archive: feedback: invalid archive session ID")
		return 2
	}

	content, err := readFeedbackFile(*file)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: %v\n", err)
		return 1
	}
	home, err := env.home()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: resolve home: %v\n", err)
		return 1
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: load config: %v\n", err)
		return 1
	} else if !found || !cfg.Archive.Enabled {
		fmt.Fprintln(stderr, "agent-archive: feedback: not set up yet; run `agent-archive setup` first")
		return 1
	}
	store, err := collector.NewLocalStore(home)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: open local store: %v\n", err)
		return 1
	}
	registration, found, err := store.LoadRegistration(sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: %v\n", err)
		return 1
	} else if !found {
		fmt.Fprintf(stderr, "agent-archive: feedback: no local session %q\n", sessionID)
		return 1
	}
	if !cfg.AcceptSession(registration) {
		fmt.Fprintf(stderr, "agent-archive: feedback: session %q is no longer eligible under the current setup\n", sessionID)
		return 1
	}
	now := env.now()
	eventID, err := feedbackEventID()
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: create evidence identity: %v\n", err)
		return 1
	}
	filtered, gaps, err := archive.FilterSupplementalEvidence([]archive.SupplementalEvidence{{
		Kind: archive.EvidenceKindExplicitFeedback, ObservedAt: now,
		Provenance: "user:agent-archive-feedback-file",
		Payload:    map[string]any{"event_id": eventID, "text": string(content), "source": "user"},
	}})
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: filter input: %v\n", err)
		return 1
	}
	if len(filtered) != 1 {
		fmt.Fprintln(stderr, "agent-archive: feedback: input contained no retainable text")
		return 1
	}
	archive.AnnotateSupplementalGaps(filtered[0].Payload, gaps)
	if err := store.SaveRequest(sessionID, "explicit_feedback", now, filtered[0]); err != nil {
		fmt.Fprintf(stderr, "agent-archive: feedback: save: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Feedback queued for session %s.\n", sessionID)
	return 0
}

func feedbackEventID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func readFeedbackFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read --file: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect --file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("--file must name a regular file")
	}
	content, err := io.ReadAll(io.LimitReader(file, maxFeedbackBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read --file: %w", err)
	}
	if len(content) > maxFeedbackBytes {
		return nil, fmt.Errorf("--file exceeds %d bytes", maxFeedbackBytes)
	}
	if strings.TrimSpace(string(content)) == "" {
		return nil, errors.New("--file is empty")
	}
	return content, nil
}
