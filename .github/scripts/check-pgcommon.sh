#!/usr/bin/env bash
# BIL-OPS-11 (LLD §3.2): every database connection, setting and statement
# goes through platform-pgcommon. depguard keeps drivers out and pgcommon
# behind the persistence boundary; this guard catches what an import rule
# cannot see:
#   1. SQLSTATE codes compared by hand: use pgcommon.IsUniqueViolation,
#      IsCheckViolation, IsForeignKeyViolation, IsSerializationFailure, …
#      (or pgcommon.ConstraintName for a code with no Is* helper);
#   2. hand-rolled SQLSTATE interfaces (errors.As to `SQLState() string`);
#   3. session settings set by hand (set_config, SET LOCAL / SESSION,
#      statement_timeout, lock_timeout): pgcommon's Config, GUCProvider and
#      WithGUCSet own them.
# Migrations and tests are exempt (tests assert on the codes).
set -euo pipefail
cd "$(dirname "$0")/../.."
fail=0
check() {
  local what=$1 pattern=$2
  local hits
  hits=$(grep -rnE --include='*.go' "$pattern" internal cmd pkg 2>/dev/null \
    | grep -v '_test\.go:' | grep -v '/migrations/' || true)
  if [[ -n "$hits" ]]; then
    echo "BIL-OPS-11 violation ($what):"; echo "$hits"; fail=1
  fi
}
check "SQLSTATE literal; use a pgcommon Is* helper" '(SQLState|sqlState|Code)\(?[a-zA-Z]*\)? *(==|!=) *"[0-9][0-9A-Z]{4}"|case +"[0-9]{2}[0-9A-Z]{3}"'
check "hand-rolled SQLState interface; map the error in adapters/postgres" 'interface *\{ *SQLState\(\) *string *\}'
check "session setting by hand; use pgcommon Config / GUCProvider / WithGUCSet" "set_config\(|SET +(LOCAL|SESSION) |SET +(statement_timeout|lock_timeout|idle_in_transaction_session_timeout)"
if [[ $fail -ne 0 ]]; then exit 1; fi
echo "BIL-OPS-11: SQLSTATEs, error mapping and session settings only through platform-pgcommon"
