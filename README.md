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

`review-pr` prepares a human review guide to a PR's architecture, models, tradeoffs, and consequential design decisions, with code-first explanations, simple language, diagrams when useful, and focused review questions. Try: “Use review-pr to show me the decisions that deserve my attention in PR <url>.”

## Add a skill

1. Copy `skills/create-skill/references/skill-template.md` to `skills/<name>/SKILL.md`
2. Set `name` to the folder name (lowercase, hyphens)
3. Write a `description` that says **when** to use it (that is the trigger)
4. Open a PR. CI runs `scripts/validate.py`

Or tell an agent to follow the `create-skill` skill in this repo.

## Validate locally

```bash
python3 scripts/validate.py
```

## Private agent-run archive

The proposed [product and engineering specification](docs/agent-run-archive-spec.md) covers the `agent-archive` CLI, onboarding for private R2 and S3 storage, Codex/Claude Code/Cursor capture, metadata, privacy, recovery, and rollout. This is a plan, not an implemented release.
