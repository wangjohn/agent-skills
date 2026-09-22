# Agent Archive audit remediation plan

Prepared: 2026-09-21. Status: all eight implementation PRs open; combined automated verification passed. Live acceptance and explicit capability blockers remain. See [the acceptance record](agent-archive-remediation-acceptance.md) for PRs, evidence, and explicit external blockers.

## Scope and baseline

Close findings A1–A11 from the independent coverage audit of commit `7c91d0170ec7385aa8a8631f6f2304320d9d9c3a`, which combined the then-current main with PRs #22–27. The product requirements remain in `agent-run-archive-spec.md` and `agent-archive-cli-plan.md`. This plan covers missing implementation, regression tests, documentation, and outstanding release verification. Evaluate Skill itself remains separate work.

Before implementation, fetch current PR heads and main, reconcile the separate Astra review, and reproduce findings against the new combined snapshot. Do not assume the audited heads are still current. Record any resolved or superseded finding with code and test evidence. Preserve unrelated local edits, including the review-pr skill edit.

Keep the existing Go CLI, local spool, background collector, filtered source bundle, metadata catalog, and R2/S3 store. Do not add a service, database, agent-written summaries, or per-turn model calls. Hooks continue to do bounded local work; parsing, capability inspection that requires more work, and uploads run outside the agent's critical path.

## Delivery strategy

Use eight focused implementation PRs followed by an acceptance-evidence update. Fixes to defects introduced by an unmerged PR should be added to that PR when small and clearly scoped; otherwise create a focused dependent PR. Do not merge known-broken prerequisite code merely to simplify branch management.

The PR numbers below are planning labels, not GitHub PR numbers. Base independent changes on the earliest branch containing their required code. Stack only actual dependencies. Review and test the combined result in addition to individual diffs.

| PR | Findings | Deliverable | Dependencies |
| --- | --- | --- | --- |
| P1 | A1 | Correct production storage probes | Existing storage/background verification implementation |
| P2 | A2 | Enforce proven session start eligibility | Existing hook/registration implementation |
| P3 | A4 | Report installed versions and capture capabilities | Reuse P2's diagnostic codes; can investigate in parallel |
| P4 | A3 | Verify each selected app/project pair | Existing status; P3 for capability explanations |
| P5 | A5, A6, A11 | Correct metadata semantics and schema provenance | Existing archive/parser implementation; P3's evidence contracts |
| P6 | A7, A8 | Complete skill evidence and version filtering | P3; coordinate schema changes with P5 |
| P7 | A9 | Archive and link supported subagent sessions | P2, P3, P5 |
| P8 | A10 | Report bucket privacy evidence and guidance | Existing setup/status; integrate with P4's rendering |

### P1 — Correct storage probe composition

- Generate `.setup-test/<unique-id>.json` as a relative object key. Only the configured store applies the destination prefix. Remove redundant prefix arguments where possible so callers cannot repeat the mistake.
- Exercise write, read/byte verification, list, and delete in the same namespace used by normal publication. Keep cleanup failures visible and isolate concurrent probes by unique keys.
- Test the real `S3Store` implementation through a fake HTTP endpoint for empty, nonempty, and normalized trailing-slash prefixes. Assert the wire key, listing namespace, and cleanup. Do not rely only on `MemoryStore`.
- Add a scheduled-collector regression that reaches scan/publication after the probe succeeds, plus failure cases that preserve pending evidence.

Acceptance: both setup and scheduled collection use exactly one configured prefix; neither constructs a leading-slash relative key. Run a synthetic live R2 probe as soon as access is available, before broad capture testing.

### P2 — Enforce session start eligibility

- Stop treating an ambiguous first-seen Cursor event as proof of a new session. Require a supported fresh-start signal or a trustworthy native start timestamp after project activation.
- Preserve eligibility for already accepted registrations. Unknown start, pre-activation start, and unsupported evidence remain uncollected with a small, content-free reason visible in status.
- Validate this policy across all three harnesses, including missing fields, resumes, duplicate hooks, and activation boundaries. A hook receipt timestamp is not a session start timestamp.
- Keep transcript inspection out of the synchronous hook path. If native evidence is required, let background validation establish eligibility before capture or upload.

