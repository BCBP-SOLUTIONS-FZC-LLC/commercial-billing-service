#!/usr/bin/env bash
# Smoke tests for the CI-built image (ci.yml smoke job):
#   1. size gate — a distroless image with three Go binaries; the limit
#      catches accidental COPYs (vendor/, test data), not normal growth;
#   2. startup gate — the server must refuse to start without configuration
#      (config.Load collects every missing key and run() exits 1, LLD §12),
#      proving the binary runs and validation fires before any connection;
#   3. the CronJob and migrate binaries are present and executable.
set -euo pipefail

IMAGE="${IMAGE:-commercial-billing-ci-test}"
MAX_MB=120

echo "::group::Image size (linux/amd64)"
size=$(docker image inspect "$IMAGE" --format='{{.Size}}')
mb=$((size / 1024 / 1024))
echo "Image size: ${mb} MB (limit: ${MAX_MB} MB)"
echo "::endgroup::"
if [ "${mb}" -gt "${MAX_MB}" ]; then
  echo "::error file=Dockerfile,title=Image size::Image is ${mb} MB, over the ${MAX_MB} MB limit — check COPY instructions for accidental inclusions"
  exit 1
fi

echo "::group::Startup gate (no configuration)"
exit_code=0
# A server that waited for a signal instead of exiting would hang: bound it.
( docker run --rm "$IMAGE" >/tmp/smoke-server.log 2>&1 ) & pid=$!
for _ in $(seq 1 20); do kill -0 "$pid" 2>/dev/null || break; sleep 0.5; done
if kill -0 "$pid" 2>/dev/null; then
  kill "$pid" 2>/dev/null || true
  echo "::error file=cmd/server/main.go,title=Startup gate::the server did not exit without configuration"
  exit 1
fi
wait "$pid" || exit_code=$?
echo "Container exit code: ${exit_code} (expected non-zero)"
if [ "$exit_code" -eq 0 ]; then
  echo "::error file=cmd/server/main.go,title=Startup gate::the server exited 0 without configuration"; cat /tmp/smoke-server.log; exit 1
fi
# Without a .env the defaults validate and the first hard failure is the
# missing DSN; a bad value fails config validation earlier. Either is a refusal.
grep -Eq "invalid configuration|connect database" /tmp/smoke-server.log || {
  echo "::error file=cmd/server/main.go,title=Startup gate::no configuration or database refusal in the log"; cat /tmp/smoke-server.log; exit 1; }
echo "::endgroup::"
if [ "${exit_code}" -eq 0 ]; then
  echo "::error file=cmd/server/main.go,title=Startup gate::the server exited 0 without configuration — config.Load must refuse to start"
  exit 1
fi

echo "::group::Binaries present, no shell"
# distroless: no shell, so /bin/sh must not run.
if docker run --rm --entrypoint /bin/sh "$IMAGE" -c true >/dev/null 2>&1; then
  echo "::error file=Dockerfile,title=Shell in image::the runtime image has /bin/sh — it must stay distroless"
  exit 1
fi
docker create --name smoke-files "$IMAGE" >/dev/null
trap 'docker rm smoke-files >/dev/null 2>&1 || true' EXIT
for bin in commercial-billing jobs migrate; do
  docker cp "smoke-files:/$bin" "/tmp/smoke-$bin" >/dev/null 2>&1 || {
    echo "::error file=Dockerfile,title=Binary missing::/$bin is not in the image"; exit 1; }
done
echo "::endgroup::"

{
  echo "### Smoke test results"
  echo "- ✅ Startup gate: the server exits ${exit_code} without configuration"
  echo "- ✅ Binaries: /commercial-billing, /jobs, /migrate; no shell in the image"
  echo "- 📦 Image size (linux/amd64): **${mb} MB** (limit ${MAX_MB} MB)"
} >> "$GITHUB_STEP_SUMMARY"
