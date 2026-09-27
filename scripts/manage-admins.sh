#!/usr/bin/env bash
# Safely inspect and mutate the bridge's database-backed administrator allowlist.
set -Eeuo pipefail

readonly script_name="$(basename "$0")"

die() {
  echo "$script_name: error: $*" >&2
  exit 1
}

usage() {
  cat <<'EOF'
Usage:
  manage-admins.sh --db PATH [--backup PATH] bootstrap USER_ID
  manage-admins.sh --db PATH [--backup PATH] add USER_ID [admin|user]
  manage-admins.sh --db PATH [--backup PATH] remove USER_ID
  manage-admins.sh --db PATH audit

Commands:
  bootstrap USER_ID  Seed the first database administrator. This is accepted
                     only when ADMIN_USER_ID is unset or 0 and no admin row
                     exists yet.
  add USER_ID ROLE    Add or update an allowlist entry. ROLE defaults to user.
  remove USER_ID      Remove an allowlist entry. The last admin is protected.
  audit               Print every allowlist row and administrator count.

Options:
  --db PATH           Existing bridge SQLite database (required).
  --backup PATH       Backup destination for a mutating command. If omitted,
                      a new timestamped sibling backup is created.
  -h, --help          Show this help.

Stop the bridge before running a mutating command. The script never edits the
ADMIN_USER_ID environment setting; configure that separately in the service.
EOF
}

sql_quote() {
  local value=$1
  value=${value//\'/\'\'}
  printf "'%s'" "$value"
}

require_user_id() {
  local user_id=$1
  [[ "$user_id" =~ ^[1-9][0-9]*$ ]] || die "user ID must be a positive integer: $user_id"
}

require_role() {
  case "$1" in
    admin|user) ;;
    *) die "role must be admin or user: $1" ;;
  esac
}

db_path=""
backup_path=""

while (($# > 0)); do
  case "$1" in
    --db)
      (($# >= 2)) || die "--db requires a path"
      db_path=$2
      shift 2
      ;;
    --backup)
      (($# >= 2)) || die "--backup requires a path"
      backup_path=$2
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    -*)
      die "unknown option: $1"
      ;;
    *)
      break
      ;;
  esac
done

[[ -n "$db_path" ]] || die "--db is required"
[[ -f "$db_path" ]] || die "database does not exist: $db_path"
command -v sqlite3 >/dev/null 2>&1 || die "sqlite3 is required"

(($# >= 1)) || { usage >&2; exit 2; }
command_name=$1
shift

query() {
  sqlite3 -batch -noheader "$db_path" "$1"
}

execute() {
  sqlite3 -batch -bail "$db_path" "$1"
}

schema_present=$(query "SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'allowed_users';")
[[ "$schema_present" == "1" ]] || die "database is missing the allowed_users table: $db_path"

admin_count() {
  query "SELECT COUNT(*) FROM allowed_users WHERE role = 'admin';"
}

make_backup() {
  [[ -n "$backup_path" ]] || backup_path="${db_path}.admin-backup.$(date -u +%Y%m%dT%H%M%SZ).$$"
  [[ ! -e "$backup_path" ]] || die "backup destination already exists: $backup_path"
  # VACUUM INTO creates a consistent SQLite backup, including any WAL state.
  execute "VACUUM INTO $(sql_quote "$backup_path");" >/dev/null
  echo "backup: $backup_path"
}

case "$command_name" in
  bootstrap)
    (($# == 1)) || die "bootstrap requires USER_ID"
    user_id=$1
    require_user_id "$user_id"
    [[ "${ADMIN_USER_ID:-0}" == "0" ]] ||
      die "ADMIN_USER_ID is non-zero; use the configured bootstrap identity or an existing admin"
    [[ "$(admin_count)" == "0" ]] ||
      die "database already has an administrator; bootstrap is only for the first admin"

    make_backup
    execute "BEGIN IMMEDIATE;
INSERT INTO allowed_users (user_id, role, added_at)
VALUES ($user_id, 'admin', datetime('now'))
ON CONFLICT(user_id) DO UPDATE SET role = 'admin';
COMMIT;"
    echo "bootstrapped database administrator: $user_id"
    ;;
  add)
    (($# >= 1 && $# <= 2)) || die "add requires USER_ID [admin|user]"
    user_id=$1
    role=${2:-user}
    require_user_id "$user_id"
    require_role "$role"

    existing_role=$(query "SELECT COALESCE(role, '') FROM allowed_users WHERE user_id = $user_id;")
    if [[ "$existing_role" == "admin" && "$role" == "user" ]]; then
      [[ "$(admin_count)" -gt 1 ]] || die "refusing to demote the last administrator"
    fi

    make_backup
    execute "BEGIN IMMEDIATE;
INSERT INTO allowed_users (user_id, role, added_at)
VALUES ($user_id, '$role', datetime('now'))
ON CONFLICT(user_id) DO UPDATE SET role = excluded.role;
COMMIT;"
    echo "allowlist entry set: $user_id ($role)"
    ;;
  remove)
    (($# == 1)) || die "remove requires USER_ID"
    user_id=$1
    require_user_id "$user_id"
    existing_role=$(query "SELECT COALESCE(role, '') FROM allowed_users WHERE user_id = $user_id;")
    [[ -n "$existing_role" ]] || die "user is not in the allowlist: $user_id"
    if [[ "$existing_role" == "admin" ]]; then
      [[ "$(admin_count)" -gt 1 ]] || die "refusing to remove the last administrator"
    fi

    make_backup
    execute "BEGIN IMMEDIATE;
DELETE FROM allowed_users WHERE user_id = $user_id;
COMMIT;"
    echo "removed allowlist entry: $user_id"
    ;;
  audit)
    (($# == 0)) || die "audit takes no arguments"
    printf 'ADMIN_USER_ID=%s\n' "${ADMIN_USER_ID:-0}"
    printf 'database_admin_count=%s\n' "$(admin_count)"
    printf 'user_id\trole\tadded_at\n'
    query "SELECT user_id || char(9) || role || char(9) || added_at FROM allowed_users ORDER BY user_id;"
    ;;
  *)
    usage >&2
    die "unknown command: $command_name"
    ;;
esac
