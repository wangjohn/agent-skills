# Agent Archive implementation and verification ledger

Source of requirements: [engineering specification](agent-run-archive-spec.md).

Status: implementation in progress. Passing synthetic tests does not establish live application or cloud compatibility. Entries remain incomplete until reviewed evidence exists.

## PR boundaries

| Slice | Requirements | Evidence required | Status |
| --- | --- | --- | --- |
| Foundation | Versioned metadata/source schemas; small module; fixture corpus | Schema examples, synthetic parser fixtures | In progress |
| Capture | Activation cutoff, project inclusion, safe native filtering, source provenance, size limits, unknown fields, model/skill gaps | Secret, hidden-content, old-session, incomplete-line tests | In progress |
| Snapshots | Deterministic bytes/hash, timestamps, parser regeneration, hook message reconciliation | Unchanged scan and source-preservation tests | In progress |
| Storage | R2/S3 SDK, explicit profile, Keychain, integrity, write-before-pointer, synthetic permissions probe | Fake endpoint + real provider round trips | In progress — code complete and now wired end to end through `agent-archive setup`, which calls `VerifyAccess` against whatever the user configures. Only an actual live R2/S3 credential exercising that path is unverified |
| Collector | Registration, hook-only evidence, scan cadence, queue, lock, retries, delayed final, compaction, disk failures | Crash/restart/ownership tests | In progress (PR in review)\*\* |
| CLI | setup/status/sync/pause/resume/help/version; actionable errors | CLI scenario tests | Done (PR #8) — `cmd/agent-archive` plus `_hook`/`_collect`/`status`/`sync`/`pause`/`resume` all implemented and tested; `setup` was a stub in #8, now real (see below) |
| Setup | Existing bucket, hidden secrets, app selection, projects, consent, no history import, activation preservation | Reconfiguration/cancel/rollback tests | Done (PR #9)\*\*\* |
| Hooks | Three harnesses, preserve unrelated hooks, trust remains explicit, prototype migration | Merge/idempotency tests, live lifecycle tests | In progress (PR #5, wired up in #8/#9) — config merge/install/rollback done and tested, and now actually invoked by `_hook`/`setup` instead of only existing as generated strings; live install against a real harness is still unverified |
| Scheduling | Login LaunchAgent, absolute runtime path, background auth, persistent pause | Plist and fresh-process tests | In progress (PR #5, wired up in #9) — plist generation, writing, and a `launchctl bootstrap` load attempt are now all invoked by `setup`; a real `launchctl`/launchd round trip remains unverified (not testable in this environment) |
| Metadata | Harness/version/settings; requested vs response model; skills installed vs discovered; evidence and gaps; counts | Mixed-model/no-skill fixtures | Done (PR #6) |
| Reader | Metadata filtering, selected-source download, hash verification, on-demand normalized view, feedback provenance | Read-back and filter tests | Done (PR #6)\* |
| Retention | Current/predecessor, grace period, whole-session deletion, same-machine ownership, pending-pointer safety | Expiry/race tests | Done (PR #10)\*\*\*\* |
| Distribution | Intel and ARM executables, checksums, signing/notarization, docs | Build artifacts and release workflow | Pending |
| System verification | Two Macs, two providers, each installed harness; enabled/disabled latency | Live smoke records, timing report | Pending external access |

\* Reader covers metadata filtering (harness, model, skill, coverage, eligibility), verified bounded source reads, and refresh-on-deletion recovery, each with tests. It does not yet surface individual explicit-feedback items with their own provenance to readers; `Metadata.Counts.ExplicitFeedback` is a count only. Closing that gap is follow-up work, not blocking.

\*\* Builds on PR #5's `internal/local` (private home directory, atomic file I/O, flock-based machine lock) rather than re-implementing them; adds the scan/build/publish loop, change detection against a local per-session cache (so an unchanged transcript costs no storage write and never gets a new `captured_at`), and a per-session minimum upload interval. Does not yet implement snapshot cleanup/retention or a parser-upgrade-only republish path — both explicitly deferred to the Retention slice and a later pass, respectively. (PR #9 added `Options.RequireSkillUse`, a third, distinctly non-retriable cache status alongside "published" and "rate-limited" — a session policy-declined for having no detected skill use is reconsidered only by an actual further content change, never by time alone.)

\*\*\* Detects applications by checking for the same `.codex`/`.claude`/`.cursor` directories `hooks.Plan` already targets (best-effort: existence is not proof of a current install, and the reverse); prompts for storage, applications, and projects, preserving existing project activation times and stored R2 credentials on reconfiguration; and only leaves anything behind (Keychain secrets, hooks, LaunchAgent plist, `internal/config`) once setup actually finishes, so a decline or a failure at any earlier step leaves a prior configuration untouched. Two exceptions this required deliberate rollback for, both found by `review-pr` and fixed rather than left as gaps: an R2 secret has to be saved to Keychain before `VerifyAccess` can read it back to probe the bucket, so a verification failure (or anything after it) now deletes that freshly-written secret again, never touching a reused existing one; and hooks/the LaunchAgent are installed before the final `config.Save`, so a `config.Save` failure now unloads and removes the LaunchAgent and rolls back the hooks, rather than leaving both live with no config behind them. The `launchctl bootstrap`/`bootout` calls and installed-application detection are both real but unverified against a live macOS install — this environment cannot run them for real, only exercise the code paths with fakes.

\*\*\*\* `internal/retention.Sweep` runs as an additional pass after every successful `collector.Run` (wired into `_collect`/`sync` via `internal/cli/collect.go`), driven by an append-only local ledger of superseded (no-longer-current) source keys recorded in `internal/collector/lineage.go` whenever a republish changes a session's current source — rather than a single-predecessor-slot pointer, so rapid successive republishes before a sweep ever runs cannot leak an untracked orphan. A superseded source is deleted once it has been superseded for longer than a 24-hour grace period; the currently referenced source is never a candidate regardless of age. Whole-session deletion triggers once a session's most recently captured evidence is older than `Config.RetentionDays` (90-day default), deleting every source snapshot for that session before its metadata pointer — so an interruption partway leaves at worst a metadata pointer to an already-deleted source, the exact case `reader.LoadSource`'s existing `ErrRefreshRequired`/`RefreshAndLoad` retry (PR #6) already handles safely, never the reverse. Only the machine that registered a session ever sweeps it, since registrations are local-only. One session's sweep failure is isolated and left retryable on the next pass, matching `collector.Run`'s own error-isolation model.

Evaluate Skill authoring and controlled evaluation runner are explicitly separate work in the engineering spec. This implementation must provide their reader/data interface, not silently omit it or claim the evaluation skill itself exists.

## Remaining PR sequence to completion

With PR #7 (Collector) in review, this is the full remaining sequence to close every Pending or partially-done row above, in dependency order. Each PR is scoped to what the spec's own component boundaries and this repo's existing PR granularity suggest; none of it needs to start from zero; the code inventory below is what each PR builds on, not what it still has to write.

**PR #8 — CLI foundation — done, in review.** `cmd/agent-archive`, `internal/cli`'s `_hook`/`_collect`/`status`/`sync`/`pause`/`resume`, and `internal/config` all landed as described below.

**PR #9 — Setup — done, in review.** `agent-archive setup`'s guided flow landed as described below; it also closed the Storage row's live-round-trip gap by being the thing that actually calls `VerifyAccess` with real configuration.

**PR #10 — Retention — done, in review.** `internal/retention` and its `_collect`/`sync` wiring landed as described above.

**PR #11 — Distribution**
A GitHub Actions release workflow: build Intel and ARM64 executables, checksum them, run macOS codesign and notarization, and publish install docs. The workflow and tooling are buildable and reviewable now; producing an actually signed, notarized artifact is blocked on the Apple credentials already listed below as unavailable to this environment — that part of this PR can't close until those exist.

**Not a PR — System verification**
Two Macs, two providers, each installed harness, and an enabled/disabled latency comparison. This is a live-access milestone gated on the same external prerequisites listed below, not code to write.

**Ongoing, not a dedicated PR — Foundation, Capture, Snapshots**
These keep hardening opportunistically as new harness versions or edge cases surface (every PR so far has touched at least one of them alongside its main deliverable) rather than needing a PR of their own.

**Out of scope — Evaluate Skill authoring and its benchmark runner**
Explicitly separate work per the spec. The Reader slice (PR #6) already provides the data interface it will need; no PR for the skill itself belongs in this sequence.

**Correction:** an earlier version of this section recommended Collector as "the next PR" without having read `internal/hooks` or `internal/local`, and so incorrectly described Hooks and Scheduling as having no code behind them. The first draft of the Collector implementation also duplicated `internal/local`'s atomic-write and lock logic with a weaker approach (a PID-staleness heuristic instead of a real `flock`) before this was caught and fixed to build on `internal/local` directly. This note exists so the mistake doesn't get repeated silently.

## Current external verification prerequisites

- Cursor is not installed on this Mac; Cursor fixture coverage cannot be labeled live-verified.
- R2/S3 test destinations and credentials are pending user configuration. Do not send private sessions to a test endpoint.
- Apple signing/notarization credentials and the second Mac are not yet available to this run.

## Review policy

Root reviews each implementation diff and test evidence before a PR becomes merge-ready. At most two implementation agents run, each in an isolated worktree. Dependent PRs target their prerequisite; independent slices share only reviewed contracts. Never include the old uncommitted Python prototype accidentally.
