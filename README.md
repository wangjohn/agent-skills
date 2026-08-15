# agent-skills

Portable [Agent Skills](https://agentskills.io/specification) for Claude Code, Cursor, Codex, and other coding agents that read `SKILL.md`.

One repo, one canonical copy. Installers drop the folders into each agent's skills directory.

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
```

Each skill is a directory whose name matches the `name` field in `SKILL.md`. Keep the body short. Put long docs in `references/` and anything deterministic in `scripts/`.

Do not put Cursor-only frontmatter (`globs`, `alwaysApply`) in these files. This repo is the portable source of truth.

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
