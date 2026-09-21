# Installing agent-archive

`agent-archive` ships as a single, self-contained macOS binary — no
runtime dependencies to install separately. Both Intel and Apple Silicon
Macs are supported.

## Install a release build

1. Download the binary for your Mac from the
   [latest release](https://github.com/wangjohn/agent-skills/releases?q=agent-archive):
   - Apple Silicon (M1 and later): `agent-archive-darwin-arm64`
   - Intel: `agent-archive-darwin-amd64`

   Also download `SHA256SUMS` from the same release.

2. Verify the checksum before running anything you downloaded:

   ```sh
   shasum -a 256 -c SHA256SUMS --ignore-missing
   ```

3. Make it executable, put it on your `PATH`, and drop the architecture
   suffix so it's invoked as plain `agent-archive`:

   ```sh
   chmod +x agent-archive-darwin-*
   sudo mv agent-archive-darwin-* /usr/local/bin/agent-archive
   ```

   On an Apple Silicon Mac with Homebrew, `/usr/local/bin` may not exist
   at all. Either create it first (`sudo mkdir -p /usr/local/bin`) or use
   Homebrew's directory, which is already on your `PATH` and needs no
   `sudo`:

   ```sh
   mv agent-archive-darwin-* /opt/homebrew/bin/agent-archive
   ```

4. Confirm it runs and check the version:

   ```sh
   agent-archive --version
   ```

   Published release binaries must be signed and accepted by Apple's notary
   service. Gatekeeper may need network access to check the notarization ticket
   on first launch. Unsigned local development builds are not release artifacts.

   To upgrade later, remove or `mv` the old binary before putting the new
   one in place. Overwriting it in place with `cp` can leave macOS refusing
   to launch it (it is killed at startup) until the file is recreated.

5. Run the guided setup:

   ```sh
   agent-archive setup
   ```

   Setup has three steps: choose apps and projects, connect storage, then
   review and start. You need an existing private R2 or S3 bucket. Type `help`
   at the storage prompt for provider instructions. Include each project explicitly. Only new sessions
   are captured; historical conversations are not imported.

   Setup offers the apps it finds together: “Include Codex and Claude Code?”
   Accept to continue, or decline to choose apps individually. If no apps are
   found, it opens the individual choices immediately. On reconfiguration,
   it offers to keep your existing selection.

   If setup finds the current Git project, it shows its full path and asks
   “Archive sessions in this project?” Accept to continue, or decline to
   enter project paths yourself.

   For R2, enter a bucket, account ID or S3 endpoint, and credentials. Secret
   input is hidden on a terminal and stored in Keychain. For S3, enter the
   bucket and choose an existing AWS profile. Setup offers profiles from your
   AWS settings and uses the selected profile's region when available. It
   asks for a region only when one is missing.

   The final summary shows the apps, projects, destination, session scope,
   and automatic deletion period. At “Start archiving? [Y/n/edit]”, choose
   `edit` to adjust apps, projects, session scope, retention, storage, the
   folder inside the bucket, or the AWS region. Ordinary setup has no
   advanced-settings questions. Storage changes are checked again before
   starting; editing other choices does not repeat the connection test.
   If the connection test fails, choose `edit` to correct the region, bucket
   folder, or other settings, or `retry` after restoring access.

   Review the exact project paths and retention period before enabling.
   The default is 90 days; older sessions are deleted automatically.
   Filtering is best effort, so archived text can still contain sensitive
   information. The storage test uses only a temporary synthetic object;
   it does not prove that the bucket is private.

   Setup saves non-secret choices after each completed step. On interruption,
   run it again to continue or start over. Staged R2 credentials have separate
   Keychain references, so a working installation keeps its old credentials.
   Reconfiguration lets you edit capture, storage, or retention separately.
   Setup preserves the machine identity, existing project activation times,
   paused state, and unrelated hooks.

   After installation, approve the hooks in each selected app (Codex CLI:
   `/hooks`), then start a harmless new session in an included project.
   Setup finishes without waiting for that session. Check progress with:

   ```sh
   agent-archive status
   agent-archive status --json
   ```

   Status distinguishes waiting for a session, observed hooks, local capture,
   and published sources with verified checksums. Background `loaded` means
   launchd knows the scheduled job; `running` means a pass is executing.
   Configuration alone never establishes capture or trust. Status uses local
   evidence and a read-only launchd check, without downloading conversations.
   `status --json` separates configured, hook-observed, captured, published,
   and read-back-verified evidence. Verification includes its timestamp and
   configuration identity; it describes the checked publication, not continuous
   remote monitoring. The background collector checks storage access with one
   synthetic round trip for a new configuration, retries failed checks, and
   refreshes the check after five minutes.
   Authentication evidence identifies whether it came from manual sync or the
   background environment. Unexposed app versions and trust remain unknown.

   Setup retires the old `com.agent-skills.skill-runs-upload` job only when
   its label and command match the prototype. It keeps the prototype's private
   records. Failed setup restores that job; an unrecognized job at the old path
   is preserved and reported for manual resolution.

## Routine use and recovery

Run `agent-archive` for a short command guide, or `agent-archive COMMAND --help`
for examples. Help never activates hooks, reads credentials, or changes state.
Invalid flags fail before a command starts. Exit codes are 0 for success/help,
1 for operational failure, and 2 for usage errors.

- `sync` collects and uploads once, reporting results. It respects pause.
- `pause` persists until `resume`. If work is still running, the command
  reports that no settings changed and asks you to retry after it finishes.
- Already registered sessions can catch up after resume, including activity
  written during the pause. New sessions begun while paused are not imported.
- A failed setup restores the previous config, hooks, and scheduler. If
  recovery is incomplete, status says so; rerun setup to recover. It refuses
  to overwrite a file edited outside setup during recovery.
- A storage change is blocked while known work is pending. Sync the current
  destination first. Switching starts a new capture boundary: old sessions
  stay with their destination and stop being collected or cleaned up by this
  Mac. Old destination references and local evidence are retained.
- Reducing retention shows the affected locally owned session count and
  cutoff before confirmation. The collector applies the resulting policy.

The wizard accepts redirected input for controlled use, but its prompt sequence
is not a scripting API. Supply secrets only through a private input stream;
never use secret command arguments or commit input files. For a terminal,
secret input fails rather than falling back to visible keystrokes.

## Inspecting what was archived

Once sessions have been published, two read-only commands show what is in
the bucket without touching local collector state:

```sh
# Every archived session, newest first: ID, harness, capture time, parser
# status, models, and skills used. Metadata only, never transcript text.
agent-archive list

# Narrow it down. --since takes a date, an RFC 3339 time, or an age.
agent-archive list --harness claude --model claude-opus-5 --since 7d
agent-archive list --skill review --skill-usage eligible_no_use
agent-archive list --skill review --skill-sha256 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
agent-archive list --complete   # complete parser coverage, no capture gaps

# One session's metadata sidecar, as JSON.
agent-archive show <archive-session-id>
```

Metadata model keys `gen_ai.provider.name`, `gen_ai.request.model`, and
`gen_ai.response.model` follow OpenTelemetry GenAI semantic conventions
v1.37.0 at commit `aec6e9d3e86754683dab7c707655d69d953b2768`.
`agent_archive.request.model_label`, `agent_archive.request.reasoning_level`,
and `agent_archive.request.setting.*` are archive-local extensions. Metadata
records this revision so a later parser can reproduce the mapping; the archive
is not an OTLP export.
Metadata derived by parser 0.2.x may still contain the former local
`gen_ai.request.reasoning.level`, `gen_ai.request.model.label`, and
`gen_ai.request.setting.*` keys. Regenerating metadata with parser 0.3.x moves
those local values to `agent_archive.*` without changing the retained source
bundle or its hash.

`show` prints conversation content only when asked: `--normalized`
downloads the session's source bundle, verifies its checksum and identity
against the metadata, and prints the normalized view (turns, tool calls,
and hook-reported final messages) after the sidecar. If the same session
ID was somehow published under more than one harness, pass `--harness` to
pick one. Both commands print `Not set up.` and exit 0 before setup has run,
the same as `status`.

### Add explicit feedback

Write your assessment to a private UTF-8 text file, then attach it to a session
owned by this Mac:

```sh
agent-archive feedback <archive-session-id> --file /private/path/feedback.txt
agent-archive sync
```

Feedback is filtered before it enters the local upload queue. It records user
provenance and observation time; finishing a turn is never treated as success.
The command rejects excluded sessions and sessions retired by a destination
change. Feedback for a paused, still-included session waits for resume.

### Skill evidence and coverage

During initial capture or new session activity, the background collector checks
known skill directories for the selected app. It records installed skills,
original instruction hashes, filtered instruction copies, and observation times.
These filesystem observations do not prove that the app discovered a skill,
made it eligible, or used that exact version during an earlier turn.

Changed inventories and instruction versions remain in session history. Empty
and removed directories produce explicit observations. Unchanged observations
are not appended again. Skill edits alone do not refresh inactive sessions.
Each observation pass reads at most 4 MiB of instruction content; oversized,
truncated, nested, and uninspected plugin content is marked as a coverage gap.
Hooks do not perform this filesystem scan.

`--skill-sha256` filters metadata only and does not download source bundles.
This distinguishes sessions using different bytes under the same skill name.
Current supported hook payloads do not expose both a complete eligible-skill
set and complete use observation, so `eligible_no_use` remains unavailable
for those harness versions instead of treating a missing use event as proof
of non-use.

## Build from source

Requires Go 1.24+ and, for real macOS Keychain access, Xcode's command
line tools (`xcode-select --install`).

```sh
git clone https://github.com/wangjohn/agent-skills.git
cd agent-skills/agent-archive
VERSION=dev ./scripts/build-release.sh
./dist/agent-archive-darwin-$(uname -m | sed 's/x86_64/amd64/') --version
```

`scripts/build-release.sh` is the same script the release workflow
(`.github/workflows/archive-release.yml`) uses, so a local build and a
published release come from identical build flags. Building with plain
`go build ./cmd/agent-archive` also works for quick local testing, but
skips version embedding and produces an unsigned, non-optimized binary.

## Signing and notarization

Release binaries are codesigned with a Developer ID Application
certificate and submitted to Apple's notary service, so Gatekeeper can
verify them without a manual approval step. This requires an Apple
Developer Program membership and its associated credentials
(a Developer ID Application certificate, an app-specific password, and a
team ID) configured as repository secrets, plus the
`APPLE_SIGNING_ENABLED` repository variable set to `true`. A tagged release
fails before building if the flag or any required signing secret is missing.
Signing, signature verification, and accepted notarization are mandatory before
publishing either architecture. Local `scripts/build-release.sh` builds remain
unsigned and are suitable for development checks only.

Required secrets: `APPLE_CERTIFICATE_P12_BASE64`, `APPLE_CERTIFICATE_PASSWORD`,
`APPLE_SIGNING_IDENTITY`, `APPLE_ID`, `APPLE_TEAM_ID`, and
`APPLE_APP_SPECIFIC_PASSWORD`. Do not put these values in repository files.

## Uninstalling

```sh
agent-archive uninstall
```

After confirmation, uninstall stops the background collector, removes its
LaunchAgent and owned hooks, and disables capture. It keeps local evidence,
settings, and credentials so `agent-archive setup` can reinstall it. Unrelated
hook handlers, remote archives, and the CLI executable are always kept.

To also delete owned local files and stored R2 credentials:


```sh
agent-archive uninstall --delete-local-data
```

This shows a pending-session count and requires a second confirmation.
Unpublished evidence will be lost. Only known archive files are removed;
unknown files are kept and reported. Small lock files remain to preserve process
coordination. Neither mode reads or deletes remote archives.

If another operation is finishing, wait and retry. If launchd is unavailable,
or a hook file was edited concurrently, resolve the reported problem and rerun
uninstall. Do not remove the data directory by hand while a collector is running.

### Downgrading after changing storage

After changing buckets or storage folders, do not downgrade to a version that
does not support `DestinationSince`. Older versions ignore this saved boundary
and may upload earlier sessions to the new destination. Keep the current version
until a supported downgrade procedure is available.

Skill comparison metadata uses parser version `0.4.0`. Older `observed_none`
sidecars remain readable, but are excluded from `eligible_no_use`: earlier parsers
could infer non-use from availability alone. Normal collection regenerates metadata after a parser upgrade when retained source is available; missing historical observation
coverage remains unknown. Multiple used hashes of the same skill are retained.

### Linked subagent sessions

Supported Claude `SubagentStop` events stage a child for background validation.
The child must belong to an already accepted parent and expose matching parent
and agent IDs plus a native start after that parent's start and project
activation. Missing files can be retried; ambiguous identity or old starts stay
unavailable. A child arriving before its parent is accepted is ignored; a later
stop can retry. `SubagentStart` alone does not prove freshness.

Each accepted child gets its own source and metadata, with `parent_session_id`.
The parent gets `linked_sessions`; it does not embed the child's transcript or
add child messages to its own counts. The local/native child identity is scoped
by parent ID and agent ID. Resumed children retain the original start. Every
later capture rechecks the mutable native file's identity and start.

`agent-archive show PARENT` adds `linked_session_availability` from direct child
metadata reads. `metadata_available` does not verify source bytes. Select the
child explicitly with `agent-archive show CHILD --normalized` for verified
conversation content. Missing links report pending, unavailable, or
unavailable-or-expired; one missing child does not block the parent. Links do not
extend retention. Children are never downloaded recursively.

This adds optional fields to schema version 1, filter version 2, adapter 0.2.0,
and parser 0.5.0. Existing bundles remain readable. Claude is fixture-tested;
real app capture is pending. Codex/Cursor child capture and native skill
eligibility remain unavailable rather than inferred from incomplete evidence.
