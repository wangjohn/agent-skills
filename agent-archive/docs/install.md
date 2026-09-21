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

   The first time macOS runs a downloaded binary it may show a Gatekeeper
   prompt. Release builds are signed and notarized by Apple once the
   maintainers' signing credentials are configured (see
   [Signing and notarization](#signing-and-notarization) below); until
   then, or for a build you made yourself, right-click the binary in
   Finder and choose **Open** once to approve it, or run
   `xattr -d com.apple.quarantine` on the installed binary.

   To upgrade later, remove or `mv` the old binary before putting the new
   one in place. Overwriting it in place with `cp` can leave macOS refusing
   to launch it (it is killed at startup) until the file is recreated.

5. Run the guided setup:

   ```sh
   agent-archive setup
   ```

   This walks through choosing storage (R2 or S3), which coding
   agents to capture from, and which project directories to activate. See
   the [engineering specification](../../docs/agent-run-archive-spec.md)
   for what each step does and why.

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
agent-archive list --complete   # complete parser coverage, no capture gaps

# One session's metadata sidecar, as JSON.
agent-archive show <archive-session-id>
```

`show` prints conversation content only when asked: `--normalized`
downloads the session's source bundle, verifies its checksum and identity
against the metadata, and prints the normalized view (turns, tool calls,
and hook-reported final messages) after the sidecar. If the same session
ID was somehow published under more than one harness, pass `--harness` to
pick one. Both commands print `Not set up.` and exit 0 before setup has run,
the same as `status`.

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
`APPLE_SIGNING_ENABLED` repository variable set to `true` to opt the
release workflow into using them. Until those credentials are configured,
the workflow still builds, checksums, and publishes unsigned binaries —
see [Install a release build](#install-a-release-build) above for the
one-time Gatekeeper approval an unsigned binary needs.

## Uninstalling

Run the built-in command:

```sh
agent-archive uninstall
```

It prints exactly what it is about to remove and asks for confirmation
before touching anything. On confirmation it:

- stops and removes the background collector LaunchAgent
  (`~/Library/LaunchAgents/com.agent-archive.collector.plist`);
- removes only its own `agent-archive _hook ...` entries from each
  included application's hook configuration (`~/.codex/hooks.json`,
  `~/.claude/settings.json`, or `~/.cursor/hooks.json`), leaving every
  unrelated hook and setting in place;
- deletes the R2 credentials setup stored in Keychain, when the
  configuration references them (an S3 setup stores none);
- removes local state: config, per-session cache, and logs under
  `~/.local/share/agent-archive` (or `$AGENT_ARCHIVE_HOME`). Only files
  agent-archive itself created are deleted; anything else in that
  directory is left in place and named in the output.

Nothing in your bucket is read, listed, or deleted: every archived session
stays exactly where it is. The binary itself is left in place; remove it
with `rm /usr/local/bin/agent-archive` (or `rm /opt/homebrew/bin/agent-archive`,
or wherever you put it).

If the command cannot complete (for example, launchd is not reachable or a
hook file was edited concurrently), it says which step failed and leaves the
rest done. The same steps by hand, as a fallback:

```sh
# Stop the background collector.
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.agent-archive.collector.plist
rm ~/Library/LaunchAgents/com.agent-archive.collector.plist

# Remove local state: config, per-session cache, logs.
rm -rf ~/.local/share/agent-archive

# Remove the binary.
rm /usr/local/bin/agent-archive
```

Then remove the `agent-archive _hook ...` entry (marked with the comment
`agent-archive lifecycle capture`) from each included application's hook
configuration, and delete the `agent-archive` item for your bucket from
Keychain Access if you used R2. Without the LaunchAgent, a leftover `_hook`
entry still records session bookkeeping under `~/.local/share/agent-archive`
on every run, but nothing is ever published to remote storage — only
`_collect`, which the LaunchAgent schedules, does that.
