#!/usr/bin/env bash
# PreToolUse guard for Edit|Write: denies edits to generated files and to .env
# secret files. .env-example stays editable. Reads the hook payload on stdin;
# prints a deny decision, or nothing (allow) for any other path.
jq -r '.tool_input.file_path // empty' | {
  read -r f
  case "$f" in
    */docs/swagger/*)
      r='docs/swagger/openapi.json is generated from the route table and DTOs: change internal/adapters/http and run go test ./internal/adapters/http -run Golden -update.' ;;
    */internal/adapters/eventbus/schemas/*.json)
      r='Event schemas are extracted from api/asyncapi.yaml: edit api/asyncapi.yaml and run make schema-extract.' ;;
    */docs/observability/metric-registry.md)
      r='metric-registry.md is generated from internal/adapters/metrics: change the code and run make metrics-inventory.' ;;
    */.env | */.env.*)
      r='.env files hold secrets (Stripe keys, webhook secrets, DB passwords) and are not edited by Claude. Change .env-example instead, or ask the user.' ;;
    *) exit 0 ;;
  esac
  jq -cn --arg r "$r" \
    '{hookSpecificOutput:{hookEventName:"PreToolUse",permissionDecision:"deny",permissionDecisionReason:$r}}'
}
