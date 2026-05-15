#!/usr/bin/env bash
# © 2026 Platform Engineering Labs Inc.
#
# SPDX-License-Identifier: Apache-2.0
#
# install-atlas.sh — download a pinned atlas CLI binary into the repo's
# bin/ directory so `make build`, integration tests, and conformance
# tests can find it via PATH (when bin/ is prepended) or via
# `exec.LookPath("atlas")` after the plugin install copies it.
#
# Mirrors the pattern formae uses for pkl-reader-helm
# (formae/scripts/install-helm-reader.sh). Stop-gap until the formae
# core installer bundles atlas alongside the formae binary; see
# ~/dev/personal/engineering-notes/formae-mcp/2026-05-14-plugin-new-skill-gaps.md G-1.
#
# Pin policy: ATLAS_VERSION env var (default tracks a known-good
# community release). Override for one-off testing.
#
# Output: ./bin/atlas (chmod 755). Re-running is a no-op when the
# binary's `version` subcommand already reports the requested pin.

set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)

# Ariga's release URL only resolves "latest"; specific versions are not
# served at well-known paths today. Once they expose per-version URLs
# under release.ariga.io, switch DEFAULT_VERSION to a real semver pin.
DEFAULT_VERSION="latest"
VERSION=${ATLAS_VERSION:-$DEFAULT_VERSION}

case "$(uname -s)-$(uname -m)" in
  Darwin-x86_64)              ASSET="atlas-darwin-amd64-${VERSION}" ;;
  Darwin-arm64)               ASSET="atlas-darwin-arm64-${VERSION}" ;;
  Linux-x86_64)               ASSET="atlas-linux-amd64-${VERSION}" ;;
  Linux-aarch64|Linux-arm64)  ASSET="atlas-linux-arm64-${VERSION}" ;;
  *)
    echo "install-atlas: unsupported platform $(uname -s)-$(uname -m)" >&2
    exit 1
    ;;
esac

URL="https://release.ariga.io/atlas/${ASSET}"

# Install to ./tools/atlas, NOT ./bin/atlas — the plugin binary itself
# is built to ./bin/atlas (because the plugin's name is "atlas"), so
# colocating would clobber it on every `make build`.
mkdir -p "${REPO_ROOT}/tools"
TARGET="${REPO_ROOT}/tools/atlas"

# With VERSION=latest there's no useful idempotency check (the upstream
# binary can change at any time), so always re-download. When a pinned
# version is available, prefer skipping when `atlas version` matches.
if [[ "$VERSION" != "latest" && -x "$TARGET" ]]; then
  if existing=$("$TARGET" version 2>/dev/null | head -1 | awk '{print $2}'); then
    if [[ "$existing" == "$VERSION" || "v$existing" == "$VERSION" ]]; then
      echo "install-atlas: atlas @ $VERSION already present"
      exit 0
    fi
    if [[ -n "$existing" ]]; then
      echo "install-atlas: replacing $existing -> $VERSION"
    fi
  fi
fi
if [[ "$VERSION" == "latest" && -x "$TARGET" ]]; then
  echo "install-atlas: atlas present (latest pin; not re-downloading — set ATLAS_VERSION to force)"
  exit 0
fi

TMP=$(mktemp "${REPO_ROOT}/bin/.atlas.XXXXXX")
trap 'rm -f "$TMP"' EXIT

echo "install-atlas: downloading $URL"
if ! curl -fsSL "$URL" -o "$TMP"; then
  echo "install-atlas: download failed" >&2
  exit 1
fi

chmod 755 "$TMP"
mv "$TMP" "$TARGET"
trap - EXIT

echo "install-atlas: installed $TARGET"
"$TARGET" version 2>/dev/null | head -1 || true
