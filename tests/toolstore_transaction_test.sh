#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/toolstore_transaction.sh
source "${REPO_ROOT}/scripts/toolstore_transaction.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_file() { [[ -f "$1" && ! -L "$1" ]] || fail "expected regular file: $1"; }
assert_absent() { [[ ! -e "$1" && ! -L "$1" ]] || fail "expected absent path: $1"; }
assert_contains() { grep -Fq -- "$2" "$1" || fail "$1 missing: $2"; }

TEST_TMP="$(mktemp -d)"
case "$TEST_TMP" in
  /tmp/*) ;;
  *) fail "unsafe test temporary directory: $TEST_TMP" ;;
esac
trap 'case "$TEST_TMP" in /tmp/*) rm -rf -- "$TEST_TMP" ;; esac' EXIT

OLD_IMAGE="example.invalid/newapi-tools@sha256:$(printf 'a%.0s' {1..64})"
CANDIDATE_IMAGE="example.invalid/newapi-tools@sha256:$(printf 'b%.0s' {1..64})"
TOOLSTORE_TXN_LIBRARY_SOURCE="${REPO_ROOT}/scripts/toolstore_transaction.sh"
TOOLSTORE_TXN_RUNNER=stub_toolstorectl

db_value() {
  local path="$1" key="$2"
  awk -F= -v k="$key" '$1 == k { value = $2 } END { print value }' "$path"
}

emit_metadata() {
  local path="$1" schema digest size ledger_digest object_digest table count first=true counts=""
  [[ -f "$path" && ! -L "$path" ]] || return 1
  grep -Fqx 'CORRUPT' "$path" && return 1
  schema="$(db_value "$path" schema)"
  [[ "$schema" =~ ^[1-9][0-9]*$ ]] || return 1
  digest="$(sha256sum -- "$path" | awk '{print $1}')"
  size="$(stat -c '%s' -- "$path")"
  ledger_digest="$(printf 'ledger-v%s' "$schema" | sha256sum | awk '{print $1}')"
  object_digest="$(printf 'objects-v%s' "$schema" | sha256sum | awk '{print $1}')"
  for table in "${TOOLSTORE_TXN_CORE_TABLES[@]}"; do
    count="$(db_value "$path" "$table")"
    [[ "$count" =~ ^[0-9]+$ ]] || continue
    if [[ "$first" == "true" ]]; then
      first=false
    else
      counts+=","
    fi
    counts+="\"${table}\":${count}"
  done
  [[ "$first" == "false" ]] || return 1
  printf '{"path":"%s","schema_version":%s,"size_bytes":%s,"sha256":"%s","integrity":"ok","migration_ledger_sha256":"%s","schema_objects_sha256":"%s","core_table_counts":{%s}}\n' \
    "$path" "$schema" "$size" "$digest" "$ledger_digest" "$object_digest" "$counts"
}

stub_toolstorectl() {
  local _image="$1" operation="$2" source="$3" destination="${4-}"
  case "$operation" in
    backup)
      [[ "${STUB_BACKUP_FAIL:-false}" != "true" ]] || return 44
      [[ ! -e "$destination" && ! -L "$destination" ]] || return 1
      cp -- "$source" "$destination"
      chmod 600 "$destination"
      emit_metadata "$destination"
      ;;
    verify)
      emit_metadata "$source"
      ;;
    restore)
      [[ "${STUB_RESTORE_FAIL:-false}" != "true" ]] || return 45
      [[ ! -e "$destination" && ! -L "$destination" ]] || return 1
      cp -- "$source" "$destination"
      chmod 600 "$destination"
      emit_metadata "$destination"
      ;;
    *) return 1 ;;
  esac
}

write_db() {
  local path="$1" schema="$2" operation_count="${3:-3}"
  mkdir -p "$(dirname -- "$path")"
  {
    printf 'schema=%s\n' "$schema"
    printf 'operation_audit=%s\n' "$operation_count"
    printf 'risk_cases=2\n'
    printf 'risk_case_events=4\n'
    printf 'support_notes=1\n'
    printf 'price_snapshots=5\n'
    printf 'reconciliation_runs=2\n'
    (( schema < 7 )) || printf 'risk_case_transition_replays=1\n'
    if (( schema >= 8 )); then
      printf 'invoice_documents=3\n'
      printf 'invoice_events=3\n'
    fi
    if (( schema >= 9 )); then
      printf 'model_probe_runs=2\n'
      printf 'model_probe_attempts=2\n'
      printf 'model_probe_rollups=2\n'
    fi
    (( schema < 10 )) || printf 'model_probe_attempt_lifecycles=2\n'
    (( schema < 11 )) || printf 'invoice_document_relations=1\n'
    (( schema < 12 )) || printf 'model_status_config_versions=1\n'
    printf 'payload=schema-%s\n' "$schema"
  } >"$path"
  chmod 600 "$path"
}

new_project() {
  local name="$1" project
  project="${TEST_TMP}/${name}"
  mkdir -p "${project}/data"
  printf 'TOOL_STORE_PATH=./data/control-plane.db\nNEWAPI_TOOLS_IMAGE=%s\nCOMPOSE_PROJECT_NAME=test-project\n' \
    "$OLD_IMAGE" >"${project}/.env"
  chmod 600 "${project}/.env"
  printf 'services:\n  newapi-tools: {}\n' >"${project}/docker-compose.yml"
  printf 'services: {}\n' >"${project}/docker-compose.host.yml"
  printf 'services: {}\n' >"${project}/docker-compose.logdb.yml"
  write_db "${project}/data/control-plane.db" 8
  printf '%s\n' "$project"
}

record_value() {
  local project="$1" key="$2" content
  content="$(toolstore_txn_load_record "$project")" || return 1
  toolstore_txn_env_value "$content" "$key"
}

# OPS01/OPS02/OPS08/OPS13: backup metadata, permissions, durable identity and
# idempotent prepare all come from a real record rather than process memory.
project="$(new_project prepare)"
TOOLSTORE_TXN_REQUEST_ID=ops-prepare-0001
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
record="$(toolstore_txn_active_record_path "$project")"
assert_file "$record"
[[ "$(record_value "$project" STATE)" == preflight ]] || fail 'OPS08 preflight state not durable'
[[ "$(record_value "$project" PREFLIGHT_SCHEMA)" == 8 ]] || fail 'OPS01 preflight schema not captured'
[[ "$(record_value "$project" PREFLIGHT_HASH)" =~ ^[0-9a-f]{64}$ ]] || fail 'OPS13 preflight hash missing'
[[ "$(record_value "$project" REQUEST_ID)" == ops-prepare-0001 ]] || fail 'OPS13 request id missing'
[[ "$(record_value "$project" COMPOSE_PROJECT_NAME)" == test-project ]] || fail 'OPS13 Compose project missing'
[[ "$(record_value "$project" OLD_IMAGE)" == "$OLD_IMAGE" ]] || fail 'OPS13 old image missing'
[[ "$(record_value "$project" CONFIG_SHA256)" =~ ^[0-9a-f]{64}$ ]] || fail 'OPS13 config hash missing'
backup="$(record_value "$project" PREFLIGHT_BACKUP_PATH)"
assert_file "$backup"
toolstore_txn_mode_is_private "$backup" || fail 'OPS02 backup is not mode 0600/equivalent'
before_hash="$(sha256sum -- "$backup")"
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
[[ "$(sha256sum -- "$backup")" == "$before_hash" ]] || fail 'OPS08 repeat prepare replaced backup'
toolstore_txn_authoritative_backup "$project"
[[ "$(record_value "$project" STATE)" == authoritative ]] || fail 'OPS01 authoritative state not durable'
[[ "$(record_value "$project" OLD_SCHEMA)" == 8 ]] || fail 'OPS01 authoritative schema not captured'
[[ "$(record_value "$project" OLD_HASH)" =~ ^[0-9a-f]{64}$ ]] || fail 'OPS13 authoritative hash missing'
backup="$(record_value "$project" BACKUP_PATH)"

# OPS09/OPS14: a v8 backup restores after a v9 candidate failure without ever
# editing schema_migrations. The migrated database is retained as evidence.
toolstore_txn_mark_candidate "$project"
write_db "${project}/data/control-plane.db" 9
toolstore_txn_verify_candidate "$project"
toolstore_txn_restore_database "$project"
archive="$(record_value "$project" ARCHIVE_PATH)"
assert_file "$archive"
[[ "$(db_value "$archive" schema)" == 9 ]] || fail 'OPS09 migrated database was not archived'
[[ "$(db_value "${project}/data/control-plane.db" schema)" == 8 ]] || fail 'OPS09 v8 database was not restored'
[[ "$(record_value "$project" STATE)" == database_restored ]] || fail 'restore state not durable'
toolstore_txn_mark_rolled_back "$project"
toolstore_txn_retire_rolled_back "$project"
assert_absent "$record"
assert_file "$backup"
[[ -n "$(find "$(toolstore_txn_root "$project")/history" -maxdepth 1 -name '*.rolled-back.env' -print -quit)" ]] ||
  fail 'rolled-back evidence record missing'

# OPS10: power loss after producing a verified restore temp is safely re-entrant.
project="$(new_project power-temp)"
TOOLSTORE_TXN_REQUEST_ID=ops-power-temp
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_mark_candidate "$project"
write_db "${project}/data/control-plane.db" 10
TOOLSTORE_TXN_FAULT_POINT=after_restore_temp
if toolstore_txn_restore_database "$project"; then fail 'OPS10 restore-temp fault unexpectedly succeeded'; fi
unset TOOLSTORE_TXN_FAULT_POINT
[[ "$(db_value "${project}/data/control-plane.db" schema)" == 10 ]] || fail 'OPS10 live DB changed before atomic activation'
toolstore_txn_restore_database "$project"
[[ "$(db_value "${project}/data/control-plane.db" schema)" == 8 ]] || fail 'OPS10 retry did not restore v8'

# OPS10: power loss after archiving the migrated DB resumes from the same temp.
project="$(new_project power-archive)"
TOOLSTORE_TXN_REQUEST_ID=ops-power-archive
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_mark_candidate "$project"
write_db "${project}/data/control-plane.db" 9
TOOLSTORE_TXN_FAULT_POINT=after_live_archive
if toolstore_txn_restore_database "$project"; then fail 'OPS10 archive fault unexpectedly succeeded'; fi
unset TOOLSTORE_TXN_FAULT_POINT
assert_absent "${project}/data/control-plane.db"
assert_file "$(record_value "$project" ARCHIVE_PATH)"
toolstore_txn_restore_database "$project"
[[ "$(db_value "${project}/data/control-plane.db" schema)" == 8 ]] || fail 'OPS10 archive retry did not activate restored DB'

# OPS11: a corrupt backup is rejected before touching the only live database.
project="$(new_project corrupt)"
TOOLSTORE_TXN_REQUEST_ID=ops-corrupt-backup
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_mark_candidate "$project"
write_db "${project}/data/control-plane.db" 9
backup="$(record_value "$project" BACKUP_PATH)"
printf 'CORRUPT\n' >"$backup"
chmod 600 "$backup"
if toolstore_txn_restore_database "$project"; then fail 'OPS11 corrupt backup was accepted'; fi
[[ "$(db_value "${project}/data/control-plane.db" schema)" == 9 ]] || fail 'OPS11 corrupt backup overwrote live DB'
assert_absent "$(record_value "$project" ARCHIVE_PATH)"

# OPS06: restore failure occurs before the migrated live DB is archived.
project="$(new_project restore-fail)"
TOOLSTORE_TXN_REQUEST_ID=ops-restore-fail
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_mark_candidate "$project"
write_db "${project}/data/control-plane.db" 9
STUB_RESTORE_FAIL=true
if toolstore_txn_restore_database "$project"; then fail 'OPS06 injected restore failure succeeded'; fi
unset STUB_RESTORE_FAIL
[[ "$(db_value "${project}/data/control-plane.db" schema)" == 9 ]] || fail 'OPS06 restore failure changed live DB'
assert_file "$(record_value "$project" BACKUP_PATH)"
assert_absent "$(record_value "$project" ARCHIVE_PATH)"

# OPS04/OPS08: backup failure has no destructive side effects and can resume.
project="$(new_project backup-fail)"
TOOLSTORE_TXN_REQUEST_ID=ops-backup-fail
sentinel="${project}/do-not-delete"
printf 'sentinel\n' >"$sentinel"
STUB_BACKUP_FAIL=true
if toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"; then
  fail 'OPS04 injected backup failure succeeded'
fi
unset STUB_BACKUP_FAIL
assert_file "$sentinel"
[[ "$(db_value "${project}/data/control-plane.db" schema)" == 8 ]] || fail 'OPS04 source DB changed'
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
[[ "$(record_value "$project" STATE)" == preflight ]] || fail 'OPS08 preparation did not resume'

# OPS03/OPS07: data is moved outside the deletion tree only after backup, then
# restored idempotently. Commit removes only this transaction's anchors.
project="$(new_project reinstall)"
TOOLSTORE_TXN_REQUEST_ID=ops-reinstall
outside_sentinel="${TEST_TMP}/outside-sentinel"
project_sentinel="${project}/keep-me"
printf 'outside\n' >"$outside_sentinel"
printf 'project\n' >"$project_sentinel"
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_stage_data "$project"
assert_absent "${project}/data"
assert_file "$(record_value "$project" STAGED_DATA_PATH)/control-plane.db"
toolstore_txn_unstage_data "$project"
assert_file "${project}/data/control-plane.db"
toolstore_txn_mark_candidate "$project"
toolstore_txn_verify_candidate "$project"
toolstore_txn_commit "$project"
assert_file "$outside_sentinel"
assert_file "$project_sentinel"
[[ "$(record_value "$project" STATE)" == committed ]] || fail 'OPS15 commit point was not retained through promotion'
toolstore_txn_finish_committed "$project"
assert_absent "$(toolstore_txn_active_record_path "$project")"

# OPS08/OPS15: a crash after the durable commit marker resumes cleanup and
# never rolls a healthy candidate back to the old schema.
project="$(new_project commit-crash)"
TOOLSTORE_TXN_REQUEST_ID=ops-commit-crash
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_mark_candidate "$project"
TOOLSTORE_TXN_FAULT_POINT=after_commit_marker
if toolstore_txn_commit "$project"; then fail 'OPS15 commit-marker fault unexpectedly completed'; fi
unset TOOLSTORE_TXN_FAULT_POINT
[[ "$(record_value "$project" STATE)" == committed ]] || fail 'OPS15 durable commit marker missing'
backup="$(record_value "$project" BACKUP_PATH)"
assert_file "$backup"
toolstore_txn_finish_committed "$project"
assert_absent "$(toolstore_txn_active_record_path "$project")"
assert_absent "$backup"

# OPS13: the database transaction carries the authoritative pre-checkout
# Compose bundle, including optional-file absence, rather than snapshotting a
# candidate checkout and calling it the rollback configuration.
project="$(new_project compose-snapshot)"
TOOLSTORE_TXN_REQUEST_ID=ops-compose-snapshot
old_compose="${TEST_TMP}/old-compose.yml"
printf 'services:\n  old-release: {}\n' >"$old_compose"
TOOLSTORE_TXN_COMPOSE_BASE_SOURCE="$old_compose"
TOOLSTORE_TXN_COMPOSE_HOST_SOURCE=__ABSENT__
TOOLSTORE_TXN_COMPOSE_LOGDB_SOURCE=__ABSENT__
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
unset TOOLSTORE_TXN_COMPOSE_BASE_SOURCE TOOLSTORE_TXN_COMPOSE_HOST_SOURCE TOOLSTORE_TXN_COMPOSE_LOGDB_SOURCE
printf 'services:\n  candidate-release: {}\n' >"${project}/docker-compose.yml"
toolstore_txn_restore_files "$project"
assert_contains "${project}/docker-compose.yml" 'old-release'
assert_absent "${project}/docker-compose.host.yml"
assert_absent "${project}/docker-compose.logdb.yml"

# The running-service preflight is never promoted. If power is lost after an
# unmarked authoritative copy, a retry discards it and captures writes that
# happened before the old service was stopped again.
project="$(new_project authoritative-recapture)"
TOOLSTORE_TXN_REQUEST_ID=ops-authoritative-recapture
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
write_db "${project}/data/control-plane.db" 8 4
TOOLSTORE_TXN_FAULT_POINT=after_authoritative_backup
if toolstore_txn_authoritative_backup "$project"; then fail 'unmarked authoritative fault unexpectedly succeeded'; fi
unset TOOLSTORE_TXN_FAULT_POINT
[[ "$(record_value "$project" STATE)" == preflight ]] || fail 'unmarked authoritative copy changed durable state'
write_db "${project}/data/control-plane.db" 8 5
toolstore_txn_authoritative_backup "$project"
actual_authoritative="$(toolstore_txn_verify_backup "$project")"
[[ "$(toolstore_txn_metadata_count "$actual_authoritative" operation_audit)" == 5 ]] ||
  fail 'retry reused stale unmarked authoritative backup'

# The adjacent transaction root and private recovery library remain usable
# when an interrupted safe reinstall has already removed the final project.
project="$(new_project external-recovery)"
TOOLSTORE_TXN_REQUEST_ID=ops-external-recovery
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_stage_data "$project"
external_root="$(toolstore_txn_root "$project")"
assert_file "${external_root}/active.env"
assert_file "${external_root}/recovery-library.sh"
toolstore_txn_mode_is_private "${external_root}/recovery-library.sh" || fail 'external recovery library is not private'
case "$project" in "${TEST_TMP}"/*) rm -rf -- "$project" ;; *) fail 'unsafe external-recovery project path' ;; esac
source "${external_root}/recovery-library.sh"
toolstore_txn_restore_files "$project"
toolstore_txn_unstage_data "$project"
assert_file "${project}/.env"
assert_file "${project}/data/control-plane.db"
[[ "$(record_value "$project" STATE)" == authoritative ]] || fail 'external recovery did not restore authoritative state'
toolstore_txn_restore_database "$project"
toolstore_txn_mark_rolled_back "$project"

# Candidate cleanup is evidence-bound. An unrelated container that happens to
# use the deterministic transaction name must never be deleted.
project="$(new_project candidate-container-identity)"
TOOLSTORE_TXN_REQUEST_ID=ops-candidate-identity
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
candidate_name=""
candidate_request=""
candidate_compose=""
candidate_image=""
candidate_project=""
toolstore_txn_candidate_identity "$project" candidate_name candidate_request candidate_compose candidate_image candidate_project
[[ "$candidate_name" == "test-project-newapi-tools-candidate-ops-candidate-identity" ]] ||
  fail 'candidate name is not bound to Compose project and request id'
candidate_present=true
candidate_removed=false
replacement_present=false
replacement_removed=false
candidate_id="$(printf 'c%.0s' {1..64})"
replacement_id="$(printf 'd%.0s' {1..64})"
label_request=unrelated-request
docker() {
  local operation="${1:-}" format="${3:-}" container="${4:-}"
  case "$operation" in
    inspect)
      if [[ "${2:-}" != "--format" ]]; then
        if [[ "${2:-}" == "$candidate_id" ]]; then
          [[ "$candidate_present" == true ]]
        elif [[ "${2:-}" == "$replacement_id" ]]; then
          [[ "$replacement_present" == true ]]
        elif [[ "${2:-}" == "$candidate_name" ]]; then
          [[ "$candidate_present" == true || "$replacement_present" == true ]]
        else
          return 1
        fi
        return
      fi
      if [[ "$container" == "$candidate_id" || "$container" == "$candidate_name" ]] &&
        [[ "$candidate_present" == true ]]; then
        :
      elif [[ "$container" == "$replacement_id" || "$container" == "$candidate_name" ]] &&
        [[ "$replacement_present" == true ]]; then
        :
      else
        return 1
      fi
      case "$format" in
        *'.Id'*) [[ "$container" == "$replacement_id" ]] && printf '%s\n' "$replacement_id" || printf '%s\n' "$candidate_id" ;;
        *'.Name'*) printf '/%s\n' "$candidate_name" ;;
        *candidate.request-id*) printf '%s\n' "$label_request" ;;
        *candidate.compose-project*) printf '%s\n' "$candidate_compose" ;;
        *candidate.project-dir*) printf '%s\n' "$candidate_project" ;;
        *candidate.purpose*) printf 'toolstore-migration-candidate\n' ;;
        *candidate.image*) printf '%s\n' "$candidate_image" ;;
        *com.docker.compose.project*) printf '%s\n' "$candidate_compose" ;;
        *'.Config.Image'*) printf '%s\n' "$candidate_image" ;;
        *) return 1 ;;
      esac
      ;;
    rm)
      [[ "${2:-}" == -f ]] || return 1
      if [[ "${3:-}" == "$candidate_id" ]]; then
        candidate_removed=true
        candidate_present=false
      elif [[ "${3:-}" == "$replacement_id" ]]; then
        replacement_removed=true
        replacement_present=false
      else
        return 1
      fi
      ;;
    *) return 1 ;;
  esac
}
if toolstore_txn_remove_candidate_container "$project" >/dev/null 2>&1; then
  fail 'unrelated same-name candidate container was accepted for deletion'
fi
[[ "$candidate_present" == true && "$candidate_removed" == false ]] ||
  fail 'unrelated same-name candidate container was deleted'
label_request="$candidate_request"
toolstore_txn_remove_candidate_container "$project"
[[ "$candidate_present" == false && "$candidate_removed" == true ]] ||
  fail 'matching transaction candidate container was not deleted'
toolstore_txn_authoritative_backup "$project"
toolstore_txn_mark_candidate "$project"
toolstore_txn_record_candidate_container_id "$project" "$candidate_id"
candidate_present=true
candidate_removed=false
label_request=post-create-label-mismatch
if toolstore_txn_candidate_container_matches "$project" "$candidate_id" >/dev/null 2>&1; then
  fail 'post-create request label mismatch was accepted'
fi
toolstore_txn_remove_created_candidate_by_id "$project" "$candidate_id"
[[ "$candidate_present" == false && "$candidate_removed" == true ]] ||
  fail 'post-create mismatch was not safely stopped by the captured container id'
toolstore_txn_record_candidate_container_id "$project" "$candidate_id"
replacement_present=true
if toolstore_txn_remove_candidate_container "$project" >/dev/null 2>&1; then
  fail 'same-name replacement was accepted after the recorded candidate id disappeared'
fi
[[ "$replacement_present" == true && "$replacement_removed" == false ]] ||
  fail 'same-name replacement container was deleted'
unset -f docker

# OPS05: configured empty/root/escape paths, file symlinks, directory symlinks,
# and a symlink project root are all rejected.
project="$(new_project unsafe-empty)"
printf 'TOOL_STORE_PATH=\n' >"${project}/.env"
if toolstore_txn_resolve_db_path "${project}/.env" "$project" >/dev/null; then fail 'OPS05 empty path accepted'; fi
printf 'TOOL_STORE_PATH=/\n' >"${project}/.env"
if toolstore_txn_resolve_db_path "${project}/.env" "$project" >/dev/null; then fail 'OPS05 root path accepted'; fi
printf 'TOOL_STORE_PATH=./data/../../outside.db\n' >"${project}/.env"
if toolstore_txn_resolve_db_path "${project}/.env" "$project" >/dev/null; then fail 'OPS05 traversal accepted'; fi
project="$(new_project unsafe-file-link)"
mv "${project}/data/control-plane.db" "${project}/data/real.db"
ln -s real.db "${project}/data/control-plane.db"
if toolstore_txn_resolve_db_path "${project}/.env" "$project" >/dev/null; then fail 'OPS05 DB symlink accepted'; fi
real_project="$(new_project unsafe-dir-link-target)"
linked_project="${TEST_TMP}/unsafe-project-link"
ln -s "$real_project" "$linked_project"
if toolstore_txn_project_path "$linked_project" >/dev/null; then fail 'OPS05 project symlink accepted'; fi

# Core row drift must block commit even when readiness happened to return green.
project="$(new_project count-drift)"
TOOLSTORE_TXN_REQUEST_ID=ops-count-drift
toolstore_txn_prepare "${project}/.env" "$project" "$OLD_IMAGE" "$CANDIDATE_IMAGE" "$CANDIDATE_IMAGE"
toolstore_txn_authoritative_backup "$project"
toolstore_txn_mark_candidate "$project"
write_db "${project}/data/control-plane.db" 9 99
if toolstore_txn_verify_candidate "$project"; then fail 'OPS01 core row-count drift accepted'; fi
assert_file "$(record_value "$project" BACKUP_PATH)"

# OPS12/OPS14/OPS15: integration ordering is checked statically in addition to
# the executable state-machine tests above.
if grep -Fq 'schema_migrations' "${REPO_ROOT}/scripts/toolstore_transaction.sh"; then
  fail 'OPS14 rollback library manually edits schema_migrations'
fi
assert_contains "${REPO_ROOT}/deploy.sh" 'toolstore_txn_restore_database "$SCRIPT_DIR"'
assert_contains "${REPO_ROOT}/deploy.sh" 'start_deploy_services_and_wait rollback "$rollback_image"'
assert_contains "${REPO_ROOT}/install.sh" 'toolstore_txn_restore_database "$project_dir"'
assert_contains "${REPO_ROOT}/install.sh" 'start_install_services_and_wait "$env_file" "$project_dir" rollback'
assert_contains "${REPO_ROOT}/scripts/toolstore_transaction.sh" 'toolstore_txn_fault_point before_commit'
assert_contains "${REPO_ROOT}/scripts/toolstore_transaction.sh" 'toolstore_txn_record_set "$record" STATE committed'
assert_contains "${REPO_ROOT}/install.sh" '--label "io.newapi-tools.candidate.request-id=${request_id}"'
assert_contains "${REPO_ROOT}/deploy.sh" '--label "io.newapi-tools.candidate.request-id=${request_id}"'
if grep -Fq 'stop_install_isolated_candidate "$project_dir" || true' "${REPO_ROOT}/install.sh"; then
  fail 'installer still ignores a candidate stop failure before database restore'
fi
assert_contains "${REPO_ROOT}/install.sh" 'toolstore_txn_remove_created_candidate_by_id "$project_dir" "$container_id"'

printf 'PASS: Tool Store OPS01-OPS16 transaction and fault-injection checks\n'
