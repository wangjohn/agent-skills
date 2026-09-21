# Capture capability evidence

Agent Archive reports application presence, installed version, documented hook
capabilities, adapter fixture coverage, and actual read-back verification as
separate facts. A configured or installed hook is not evidence that capture
works for an installed application version.

- Codex hooks document `SessionStart.source`, `transcript_path`, lifecycle
  events, and that the transcript format is not a stable hook interface:
  <https://learn.chatgpt.com/docs/hooks>.
- Codex version discovery uses `codex --version` and the documented bundled
  executable path: <https://learn.chatgpt.com/docs/reference/troubleshooting>.
- Claude Code documents `SessionStart`, native JSONL transcripts, and
  `claude --version`: <https://code.claude.com/docs/en/hooks> and
  <https://code.claude.com/docs/en/claude-directory>.
- Cursor documents `sessionStart`, `cursor_version`, `transcript_path`, and
  lifecycle hooks. Agent Archive reads the macOS bundle version rather than
  substituting the separately versioned Agent CLI: <https://cursor.com/docs/hooks>.

`documented` means the vendor exposes the named evidence. `unavailable` means
the payload is insufficient for the archive claim. Installed-version support
remains `unverified` until a session from that observed version is published
and read back under the current configuration.
