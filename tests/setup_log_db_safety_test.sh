#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fixture="$(mktemp -d)"
trap 'rm -rf "$fixture"' EXIT

fail() {
  printf 'setup-log-db safety test failed: %s\n' "$1" >&2
  exit 1
}

assert_eq() {
  local expected="$1" actual="$2" message="$3"
  [[ "$actual" == "$expected" ]] ||
    fail "$message (expected=[$expected], actual=[$actual])"
}

assert_mode_600() {
  local path="$1" mode
  mode="$(stat -c '%a' -- "$path")"
  [[ "$mode" == "600" ]] || fail "$path mode is $mode instead of 600"
}

assert_no_temp_files() {
  local target="$1"
  if compgen -G "${target}.tmp.*" >/dev/null; then
    fail "temporary files remain for $target"
  fi
}

# setup-log-db.sh is sourceable so these tests exercise the production helpers
# directly without requiring a Docker daemon.
source "$repo_root/setup-log-db.sh"

clear_log_db_transaction_memory() {
  LOG_ENV_SNAPSHOT_CONTENT=""
  LOG_ENV_SNAPSHOT_IDENTITY=""
  LOG_ENV_ATTEMPT_CONTENT=""
  LOG_ENV_ATTEMPT_READY=false
  LOG_ENV_CANDIDATE_CONTENT=""
  LOG_OVERRIDE_SNAPSHOT_EXISTS=false
  LOG_OVERRIDE_SNAPSHOT_CONTENT=""
  LOG_OVERRIDE_SNAPSHOT_IDENTITY=""
  LOG_OVERRIDE_ATTEMPT_CONTENT=""
  LOG_OVERRIDE_ATTEMPT_READY=false
  LOG_OVERRIDE_ATTEMPT_REMOVED=false
  LOG_OVERRIDE_CANDIDATE_EXISTS=false
  LOG_OVERRIDE_CANDIDATE_CONTENT=""
}

lock_project="$fixture/lock-project"
mkdir "$lock_project"
lock_path="$(setup_project_state_lock_path "$lock_project")"
assert_eq '.lock-project.state.lock' "$(basename -- "$lock_path")" \
  'setup state lock did not use the shared sibling path contract'
lock_victim="$fixture/lock-victim"
printf 'victim-content\n' > "$lock_victim"
ln -s "$lock_victim" "$lock_path"
if (
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }
  acquire_setup_state_lock "$lock_project"
) >/dev/null 2>&1; then
  fail 'setup state lock accepted a symlink'
fi
assert_eq 'victim-content' "$(<"$lock_victim")" \
  'setup state lock symlink rejection modified its victim'
rm -f -- "$lock_path"
: > "$lock_path"
chmod 666 "$lock_path"
(
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }
  acquire_setup_state_lock "$lock_project"
) || fail 'setup state lock rejected a current-user regular file'
assert_eq '600' "$(stat -c '%a' -- "$lock_path")" \
  'setup state lock did not repair mode to 600'
assert_eq "$(id -u)" "$(stat -c '%u' -- "$lock_path")" \
  'setup state lock changed or failed to verify its owner'
if (
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  setup_state_lock_path_owner() { printf '%s\n' "$(( $(id -u) + 1 ))"; }
  log_error() { :; }
  acquire_setup_state_lock "$lock_project"
) >/dev/null 2>&1; then
  fail 'setup state lock accepted a file reported as foreign-owned'
fi

if ! (
  docker() {
    [[ "$1" == "compose" && "$2" == "version" ]] || return 1
    printf 'Docker Compose version v2.24.0\n'
  }
  detect_docker_compose
  [[ "$DOCKER_COMPOSE" == "docker compose" ]]
); then
  fail 'Docker Compose v2.24.0 was rejected'
fi
if (
  docker() { printf 'Docker Compose version v2.23.3\n'; }
  detect_docker_compose >/dev/null 2>&1
); then
  fail 'Docker Compose older than v2.24 was accepted'
fi
if (
  docker() { printf 'docker-compose version 1.29.2\n'; }
  detect_docker_compose >/dev/null 2>&1
); then
  fail 'legacy docker-compose v1 was accepted'
fi

pg_url='postgresql://ops:p%7C%26%5C@127.0.0.1:5433/logs?sslmode=require&application_name=relay'
assert_eq 'postgres_url' "$(detect_dsn_format "$pg_url")" 'PostgreSQL URL format was not detected'
assert_eq 'postgres' "$(extract_dsn_engine "$pg_url")" 'PostgreSQL URL engine was not detected'
assert_eq '127.0.0.1' "$(extract_dsn_host "$pg_url")" 'PostgreSQL URL host was not extracted'
assert_eq '5433' "$(extract_dsn_port "$pg_url")" 'PostgreSQL URL port was not extracted'
assert_eq \
  'postgresql://ops:p%7C%26%5C@log-db:5544/logs?sslmode=require&application_name=relay' \
  "$(rewrite_dsn_host_port "$pg_url" 'log-db' '5544')" \
  'PostgreSQL URL rewrite did not preserve credentials and query options'

