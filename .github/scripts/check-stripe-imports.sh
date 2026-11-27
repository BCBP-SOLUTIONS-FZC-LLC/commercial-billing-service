#!/usr/bin/env bash
# BIL-PCI-1 (LLD §3.2): only internal/adapters/provider/stripe may import
# github.com/stripe/*. No provider type crosses a port.
set -euo pipefail
bad=$(grep -rln --include='*.go' '"github.com/stripe/' . \
  | grep -v '^\./internal/adapters/provider/stripe/' \
  | grep -v '^\./test/' || true)
if [ -n "$bad" ]; then
  echo "BIL-PCI-1 violation: github.com/stripe imported outside internal/adapters/provider/stripe:"
  echo "$bad"
  exit 1
fi
echo "BIL-PCI-1: stripe imports confined to internal/adapters/provider/stripe"
