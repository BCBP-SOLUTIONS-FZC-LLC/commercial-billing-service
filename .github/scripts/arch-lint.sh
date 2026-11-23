#!/usr/bin/env bash
# Enforce .go-arch-lint.yml component boundaries (LLD §3.2).
set -euo pipefail
if ! command -v go-arch-lint >/dev/null; then
  go install github.com/fe3dback/go-arch-lint@v1.15.0
fi
go-arch-lint check --project-path .