pg_keyword='host=127.0.0.1 port=5432 user=ops password=p|a&ss dbname=logs sslmode=require application_name=relay'
assert_eq 'postgres_keyword' "$(detect_dsn_format "$pg_keyword")" \
  'PostgreSQL keyword format was not detected'
assert_eq \
  'host=log-db port=5544 user=ops password=p|a&ss dbname=logs sslmode=require application_name=relay' \
  "$(rewrite_dsn_host_port "$pg_keyword" 'log-db' '5544')" \
  'PostgreSQL keyword rewrite changed non-routing fields'
if detect_dsn_format "host=db user=ops password='with space' dbname=logs" >/dev/null 2>&1; then
  fail 'ambiguous quoted PostgreSQL keyword DSN was accepted'
fi
if detect_dsn_format 'host=db hostaddr=127.0.0.1 dbname=logs' >/dev/null 2>&1; then
  fail 'PostgreSQL hostaddr routing override was accepted'
fi
for nested_dbname in \
  'host=db port=5432 dbname=host=evil.example' \
  'host=db port=5432 dbname=postgresql://evil.example/logs' \
  'host=db port=5432 dbname=POSTGRES://evil.example/logs'
do
  if detect_dsn_format "$nested_dbname" >/dev/null 2>&1; then
    fail "PostgreSQL keyword dbname nested connection string was accepted: $nested_dbname"
  fi
done

mysql_url='mysql://ops:p%7C%26%5C@127.0.0.1:3306/logs?charset=utf8mb4&parseTime=true'
assert_eq 'mysql_url' "$(detect_dsn_format "$mysql_url")" 'MySQL URL format was not detected'
assert_eq 'mysql' "$(extract_dsn_engine "$mysql_url")" 'MySQL URL engine was not detected'
assert_eq \
  'mysql://ops:p%7C%26%5C@mysql-log:4406/logs?charset=utf8mb4&parseTime=true' \
  "$(rewrite_dsn_host_port "$mysql_url" 'mysql-log' '4406')" \
  'MySQL URL rewrite did not preserve credentials and query options'
inspected_mysql_url="$(
  docker() {
    printf 'OTHER=value\nLOG_SQL_DSN=%s\n' "$mysql_url"
  }
  docker_inspect_env_value 'new-api' 'LOG_SQL_DSN'
)"
assert_eq "$mysql_url" "$inspected_mysql_url" \
  'Docker environment extraction truncated DSN content after an equals sign'

mysql_go='ops:p|a&ss\\word@tcp(127.0.0.1:3306)/logs?charset=utf8mb4&parseTime=true'
assert_eq 'mysql_go' "$(detect_dsn_format "$mysql_go")" 'MySQL Go DSN format was not detected'
assert_eq \
  'ops:p|a&ss\\word@tcp(mysql-log:4406)/logs?charset=utf8mb4&parseTime=true' \
  "$(rewrite_dsn_host_port "$mysql_go" 'mysql-log' '4406')" \
  'MySQL Go DSN rewrite changed credentials or query options'

ipv6_url='postgres://ops:secret@[2001:db8::5]:5432/logs?sslmode=require'
assert_eq '2001:db8::5' "$(extract_dsn_host "$ipv6_url")" 'IPv6 URL host was not extracted'
assert_eq \
  'postgres://ops:secret@[2001:db8::6]:6543/logs?sslmode=require' \
  "$(rewrite_dsn_host_port "$ipv6_url" '2001:db8::6' '6543')" \
  'IPv6 URL rewrite did not preserve bracket syntax'
grep -Fq 'GlobalIPv6Address' "$repo_root/setup-log-db.sh" ||
  fail 'Docker network discovery does not inspect container IPv6 addresses'
if detect_dsn_format 'postgres://ops:secret@db:5432/logs?host=evil' >/dev/null 2>&1; then
  fail 'PostgreSQL URL host query override was accepted'
fi
if detect_dsn_format 'postgres://ops:secret@db:5432/logs?%68ost=evil' >/dev/null 2>&1; then
  fail 'encoded PostgreSQL URL host query override was accepted'
fi

project="$fixture/project"
mkdir -p "$project"
PROJECT_DIR="$project"
ENV_FILE="$project/.env"
cat > "$ENV_FILE" <<'EOF'
NEWAPI_NETWORK='new-api_default'
LOG_SQL_DSN='old-value'
LOG_SQL_DSN='duplicate-old-value'
LOG_NETWORK='old-network'
UNCHANGED='keep | & \\ spaces'
EOF
chmod 644 "$ENV_FILE"

special_dsn="host=db port=5432 user=report password=p|a&ss\\word with spaces and 'quote' dbname=logs sslmode=disable"
persist_log_db_env "$special_dsn" 'logs-net_1' ||
  fail 'atomic .env persistence rejected a valid special-character DSN'
assert_eq "$special_dsn" "$(read_env_value LOG_SQL_DSN)" \
  'special-character DSN did not round-trip exactly'
assert_eq 'logs-net_1' "$(read_env_value LOG_NETWORK)" \
  'LOG_NETWORK did not round-trip exactly'
