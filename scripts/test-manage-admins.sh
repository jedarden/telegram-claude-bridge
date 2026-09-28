#!/usr/bin/env bash
# Integration coverage for the offline administrator allowlist workflow.
set -Eeuo pipefail

readonly script_dir="$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly manage_script="$script_dir/manage-admins.sh"
readonly temp_dir="$(mktemp -d)"
readonly db_path="$temp_dir/bridge.db"

cleanup() {
  rm -rf "$temp_dir"
}
trap cleanup EXIT

fail() {
  echo "test-manage-admins.sh: error: $*" >&2
  exit 1
}

assert_eq() {
  local actual=$1 expected=$2 description=$3
  [[ "$actual" == "$expected" ]] ||
    fail "$description: got $(printf '%q' "$actual"), want $(printf '%q' "$expected")"
}

query() {
  sqlite3 -batch -noheader "$db_path" "$1"
}

run_manage() {
  local backup=$1
  shift
  ADMIN_USER_ID=0 "$manage_script" --db "$db_path" --backup "$temp_dir/$backup" "$@"
}

sqlite3 "$db_path" <<'SQL'
CREATE TABLE allowed_users (
  user_id INTEGER PRIMARY KEY,
  role TEXT NOT NULL,
  added_at TEXT NOT NULL
);
SQL

run_manage bootstrap-backup bootstrap 9001 >/dev/null
assert_eq "$(query "SELECT role FROM allowed_users WHERE user_id = 9001;")" "admin" "bootstrap role"
assert_eq "$(query "SELECT COUNT(*) FROM allowed_users WHERE role = 'admin';")" "1" "bootstrap administrator count"

run_manage grant-backup add 9002 admin >/dev/null
run_manage demote-backup add 9002 user >/dev/null
assert_eq "$(query "SELECT role FROM allowed_users WHERE user_id = 9002;")" "user" "demoted role"

run_manage add-user-backup add 9003 user >/dev/null
run_manage remove-user-backup remove 9003 >/dev/null
assert_eq "$(query "SELECT COUNT(*) FROM allowed_users WHERE user_id = 9003;")" "0" "removed user count"

if ADMIN_USER_ID=0 "$manage_script" --db "$db_path" --backup "$temp_dir/last-admin-backup" remove 9001 >/dev/null 2>"$temp_dir/last-admin.err"; then
  fail "removing the last administrator unexpectedly succeeded"
fi
grep -F "last administrator" "$temp_dir/last-admin.err" >/dev/null ||
  fail "last-administrator rejection did not explain the protected state"

if ADMIN_USER_ID=123 "$manage_script" --db "$db_path" --backup "$temp_dir/nonzero-bootstrap-backup" bootstrap 9010 >/dev/null 2>"$temp_dir/nonzero-bootstrap.err"; then
  fail "bootstrap with a configured ADMIN_USER_ID unexpectedly succeeded"
fi
grep -F "ADMIN_USER_ID is non-zero" "$temp_dir/nonzero-bootstrap.err" >/dev/null ||
  fail "non-zero ADMIN_USER_ID rejection was not reported"

if ADMIN_USER_ID=0 "$manage_script" --db "$db_path" --backup "$temp_dir/repeat-bootstrap-backup" bootstrap 9010 >/dev/null 2>"$temp_dir/repeat-bootstrap.err"; then
  fail "repeat bootstrap unexpectedly succeeded"
fi
grep -F "already has an administrator" "$temp_dir/repeat-bootstrap.err" >/dev/null ||
  fail "repeat bootstrap rejection was not reported"

audit=$(ADMIN_USER_ID=0 "$manage_script" --db "$db_path" audit)
grep -F "ADMIN_USER_ID=0" <<<"$audit" >/dev/null ||
  fail "audit did not report the zero-bootstrap state"
grep -F "database_admin_count=1" <<<"$audit" >/dev/null ||
  fail "audit did not report the administrator count"
grep -F $'9001\tadmin\t' <<<"$audit" >/dev/null ||
  fail "audit did not report the bootstrap administrator"

for backup in bootstrap-backup grant-backup demote-backup add-user-backup remove-user-backup; do
  [[ -s "$temp_dir/$backup" ]] || fail "missing SQLite backup: $backup"
done

echo "administrator allowlist workflow: ok"
