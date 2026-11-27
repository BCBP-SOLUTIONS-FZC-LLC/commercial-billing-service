#!/usr/bin/env bash
# The checked-out HEAD must be exactly RELEASE_TAG, not a branch tip, and the
# tagged commit must be on main (released code went through review and CI).
#
# This runs from the tagged commit, so a tag pushed on an unreviewed commit
# could carry an edited copy of this script: it catches mistakes, not abuse.
# Tag rulesets on v* (only release maintainers create, move or delete them)
# must also be enabled in the repository settings.
#
# FETCH_TOKEN (optional): token used to fetch main when the checkout did not
# persist credentials; passed to git through the environment, never on disk.
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"

if ! test "$RELEASE_TAG" = "$(git describe --tags --exact-match)"; then
  echo "::error title=Tag mismatch::RELEASE_TAG=${RELEASE_TAG} does not match git describe --tags --exact-match at HEAD"
  exit 1
fi
echo "  ✔  tag verified: ${RELEASE_TAG}"

fetch_main() {
  if [ -n "${FETCH_TOKEN:-}" ]; then
    local basic
    basic=$(printf 'x-access-token:%s' "$FETCH_TOKEN" | base64 | tr -d '\n')
    echo "::add-mask::${basic}"
    GIT_CONFIG_COUNT=1 \
      GIT_CONFIG_KEY_0="http.https://github.com/.extraheader" \
      GIT_CONFIG_VALUE_0="AUTHORIZATION: basic ${basic}" \
      git fetch --no-tags --quiet origin "+refs/heads/main:refs/remotes/origin/main"
  else
    git fetch --no-tags --quiet origin "+refs/heads/main:refs/remotes/origin/main"
  fi
}

if ! fetch_main; then
  echo "::error title=Main not fetched::could not fetch origin/main to check that ${RELEASE_TAG} is on it"
  exit 1
fi

tag_commit=$(git rev-parse "${RELEASE_TAG}^{commit}")
if ! git merge-base --is-ancestor "$tag_commit" origin/main; then
  echo "::error title=Tag not on main::${RELEASE_TAG} (${tag_commit}) is not an ancestor of origin/main; tag a commit that has been merged to main"
  exit 1
fi
echo "  ✔  ${RELEASE_TAG} is on origin/main"
