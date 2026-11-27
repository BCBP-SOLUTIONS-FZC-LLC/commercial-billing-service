#!/usr/bin/env bash
# Writes docker/build-push-action cache-from / cache-to multiline outputs
# (ci.yml build-image job; keeps heredoc markers out of inline YAML).
#
# The registry cache (REGISTRY_REF) is shared by every ref and read by the
# main push job that publishes the image, so only that job (main only,
# packages: write) writes it, in its own cache-to: a pull request, even from
# this repository, could otherwise poison the layers of the published image.
# build-image runs with packages: read for every event: it reads the
# registry cache and writes only the GHA cache, which GitHub scopes to the
# ref (main never reads a pull request's GHA entries).
set -euo pipefail

: "${REGISTRY_REF:?REGISTRY_REF is required}"

{
  echo 'from<<EOF'
  echo 'type=gha'
  echo "type=registry,ref=${REGISTRY_REF}"
  echo 'EOF'
  echo 'to<<EOF'
  echo 'type=gha,mode=max'
  echo 'EOF'
} >> "$GITHUB_OUTPUT"
