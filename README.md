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
```

## Related

[agent-archive](https://github.com/wangjohn/agent-archive) is a macOS CLI that keeps a private, local-first archive of your Codex, Claude Code, and Cursor sessions in a bucket you own. It used to live in this repo and now has its own.
