#!/usr/bin/env bash
# Cross-compiles the three runtime binaries (server, CronJobs, migrate) for
# Linux amd64 / arm64 — the service runs only in containers, so no darwin or
# windows builds — and writes a .sha256 per binary. The container image is
# the primary artifact; these are for audit and break-glass use.
#
#   commercial-billing_<tag>_linux_<arch>, jobs_<tag>_linux_<arch>, migrate_<tag>_linux_<arch>
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"

# name → package (a case, not an associative array: macOS still ships bash 3.2).
pkg() {
  case "$1" in
    commercial-billing) echo ./cmd/server ;;
    jobs) echo ./cmd/jobs ;;
    migrate) echo ./cmd/migrate ;;
  esac
}

for arch in amd64 arm64; do
  for name in commercial-billing jobs migrate; do
    out="${name}_${RELEASE_TAG}_linux_${arch}"
    ldflags="-s -w"
    [ "$name" != migrate ] && ldflags="$ldflags -X main.buildVersion=${RELEASE_TAG}"
    echo "  → ${out}"
    CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -ldflags "$ldflags" -o "$out" "$(pkg "$name")"
    chmod +x "$out"
    if command -v sha256sum >/dev/null; then sha256sum "$out" > "$out.sha256"; else shasum -a 256 "$out" > "$out.sha256"; fi
  done
done
echo "All binaries prepared."
