#!/usr/bin/env bash
# Validate presence without printing credential values. Used only by release CI.
set -euo pipefail
if [[ "${APPLE_SIGNING_ENABLED:-}" != true ]]; then
  echo 'Release signing must be enabled; unsigned releases are not published.' >&2
  exit 1
fi
for name in APPLE_CERTIFICATE_P12_BASE64 APPLE_CERTIFICATE_PASSWORD APPLE_SIGNING_IDENTITY APPLE_ID APPLE_TEAM_ID APPLE_APP_SPECIFIC_PASSWORD; do
  if [[ -z "${!name:-}" ]]; then
    echo "Missing release secret: $name" >&2
    exit 1
  fi
done
