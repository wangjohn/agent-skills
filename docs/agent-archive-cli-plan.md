# Agent Archive setup and CLI improvement plan

Status: implemented; automated verification is recorded in the implementation ledger. Live app/cloud and signed-release verification remain pending. Prepared 2026-09-20 against the Go implementation integrated from `origin/main` at `bdcd7c3`, with the pre-existing local work preserved in `a44dbf4` and merge `94431f7`.

## Goal and scope

Make first-time setup a short, understandable flow; make interruptions recoverable; and make routine commands explain what happened and what the user should do next. Build on the existing Go CLI, storage SDK, hook manager, collector, reader, and release workflow. Do not build another CLI around the legacy Python recorder.

The existing product specification remains the source of capture, storage, privacy, and retention requirements. This plan refines its user experience and closes relevant implementation gaps. Evaluation, historical imports, infrastructure provisioning, a dashboard, and a new capture architecture are outside this change.

## Verified starting point

- `internal/cli/cli.go` already dispatches setup, status, sync, pause, resume, list, show, and uninstall. Internal `_hook` and `_collect` commands are already absent from public help.
- Setup already connects R2/S3, probes object access, selects apps/projects, installs hooks and a LaunchAgent, and attempts rollback. It currently asks storage questions first and repeats the entire flow on reconfiguration.
- `setup`, `status`, `sync`, and `uninstall` ignore their argument slices. Pause/resume never receive theirs. Consequently `pause --help` can change state and `setup --help` can start onboarding.
- `prompt.go` reads secrets as ordinary lines. Although the program does not print them, terminal echo can display typed credentials.
- Status is already human-readable, but reports collection as active from configuration alone and lacks per-app verification, job health, and JSON output.
- Setup's rollback removes the LaunchAgent plist after a late failure rather than restoring a previous installation. Its new config also drops an existing paused state. A destination change is not guarded against pending work.
- The shipped list/show commands read remote metadata. Conversation content requires `show --normalized`; preserve that explicit boundary.
- Uninstall already exists and limits deletion to owned files, but removes local state and credentials. Preserve pending evidence by changing its default behavior deliberately.

The integrated baseline passes `go test ./...`, `go vet ./...`, skill validation, and the 15 Python prototype tests. Live cloud, trust, launchd, signing, and multi-Mac verification remain separate requirements.

## User-facing contract

### Commands

Keep the existing names. Group help into Get started, Manage capture, Inspect history, and Maintenance. Bare `agent-archive` prints concise help successfully. `help COMMAND` and `COMMAND --help` are equivalent, succeed, and perform no configuration, credential, network, hook, or scheduler operations. Unknown flags and extra arguments fail before any side effects.

| Command | Behavior |
| --- | --- |
| `setup` | Start, continue, or edit setup |
| `status [--json]` | Explain current state, evidence age, and next action without uploading content |
| `sync` | Collect and upload once; report counts; respect pause |
| `pause` | Stop new collection/upload/cleanup work persistently and report when current work has stopped |
| `resume` | Restore scheduled work and explain catch-up behavior |
| `list` | Find archived sessions, retaining current filters |
| `show ID` | Inspect metadata; retain explicit `--normalized` for conversation content |
| `uninstall` | Remove integrations while retaining local evidence and credentials by default |

Use `--json` for a versioned status document first. Keep existing list tables and show JSON output compatible during this work; avoid silently breaking reader scripts. Add examples to their help. Use exit 0 for a completed request or help, 1 for operational failure/incomplete installation, and 2 for usage errors. Capture waiting for its first session is a successful configuration state, not an installation failure.

Do not add a separate doctor or repair command initially. `status` diagnoses; `setup` offers the relevant repair or edit. Never fix or mutate the installation merely by running status.

### Three-step setup

