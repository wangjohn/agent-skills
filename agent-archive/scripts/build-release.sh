#!/usr/bin/env bash
# Builds agent-archive's Intel and ARM64 macOS binaries and their SHA-256
# checksums into dist/. Used by both the release workflow and local
# maintainers, so the two never drift: whatever this script produces is
# exactly what gets signed, notarized, and published.
#
# Usage: VERSION=v1.2.3 scripts/build-release.sh
# VERSION defaults to "dev" (a build that reports `agent-archive --version`
# as "dev", never suitable for release).
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

version="${VERSION:-dev}"
out_dir="dist"
rm -rf "$out_dir"
mkdir -p "$out_dir"

ldflags="-s -w -X github.com/wangjohn/agent-skills/agent-archive/internal/cli.Version=${version}"

for arch in amd64 arm64; do
  binary="$out_dir/agent-archive-darwin-${arch}"
  echo "building $binary (version=${version})"
  GOOS=darwin GOARCH="$arch" CGO_ENABLED=1 go build -trimpath -ldflags "$ldflags" -o "$binary" ./cmd/agent-archive
done

(
  cd "$out_dir"
  shasum -a 256 agent-archive-darwin-amd64 agent-archive-darwin-arm64 > SHA256SUMS
)

echo "built:"
ls -la "$out_dir"