assert_eq 'keep | & \\ spaces' "$(read_env_value UNCHANGED)" \
  'unrelated dotenv content changed'
[[ "$(grep -c '^LOG_SQL_DSN=' "$ENV_FILE")" == "1" ]] ||
  fail 'LOG_SQL_DSN duplicates were not removed'
[[ "$(grep -c '^LOG_NETWORK=' "$ENV_FILE")" == "1" ]] ||
  fail 'LOG_NETWORK duplicates were not removed'
assert_mode_600 "$ENV_FILE"
assert_no_temp_files "$ENV_FILE"

before_newline_rejection="$(<"$ENV_FILE")"
if persist_log_db_env $'host=db\npassword=split' 'logs-net_1'; then
  fail 'newline-containing DSN was accepted'
fi
assert_eq "$before_newline_rejection" "$(<"$ENV_FILE")" \
  'newline rejection changed the existing .env'

victim="$fixture/symlink-victim"
printf 'victim-content\n' > "$victim"
symlink_env="$fixture/symlink.env"
ln -s "$victim" "$symlink_env"
ENV_FILE="$symlink_env"
if persist_log_db_env "$special_dsn" 'logs-net_1'; then
  fail 'symlink .env target was accepted'
fi
assert_eq 'victim-content' "$(<"$victim")" 'symlink victim was modified'

directory_env="$fixture/directory.env"
mkdir "$directory_env"
ENV_FILE="$directory_env"
if persist_log_db_env "$special_dsn" 'logs-net_1'; then
  fail 'directory .env target was accepted'
fi

sync_failure="$fixture/sync-failure.env"
printf 'old-sync\n' > "$sync_failure"
if (
  sync() { return 1; }
  atomic_write_setup_file "$sync_failure" 'new-sync'
); then
  fail 'file sync failure was reported as success'
fi
assert_eq 'old-sync' "$(<"$sync_failure")" \
  'file sync failure replaced the old target'
assert_no_temp_files "$sync_failure"

move_failure="$fixture/move-failure.env"
printf 'old-move\n' > "$move_failure"
if (
  mv() { return 1; }
  atomic_write_setup_file "$move_failure" 'new-move'
); then
  fail 'rename failure was reported as success'
fi
assert_eq 'old-move' "$(<"$move_failure")" \
  'rename failure replaced the old target'
assert_no_temp_files "$move_failure"

ownership_failure="$fixture/ownership-failure.env"
printf 'old-ownership\n' > "$ownership_failure"
ownership_before="$(stat -c '%u:%g' -- "$ownership_failure")"
if (
  chown() { return 1; }
  atomic_write_setup_file "$ownership_failure" 'new-ownership'
); then
  fail 'ownership preservation failure was reported as success'
fi
assert_eq 'old-ownership' "$(<"$ownership_failure")" \
  'ownership preservation failure replaced the old target'
assert_eq "$ownership_before" "$(stat -c '%u:%g' -- "$ownership_failure")" \
  'ownership preservation failure changed the old target owner or group'
assert_no_temp_files "$ownership_failure"

primary_gid="$(id -g)"
secondary_gid="$(id -G | tr ' ' '\n' | awk -v primary="$primary_gid" '$0 != primary { print; exit }')"
if [[ -n "$secondary_gid" ]]; then
  ownership_target="$fixture/ownership-target.env"
  printf 'old-owner\n' > "$ownership_target"
  chgrp "$secondary_gid" "$ownership_target"
  ownership_before="$(stat -c '%u:%g' -- "$ownership_target")"
  atomic_write_setup_file "$ownership_target" 'new-owner' ||
    fail 'atomic replacement could not preserve an existing owner and group'
  assert_eq "$ownership_before" "$(stat -c '%u:%g' -- "$ownership_target")" \
    'atomic replacement changed the existing target owner or group'
  assert_mode_600 "$ownership_target"
  assert_no_temp_files "$ownership_target"
fi

race_target="$fixture/race-target.env"
race_victim="$fixture/race-victim.env"
printf 'old-race\n' > "$race_target"
printf 'race-victim\n' > "$race_victim"
race_identity="$(setup_target_identity "$race_target")"
if (
  sync_calls=0
  sync() {
    sync_calls=$((sync_calls + 1))
    if (( sync_calls == 1 )); then
      rm -f -- "$race_target"
      ln -s "$race_victim" "$race_target"
    fi
    command sync "$@"
  }
  atomic_write_setup_file "$race_target" 'new-race' "$race_identity"
); then
  fail 'target symlink race was reported as success'
fi
[[ -L "$race_target" ]] || fail 'race fixture did not replace the target with a symlink'
assert_eq 'race-victim' "$(<"$race_victim")" \
  'target symlink race modified the victim'
assert_no_temp_files "$race_target"

parent_sync_failure="$fixture/parent-sync-failure.env"
printf 'old-parent-sync\n' > "$parent_sync_failure"
if (
  sync_calls=0
  sync() {
    sync_calls=$((sync_calls + 1))
    if (( sync_calls == 2 )); then
      return 1
    fi
    command sync "$@"
  }
  atomic_write_setup_file "$parent_sync_failure" 'new-parent-sync'
); then
  fail 'parent sync failure was reported as success'
