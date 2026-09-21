package cli

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/wangjohn/agent-skills/agent-archive/internal/archive"
	"github.com/wangjohn/agent-skills/agent-archive/internal/config"
	"github.com/wangjohn/agent-skills/agent-archive/internal/reader"
	"github.com/wangjohn/agent-skills/agent-archive/internal/storage"
)

// archiveSessionsPrefix is the provider-relative prefix every published
// session lives under; see archive.MetadataObjectKey and
// archive.SourceObjectKey, which both produce "sessions/<harness>/<id>/...".
const archiveSessionsPrefix = "sessions"

// notSetUpMessage is what every read-only command prints, with exit 0,
// when setup has never run. It is deliberately the same line `status`
// prints, so a first-time user gets one consistent answer.
const notSetUpMessage = "Not set up. Run `agent-archive setup` to get started."

// openReadOnlyStore loads configuration and opens the configured object
// store the same way a collector pass does (env.openStore), but without the
// machine lock or the pause check: `list` and `show` only read remote
// objects and never touch local collector state, so they may run alongside
// a scheduled `_collect` and while collection is paused. found is false,
// with a nil error, when setup has never run.
func openReadOnlyStore(env Env) (storage.ObjectStore, bool, error) {
	home, err := env.home()
	if err != nil {
		return nil, false, fmt.Errorf("resolve home: %w", err)
	}
	cfg, found, err := config.Load(home)
	if err != nil {
		return nil, false, fmt.Errorf("load config: %w", err)
	}
	if !found {
		return nil, false, nil
	}
	store, err := env.openStore(cfg)
	if err != nil {
		return nil, true, fmt.Errorf("open storage: %w", err)
	}
	return store, true, nil
}

// runListCommand implements `agent-archive list`. It reads only metadata
// sidecars (reader.ListMetadata downloads no source bundle) and prints only
// metadata fields, so its output can never contain transcript content.
func runListCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", "", "only sessions from this harness (codex, claude, cursor)")
	model := fs.String("model", "", "only sessions that requested or observed this model")
	skill := fs.String("skill", "", "only sessions involving this skill (see --skill-usage)")
	skillSHA256 := fs.String("skill-sha256", "", "only sessions involving this exact lowercase skill SHA-256")
	skillUsage := fs.String("skill-usage", string(reader.SkillUsageUsed), "with --skill/--skill-sha256: used, available, or eligible_no_use")
	since := fs.String("since", "", "only sessions captured at or after this date (2026-01-31), RFC 3339 time, or age (7d, 12h)")
	complete := fs.Bool("complete", false, "only sessions with complete parser coverage and no capture gaps")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "agent-archive: list: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *skillSHA256 != "" && !validLowerSHA256(*skillSHA256) {
		fmt.Fprintln(stderr, "agent-archive: list: --skill-sha256 must be exactly 64 lowercase hexadecimal characters")
		return 2
	}
	filter := reader.Filter{Harness: *harness, Model: *model, Skill: *skill, SkillSHA256: *skillSHA256, RequireCompleteCoverage: *complete}
	switch usage := reader.SkillUsage(*skillUsage); usage {
	case reader.SkillUsageUsed, reader.SkillUsageAvailable, reader.SkillUsageEligibleNoUse:
		filter.SkillUsage = usage
	default:
		fmt.Fprintf(stderr, "agent-archive: list: --skill-usage must be used, available, or eligible_no_use, not %q\n", *skillUsage)
		return 2
	}
	if *since != "" {
		from, err := parseSince(*since, env.now())
		if err != nil {
			fmt.Fprintf(stderr, "agent-archive: list: --since: %v\n", err)
			return 2
		}
		filter.From = from
	}

	store, found, err := openReadOnlyStore(env)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintln(stdout, notSetUpMessage)
		return 0
	}
	sessions, err := reader.ListMetadata(context.Background(), store, archiveSessionsPrefix, filter)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	if len(sessions) == 0 {
		fmt.Fprintln(stdout, "No archived sessions match.")
		if filter.SkillUsage == reader.SkillUsageEligibleNoUse {
			fmt.Fprintln(stdout, "Sessions without complete eligibility and use-observation evidence are excluded; an empty result does not prove no eligible sessions exist.")
		}
		return 0
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SESSION\tHARNESS\tCAPTURED\tPARSER\tMODELS\tSKILLS USED")
	for _, m := range sessions {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", m.SessionID, m.Harness.Name, formatTimeOrNever(m.CapturedAt), m.Parser.Status, listOrDash(modelNames(m)), listOrDash(skillNames(m)))
	}
	if err := tw.Flush(); err != nil {
		fmt.Fprintf(stderr, "agent-archive: list: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%d session(s).\n", len(sessions))
	if filter.SkillUsage == reader.SkillUsageEligibleNoUse {
		fmt.Fprintln(stdout, "Sessions without complete eligibility and use-observation evidence are excluded.")
	}
	return 0
}

func validLowerSHA256(value string) bool {
	if len(value) != 64 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// runShowCommand implements `agent-archive show <archive-session-id>`. By
// default it prints only the session's metadata sidecar. Conversation
// content — the normalized view derived from the verified source bundle —
// is printed only when the user passes --normalized explicitly, keeping the
// spec's rule that nothing prints transcript contents unless asked.
func runShowCommand(args []string, stdout, stderr io.Writer, env Env) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	harness := fs.String("harness", "", "the session's harness, if the same ID exists under more than one")
	normalized := fs.Bool("normalized", false, "also download, verify, and print the normalized conversation view (this prints transcript content)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, "agent-archive: show: an archive session ID is required (see `agent-archive list`)")
		return 2
	}
	sessionID := fs.Arg(0)
	// Accept flags after the positional ID too (`show ID --normalized`),
	// which the flag package otherwise stops parsing at.
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "agent-archive: show: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	store, found, err := openReadOnlyStore(env)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}
	if !found {
		fmt.Fprintln(stdout, notSetUpMessage)
		return 0
	}
	ctx := context.Background()
	key, err := locateMetadataKey(ctx, store, *harness, sessionID)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
		return 1
	}

	if !*normalized {
		metadata, err := reader.ReadMetadata(ctx, store, key)
		if err != nil {
			fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
			return 1
		}
		return printJSON(stdout, stderr, metadata)
	}

	metadata, bundle, err := reader.RefreshAndLoad(ctx, store, key, reader.Limits{})
	if err != nil {
		if errors.Is(err, reader.ErrRefreshRequired) {
			fmt.Fprintf(stderr, "agent-archive: show: the session's source bundle is not available (it may have just been replaced or deleted by retention); retry, or run `agent-archive show %s` without --normalized for its metadata\n", sessionID)
		} else {
			fmt.Fprintf(stderr, "agent-archive: show: %v\n", err)
		}
		return 1
	}
	view, err := archive.ParseNormalized(bundle)
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: normalized view unavailable: %v\n", err)
		return 1
	}
	if code := printJSON(stdout, stderr, metadata); code != 0 {
		return code
	}
	return printJSON(stdout, stderr, normalizedOutput{Turns: view.Turns, ToolCalls: view.ToolCalls, HookFinals: view.HookFinals})
}

