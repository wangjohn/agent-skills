# John Wang's agent skills

Personal [Agent Skills](https://agentskills.io/specification) I use across Claude Code, Cursor, Codex, and any other coding agent that reads `SKILL.md`.

## Install

```bash
# this project
npx skills add wangjohn/agent-skills

# every project on this machine
npx skills add wangjohn/agent-skills -g
```

List what's in the repo first:

```bash
npx skills add wangjohn/agent-skills --list
```

## Layout

```
skills/
  create-skill/
    SKILL.md
    references/
  review-pr/
    SKILL.md
```

Each skill is a directory whose name matches the `name` field in `SKILL.md`. Keep the body short. Put long docs in `references/` and anything deterministic in `scripts/`.

Do not put Cursor-only frontmatter (`globs`, `alwaysApply`) in these files. This repo is the portable source of truth.

`review-pr` runs native code review and design review in separate contexts, then combines them into a human guide: a short summary, change map, design decisions, proposed fixes, and coverage. It uses code-first explanations and supports design-only requests. Try: “Use review-pr to review PR <url>.”

## Add a skill

1. Copy `skills/create-skill/references/skill-template.md` to `skills/<name>/SKILL.md`
2. Set `name` to the folder name (lowercase, hyphens)
3. Write a `description` that says **when** to use it (that is the trigger)
4. Open a PR. CI runs `scripts/validate.py`

Or tell an agent to follow the `create-skill` skill in this repo.

## Validate locally

```bash
python3 scripts/validate.py
python3 -m unittest discover -s tests -v
```

## Private skill history

`agent-archive` is a small macOS CLI (in [`agent-archive/`](agent-archive/)) that keeps a private, local-first archive of your Codex, Claude Code, and Cursor sessions in a bucket you own. It installs lifecycle hooks in each app, runs a background collector that uploads session transcripts and metadata to a private Cloudflare R2 or Amazon S3 bucket, and prunes them on a retention schedule. No account or hosted service is involved.

Commands:

- `agent-archive setup` guides first-time setup or a safe reconfiguration.
- `agent-archive status` shows storage, collector, hooks, and capture coverage.
- `agent-archive sync` runs one collection and upload pass now.
- `agent-archive pause` and `agent-archive resume` suspend and restore scheduled work.
- `agent-archive list` and `agent-archive show` inspect archived sessions from the bucket, metadata first.
- `agent-archive uninstall` removes hooks and the collector while keeping local evidence and credentials; `--delete-local-data` explicitly removes owned local data after confirmation. It never touches the bucket.

See [agent-archive/docs/install.md](agent-archive/docs/install.md) for install and uninstall steps, and the [product and engineering specification](docs/agent-run-archive-spec.md) for the design.

The earlier [Python skill-run recorder](docs/skill-runs.md) is retained as a legacy prototype. It uses a separate data format and installation; use the Go CLI above for the current agent archive.