fi
assert_eq 'new-parent-sync' "$(<"$parent_sync_failure")" \
  'parent sync failure did not leave the complete renamed file image'
assert_mode_600 "$parent_sync_failure"
assert_no_temp_files "$parent_sync_failure"

ENV_FILE="$project/.env"
PROJECT_DIR="$project"
commit_log_db_configuration "$special_dsn" 'logs-net_1' ||
  fail 'successful two-file configuration transaction was rejected'
override="$project/docker-compose.override.yml"
grep -Fq "name: 'logs-net_1'" "$override" ||
  fail 'generated override did not quote the Docker network name'
grep -Fq 'log-db-network:' "$override" ||
  fail 'generated override is missing the log database network'
assert_mode_600 "$override"
assert_no_temp_files "$override"

old_env_content="$(<"$ENV_FILE")"
old_override_content="$(<"$override")"
if (
  sync_calls=0
  sync() {
    sync_calls=$((sync_calls + 1))
    if (( sync_calls == 2 )); then
      return 1
    fi
    command sync "$@"
  }
  commit_log_db_configuration "$special_dsn" 'logs-net-parent-sync-failure'
); then
  fail 'override parent sync failure was reported as a committed transaction'
fi
assert_eq "$old_env_content" "$(<"$ENV_FILE")" \
  'override failure changed .env before the credential commit'
assert_eq "$old_override_content" "$(<"$override")" \
  'override failure did not restore the previous override snapshot'

if (
  persist_log_db_env() { return 1; }
  commit_log_db_configuration "$special_dsn" 'logs-net-env-failure'
); then
  fail '.env persistence failure was reported as a committed transaction'
fi
assert_eq "$old_env_content" "$(<"$ENV_FILE")" \
  '.env failure changed the previous dotenv image'
assert_eq "$old_override_content" "$(<"$override")" \
  '.env failure did not restore the previous override snapshot'

if (
  sync_calls=0
  sync() {
    sync_calls=$((sync_calls + 1))
    if (( sync_calls == 4 )); then
      return 1
    fi
    command sync "$@"
  }
  commit_log_db_configuration "$special_dsn" 'logs-net-env-parent-sync-failure'
); then
  fail '.env parent sync failure was reported as a committed transaction'
fi
assert_eq "$old_env_content" "$(<"$ENV_FILE")" \
  '.env parent sync failure did not restore the old dotenv snapshot'
assert_eq "$old_override_content" "$(<"$override")" \
  '.env parent sync failure did not restore the old override snapshot'
assert_no_temp_files "$ENV_FILE"
assert_no_temp_files "$override"

crash_project="$fixture/post-commit-crash-project"
mkdir -p "$crash_project"
PROJECT_DIR="$crash_project"
ENV_FILE="$crash_project/.env"
COMPOSE_FILE="$crash_project/docker-compose.yml"
printf '%s\n' \
  "NEWAPI_NETWORK='new-api_default'" \
  "LOG_SQL_DSN='old-crash-secret'" \
  "LOG_NETWORK='old-crash-log-net'" > "$ENV_FILE"
chmod 600 "$ENV_FILE"
printf 'services: {}\n' > "$COMPOSE_FILE"
printf 'services: {}\n' > "$crash_project/docker-compose.logdb.yml"
commit_log_db_configuration "$special_dsn" 'old-crash-log-net' ||
  fail 'could not prepare the post-commit crash baseline'
crash_old_env="$(<"$ENV_FILE")"
crash_override="$crash_project/docker-compose.override.yml"
crash_old_override="$(<"$crash_override")"

# The marker rename can succeed even when its following parent-directory fsync
# fails. That ambiguous publish result must retain all four backing images so a
# later locked startup can still recover the transaction.
marker_sync_counter="$fixture/marker-parent-sync-counter"
printf '0\n' > "$marker_sync_counter"
if (
  sync() {
    local count
    count="$(<"$marker_sync_counter")"
    count=$((count + 1))
    printf '%s\n' "$count" > "$marker_sync_counter"
    if (( count == 10 )); then
      return 1
    fi
    command sync "$@"
  }
  begin_log_db_configuration_transaction "$special_dsn" 'candidate-marker-sync-log-net'
); then
  fail 'marker parent sync failure was reported as a durable transaction begin'
fi
crash_marker="$(log_db_transaction_marker_path)"
crash_env_snapshot="$(log_db_transaction_env_snapshot_path)"
crash_override_snapshot="$(log_db_transaction_override_snapshot_path)"
crash_env_candidate="$(log_db_transaction_env_candidate_path)"
crash_override_candidate="$(log_db_transaction_override_candidate_path)"
for transaction_artifact in \
  "$crash_marker" "$crash_env_snapshot" "$crash_override_snapshot" \
  "$crash_env_candidate" "$crash_override_candidate"
do
  assert_mode_600 "$transaction_artifact"
