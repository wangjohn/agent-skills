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

`review-pr` runs native code review and design review in separate contexts, then combines them into a human guide: change map, design decisions, proposed fixes, and coverage. It uses code-first explanations and supports design-only requests. Try: “Use review-pr to review PR <url>.”

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

The proposed cross-application design is documented in the [agent-run archive product and engineering spec](docs/agent-run-archive-spec.md), including process maps, metadata, storage, recovery, and rollout. It replaces the prototype design below; it is not yet implemented.

The optional [skill-run recorder](docs/skill-runs.md) captures applied skills in Codex and uploads records to a private Cloudflare R2 bucket. Install it separately on each Mac. Run data and credentials stay outside this public repository. This provides evidence for a future `evaluate-skill` skill; it does not evaluate or change skills automatically.