Acceptance: new synthetic sessions are collected when evidence establishes their start; old or ambiguous sessions produce no transcript upload. Diagnostics do not contain the rejected transcript. Live Cursor testing remains gated until this behavior passes.

### P3 — Detect versions and declare capabilities

- Check official app documentation and harmless local synthetic sessions for supported versions and payloads before writing new adapters. Record evidence provenance, app version, and format version where exposed.
- Add bounded, non-mutating version discovery with explicit unknown results. Distinguish installed version, observed session version, tested adapter support, and actual verified capture.
- Keep a small capability table for fresh-start evidence, transcript format, lifecycle events, skill discovery/use evidence, and subagent linkage. Do not build a plugin framework or assume every newer version is supported.
- Show unknown/unsupported/missing-transcript capability in setup review and status, with one next action. Status remains local and read-only; no recurring version process launch or network check on each hook.

Acceptance: absent apps, unknown versions, unsupported formats, and supported fixtures have distinct testable results. Installing a hook never by itself means capture is verified. The table identifies the real evidence available to P5–P7.

### P4 — Verify every selected app/project pair

- Track configured, hook-observed, locally captured, published, and read-back-verified evidence per selected app/project pair.
- Overall readiness requires read-back evidence for all selected pairs under the relevant current configuration. Show uncovered pairs and name one pair in the next action.
- Preserve valid evidence for unchanged pairs; new projects/apps start unverified, removed pairs stop affecting readiness, and destination or relevant capability changes invalidate affected verification. Version JSON changes deliberately and retain existing fields where practical.
- Test two projects with only one verified, two apps with partial coverage, failed read-back, project addition/removal, and destination changes.

Acceptance: a verified session in project A cannot make project B appear verified. Adding an included project immediately creates an explicit verification task without erasing unrelated valid evidence.

### P5 — Correct metadata semantics

- Preserve minimal filtered lifecycle evidence and derive session state separately from turn outcome. A turn stop usually yields idle; only supported closure evidence yields closed. Missing evidence remains unknown.
- Define deterministic ordering/deduplication for duplicate, delayed, and out-of-order events. Repeated unchanged collection must remain a no-op. Never infer successful task outcome merely from a stop event.
- Publish unknown message/tool counts for unparsed text instead of zero. Only complete observation of the relevant count supports an observed zero; preserve partial-coverage explanations.
- Pin an exact OpenTelemetry GenAI conventions revision after checking the upstream source. Document mappings and local extensions without adopting a tracing backend.
- Make schema additions backward compatible where possible, update parser/schema versions deliberately, and test old bundles plus metadata regeneration. Historical lifecycle evidence that was never saved remains unknown.

Acceptance: active → idle → active and supported closure/interruption/error fixtures derive correctly; duplicates do not change results; unparsed Cursor text has unknown counts; older archives still load; regeneration does not alter the retained source hash.

### P6 — Complete skill comparison and version filtering

- Add `list --skill-sha256` using the existing reader filter, validate the argument, and document examples with the same skill name across versions. Filtering must use metadata without downloading source bundles.
- Ingest native discovery/eligibility evidence only where P3 establishes that an app exposes it. Retain source, scope, time, and uncertainty; keep installed-only inventory distinct.
- Derive eligible-but-unused only when both eligibility and sufficient use-observation coverage are established for the comparison interval. Skill presence on disk, a discovery event alone, or missing use events in a partial transcript does not prove non-use.
- Test used, eligible-but-unused, installed-only, incomplete observation, changed skill versions, and multiple models/harnesses through the production capture-to-list path.

Acceptance: supported native fixtures produce trustworthy comparison results; unsupported apps report unavailable coverage. A CLI empty result must not imply that no eligible sessions exist when evidence is unavailable. If no current app exposes reliable eligibility, record that product capability as blocked; do not close A7 merely by adding an unavailable label or inventing evidence.

### P7 — Capture supported subagents

- For capabilities demonstrated by P3, register child sessions separately and preserve parent/child identity. Apply the same project, destination, filtering, and start-eligibility boundaries before collection.
- Keep each child source independently verifiable and reference it by stable archive identity. Do not embed entire child transcripts into parent bundles or duplicate their message counts.
- Define reader behavior for pending, unavailable, and expired child sources. Missing child coverage must remain explicit; parent references must not silently prevent normal retention forever.
- Test duplicate child events, child-before-parent arrival, independent retries, missing source paths, and child retention. Unsupported formats remain a specific coverage gap.