done
clear_log_db_transaction_memory
docker() { return 0; }
recover_incomplete_log_db_transaction ||
  fail 'startup could not recover a marker published before parent sync failure'
unset -f docker
for transaction_artifact in \
  "$crash_marker" "$crash_env_snapshot" "$crash_override_snapshot" \
  "$crash_env_candidate" "$crash_override_candidate"
do
  [[ ! -e "$transaction_artifact" && ! -L "$transaction_artifact" ]] ||
    fail "marker parent sync recovery retained $transaction_artifact"
done

begin_log_db_configuration_transaction "$special_dsn" 'candidate-crash-log-net' ||
  fail 'could not persist the post-commit crash transaction snapshot'
assert_mode_600 "$crash_marker"
assert_mode_600 "$crash_env_snapshot"
assert_mode_600 "$crash_override_snapshot"
assert_mode_600 "$crash_env_candidate"
assert_mode_600 "$crash_override_candidate"
commit_log_db_configuration "$special_dsn" 'candidate-crash-log-net' ||
  fail 'could not commit the post-commit crash candidate'
[[ "$(<"$ENV_FILE")" != "$crash_old_env" ]] ||
  fail 'post-commit crash candidate did not change .env'

# Simulate losing every in-memory snapshot after commit and before recreate.
clear_log_db_transaction_memory
crash_recreate_counter="$fixture/post-commit-crash-recreate-counter"
printf '0\n' > "$crash_recreate_counter"
docker() {
  printf '%s\n' "$(( $(<"$crash_recreate_counter") + 1 ))" > "$crash_recreate_counter"
  return 0
}
recover_incomplete_log_db_transaction ||
  fail 'startup could not recover the durable post-commit crash transaction'
unset -f docker
assert_eq "$crash_old_env" "$(<"$ENV_FILE")" \
  'post-commit crash recovery did not restore the old dotenv'
assert_eq "$crash_old_override" "$(<"$crash_override")" \
  'post-commit crash recovery did not restore the old Compose override'
assert_eq '1' "$(<"$crash_recreate_counter")" \
  'post-commit crash recovery did not recreate the old service exactly once'
for transaction_artifact in \
  "$crash_marker" "$crash_env_snapshot" "$crash_override_snapshot" \
  "$crash_env_candidate" "$crash_override_candidate"
do
  [[ ! -e "$transaction_artifact" && ! -L "$transaction_artifact" ]] ||
    fail "successful post-commit crash recovery retained $transaction_artifact"
done

# Simulate a crash after the override rename but before the .env rename. The
# recovery validator must accept this old/candidate combination and roll it back.
begin_log_db_configuration_transaction "$special_dsn" 'candidate-mid-commit-log-net' ||
  fail 'could not persist the mid-commit crash transaction snapshot'
load_mode600_setup_transaction_file "$crash_override_candidate" ||
  fail 'could not read the persisted candidate override image'
atomic_write_setup_file "$crash_override" "$SETUP_TRANSACTION_FILE_CONTENT" ||
  fail 'could not simulate the candidate override rename'
assert_eq "$crash_old_env" "$(<"$ENV_FILE")" \
  'mid-commit crash simulation changed .env before its rename'
clear_log_db_transaction_memory
printf '0\n' > "$crash_recreate_counter"
docker() {
  printf '%s\n' "$(( $(<"$crash_recreate_counter") + 1 ))" > "$crash_recreate_counter"
  return 0
}
recover_incomplete_log_db_transaction ||
  fail 'startup rejected the valid old-env/candidate-override crash window'
unset -f docker
assert_eq "$crash_old_env" "$(<"$ENV_FILE")" \
  'mid-commit crash recovery changed the old dotenv'
assert_eq "$crash_old_override" "$(<"$crash_override")" \
  'mid-commit crash recovery did not restore the old override'
assert_eq '1' "$(<"$crash_recreate_counter")" \
  'mid-commit crash recovery did not recreate the old service exactly once'

# A same-user manual edit can bypass the advisory flock after a process crash.
# Recovery must refuse to overwrite either file unless its complete image is
# exactly the persisted old or candidate image, and must retain the marker.
begin_log_db_configuration_transaction "$special_dsn" 'candidate-tamper-log-net' ||
  fail 'could not persist the tamper recovery transaction snapshot'
commit_log_db_configuration "$special_dsn" 'candidate-tamper-log-net' ||
  fail 'could not commit the tamper recovery candidate'
tamper_candidate_env="$(<"$ENV_FILE")"
tamper_candidate_override="$(<"$crash_override")"
printf "MANUAL_ENV_EDIT='preserve-me'\n" > "$ENV_FILE"
chmod 600 "$ENV_FILE"
clear_log_db_transaction_memory
printf '0\n' > "$crash_recreate_counter"
docker() {
  printf '%s\n' "$(( $(<"$crash_recreate_counter") + 1 ))" > "$crash_recreate_counter"
  return 0
}
if recover_incomplete_log_db_transaction >/dev/null 2>&1; then
  fail 'recovery overwrote a post-crash manual .env edit'
