#!/usr/bin/env bash
# RFC 9457 (LLD §5.1, §17): platform-gincommon is the only producer of error
# responses. No production code may hand-write a problem body, set the
# problem media type itself, answer an error status with c.JSON / c.String /
# c.Status / AbortWithStatus*, or install its own recovery. Error paths go
# through gincommon.AbortWithProblem (or writeError, which calls it).
# Allowed: the idempotency replay (re-sends a stored gincommon response
# byte for byte) and the OpenAPI document (describes the media type).
set -euo pipefail
cd "$(dirname "$0")/../.."
allow='internal/adapters/http/(idempotency|openapi)\.go'
patterns=(
  '"application/problem\+json"'
  'gin\.Recovery(WithWriter)?\('
  'AbortWithStatus(JSON)?\('
  '\.(JSON|String|IndentedJSON|PureJSON)\(http\.Status(BadRequest|Unauthorized|Forbidden|NotFound|MethodNotAllowed|Conflict|Gone|RequestEntityTooLarge|UnsupportedMediaType|UnprocessableEntity|TooManyRequests|InternalServerError|NotImplemented|BadGateway|ServiceUnavailable|GatewayTimeout)'
  '\.(JSON|String)\((4|5)[0-9][0-9],'
  '\.Status\(http\.Status(BadRequest|Unauthorized|Forbidden|NotFound|MethodNotAllowed|Conflict|InternalServerError|ServiceUnavailable)'
)
fail=0
for p in "${patterns[@]}"; do
  hits=$(grep -rnE --include='*.go' "$p" internal cmd pkg 2>/dev/null | grep -v '_test\.go:' | grep -vE "^($allow):" || true)
  if [[ -n "$hits" ]]; then
    echo "RFC 9457 violation ($p):"; echo "$hits"; fail=1
  fi
done
[[ $fail -eq 0 ]] || { echo "error responses must go through gincommon.AbortWithProblem"; exit 1; }
echo "RFC 9457: every error response goes through platform-gincommon"
