# Private skill history

This optional recorder collects evidence for a future `evaluate-skill` skill. It supports Codex lifecycle hooks on macOS. It does not add hooks to Claude Code or Cursor, and it does not import historical conversations.

## How it works

1. `UserPromptSubmit` asks the agent to mark each skill it applies.
2. The agent runs a read-only `signal` command. `PostToolUse` records the skill snapshot and subsequent tool evidence.
3. `Stop` or `Interrupt` writes one compressed JSON bundle to a private local outbox.
4. A macOS LaunchAgent uploads pending bundles to R2 every three minutes and at login. Failed uploads stay queued.
5. Evaluation runs download the shared history on demand with `pull`.

Codex does not expose a dedicated skill-use hook. These records are **agent-reported use**, not proof that every skill was detected or followed. Reading a skill for research or editing should not produce a marker. Subagent skill use is not automatically attributed. A force-quit can leave an unfinished local record without a completed bundle.

Each machine uses a separate random ID and each event uses a UUID. Machines never edit one shared log. Uploads use `rclone copyto --immutable`, not a deletion-propagating sync. This is an application convention, not an enforced append-only bucket: R2 Object Read & Write credentials can overwrite or delete objects.

## What a record contains

| Evidence | Purpose |
| --- | --- |
| Session, turn, run, machine IDs and timestamps | Link events and distinguish machines |
| Skill name, path, exact-content SHA-256, redacted snapshot, Git revision when available | Identify the instructions used |
| Current request and final answer | Inspect the requested task and result |
| Model, reasoning-effort setting and token usage when available | Compare runs under different conditions |
| Repository HEAD, branch and dirty status | Identify the working context |
| Bounded tool inputs and outputs after the first marker | Inspect evidence behind the result |
| Completion/interruption and capture gaps | Avoid treating incomplete records as complete |
| Next user message, linked but unclassified | Preserve possible corrections without assuming it is feedback |
| Explicit feedback events | Record the user's assessment without rewriting history |

Text excerpts are capped at 32,000 characters; skill snapshots at 128,000. At most 100 tool events are retained per turn. Truncation and dropped-tool counts are recorded. Referenced skill resources, earlier conversation context, input code revisions, images and generated files are not bundled. This supports evidence-based inspection, not exact replay. A future evaluator must report these gaps and must not equate task completion with success.

Raw transcripts, hidden reasoning and system instructions are not exported. The transcript adapter reads only allowlisted model and usage fields. Credential redaction is best effort. Records can contain private code, paths, prompts and tool results; treat the entire archive as confidential.

## Install on each Mac

Requirements: Python 3.9+, Codex with lifecycle hooks, and rclone.

```bash
brew install rclone
python3 scripts/install_skill_runs.py
```

The installer copies the runtime to `~/.local/share/agent-skill-runs/bin/skill_runs.py` and merges its handlers into `~/.codex/hooks.json`. Existing handlers are preserved. The first existing hooks file is backed up before modification. Re-run the installer to update the runtime.

Review and trust the hooks through `/hooks` in Codex CLI, then start a new task. Installation does not bypass Codex's hook trust check. Test a skill invocation and check `status` and `list` before relying on capture.

Local files live under `~/.local/share/agent-skill-runs/`. The recorder rejects a data directory inside a Git working tree. Do not copy its SQLite database or machine ID to the second Mac; install independently there.

## Configure private R2

1. Create a dedicated R2 bucket, for example `agent-skill-runs`. Leave public access (`r2.dev` and custom domains) disabled.
2. Create a separate R2 API token for each Mac, with **Object Read & Write** limited to that bucket. Use the S3 access key ID and secret access key, not the API bearer token.
3. Run `rclone config` locally. Add an S3 remote named `skill-r2`, provider `Cloudflare`, region `auto`, and the endpoint shown in the R2 dashboard. Enter credentials locally; never put them in chat or this repo. Set `no_check_bucket = true` in advanced settings.
4. Restrict access to the rclone configuration file (`rclone config file` prints its path). Standard rclone configuration stores credentials locally; an unattended job must be able to read them. Use a bucket-scoped token, not an account-wide admin credential.
5. Check access and schedule uploads:

```bash
rclone lsf skill-r2:agent-skill-runs
python3 scripts/install_skill_runs.py \
  --remote skill-r2:agent-skill-runs/v1 \
  --launch
```

Use the same bucket/prefix on both Macs, with separate credentials. The LaunchAgent uses an absolute executable path and runs while the user is logged in. A sleeping or offline Mac resumes uploads on a later scheduled run. No server or continuously running agent is needed.

Before enabling uploads, inspect a captured local run. Check that its request, skill, answer and metadata are useful and that its contents are suitable for private storage. Verify a round trip with a synthetic task before using sensitive tasks.

The installer does not provision a bucket, configure credentials or establish a retention policy. Local archives and remote objects remain until explicitly deleted. If you need enforced retention or deletion protection, configure that separately in R2 after deciding how long records should live.

## Inspect and evaluate

From this checkout:

```bash
python3 scripts/skill_runs.py status
python3 scripts/skill_runs.py list --skill review-pr
python3 scripts/skill_runs.py show RUN_ID
python3 scripts/skill_runs.py feedback RUN_ID --file /private/path/feedback.txt
python3 scripts/skill_runs.py upload
python3 scripts/skill_runs.py pull
```

`show` includes linked follow-up and explicit feedback events. `pull` downloads the shared prefix into a local cache. `list` and `show` deduplicate local and downloaded events. Pulling currently downloads all bundles; add indexing or filtering only when the archive is large enough to need it.

An evaluator should group runs by skill hash and model, inspect corrections and outcomes, state coverage gaps, and propose changes for human review. The stored schema includes `schema_version` for later migrations. The evaluation skill itself is not implemented here.

## Exclude a project or stop recording

Add `excluded_roots` to `~/.local/share/agent-skill-runs/config.json`, preserving any other fields:

```json
{"excluded_roots": ["/absolute/path/to/excluded-project"]}
```

Exclusions take effect for new turns. For complete disablement, remove the four handlers labeled `Recording private skill-run evidence` from `~/.codex/hooks.json`, preserving other handlers, and stop the upload job:

```bash
launchctl bootout gui/$(id -u)/com.agent-skills.skill-runs-upload
rm ~/Library/LaunchAgents/com.agent-skills.skill-runs-upload.plist
```

Disabling does not delete existing records or credentials. Revoke the individual Mac's R2 token when retiring that machine. Do not sync the whole recorder directory with iCloud or Dropbox; R2 is the shared archive.

## References

- [Codex hooks](https://learn.chatgpt.com/docs/hooks)
- [R2 with rclone](https://developers.cloudflare.com/r2/examples/rclone/)
- [R2 API tokens](https://developers.cloudflare.com/r2/api/tokens/)