fi
unset -f docker
assert_eq "MANUAL_ENV_EDIT='preserve-me'" "$(<"$ENV_FILE")" \
  'failed recovery changed the post-crash manual .env image'
assert_eq "$tamper_candidate_override" "$(<"$crash_override")" \
  'failed .env validation changed the candidate override'
assert_eq '0' "$(<"$crash_recreate_counter")" \
  'recovery recreated services before validating the current .env image'
for transaction_artifact in \
  "$crash_marker" "$crash_env_snapshot" "$crash_override_snapshot" \
  "$crash_env_candidate" "$crash_override_candidate"
do
  [[ -f "$transaction_artifact" && ! -L "$transaction_artifact" ]] ||
    fail "tamper rejection discarded $transaction_artifact"
  assert_mode_600 "$transaction_artifact"
done
load_mode600_setup_transaction_file "$crash_env_candidate" ||
  fail 'could not reload the candidate .env image after tamper rejection'
atomic_write_setup_file "$ENV_FILE" "$SETUP_TRANSACTION_FILE_CONTENT" ||
  fail 'could not restore the candidate .env image for override tamper coverage'

printf 'services:\n  manual-post-crash-edit: {}\n' > "$crash_override"
chmod 600 "$crash_override"
clear_log_db_transaction_memory
printf '0\n' > "$crash_recreate_counter"
docker() {
  printf '%s\n' "$(( $(<"$crash_recreate_counter") + 1 ))" > "$crash_recreate_counter"
  return 0
}
if recover_incomplete_log_db_transaction >/dev/null 2>&1; then
  fail 'recovery overwrote a post-crash manual Compose override edit'
fi
unset -f docker
assert_eq "$tamper_candidate_env" "$(<"$ENV_FILE")" \
  'failed override validation changed the candidate .env'
grep -Fq 'manual-post-crash-edit: {}' "$crash_override" ||
  fail 'failed recovery changed the post-crash manual override image'
assert_eq '0' "$(<"$crash_recreate_counter")" \
  'recovery recreated services before validating the current override image'
for transaction_artifact in \
  "$crash_marker" "$crash_env_snapshot" "$crash_override_snapshot" \
  "$crash_env_candidate" "$crash_override_candidate"
do
  [[ -f "$transaction_artifact" && ! -L "$transaction_artifact" ]] ||
    fail "override tamper rejection discarded $transaction_artifact"
  assert_mode_600 "$transaction_artifact"
done

# Restore the exact candidate override only to finish and clean the test
# transaction; the preceding rejection assertions prove manual data survived.
load_mode600_setup_transaction_file "$crash_override_candidate" ||
  fail 'could not reload the candidate override image after tamper rejection'
atomic_write_setup_file "$crash_override" "$SETUP_TRANSACTION_FILE_CONTENT" ||
  fail 'could not restore the candidate override image for final recovery'
clear_log_db_transaction_memory
docker() { return 0; }
recover_incomplete_log_db_transaction ||
  fail 'valid candidate images could not recover after the tamper checks'
unset -f docker
for transaction_artifact in \
  "$crash_marker" "$crash_env_snapshot" "$crash_override_snapshot" \
  "$crash_env_candidate" "$crash_override_candidate"
do
  [[ ! -e "$transaction_artifact" && ! -L "$transaction_artifact" ]] ||
    fail "final tamper recovery retained $transaction_artifact"
done

PROJECT_DIR="$project"
ENV_FILE="$project/.env"
override="$project/docker-compose.override.yml"
COMPOSE_FILE="$project/docker-compose.yml"
printf 'services:\n  newapi-tools:\n    image: example.invalid/test\n' > "$COMPOSE_FILE"
printf 'services: {}\n' > "$project/docker-compose.logdb.yml"
restart_old_env="$(<"$ENV_FILE")"
restart_old_override="$(<"$override")"
commit_log_db_configuration "$special_dsn" 'logs-net-restart-candidate' ||
  fail 'could not prepare the restart rollback fixture'
restart_counter="$fixture/restart-counter"
printf '0\n' > "$restart_counter"
docker() {
  local count
  count="$(<"$restart_counter")"
  count=$((count + 1))
  printf '%s\n' "$count" > "$restart_counter"
  (( count == 1 )) && return 1
  return 0
}
if restart_log_db_services_transactionally; then
  fail 'candidate restart failure was reported as success'
else
  restart_status=$?
fi
unset -f docker
[[ "$restart_status" == "10" ]] ||
  fail "candidate restart rollback returned status $restart_status instead of 10"
assert_eq '2' "$(<"$restart_counter")" \
  'restart rollback did not attempt candidate then old service exactly once each'
assert_eq "$restart_old_env" "$(<"$ENV_FILE")" \
  'restart failure did not restore the old .env snapshot'
assert_eq "$restart_old_override" "$(<"$override")" \
  'restart failure did not restore the old override snapshot'
assert_no_temp_files "$ENV_FILE"
assert_no_temp_files "$override"

