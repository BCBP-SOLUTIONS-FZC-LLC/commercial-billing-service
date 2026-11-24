#!/usr/bin/env bash
# BIL-OPS-2 (LLD §3.2): the outbox, its dead letters and the inbox's
# processed-message ledger belong to platform-events (outbox.EnqueueOrdered,
# outbox.Runner, inbox.Store). No hand-written SQL may read or write
# outbox_events, outbox_dead_letters or processed_events outside migrations
# and tests (which assert on them).
set -euo pipefail
tables='(outbox_events|outbox_dead_letters|processed_events)'
bad=$(grep -rniE --include='*.go' \
  "(insert[[:space:]]+into|update|delete[[:space:]]+from|from|join)[[:space:]]+${tables}\b" . \
  | grep -v '/migrations/' | grep -v '^\./test/' | grep -v '_test\.go:' || true)
if [ -n "$bad" ]; then
  echo "BIL-OPS-2 violation: direct SQL on a platform-events table:"
  echo "$bad"
  exit 1
fi
echo "BIL-OPS-2: outbox_events, outbox_dead_letters and processed_events only through platform-events"