1. **Choose capture.** Offer detected supported applications together using friendly names, for example “Include Codex and Claude Code?” On decline, let the user choose each app; when none are detected, go directly to those choices. Offer to keep the existing selection on reconfiguration. Detection does not establish compatibility or verified capture. Require explicit app/project inclusion. Suggest the current directory only if it resolves to an appropriate project; otherwise ask for paths. Normalize and deduplicate project roots, reject missing/non-directory paths, and preserve activation times for existing roots. Display the policies “New sessions only” and “Include sessions without skills.” Keep the latter editable under advanced settings, preserving existing choices.
2. **Connect storage.** Select R2 or S3. R2 asks for bucket, one account-ID-or-endpoint field, access key, and a hidden secret; S3 asks for bucket, region, and an existing profile. Reuse existing credentials by explicit choice. Default the prefix to `agent-archive/`; expose prefix and other optional settings through an advanced-settings choice. Validate fields immediately and retry only the invalid field. Run the existing synthetic write/read/list/delete probe automatically, showing stages and concise errors. Distinguish access verified from bucket privacy unknown.
3. **Review and enable.** Show application names, exact project paths, destination, capture policy, and retention (90 days initially, editable). Explain best-effort filtering and retention deletion in the review. Require confirmation before activating capture. Install and start integrations, then show app-specific trust instructions and verification state. Finish the wizard without waiting for the user to switch apps and create sessions.

Example completion, where the evidence supports each line:

```text
Setup saved.

Storage       Access verified; privacy not verified
Background    Started
Codex         Waiting for first session
Projects      /Users/example/work/project

Next: complete the application's hook approval, then start a new
session in this project. Run agent-archive status to check capture.
```

Do not claim a trust rejection unless the application exposes evidence of it. “No hook activity yet” is more accurate than guessing the cause.

### Resume and edit setup

Persist a versioned private draft of non-secret choices and the last completed step, separate from active config. A rerun offers Continue saved setup, Edit current setup, or Start over where applicable. Keep editing scoped to capture, storage, or retention rather than asking every question again. EOF/Ctrl-C cancels safely; invalid values reprompt. Do not silently accept defaults after EOF.

Never persist secret input in drafts, logs, errors, or command arguments. Use isolated temporary Keychain references during validation, so a running installation continues using its existing credential. Reuse a staged credential on resume only if its reference still resolves; otherwise prompt again. Discarded drafts clean up only their own staged references. Never overwrite a credential whose prior state cannot be read safely.

Draft completion does not establish project activation. Assign new activation times at successful enablement; keep existing times stable. Changes to a provider, destination, credential, or app selection invalidate the relevant previous verification results.

## Implementation design

### Command parsing and prompts

Centralize command metadata, help, argument validation, and usage errors in `internal/cli`. Keep business logic in the existing command handlers and preserve injected `Env` dependencies. All public commands parse before resolving home or opening stores.

Introduce prompt operations for choices, validated values, project lists, and secrets. Use a terminal-aware hidden-input implementation for real interactive secrets, with a test double for fixtures. Do not silently fall back to echoed terminal input when hiding fails. Define redirected-input behavior explicitly and test it; no secret flags. Avoid a full-screen terminal UI or large framework for this flow.

### Transactional setup and reconfiguration

Separate Collect choices -> Validate -> Plan -> Confirm -> Apply -> Verify. Represent the proposed changes as a plan before touching active installation state.

Coordinate setup with the existing machine lock. During apply, prevent hooks and background work from consuming half-applied configuration. Preserve the old config, credential references, owned hook entries, plist bytes, and scheduler state. Save the new config before the new worker can run, and restore the previous state on failure. Journal apply progress durably so a crash can be diagnosed and recovered on the next setup run; do not rely solely on deferred rollback.

Preserve machine identity, paused state, unrelated hook handlers, and existing activation times. Remove only owned handlers for deselected applications. Report rollback failures with the exact recoverable next step, never “No changes made” when restoration failed. A failed scheduler start is an incomplete setup with a retry path, not an unconditional success or a promise that next login will repair it.

For destination changes, initially block switching while pending sessions exist, explain how to sync the old destination, and preserve the current config. Also prevent existing registered/published sessions from being republished to a new destination: retain old destination ownership and retire those registrations from future collection when the user explicitly switches to new sessions at the new destination. Do not silently move old evidence. Credential rotation for the same destination does not reset session eligibility.

For reduced retention, calculate and show the affected owned-session count and cutoff before confirmation. If the effect cannot be determined, leave the current retention unchanged and report why. Confirmation authorizes the displayed policy; cleanup occurs through the existing retention path.

### Evidence-based status

Build one typed status model, rendered as readable text or `--json`. Include setup state, destination, paused state, installed/running background state, included projects, pending sessions, scan/publication times, per-app capture stages, and next actions. Include schema version and machine-readable state codes. Do not put secrets or conversation content in either renderer.