saved_project_dir="$PROJECT_DIR"
saved_env_file="$ENV_FILE"
saved_compose_file="$COMPOSE_FILE"
host_project="$fixture/host-project"
mkdir -p "$host_project"
PROJECT_DIR="$host_project"
ENV_FILE="$host_project/.env"
COMPOSE_FILE="$host_project/docker-compose.yml"
printf "NEWAPI_NETWORK=''\nLOG_NETWORK='host-log-net'\n" > "$ENV_FILE"
chmod 600 "$ENV_FILE"
printf 'services: {}\n' > "$COMPOSE_FILE"
printf 'services: {}\n' > "$host_project/docker-compose.host.yml"
printf 'services: {}\n' > "$host_project/docker-compose.logdb.yml"
printf 'services: {}\n' > "$host_project/docker-compose.override.yml"
chmod 600 "$host_project/docker-compose.override.yml"
compose_args_capture="$fixture/compose-args"
docker() {
  printf '%s\n' "$*" > "$compose_args_capture"
  return 0
}
run_setup_compose_recreate || fail 'host-mode Compose recreation fixture was rejected'
unset -f docker
expected_compose_args="compose --env-file $ENV_FILE -f $COMPOSE_FILE -f $host_project/docker-compose.host.yml -f $host_project/docker-compose.logdb.yml -f $host_project/docker-compose.override.yml up -d --force-recreate --wait --wait-timeout 180 newapi-tools"
assert_eq "$expected_compose_args" "$(<"$compose_args_capture")" \
  'host-mode Compose overlays were not ordered base,host,logdb,override'
rm -f "$host_project/docker-compose.host.yml"
if run_setup_compose_recreate >/dev/null 2>&1; then
  fail 'host mode was accepted without a safe docker-compose.host.yml overlay'
fi
printf 'services: {}\n' > "$host_project/docker-compose.host.yml"
rm -f "$host_project/docker-compose.logdb.yml"
if run_setup_compose_recreate >/dev/null 2>&1; then
  fail 'separate log network was accepted without a safe docker-compose.logdb.yml overlay'
fi
PROJECT_DIR="$saved_project_dir"
ENV_FILE="$saved_env_file"
COMPOSE_FILE="$saved_compose_file"

concurrent_project="$fixture/concurrent-project"
mkdir -p "$concurrent_project"
PROJECT_DIR="$concurrent_project"
ENV_FILE="$concurrent_project/.env"
COMPOSE_FILE="$concurrent_project/docker-compose.yml"
printf "NEWAPI_NETWORK='new-api_default'\nLOG_SQL_DSN='old'\nLOG_NETWORK='old-log-net'\n" > "$ENV_FILE"
chmod 600 "$ENV_FILE"
printf 'services: {}\n' > "$COMPOSE_FILE"
printf 'services: {}\n' > "$concurrent_project/docker-compose.logdb.yml"
commit_log_db_configuration "$special_dsn" 'old-log-net' ||
  fail 'could not create concurrent override rollback baseline'
concurrent_old_env="$(<"$ENV_FILE")"
commit_log_db_configuration "$special_dsn" 'candidate-log-net' ||
  fail 'could not create concurrent override candidate'
concurrent_override="$concurrent_project/docker-compose.override.yml"
printf 'services:\n  user-concurrent-change: {}\n' > "$concurrent_override"
chmod 600 "$concurrent_override"
concurrent_counter="$fixture/concurrent-restart-counter"
printf '0\n' > "$concurrent_counter"
docker() {
  local count
  count="$(<"$concurrent_counter")"
  printf '%s\n' "$((count + 1))" > "$concurrent_counter"
  return 1
}
if restart_log_db_services_transactionally; then
  fail 'concurrent override modification was silently treated as a successful rollback'
else
  concurrent_status=$?
fi
unset -f docker
[[ "$concurrent_status" == "20" ]] ||
  fail "concurrent override rollback returned $concurrent_status instead of high-risk status 20"
assert_eq '1' "$(<"$concurrent_counter")" \
  'old service restart was attempted despite an incomplete configuration rollback'
assert_eq "$concurrent_old_env" "$(<"$ENV_FILE")" \
  'concurrent override rollback did not restore the old .env'
grep -Fq 'user-concurrent-change: {}' "$concurrent_override" ||
  fail 'rollback overwrote a concurrent user modification to the override'

PROJECT_DIR="$saved_project_dir"
ENV_FILE="$saved_env_file"
COMPOSE_FILE="$saved_compose_file"

commit_log_db_configuration "$special_dsn" 'new-api_default' ||
  fail 'switching the log database onto the main network failed'
[[ ! -e "$override" && ! -L "$override" ]] ||
  fail 'old generated override survived the switch to the main network'
assert_eq '' "$(read_env_value LOG_NETWORK)" \
  'main-network transition persisted a duplicate LOG_NETWORK instead of using the base attachment'

printf 'services:\n  custom: {}\n' > "$override"
chmod 600 "$override"
remove_generated_log_network_override
[[ -f "$override" ]] || fail 'non-generated override was deleted'
grep -Fq 'custom: {}' "$override" || fail 'non-generated override content changed'
custom_env_content="$(<"$ENV_FILE")"
custom_override_content="$(<"$override")"
if commit_log_db_configuration "$special_dsn" 'logs-net-custom-conflict'; then
  fail 'transaction overwrote a user-managed Compose override'
