# Agent Archive implementation and verification ledger

Source of requirements: [engineering specification](agent-run-archive-spec.md).

Status: implementation in progress. Passing synthetic tests does not establish live application or cloud compatibility. Entries remain incomplete until reviewed evidence exists.

## PR boundaries

| Slice | Requirements | Evidence required | Status |
| --- | --- | --- | --- |
| Foundation | Versioned metadata/source schemas; small module; fixture corpus | Schema examples, synthetic parser fixtures | In progress |
| Capture | Activation cutoff, project inclusion, safe native filtering, source provenance, size limits, unknown fields, model/skill gaps | Secret, hidden-content, old-session, incomplete-line tests | In progress |
| Snapshots | Deterministic bytes/hash, timestamps, parser regeneration, hook message reconciliation | Unchanged scan and source-preservation tests | In progress |
| Storage | R2/S3 SDK, explicit profile, Keychain, integrity, write-before-pointer, synthetic permissions probe | Fake endpoint + real provider round trips | In progress — code complete: `credentials.LoadAWSConfig`/`LoadR2Config` (explicit profile, no env fallback), Keychain-backed `CredentialStore` (darwin + a stub fallback), `storage.NewConfiguredStore` wiring both providers into one `ObjectStore`, `VerifyAccess`'s synthetic probe, and `PutSourceThenMetadata`'s write-before-pointer with retry, all tested against a fake S3 HTTP endpoint. Only a live R2/S3 round trip is unverified, and is expected to happen through Setup's own `VerifyAccess` call once real credentials exist, not a separate PR |
| Collector | Registration, hook-only evidence, scan cadence, queue, lock, retries, delayed final, compaction, disk failures | Crash/restart/ownership tests | In progress (PR in review)\*\* |
| CLI | setup/status/sync/pause/resume/help/version; actionable errors | CLI scenario tests | Pending — no `cmd/` entry point exists yet; `_hook`/`_collect` are only string literals in generated hook/LaunchAgent config so far |
| Setup | Existing bucket, hidden secrets, app selection, projects, consent, no history import, activation preservation | Reconfiguration/cancel/rollback tests | Pending |
| Hooks | Three harnesses, preserve unrelated hooks, trust remains explicit, prototype migration | Merge/idempotency tests, live lifecycle tests | In progress (PR #5) — config merge/install/rollback done and tested (idempotency, unrelated-handler preservation, invalid-config rejection, prototype migration); live install against a real harness is unverified |
| Scheduling | Login LaunchAgent, absolute runtime path, background auth, persistent pause | Plist and fresh-process tests | In progress (PR #5) — LaunchAgent plist generation and path escaping done and tested; no `_collect` binary to schedule yet and no live loaded-agent test |
| Metadata | Harness/version/settings; requested vs response model; skills installed vs discovered; evidence and gaps; counts | Mixed-model/no-skill fixtures | Done (PR #6) |
| Reader | Metadata filtering, selected-source download, hash verification, on-demand normalized view, feedback provenance | Read-back and filter tests | Done (PR #6)\* |
| Retention | Current/predecessor, grace period, whole-session deletion, same-machine ownership, pending-pointer safety | Expiry/race tests | Pending |
| Distribution | Intel and ARM executables, checksums, signing/notarization, docs | Build artifacts and release workflow | Pending |
| System verification | Two Macs, two providers, each installed harness; enabled/disabled latency | Live smoke records, timing report | Pending external access |

\* Reader covers metadata filtering (harness, model, skill, coverage, eligibility), verified bounded source reads, and refresh-on-deletion recovery, each with tests. It does not yet surface individual explicit-feedback items with their own provenance to readers; `Metadata.Counts.ExplicitFeedback` is a count only. Closing that gap is follow-up work, not blocking.

\*\* Builds on PR #5's `internal/local` (private home directory, atomic file I/O, flock-based machine lock) rather than re-implementing them; adds the scan/build/publish loop, change detection against a local per-session cache (so an unchanged transcript costs no storage write and never gets a new `captured_at`), and a per-session minimum upload interval. Does not yet implement snapshot cleanup/retention or a parser-upgrade-only republish path — both explicitly deferred to the Retention slice and a later pass, respectively.

Evaluate Skill authoring and controlled evaluation runner are explicitly separate work in the engineering spec. This implementation must provide their reader/data interface, not silently omit it or claim the evaluation skill itself exists.

## Remaining PR sequence to completion

With PR #7 (Collector) in review, this is the full remaining sequence to close every Pending or partially-done row above, in dependency order. Each PR is scoped to what the spec's own component boundaries and this repo's existing PR granularity suggest; none of it needs to start from zero; the code inventory below is what each PR builds on, not what it still has to write.

**PR #8 — CLI foundation** (needs: #7 Collector, Storage, `internal/local`, `internal/hooks`)
Adds the first `cmd/` entry point. No user-facing CLI or hidden subcommand exists yet, so this closes the gap that PR #5's generated hook commands and LaunchAgent plist already reference by name (`_hook --harness <name>`, `_collect`) but nothing implements:
- Hidden `_hook` subcommand: on a start/resume event, write a `collector.LocalStore` registration; on stop/failure/end, write a request carrying whatever hook-only evidence (final response, model selection) the harness supplied.
- Hidden `_collect` subcommand: acquire `local.Lock`, build an `ObjectStore` from `credentials`+`storage.NewConfiguredStore`, and run one `collector.Run` pass unless paused — this is what `install.LaunchAgent`'s `StartInterval: 60` already schedules.
- User-facing `status`, `sync`, `pause`, `resume`, `--help`, `--version`. `pause`/`resume` persist a flag that `_hook`, `_collect`, and `sync` all check before doing new work, per the spec's "pause is not a privacy exclusion" requirement.
- `setup` gets a stub here (so dispatch and `--help` are complete); its real implementation is PR #9.
- Evidence: CLI scenario tests per command (success/failure exit codes and actionable error text), pause/resume persistence across process restarts.

**PR #9 — Setup** (needs: #8 CLI, `credentials`, `storage.VerifyAccess`, `hooks.Plan`/`Apply`, `install.LaunchAgent`, `archive.Config`/`ProjectActivation`)
Implements `agent-archive setup`'s guided flow exactly as the spec's "Setup on each Mac" section lays it out: connect an existing bucket (R2 or S3) → `storage.VerifyAccess`'s synthetic probe plus a best-effort public-access check → detect installed harnesses and let the user choose projects, writing `archive.Config`/`ProjectActivation` (the activation-cutoff logic already exists; this PR is what calls it) → one confirmation summary, then `hooks.Plan`/`Apply` plus generating and actually loading the LaunchAgent → verify real capture by waiting for a live session. Reruns must preserve unrelated hooks and existing activation times (partially covered already by `hooks.Apply`'s concurrent-edit rollback) and roll back cleanly on cancel.
Evidence: reconfiguration/cancel/rollback scenario tests. This is also where a live R2/S3 round trip finally gets exercised, closing the one open item on the Storage row above.

**PR #10 — Retention**
Keep the current source snapshot and its predecessor; delete older unreferenced snapshots after a 24-hour grace period. Delete metadata and all of a session's source snapshots together once it ages past the configured window (90 days by default) — never a source-only age rule that could leave a live metadata pointer dangling. Only the owning machine deletes its own sessions' snapshots. This was explicitly deferred out of PR #7's scope; it most naturally runs as an additional pass alongside `collector.Run` or `_collect`.
Evidence: expiry/race tests — a delete racing a concurrent `reader.LoadSource` should be caught by the reader's existing `ErrRefreshRequired`/`RefreshAndLoad` retry (already built in PR #6); this PR needs the matching writer-side test.

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