Track configured, hook observed, captured locally, published, and read-back verified separately. Record verification timestamps and the configuration identity they apply to. Use the existing collector and reader integrity checks for verification; do not download all archived conversations to produce status. Storage setup-test success does not verify application capture. Installed plist/configured enablement does not prove a running worker. Unknown/stale evidence remains visible.

Default status uses local evidence and read-only scheduler inspection. Persist verification results during setup/collection; avoid an implicit cloud probe on every status invocation. Prioritize one useful next action: complete setup, restore authentication, start collector, complete app trust/start a session, or retry sync.

### Pause, resume, and uninstall

Audit pause against in-flight collection: report a pause request until current work finishes or stops safely, then confirm paused. Synchronize config updates and recheck pause before starting more collection, uploads, or cleanup. State in help/output that already registered sessions can catch up after resume; new sessions begun while paused are not imported retroactively.

Default uninstall stops the worker and removes owned hooks/LaunchAgent, keeping local evidence, config, and credential references available for recovery. Offer an explicit destructive local-data option with a concrete summary of pending evidence and a separate confirmation. Preserve the existing ownership allowlist; never delete unknown files or remote objects. Update help/docs to describe this intentional behavior change.

## Delivery order

| Slice | Files/areas | Completion condition |
| --- | --- | --- |
| 1. Safe command contract | `internal/cli/cli.go`, command handlers, CLI tests | Every help path is read-only; invalid arguments fail before side effects; grouped help and examples work |
| 2. Setup engine and recovery | `setup.go`, `prompt.go`, config/local/hooks integration | Draft/resume, secret handling, transaction journal, rollback, destination and retention guards pass fault-injection tests |
| 3. Three-step onboarding | setup prompts and rendering | First-time and edit flows use the reviewed structure, show actual paths, preserve prior choices, and retry only failed steps |
| 4. Operational status | status model/renderers, collector verification, scheduler inspection | Text and JSON agree; per-app evidence is accurate; incomplete setup has an actionable next step |
| 5. Routine controls | pause/resume, uninstall, associated tests | In-flight work and retained-data semantics match the public contract |
| 6. Docs and release checks | install guide, README, spec, implementation ledger, release checks | Documentation matches shipped commands; existing release builds still work; verification gaps are recorded accurately |

Each slice builds on the previous contracts and is reviewable independently. Avoid rewriting storage, reader, or transcript adapters merely to improve CLI presentation. Keep the legacy recorder clearly labeled and outside the new runtime path.

## Verification and acceptance

- Table-driven CLI tests cover every command's help, aliases, unknown flags, extra arguments, and exit code. Inject dependencies that fail if help attempts any I/O or mutation.
- Scenario tests cover fresh R2/S3 setup, existing setup edits, resumed drafts, invalid values, EOF/cancel, hidden input, path normalization, no-app/no-project selection, and skipped advanced options.
- Failure injection covers Keychain denial, storage probe failure/cleanup failure, config write failure, hook failure, scheduler load failure, interrupted apply, and rollback failure. Assert old installations remain usable, paused state and activation times survive, and secrets never appear in output/drafts.
- Reconfiguration tests cover removed apps/projects, duplicate paths, credential rotation, pending destination changes, previously published session ownership, and retention-reduction confirmation.
- Status tests cover never configured, draft only, installation incomplete, unknown/stale job state, first-session waiting, local-only capture, publication/read-back, expired auth, paused, and partial app coverage. Never infer verification from configuration alone.
- Control tests cover pause during collection/upload/cleanup, resume catch-up, and uninstall with pending evidence and unrelated files. Uninstall never calls remote delete.
- Run `go test -race ./...`, `go vet ./...`, repository skill validation, and prototype regression tests. Build through the existing macOS release script where the environment permits; CI covers supported platform combinations.
- Use only synthetic sessions for live smoke tests: terminal secret echo, LaunchAgent start/restart, actual app hook trust, R2 and S3 round trips, uninstall/reinstall, and second-Mac independence. Do not install hooks or upload this user's private sessions merely to test the implementation.

Done means a new user can follow the three-step flow without editing JSON or knowing launchctl/rclone; every failure offers a specific recovery path; setup resumes without damaging a working install; and status distinguishes configured, captured, and verified. Live credentials, missing apps, a second Mac, and signing credentials are external verification dependencies, not reasons to label untested behavior complete.