Acceptance: a supported synthetic parent/child run can be followed through capture, publication, and read-back with separate counts and source ownership. Parent-only evidence is never labeled complete when a known child is missing. If no supported native linkage exists, document the external capability blocker rather than claiming implementation is complete.

### P8 — Report privacy evidence and guidance

- Add provider-specific, read-only inspection only when the existing configured credentials support it. Check current provider APIs during implementation; R2 object credentials must not be assumed to authorize Cloudflare management APIs.
- Do not add a mandatory management token or administrator permission to onboarding. Access denied or incomplete checks yield `privacy not verified`, with a direct provider instructions link in setup review and status.
- Record what was checked and when. Only claim verified-private when the inspection covers the relevant public exposure mechanisms; object access success or one partial setting is insufficient.
- Keep checks bounded and non-fatal to the CLI; expose a detected public configuration clearly. Persist results so routine status does not make a cloud call.

Acceptance: fully checked private, public, denied, unsupported, stale, and incomplete inspection cases render accurately without secrets. Existing least-privilege object credentials still work. Permission-limited checks remain a documented capability limit.

## Integration, review, and acceptance

For each PR, include its audit IDs, concrete before/after behavior, regression evidence, compatibility effects, and unresolved external dependencies. A failing regression should demonstrate each corrected defect before the fix. Use production entry points for evidence-producing features, not only helper tests. Reviewers should verify behavior independently rather than accepting a completion checklist.

After the stack is integrated, run Go race tests and vet, schema/reader compatibility tests, repository skill validation, prototype regressions, and macOS release builds. Run required HTTP tests in an environment that permits loopback servers; a sandbox bind denial is neither a pass nor a product defect. Re-run the independent requirements audit on the final combined commit, including the separate Astra review findings once available.

Keep an acceptance record with exact commit, app/model versions when relevant, operating system, synthetic scenario, expected/actual result, and evidence location. Do not commit real transcripts, credentials, or private machine configuration to the public repository.

| Acceptance group | Required checks | Completion condition |
| --- | --- | --- |
| Private R2 first | Synthetic probe, scheduled capture, source/metadata read-back, interruption/retry, cleanup and credential failure | Real configured R2 store succeeds under foreground and background credential contexts |
| App capture | Codex, Claude Code, Cursor: actual trust/setup, new/resumed sessions, transcript format, delayed final evidence, model changes, available skill/subagent capabilities | Supported versions have reproducible synthetic evidence; unsupported cases remain explicit |
| macOS operation | Keychain, launchd install/start/restart/login, pause/resume, uninstall/reinstall, legacy migration, pending work | Existing state and unrelated hooks survive; failures offer a tested recovery path |
| Two Macs | Independent identities, combined listing, offline recovery, no cross-machine overwrite | Synthetic sessions from both machines remain independently readable |
| Release artifacts | Intel and ARM build/download/checksum/launch, signing/notarization and release gate | Actual delivered artifacts pass, not only build scripts |
| Performance | Hook p95, enabled/disabled task latency, CPU/memory/I/O on representative session sizes | Report measurements against the spec's proposed 100 ms hook target and task-overhead goals; investigate material regression |
| AWS S3 | Same storage scenarios plus profile/SSO refresh and expired credentials | Explicitly pending until AWS access is available, per user decision |

Prepare the live test procedures early and run available checks as their prerequisites land. Use only synthetic sessions. Missing credentials, a second Mac, app capabilities, or signing access must stay visible as pending or blocked, with an exact prerequisite. They do not justify marking the overall specification complete. An R2-validated milestone may be reported separately while AWS verification remains pending.

## Completion rule

Every A1–A11 item must end with either a verified implementation and linked evidence or an explicit unresolved external capability/verification dependency. An unsupported label is a correct user-facing behavior, but does not manufacture the missing product capability. Any proposed scope reduction requires a separate decision; do not silently rewrite the specification to match the implementation.

Completion claims must distinguish code implemented, automated checks passed, and live acceptance passed. Update earlier implementation-status statements only with that evidence. Leave the future Evaluate Skill engine, historical imports, dashboard, and benchmark product outside this remediation.