fi
assert_eq "$custom_env_content" "$(<"$ENV_FILE")" \
  'user override conflict changed .env'
assert_eq "$custom_override_content" "$(<"$override")" \
  'user override conflict changed custom YAML'

rm -f "$override"
write_log_network_override 'logs-net_1'

override_victim="$fixture/override-victim"
printf 'override-victim\n' > "$override_victim"
rm -f "$override"
ln -s "$override_victim" "$override"
if (write_log_network_override 'logs-net_2' >/dev/null 2>&1); then
  fail 'symlink Compose override target was accepted'
fi
assert_eq 'override-victim' "$(<"$override_victim")" \
  'Compose override symlink victim was modified'

rm -f "$override"
if (write_log_network_override $'bad\nnetwork' >/dev/null 2>&1); then
  fail 'newline-containing Docker network name was accepted'
fi

workflow="$repo_root/.github/workflows/build.yml"
grep -Fq "if: github.ref_type == 'tag'" "$workflow" ||
  fail 'release tag identity gate is not tag-only'
grep -Fq '[[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]' "$workflow" ||
  fail 'release tag gate does not require strict semantic syntax'
grep -Fq 'tag_commit="$(git rev-parse --verify "refs/tags/${tag}^{commit}" 2>/dev/null)"' "$workflow" ||
  fail 'release tag gate does not peel the annotated tag to its commit'
grep -Fq '[[ "$tag_commit" == "$GITHUB_SHA" ]]' "$workflow" ||
  fail 'release tag gate does not bind the tag target to the workflow commit'
grep -Fq 'git merge-base --is-ancestor "$tag_commit" refs/remotes/origin/main' "$workflow" ||
  fail 'release tag gate does not require origin/main ancestry'
grep -Fq 'git cat-file -t "refs/tags/${tag}"' "$workflow" ||
  fail 'release tag gate does not require an annotated tag object'
grep -Fq 'backend/internal/buildinfo/buildinfo.go' "$workflow" ||
  fail 'release tag gate does not validate the source version'
grep -Fq 'release_doc="RELEASE_${version}.md"' "$workflow" ||
  fail 'release tag gate does not require a matching release document'
grep -Fq 'grep -Fxq "# NewAPI Tools v${version} 发行说明"' "$workflow" ||
  fail 'release tag gate does not validate the release document title'
grep -Fq 'grep -Fq "NEWAPI_TOOLS_REF=${tag} \\"' "$workflow" ||
  fail 'release tag gate does not validate the documented install ref'
grep -Fq 'grep -Fxq "# NewAPI Tools v${version}" README.md' "$workflow" ||
  fail 'release tag gate does not validate the README version'
grep -Fq 'bash tests/setup_log_db_safety_test.sh' "$workflow" ||
  fail 'setup-log-db safety test is not wired into the build workflow'

for good_tag in v0.5.1 v10.20.30; do
  [[ "$good_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
    fail "strict release regex rejected $good_tag"
done
for bad_tag in v0.5 v0.5.1-rc1 0.5.1 vx.y.z 'v1.2.3/extra' v01.2.3 v1.02.3 v1.2.03; do
  if [[ "$bad_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    fail "strict release regex accepted $bad_tag"
  fi
done

release_tag_identity_matches() {
  local repository="$1" tag="$2" github_sha="$3" tag_commit
  [[ "$(git -C "$repository" cat-file -t "refs/tags/${tag}" 2>/dev/null)" == 'tag' ]] ||
    return 1
  tag_commit="$(git -C "$repository" rev-parse --verify "refs/tags/${tag}^{commit}" 2>/dev/null)" ||
    return 1
  [[ "$tag_commit" == "$github_sha" ]]
}

tag_gate_repo="$fixture/tag-gate-repo"
git init -q -b main "$tag_gate_repo"
git -C "$tag_gate_repo" config user.name 'tag gate test'
git -C "$tag_gate_repo" config user.email 'tag-gate@example.invalid'
printf 'first\n' > "$tag_gate_repo/state"
git -C "$tag_gate_repo" add state
git -C "$tag_gate_repo" commit -qm 'first'
tag_target="$(git -C "$tag_gate_repo" rev-parse HEAD)"
git -C "$tag_gate_repo" tag -a v1.2.3 -m 'release' "$tag_target"
printf 'second\n' >> "$tag_gate_repo/state"
git -C "$tag_gate_repo" commit -qam 'second'
different_main_commit="$(git -C "$tag_gate_repo" rev-parse HEAD)"

release_tag_identity_matches "$tag_gate_repo" v1.2.3 "$tag_target" ||
  fail 'annotated tag target did not match its workflow commit'
if release_tag_identity_matches "$tag_gate_repo" v1.2.3 "$different_main_commit"; then
  fail 'release tag gate accepted a different origin/main commit as GITHUB_SHA'
fi

printf 'setup-log-db safety tests passed\n'
