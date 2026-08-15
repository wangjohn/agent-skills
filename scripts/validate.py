#!/usr/bin/env python3
"""Validate Agent Skills in skills/ against agentskills.io naming rules."""
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
SKILLS = ROOT / "skills"
NAME_RE = re.compile(r"^[a-z0-9]+(?:-[a-z0-9]+)*$")


def parse_frontmatter(text: str) -> dict:
    if not text.startswith("---"):
        raise ValueError("missing YAML frontmatter")
    parts = text.split("---", 2)
    if len(parts) < 3:
        raise ValueError("unterminated YAML frontmatter")
    data = {}
    for line in parts[1].splitlines():
        line = line.strip()
        if not line or line.startswith("#") or ":" not in line:
            continue
        key, val = line.split(":", 1)
        data[key.strip()] = val.strip().strip('"').strip("'")
    return data


def main() -> int:
    if not SKILLS.is_dir():
        print("skills/ directory missing", file=sys.stderr)
        return 1
    errors = []
    skill_dirs = sorted(
        p for p in SKILLS.iterdir() if p.is_dir() and not p.name.startswith(".")
    )
    if not skill_dirs:
        print("no skills found under skills/", file=sys.stderr)
        return 1
    for d in skill_dirs:
        skill_md = d / "SKILL.md"
        if not skill_md.is_file():
            errors.append(f"{d.relative_to(ROOT)}: missing SKILL.md")
            continue
        try:
            fm = parse_frontmatter(skill_md.read_text(encoding="utf-8"))
        except ValueError as e:
            errors.append(f"{skill_md.relative_to(ROOT)}: {e}")
            continue
        name = fm.get("name", "")
        desc = fm.get("description", "")
        if not name:
            errors.append(f"{skill_md.relative_to(ROOT)}: missing name")
        elif not NAME_RE.fullmatch(name) or len(name) > 64:
            errors.append(f"{skill_md.relative_to(ROOT)}: invalid name {name!r}")
        elif name != d.name:
            errors.append(
                f"{skill_md.relative_to(ROOT)}: name {name!r} != folder {d.name!r}"
            )
        if not desc:
            errors.append(f"{skill_md.relative_to(ROOT)}: missing description")
        elif len(desc) > 1024:
            errors.append(
                f"{skill_md.relative_to(ROOT)}: description too long ({len(desc)})"
            )
    if errors:
        print("\n".join(errors), file=sys.stderr)
        return 1
    print(f"ok: {len(skill_dirs)} skill(s)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
