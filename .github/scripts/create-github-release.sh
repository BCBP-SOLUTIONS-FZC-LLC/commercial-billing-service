#!/usr/bin/env bash
# Creates the GitHub release with the binaries, checksums, SBOM and SLSA
# provenance attached.
set -euo pipefail

: "${RELEASE_TAG:?RELEASE_TAG is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${IMAGE_DIGEST:?IMAGE_DIGEST is required}"

shopt -s nullglob
assets=(commercial-billing_* jobs_* migrate_*)
binaries=()
for f in "${assets[@]}"; do
  case "$f" in *.sha256) ;; *) binaries+=("$f") ;; esac
done
if [ "${#binaries[@]}" -eq 0 ]; then
  echo "::error::no release binaries — run prepare-release-binary.sh first"
  exit 1
fi
for f in release-notes.md provenance.slsa.json sbom.cyclonedx.json; do
  test -f "$f" || { echo "::error::Release asset missing: ${f}"; exit 1; }
done

# One verifiable file: sha256sum --check checksums.txt
sha256sum "${binaries[@]}" > checksums.txt

PRERELEASE_FLAG=""
if echo "$RELEASE_TAG" | grep -q -- '-'; then
  PRERELEASE_FLAG="--prerelease"
fi

DIGEST_SHORT="${IMAGE_DIGEST#sha256:}"
DIGEST_SHORT="${DIGEST_SHORT:0:12}"

gh release create "$RELEASE_TAG" \
  --title "${RELEASE_TAG} · sha256:${DIGEST_SHORT}" \
  --notes-file release-notes.md \
  "${binaries[@]}" checksums.txt provenance.slsa.json sbom.cyclonedx.json \
  $PRERELEASE_FLAG
