#!/usr/bin/env bash
# BIL-OPS-2 (LLD §3.2): every outbox_events mutation goes through
# platform-events. No hand-written SQL may touch outbox_events outside
# migrations.
set -euo pipefail
bad=$(grep -rlniE --include='*.go' '(insert[[:space:]]+into|update|delete[[:space:]]+from)[[:space:]]+outbox_events' . \
  | grep -v '/migrations/' | grep -v '^\./test/' || true)
if [ -n "$bad" ]; then
  echo "BIL-OPS-2 violation: direct outbox_events mutation outside platform-events:"
  echo "$bad"
  exit 1
fi
echo "BIL-OPS-2: no direct outbox_events mutations"
