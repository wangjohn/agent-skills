---
name: create-skill
description: Add a new portable Agent Skill to this repo. Use when creating, scaffolding, or reviewing a SKILL.md for Claude Code, Cursor, Codex, or other agentskills.io-compatible tools.
license: MIT
---

# Create a skill

Add skills under `skills/<name>/SKILL.md`. Follow the [Agent Skills spec](https://agentskills.io/specification).

## Rules

1. Folder name equals the `name` field.
2. `name` is lowercase alphanumeric and hyphens, 1-64 chars, no leading/trailing or consecutive hyphens.
3. `description` is required. It must say what the skill does AND when to use it. Agents only see this until they activate the skill.
4. Keep `SKILL.md` under 500 lines. Put long material in `references/` and load it on demand.
5. Optional dirs: `scripts/`, `references/`, `assets/`.
6. Do not add product-only frontmatter (`globs`, `alwaysApply`) to portable skills.
7. Skills must be generic. No personal Slack channels, repo names, or machine paths.

## Steps

1. Choose a kebab-case `name`.
2. Create `skills/<name>/`.
3. Start from [the template](references/skill-template.md).
4. Run `python3 scripts/validate.py` from the repo root.
5. Commit. Do not copy the skill into `.cursor/skills` or `.claude/skills` in this repo. Installers do that.

## Done when

`python3 scripts/validate.py` exits 0 and the new folder contains a valid `SKILL.md`.
