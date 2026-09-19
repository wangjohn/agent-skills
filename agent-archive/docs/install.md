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
   `xattr -d com.apple.quarantine /usr/local/bin/agent-archive`.

5. Run the guided setup:

   ```sh
   agent-archive setup
   ```

   This walks through choosing storage (R2 or S3), which coding
   agents to capture from, and which project directories to activate. See
   the [engineering specification](../../docs/agent-run-archive-spec.md)
   for what each step does and why.

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

There is no dedicated `agent-archive uninstall` command yet. To fully
remove it:

```sh
# Stop the background collector.
launchctl bootout gui/$(id -u) ~/Library/LaunchAgents/com.agent-archive.collector.plist
rm ~/Library/LaunchAgents/com.agent-archive.collector.plist

# Remove local state: config, per-session cache, logs.
rm -rf ~/.local/share/agent-archive

# Remove the binary.
rm /usr/local/bin/agent-archive
```

Setup also adds an `agent-archive _hook ...` entry to each included
application's own hook configuration (`~/.codex/hooks.json`,
`~/.claude/settings.json`, or `~/.cursor/hooks.json`); remove that entry
by hand if you no longer want the application invoking it. Without the
LaunchAgent, `_hook` still records session bookkeeping under
`~/.local/share/agent-archive` on every run, but nothing is ever
published to remote storage — only `_collect`, which the LaunchAgent
schedules, does that.