// normalizedOutput is the JSON shape `show --normalized` prints for
// archive.NormalizedView, which carries no JSON tags of its own.
// NativeSkillUses is left out: it is an intermediate deriveSkills folds into
// the metadata's skills_used, which the sidecar printed first already shows.
type normalizedOutput struct {
	Turns      []archive.NormalizedTurn          `json:"turns"`
	ToolCalls  []archive.NormalizedToolCall      `json:"tool_calls"`
	HookFinals []archive.HookFinalReconciliation `json:"hook_finals"`
}

// locateMetadataKey resolves an archive session ID to its metadata sidecar
// key. With a harness the key is derived directly (one Get, no listing);
// without one the archive is listed for the ID, and the same ID published
// under more than one harness is reported as ambiguous rather than guessed.
func locateMetadataKey(ctx context.Context, store storage.ObjectStore, harness, sessionID string) (string, error) {
	if harness != "" {
		key, err := archive.MetadataObjectKey(harness, sessionID)
		if err != nil {
			return "", err
		}
		return key, nil
	}
	keys, err := reader.FindMetadataKeys(ctx, store, archiveSessionsPrefix, sessionID)
	if err != nil {
		return "", err
	}
	switch len(keys) {
	case 0:
		return "", fmt.Errorf("no archived session %q (see `agent-archive list`)", sessionID)
	case 1:
		return keys[0], nil
	}
	harnesses := make([]string, 0, len(keys))
	for _, key := range keys {
		harnesses = append(harnesses, strings.Split(strings.TrimPrefix(key, archiveSessionsPrefix+"/"), "/")[0])
	}
	return "", fmt.Errorf("session %q exists under more than one harness (%s); pass --harness", sessionID, strings.Join(harnesses, ", "))
}

func printJSON(stdout, stderr io.Writer, value any) int {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "agent-archive: show: encode output: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, string(data))
	return 0
}

// parseSince accepts a calendar date (2026-01-31, taken as midnight UTC), an
// RFC 3339 time, or an age relative to now written as a Go duration (12h,
// 90m) or in whole days (7d).
func parseSince(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if t, err := time.Parse(time.RFC3339, value); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		return t, nil
	}
	if strings.HasSuffix(value, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(value, "d")); err == nil && days >= 0 {
			return now.Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(value); err == nil && d >= 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, fmt.Errorf("%q is not a date (2026-01-31), an RFC 3339 time, or an age (7d, 12h)", value)
}

// modelNames returns the distinct model names a session's metadata
// attributes to it, whichever side (request or response) reported them.
func modelNames(m archive.Metadata) []string {
	return distinctSorted(func(add func(string)) {
		for _, model := range m.Models {
			add(model.Attributes["gen_ai.request.model"])
			add(model.Attributes["gen_ai.response.model"])
		}
	})
}

func skillNames(m archive.Metadata) []string {
	return distinctSorted(func(add func(string)) {
		for _, skill := range m.SkillsUsed {
			add(skill.Name)
		}
	})
}

func distinctSorted(collect func(add func(string))) []string {
	seen := map[string]bool{}
	var out []string
	collect(func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	})
	sort.Strings(out)
	return out
}

func listOrDash(names []string) string {
	if len(names) == 0 {
		return "-"
	}
	return strings.Join(names, ",")
}
