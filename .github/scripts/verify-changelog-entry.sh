#!/usr/bin/env bash
# Fails the release when CHANGELOG.md has no section for RELEASE_TAG
# (VERSIONING.md, maintainer release process step 2).
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"

VERSION="${RELEASE_TAG#v}"
if ! grep -q "^## \[${VERSION}\]" CHANGELOG.md 2>/dev/null; then
  echo "::error file=CHANGELOG.md::Missing '## [${VERSION}] - YYYY-MM-DD' section — move the [Unreleased] entries into it before tagging"
  exit 1
fi
echo "  ✔  changelog entry found for ${VERSION}"
