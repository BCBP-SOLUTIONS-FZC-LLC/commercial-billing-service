#!/usr/bin/env bash
# Reproducible image builds: SOURCE_DATE_EPOCH = the commit time of HEAD.
set -euo pipefail
SDE=$(git log -1 --format=%ct)
echo "SOURCE_DATE_EPOCH=${SDE}" >> "$GITHUB_ENV"
echo "SOURCE_DATE_EPOCH=${SDE}" >> "$GITHUB_OUTPUT"
