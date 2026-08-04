#!/usr/bin/env bash

# Crash-safe Tool Store backup/rollback primitives shared by install.sh and
# deploy.sh. The caller must hold the project state lock for every mutating
# operation. No function sources dotenv or transaction files.

TOOLSTORE_TXN_FORMAT_VERSION=1
TOOLSTORE_TXN_CORE_TABLES=(
  operation_audit
  risk_cases
  risk_case_events
  support_notes
  price_snapshots
  reconciliation_runs
  risk_case_transition_replays
  invoice_documents
  invoice_events
  model_probe_runs
  model_probe_attempts
  model_probe_rollups
  model_probe_attempt_lifecycles
  invoice_document_relations
  model_status_config_versions
)

toolstore_txn_table_min_version() {
  case "$1" in
    operation_audit) printf '1\n' ;;
    risk_cases|risk_case_events) printf '2\n' ;;
    support_notes) printf '3\n' ;;
    price_snapshots) printf '4\n' ;;
    reconciliation_runs) printf '5\n' ;;
    risk_case_transition_replays) printf '7\n' ;;
    invoice_documents|invoice_events) printf '8\n' ;;
    model_probe_runs|model_probe_attempts|model_probe_rollups) printf '9\n' ;;
    model_probe_attempt_lifecycles) printf '10\n' ;;
    invoice_document_relations) printf '11\n' ;;
    model_status_config_versions) printf '12\n' ;;
    *) return 1 ;;
  esac
}

toolstore_txn_error() {
  if declare -F log_error >/dev/null 2>&1; then
    log_error "$*"
  else
    printf 'toolstore transaction: %s\n' "$*" >&2
  fi
}

toolstore_txn_warn() {
  if declare -F log_warn >/dev/null 2>&1; then
    log_warn "$*"
  else
    printf 'toolstore transaction warning: %s\n' "$*" >&2
  fi
}

toolstore_txn_fault_point() {
  local point="$1"
  if [[ "${TOOLSTORE_TXN_FAULT_POINT:-}" == "$point" ]]; then
    toolstore_txn_error "fault injection at ${point}"
    return 97
  fi
}

toolstore_txn_env_value() {
  local content="$1" key="$2" value
  value="$(printf '%s\n' "$content" | awk -v k="$key" '
    index($0, k "=") == 1 { value = substr($0, length(k)+2); found = 1 }
    END { if (found) print value }
  ')"
  value="${value%$'\r'}"
  if [[ ${#value} -ge 2 && "$value" == \'*\' ]]; then
    value="${value:1:${#value}-2}"
    value="${value//\\\'/\'}"
  elif [[ ${#value} -ge 2 && "$value" == \"*\" ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s\n' "$value"
}

toolstore_txn_quote() {
  local value="${1-}" escaped
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || return 1
  escaped="${value//\'/\\\'}"
  printf "'%s'" "$escaped"
}

toolstore_txn_record_line() {
  local key="$1" value="${2-}"
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || return 1
  printf '%s=%s\n' "$key" "$(toolstore_txn_quote "$value")"
}

toolstore_txn_sync_file() {
  sync -f "$1" 2>/dev/null || sync "$1" 2>/dev/null
}

toolstore_txn_mode_is_private() {
  local path="$1" mode system
  mode="$(stat -Lc '%a' -- "$path" 2>/dev/null)" || return 1
  [[ "$mode" == "600" ]] && return 0
  system="$(uname -s 2>/dev/null || true)"
  [[ "$system" == MINGW* || "$system" == MSYS* || "$system" == CYGWIN* ]]
}

toolstore_txn_path_has_no_symlinks() {
  local path="$1" lexical resolved
  command -v realpath >/dev/null 2>&1 || return 1
  lexical="$(realpath -m -s -- "$path" 2>/dev/null)" || return 1
  resolved="$(realpath -m -- "$path" 2>/dev/null)" || return 1
  [[ "$lexical" == "$resolved" ]]
}

toolstore_txn_project_path() {
  local project_dir="$1" lexical resolved parent base
  [[ -n "$project_dir" && "$project_dir" != "/" ]] || return 1
  command -v realpath >/dev/null 2>&1 || return 1
  lexical="$(realpath -m -s -- "$project_dir" 2>/dev/null)" || return 1
  resolved="$(realpath -m -- "$project_dir" 2>/dev/null)" || return 1
  [[ "$lexical" == "$resolved" && "$resolved" != "/" ]] || return 1
  parent="$(dirname -- "$resolved")"
  base="$(basename -- "$resolved")"
  [[ -d "$parent" && ! -L "$parent" ]] || return 1
  [[ -n "$base" && "$base" != "." && "$base" != ".." && "$base" != "/" ]] || return 1
  printf '%s\n' "$resolved"
}

toolstore_txn_root() {
  local project root
  project="$(toolstore_txn_project_path "$1")" || return 1
  root="$(dirname -- "$project")/.$(basename -- "$project").toolstore-transactions"
  toolstore_txn_path_has_no_symlinks "$root" || return 1
  printf '%s\n' "$root"
}

toolstore_txn_active_record_path() {
  printf '%s/active.env\n' "$(toolstore_txn_root "$1")"
}

toolstore_txn_assert_under() {
  local child="$1" parent="$2" child_abs parent_abs
  child_abs="$(realpath -m -s -- "$child" 2>/dev/null)" || return 1
  parent_abs="$(realpath -m -s -- "$parent" 2>/dev/null)" || return 1
  [[ "$child_abs" == "$parent_abs"/* ]]
}

toolstore_txn_atomic_write() {
  local target="$1" content="$2" parent tmp
  parent="$(dirname -- "$target")"
  [[ -d "$parent" && ! -L "$parent" ]] || return 1
  toolstore_txn_path_has_no_symlinks "$parent" || return 1
  [[ ! -e "$target" || ( -f "$target" && ! -L "$target" ) ]] || return 1
  tmp="$(umask 077; mktemp "${target}.tmp.XXXXXX")" || return 1
  if ! printf '%s\n' "$content" >"$tmp" ||
    ! chmod 600 "$tmp" ||
    ! toolstore_txn_sync_file "$tmp" ||
    ! mv -Tf -- "$tmp" "$target" ||
    ! toolstore_txn_sync_file "$parent"; then
    rm -f -- "$tmp"
    return 1
  fi
  [[ -f "$target" && ! -L "$target" ]] && toolstore_txn_mode_is_private "$target"
}

toolstore_txn_atomic_copy() {
  local source="$1" target="$2" parent tmp
  [[ -f "$source" && ! -L "$source" ]] || return 1
  toolstore_txn_path_has_no_symlinks "$source" || return 1
  parent="$(dirname -- "$target")"
  [[ -d "$parent" && ! -L "$parent" ]] || return 1
  tmp="$(umask 077; mktemp "${target}.tmp.XXXXXX")" || return 1
  if ! cp -- "$source" "$tmp" ||
    ! chmod 600 "$tmp" ||
    ! toolstore_txn_sync_file "$tmp" ||
    ! mv -Tf -- "$tmp" "$target" ||
    ! toolstore_txn_sync_file "$parent"; then
    rm -f -- "$tmp"
    return 1
  fi
  [[ -f "$target" && ! -L "$target" ]] && toolstore_txn_mode_is_private "$target"
}

toolstore_txn_load_private_file() {
  local path="$1" before after content
  [[ -f "$path" && ! -L "$path" && -r "$path" ]] || return 1
  toolstore_txn_path_has_no_symlinks "$path" || return 1
  toolstore_txn_mode_is_private "$path" || return 1
  before="$(stat -Lc '%d:%i' -- "$path" 2>/dev/null)" || return 1
  content="$(<"$path")" || return 1
  after="$(stat -Lc '%d:%i' -- "$path" 2>/dev/null)" || return 1
  [[ "$before" == "$after" && -f "$path" && ! -L "$path" ]] || return 1
  printf '%s\n' "$content"
}

toolstore_txn_record_set() {
  local record="$1" key="$2" value="$3" content updated
  content="$(toolstore_txn_load_private_file "$record")" || return 1
  updated="$(printf '%s\n' "$content" | awk -v k="$key" 'index($0, k "=") == 1 { next } { print }')"
  updated+=$'\n'
  updated+="$(toolstore_txn_record_line "$key" "$value")"
  toolstore_txn_atomic_write "$record" "${updated%$'\n'}"
}

toolstore_txn_sha256() {
  sha256sum -- "$1" | awk '{print $1}'
}

toolstore_txn_resolve_db_path() {
  local env_file="$1" project_dir="$2" project raw relative host data_root
  project="$(toolstore_txn_project_path "$project_dir")" || return 1
  [[ -f "$env_file" && ! -L "$env_file" ]] || return 1
  if grep -qE '^TOOL_STORE_PATH=' "$env_file"; then
    raw="$(toolstore_txn_env_value "$(<"$env_file")" TOOL_STORE_PATH)"
    [[ -n "$raw" ]] || return 1
  else
    raw="/app/data/control-plane.db"
  fi
  case "$raw" in
    /app/data/*) relative="data/${raw#/app/data/}" ;;
    ./data/*) relative="${raw#./}" ;;
    data/*) relative="$raw" ;;
    *) return 1 ;;
  esac
  [[ "$relative" != "data/" && "$relative" != */ && "$relative" != *$'\n'* ]] || return 1
  host="$(realpath -m -s -- "${project}/${relative}" 2>/dev/null)" || return 1
  data_root="$(realpath -m -s -- "${project}/data" 2>/dev/null)" || return 1
  [[ "$host" == "$data_root"/* && "$host" != "/" ]] || return 1
  toolstore_txn_path_has_no_symlinks "$host" || return 1
  printf '%s\n' "$host"
}

toolstore_txn_metadata_field() {
  local json="$1" field="$2"
  case "$field" in
    schema_version)
      printf '%s' "$json" | sed -n 's/.*"schema_version"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p'
      ;;
    size_bytes)
      printf '%s' "$json" | sed -n 's/.*"size_bytes"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p'
      ;;
    sha256)
      printf '%s' "$json" | sed -n 's/.*"sha256"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F][0-9a-fA-F]*\)".*/\1/p' | tr 'A-F' 'a-f'
      ;;
    migration_ledger_sha256)
      printf '%s' "$json" | sed -n 's/.*"migration_ledger_sha256"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F][0-9a-fA-F]*\)".*/\1/p' | tr 'A-F' 'a-f'
      ;;
    schema_objects_sha256)
      printf '%s' "$json" | sed -n 's/.*"schema_objects_sha256"[[:space:]]*:[[:space:]]*"\([0-9a-fA-F][0-9a-fA-F]*\)".*/\1/p' | tr 'A-F' 'a-f'
      ;;
    integrity)
      printf '%s' "$json" | sed -n 's/.*"integrity"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
      ;;
    *) return 1 ;;
  esac
}

toolstore_txn_metadata_count() {
  local json="$1" table="$2"
  [[ " ${TOOLSTORE_TXN_CORE_TABLES[*]} " == *" ${table} "* ]] || return 1
  printf '%s' "$json" | sed -n "s/.*\"${table}\"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p"
}

toolstore_txn_metadata_valid() {
  local json="$1" schema size digest ledger schema_objects integrity table min_version count
  schema="$(toolstore_txn_metadata_field "$json" schema_version)"
  size="$(toolstore_txn_metadata_field "$json" size_bytes)"
  digest="$(toolstore_txn_metadata_field "$json" sha256)"
  ledger="$(toolstore_txn_metadata_field "$json" migration_ledger_sha256)"
  schema_objects="$(toolstore_txn_metadata_field "$json" schema_objects_sha256)"
  integrity="$(toolstore_txn_metadata_field "$json" integrity)"
  [[ "$schema" =~ ^[1-9][0-9]*$ && "$size" =~ ^[1-9][0-9]*$ &&
    "$digest" =~ ^[0-9a-f]{64}$ && "$ledger" =~ ^[0-9a-f]{64}$ &&
    "$schema_objects" =~ ^[0-9a-f]{64}$ && "$integrity" == "ok" ]] || return 1
  for table in "${TOOLSTORE_TXN_CORE_TABLES[@]}"; do
    min_version="$(toolstore_txn_table_min_version "$table")" || return 1
    (( schema >= min_version )) || continue
    count="$(toolstore_txn_metadata_count "$json" "$table")"
    [[ "$count" =~ ^[0-9]+$ ]] || return 1
  done
}

toolstore_txn_core_counts_match() {
  local expected="$1" actual="$2" expected_schema table min_version left right
  toolstore_txn_metadata_valid "$expected" && toolstore_txn_metadata_valid "$actual" || return 1
  expected_schema="$(toolstore_txn_metadata_field "$expected" schema_version)"
  for table in "${TOOLSTORE_TXN_CORE_TABLES[@]}"; do
    min_version="$(toolstore_txn_table_min_version "$table")" || return 1
    (( expected_schema >= min_version )) || continue
    left="$(toolstore_txn_metadata_count "$expected" "$table")"
    right="$(toolstore_txn_metadata_count "$actual" "$table")"
    [[ "$left" == "$right" ]] || return 1
  done
}

toolstore_txn_metadata_same_backup() {
  local left="$1" right="$2"
  toolstore_txn_metadata_same_database_truth "$left" "$right" &&
    [[ "$(toolstore_txn_metadata_field "$left" size_bytes)" == "$(toolstore_txn_metadata_field "$right" size_bytes)" ]] &&
    [[ "$(toolstore_txn_metadata_field "$left" sha256)" == "$(toolstore_txn_metadata_field "$right" sha256)" ]]
}

toolstore_txn_metadata_same_database_truth() {
  local left="$1" right="$2"
  toolstore_txn_metadata_valid "$left" && toolstore_txn_metadata_valid "$right" &&
    [[ "$(toolstore_txn_metadata_field "$left" schema_version)" == "$(toolstore_txn_metadata_field "$right" schema_version)" ]] &&
    [[ "$(toolstore_txn_metadata_field "$left" migration_ledger_sha256)" == "$(toolstore_txn_metadata_field "$right" migration_ledger_sha256)" ]] &&
    [[ "$(toolstore_txn_metadata_field "$left" schema_objects_sha256)" == "$(toolstore_txn_metadata_field "$right" schema_objects_sha256)" ]] &&
    toolstore_txn_core_counts_match "$left" "$right" &&
    toolstore_txn_core_counts_match "$right" "$left"
}

toolstore_txn_run_ctl() {
  local image="$1" operation="$2" source="$3" destination="${4-}"
  local source_parent source_name destination_parent destination_name
  if [[ -n "${TOOLSTORE_TXN_RUNNER:-}" ]]; then
    "$TOOLSTORE_TXN_RUNNER" "$image" "$operation" "$source" "$destination"
    return
  fi
  [[ "$image" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
  source_parent="$(dirname -- "$source")"
  source_name="$(basename -- "$source")"
  case "$operation" in
    verify)
      docker run --rm --network none --read-only --cap-drop ALL \
        --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,size=16m \
        --mount "type=bind,src=${source_parent},dst=/toolstore/source,readonly" \
        --entrypoint /app/toolstorectl "$image" verify \
        --path "/toolstore/source/${source_name}"
      ;;
    backup|restore)
      [[ -n "$destination" ]] || return 1
      destination_parent="$(dirname -- "$destination")"
      destination_name="$(basename -- "$destination")"
      if [[ "$operation" == "backup" ]]; then
        docker run --rm --network none --read-only --cap-drop ALL \
          --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,size=16m \
          --mount "type=bind,src=${source_parent},dst=/toolstore/source,readonly" \
          --mount "type=bind,src=${destination_parent},dst=/toolstore/destination" \
          --entrypoint /app/toolstorectl "$image" backup \
          --source "/toolstore/source/${source_name}" \
          --destination "/toolstore/destination/${destination_name}"
      else
        docker run --rm --network none --read-only --cap-drop ALL \
          --security-opt no-new-privileges --tmpfs /tmp:rw,noexec,nosuid,size=16m \
          --mount "type=bind,src=${source_parent},dst=/toolstore/source,readonly" \
          --mount "type=bind,src=${destination_parent},dst=/toolstore/destination" \
          --entrypoint /app/toolstorectl "$image" restore \
          --backup "/toolstore/source/${source_name}" \
          --destination "/toolstore/destination/${destination_name}"
      fi
      ;;
    *) return 1 ;;
  esac
}

toolstore_txn_validate_record() {
  local project_dir="$1" content="$2" project root request_id compose_project candidate_container_id txn_dir db_path data_dir
  local preflight_path preflight_metadata_path backup_path metadata_path config_path archive_path staged_path state name key snapshot
  project="$(toolstore_txn_project_path "$project_dir")" || return 1
  root="$(toolstore_txn_root "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" FORMAT)" == "$TOOLSTORE_TXN_FORMAT_VERSION" ]] || return 1
  [[ "$(toolstore_txn_env_value "$content" PROJECT_DIR)" == "$project" ]] || return 1
  request_id="$(toolstore_txn_env_value "$content" REQUEST_ID)"
  [[ "$request_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{5,127}$ ]] || return 1
  compose_project="$(toolstore_txn_env_value "$content" COMPOSE_PROJECT_NAME)"
  [[ "$compose_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || return 1
  candidate_container_id="$(toolstore_txn_env_value "$content" CANDIDATE_CONTAINER_ID)"
  [[ -z "$candidate_container_id" || "$candidate_container_id" =~ ^[0-9a-f]{64}$ ]] || return 1
  txn_dir="$(toolstore_txn_env_value "$content" TXN_DIR)"
  [[ "$txn_dir" == "${root}/requests/${request_id}" ]] || return 1
  toolstore_txn_assert_under "$txn_dir" "$root" || return 1
  [[ -d "$txn_dir" && ! -L "$txn_dir" ]] || return 1
  toolstore_txn_path_has_no_symlinks "$txn_dir" || return 1
  config_path="$(toolstore_txn_env_value "$content" CONFIG_SNAPSHOT)"
  [[ "$config_path" == "${txn_dir}/config.env" ]] || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  case "$state" in
    preparing|preflight|authoritative|candidate_active|reinstall_staged|database_restored|rolled_back|committed) ;;
    *) return 1 ;;
  esac
  db_path="$(toolstore_txn_env_value "$content" DB_PATH)"
  data_dir="$(toolstore_txn_env_value "$content" DATA_DIR)"
  [[ "$data_dir" == "${project}/data" && "$db_path" == "${data_dir}"/* ]] || return 1
  if [[ "$state" != "committed" ]]; then
    [[ "$db_path" == "$(toolstore_txn_resolve_db_path "$config_path" "$project_dir" 2>/dev/null || true)" ]] || return 1
  fi
  backup_path="$(toolstore_txn_env_value "$content" BACKUP_PATH)"
  metadata_path="$(toolstore_txn_env_value "$content" BACKUP_METADATA_PATH)"
  preflight_path="$(toolstore_txn_env_value "$content" PREFLIGHT_BACKUP_PATH)"
  preflight_metadata_path="$(toolstore_txn_env_value "$content" PREFLIGHT_METADATA_PATH)"
  [[ "$backup_path" == "${txn_dir}/backup.db" && "$metadata_path" == "${txn_dir}/backup.metadata.json" ]] || return 1
  [[ "$preflight_path" == "${txn_dir}/preflight.db" &&
    "$preflight_metadata_path" == "${txn_dir}/preflight.metadata.json" ]] || return 1
  archive_path="$(toolstore_txn_env_value "$content" ARCHIVE_PATH)"
  staged_path="$(toolstore_txn_env_value "$content" STAGED_DATA_PATH)"
  [[ "$archive_path" == "${txn_dir}/evidence/migrated.db" && "$staged_path" == "${txn_dir}/staged-data" ]] || return 1
  for name in docker-compose.yml docker-compose.host.yml docker-compose.logdb.yml; do
    case "$name" in
      docker-compose.yml) key=COMPOSE_BASE ;;
      docker-compose.host.yml) key=COMPOSE_HOST ;;
      docker-compose.logdb.yml) key=COMPOSE_LOGDB ;;
    esac
    snapshot="$(toolstore_txn_env_value "$content" "${key}_SNAPSHOT")"
    [[ "$snapshot" == "${txn_dir}/${name}" ]] || return 1
  done
  [[ "$(toolstore_txn_env_value "$content" OLD_IMAGE)" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
  [[ "$(toolstore_txn_env_value "$content" CANDIDATE_IMAGE)" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
  [[ "$(toolstore_txn_env_value "$content" CTL_IMAGE)" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
}

toolstore_txn_load_record() {
  local project_dir="$1" record content
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_private_file "$record")" || return 1
  toolstore_txn_validate_record "$project_dir" "$content" || return 1
  printf '%s\n' "$content"
}

toolstore_txn_has_active() {
  local record
  record="$(toolstore_txn_active_record_path "$1" 2>/dev/null)" || return 1
  [[ -e "$record" || -L "$record" ]]
}

toolstore_txn_candidate_identity() {
  local project_dir="$1" name_ref="$2" request_ref="$3" compose_ref="$4" image_ref="$5" project_ref="$6"
  local content txn_project_value txn_request_value txn_compose_value txn_image_value
  local -n name_out="$name_ref" request_out="$request_ref" compose_out="$compose_ref"
  local -n image_out="$image_ref" project_out="$project_ref"
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  txn_project_value="$(toolstore_txn_project_path "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" PROJECT_DIR)" == "$txn_project_value" ]] || return 1
  txn_request_value="$(toolstore_txn_env_value "$content" REQUEST_ID)"
  txn_compose_value="$(toolstore_txn_env_value "$content" COMPOSE_PROJECT_NAME)"
  txn_image_value="$(toolstore_txn_env_value "$content" CANDIDATE_IMAGE)"
  [[ "$txn_request_value" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{5,127}$ ]] || return 1
  [[ "$txn_compose_value" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || return 1
  [[ "$txn_image_value" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
  name_out="${txn_compose_value}-newapi-tools-candidate-${txn_request_value}"
  request_out="$txn_request_value"
  compose_out="$txn_compose_value"
  image_out="$txn_image_value"
  project_out="$txn_project_value"
}

toolstore_txn_candidate_container_matches() {
  local project_dir="$1" container="$2"
  local expected_name request_id compose_project candidate_image project
  local content recorded_id actual_id actual_name actual_request actual_compose actual_project actual_purpose
  local actual_image_label actual_config_image actual_compose_label
  toolstore_txn_candidate_identity "$project_dir" expected_name request_id compose_project candidate_image project || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  recorded_id="$(toolstore_txn_env_value "$content" CANDIDATE_CONTAINER_ID)"
  docker inspect "$container" >/dev/null 2>&1 || return 1
  actual_id="$(docker inspect --format '{{.Id}}' "$container" 2>/dev/null)" || return 1
  actual_name="$(docker inspect --format '{{.Name}}' "$container" 2>/dev/null)" || return 1
  actual_name="${actual_name#/}"
  [[ "$actual_id" =~ ^[0-9a-f]{64}$ && "$actual_name" == "$expected_name" ]] || return 1
  [[ -z "$recorded_id" || "$actual_id" == "$recorded_id" ]] || return 1
  actual_request="$(docker inspect --format '{{ index .Config.Labels "io.newapi-tools.candidate.request-id" }}' "$container" 2>/dev/null)" || return 1
  actual_compose="$(docker inspect --format '{{ index .Config.Labels "io.newapi-tools.candidate.compose-project" }}' "$container" 2>/dev/null)" || return 1
  actual_project="$(docker inspect --format '{{ index .Config.Labels "io.newapi-tools.candidate.project-dir" }}' "$container" 2>/dev/null)" || return 1
  actual_purpose="$(docker inspect --format '{{ index .Config.Labels "io.newapi-tools.candidate.purpose" }}' "$container" 2>/dev/null)" || return 1
  actual_image_label="$(docker inspect --format '{{ index .Config.Labels "io.newapi-tools.candidate.image" }}' "$container" 2>/dev/null)" || return 1
  actual_compose_label="$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' "$container" 2>/dev/null)" || return 1
  actual_config_image="$(docker inspect --format '{{.Config.Image}}' "$container" 2>/dev/null)" || return 1
  [[ "$actual_request" == "$request_id" ]] || return 1
  [[ "$actual_compose" == "$compose_project" && "$actual_compose_label" == "$compose_project" ]] || return 1
  [[ "$actual_project" == "$project" ]] || return 1
  [[ "$actual_purpose" == "toolstore-migration-candidate" ]] || return 1
  [[ "$actual_image_label" == "$candidate_image" && "$actual_config_image" == "$candidate_image" ]] || return 1
}

toolstore_txn_record_candidate_container_id() {
  local project_dir="$1" container_id="$2" content state record
  [[ -z "$container_id" || "$container_id" =~ ^[0-9a-f]{64}$ ]] || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  [[ -z "$container_id" || "$state" == candidate_active ]] || return 1
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  toolstore_txn_record_set "$record" CANDIDATE_CONTAINER_ID "$container_id" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" CANDIDATE_CONTAINER_ID)" == "$container_id" ]]
}

toolstore_txn_remove_created_candidate_by_id() {
  local project_dir="$1" container_id="$2" expected_name request_id compose_project candidate_image project
  local content recorded_id actual_id actual_name actual_config_image
  [[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || return 1
  toolstore_txn_candidate_identity "$project_dir" expected_name request_id compose_project candidate_image project || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  recorded_id="$(toolstore_txn_env_value "$content" CANDIDATE_CONTAINER_ID)"
  [[ "$recorded_id" == "$container_id" ]] || return 1
  if ! docker inspect "$container_id" >/dev/null 2>&1; then
    ! docker inspect "$expected_name" >/dev/null 2>&1 || return 1
    toolstore_txn_record_candidate_container_id "$project_dir" ''
    return
  fi
  actual_id="$(docker inspect --format '{{.Id}}' "$container_id" 2>/dev/null)" || return 1
  actual_name="$(docker inspect --format '{{.Name}}' "$container_id" 2>/dev/null)" || return 1
  actual_config_image="$(docker inspect --format '{{.Config.Image}}' "$container_id" 2>/dev/null)" || return 1
  actual_name="${actual_name#/}"
  [[ "$actual_id" == "$container_id" && "$actual_name" == "$expected_name" ]] || return 1
  [[ "$actual_config_image" == "$candidate_image" ]] || return 1
  docker rm -f "$container_id" >/dev/null 2>&1 || return 1
  ! docker inspect "$container_id" >/dev/null 2>&1 || return 1
  ! docker inspect "$expected_name" >/dev/null 2>&1 || return 1
  toolstore_txn_record_candidate_container_id "$project_dir" ''
}

toolstore_txn_remove_candidate_container() {
  local project_dir="$1" container request_id compose_project candidate_image project content recorded_id container_id
  toolstore_txn_candidate_identity "$project_dir" container request_id compose_project candidate_image project || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  recorded_id="$(toolstore_txn_env_value "$content" CANDIDATE_CONTAINER_ID)"
  if [[ -n "$recorded_id" ]]; then
    if docker inspect "$recorded_id" >/dev/null 2>&1; then
      container_id="$recorded_id"
    else
      if docker inspect "$container" >/dev/null 2>&1; then
        toolstore_txn_error "refusing to remove replacement candidate container ${container}: recorded container id is absent"
        return 1
      fi
      toolstore_txn_record_candidate_container_id "$project_dir" ''
      return
    fi
  elif docker inspect "$container" >/dev/null 2>&1; then
    container_id="$(docker inspect --format '{{.Id}}' "$container" 2>/dev/null)" || return 1
    [[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || return 1
  else
    return 0
  fi
  if ! toolstore_txn_candidate_container_matches "$project_dir" "$container_id"; then
    toolstore_txn_error "refusing to remove candidate container ${container}: identity does not match active transaction"
    return 1
  fi
  docker rm -f "$container_id" >/dev/null 2>&1 || return 1
  ! docker inspect "$container_id" >/dev/null 2>&1 || return 1
  ! docker inspect "$container" >/dev/null 2>&1 || return 1
  toolstore_txn_record_candidate_container_id "$project_dir" ''
}

toolstore_txn_snapshot_files() {
  local project="$1" txn_dir="$2" config="$3" name source target state hash content="" override_var override
  toolstore_txn_atomic_copy "$config" "${txn_dir}/config.env" || return 1
  content+="$(toolstore_txn_record_line CONFIG_SNAPSHOT "${txn_dir}/config.env")"$'\n'
  content+="$(toolstore_txn_record_line CONFIG_SHA256 "$(toolstore_txn_sha256 "${txn_dir}/config.env")")"$'\n'
  for name in docker-compose.yml docker-compose.host.yml docker-compose.logdb.yml; do
    case "$name" in
      docker-compose.yml) override_var=TOOLSTORE_TXN_COMPOSE_BASE_SOURCE ;;
      docker-compose.host.yml) override_var=TOOLSTORE_TXN_COMPOSE_HOST_SOURCE ;;
      docker-compose.logdb.yml) override_var=TOOLSTORE_TXN_COMPOSE_LOGDB_SOURCE ;;
    esac
    override="${!override_var-}"
    source="${project}/${name}"
    [[ -z "$override" || "$override" == "__ABSENT__" ]] || source="$override"
    target="${txn_dir}/${name}"
    state=absent
    hash=""
    if [[ "$override" != "__ABSENT__" && ( -e "$source" || -L "$source" ) ]]; then
      toolstore_txn_atomic_copy "$source" "$target" || return 1
      state=present
      hash="$(toolstore_txn_sha256 "$target")" || return 1
    fi
    case "$name" in
      docker-compose.yml) name=COMPOSE_BASE ;;
      docker-compose.host.yml) name=COMPOSE_HOST ;;
      docker-compose.logdb.yml) name=COMPOSE_LOGDB ;;
    esac
    content+="$(toolstore_txn_record_line "${name}_STATE" "$state")"$'\n'
    content+="$(toolstore_txn_record_line "${name}_SNAPSHOT" "$target")"$'\n'
    content+="$(toolstore_txn_record_line "${name}_SHA256" "$hash")"$'\n'
  done
  printf '%s' "$content"
}

toolstore_txn_complete_preparing() {
  local project_dir="$1" record content backup db ctl output verified metadata schema digest
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" STATE)" == "preparing" ]] || return 1
  backup="$(toolstore_txn_env_value "$content" PREFLIGHT_BACKUP_PATH)"
  db="$(toolstore_txn_env_value "$content" DB_PATH)"
  ctl="$(toolstore_txn_env_value "$content" CTL_IMAGE)"
  metadata="$(toolstore_txn_env_value "$content" PREFLIGHT_METADATA_PATH)"
  if [[ -e "$backup" || -L "$backup" ]]; then
    [[ -f "$backup" && ! -L "$backup" ]] || return 1
    output="$(toolstore_txn_run_ctl "$ctl" verify "$backup")" || return 1
  else
    [[ -f "$db" && ! -L "$db" ]] || return 1
    output="$(toolstore_txn_run_ctl "$ctl" backup "$db" "$backup")" || return 1
  fi
  toolstore_txn_fault_point after_preflight_backup || return $?
  [[ -f "$backup" && ! -L "$backup" ]] || return 1
  toolstore_txn_mode_is_private "$backup" || return 1
  verified="$(toolstore_txn_run_ctl "$ctl" verify "$backup")" || return 1
  toolstore_txn_metadata_same_backup "$output" "$verified" || return 1
  toolstore_txn_atomic_write "$metadata" "$verified" || return 1
  schema="$(toolstore_txn_metadata_field "$verified" schema_version)"
  digest="$(toolstore_txn_metadata_field "$verified" sha256)"
  toolstore_txn_record_set "$record" PREFLIGHT_SCHEMA "$schema" || return 1
  toolstore_txn_record_set "$record" PREFLIGHT_HASH "$digest" || return 1
  toolstore_txn_record_set "$record" STATE preflight || return 1
  toolstore_txn_fault_point after_preflight || return $?
}

toolstore_txn_verify_preflight() {
  local project_dir="$1" content backup metadata ctl expected actual
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" STATE)" == "preflight" ]] || return 1
  backup="$(toolstore_txn_env_value "$content" PREFLIGHT_BACKUP_PATH)"
  metadata="$(toolstore_txn_env_value "$content" PREFLIGHT_METADATA_PATH)"
  ctl="$(toolstore_txn_env_value "$content" CTL_IMAGE)"
  [[ -f "$backup" && ! -L "$backup" ]] || return 1
  toolstore_txn_mode_is_private "$backup" || return 1
  expected="$(toolstore_txn_load_private_file "$metadata")" || return 1
  actual="$(toolstore_txn_run_ctl "$ctl" verify "$backup")" || return 1
  toolstore_txn_metadata_same_backup "$expected" "$actual" || return 1
  [[ "$(toolstore_txn_env_value "$content" PREFLIGHT_SCHEMA)" == "$(toolstore_txn_metadata_field "$actual" schema_version)" ]] || return 1
  [[ "$(toolstore_txn_env_value "$content" PREFLIGHT_HASH)" == "$(toolstore_txn_metadata_field "$actual" sha256)" ]] || return 1
  printf '%s\n' "$actual"
}

# The preflight snapshot proves that backup tooling and the source path work,
# but it is never a rollback anchor. Call this only after the old service and
# every write entry have stopped. Any unmarked backup left by a crash is
# discarded and recaptured so intervening writes can never be lost.
toolstore_txn_authoritative_backup() {
  local project_dir="$1" record content state backup metadata db ctl preflight output verified schema digest path
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  if [[ "$state" == "authoritative" ]]; then
    toolstore_txn_verify_backup "$project_dir" >/dev/null
    return
  fi
  [[ "$state" == "preflight" ]] || return 1
  preflight="$(toolstore_txn_verify_preflight "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  backup="$(toolstore_txn_env_value "$content" BACKUP_PATH)"
  metadata="$(toolstore_txn_env_value "$content" BACKUP_METADATA_PATH)"
  db="$(toolstore_txn_env_value "$content" DB_PATH)"
  ctl="$(toolstore_txn_env_value "$content" CTL_IMAGE)"
  for path in "$backup" "$metadata"; do
    if [[ -e "$path" || -L "$path" ]]; then
      [[ -f "$path" && ! -L "$path" ]] || return 1
      rm -f -- "$path" || return 1
    fi
  done
  toolstore_txn_sync_file "$(dirname -- "$backup")" || return 1
  [[ -f "$db" && ! -L "$db" ]] || return 1
  output="$(toolstore_txn_run_ctl "$ctl" backup "$db" "$backup")" || return 1
  toolstore_txn_fault_point after_authoritative_backup || return $?
  [[ -f "$backup" && ! -L "$backup" ]] || return 1
  toolstore_txn_mode_is_private "$backup" || return 1
  verified="$(toolstore_txn_run_ctl "$ctl" verify "$backup")" || return 1
  toolstore_txn_metadata_same_backup "$output" "$verified" || return 1
  [[ "$(toolstore_txn_metadata_field "$preflight" schema_version)" == "$(toolstore_txn_metadata_field "$verified" schema_version)" ]] || return 1
  [[ "$(toolstore_txn_metadata_field "$preflight" migration_ledger_sha256)" == "$(toolstore_txn_metadata_field "$verified" migration_ledger_sha256)" ]] || return 1
  [[ "$(toolstore_txn_metadata_field "$preflight" schema_objects_sha256)" == "$(toolstore_txn_metadata_field "$verified" schema_objects_sha256)" ]] || return 1
  toolstore_txn_atomic_write "$metadata" "$verified" || return 1
  schema="$(toolstore_txn_metadata_field "$verified" schema_version)"
  digest="$(toolstore_txn_metadata_field "$verified" sha256)"
  toolstore_txn_record_set "$record" OLD_SCHEMA "$schema" || return 1
  toolstore_txn_record_set "$record" OLD_HASH "$digest" || return 1
  toolstore_txn_record_set "$record" AUTHORITATIVE_AT "$(date -u +%Y-%m-%dT%H:%M:%SZ)" || return 1
  toolstore_txn_record_set "$record" STATE authoritative || return 1
  toolstore_txn_fault_point after_authoritative_marker || return $?
}

toolstore_txn_prepare() {
  local env_file="$1" project_dir="$2" old_image="$3" candidate_image="$4" ctl_image="$5"
  local project root record content state request_id compose_project env_content txn_dir db snapshot_lines initial
  project="$(toolstore_txn_project_path "$project_dir")" || return 1
  [[ -d "$project" && ! -L "$project" ]] || return 1
  [[ -f "$env_file" && ! -L "$env_file" ]] || return 1
  env_content="$(<"$env_file")" || return 1
  compose_project="$(toolstore_txn_env_value "$env_content" COMPOSE_PROJECT_NAME)"
  [[ "$compose_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || return 1
  [[ "$old_image" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
  [[ "$candidate_image" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
  [[ "$ctl_image" =~ @sha256:[0-9a-fA-F]{64}$ ]] || return 1
  root="$(toolstore_txn_root "$project")" || return 1
  record="${root}/active.env"
  if [[ -e "$record" || -L "$record" ]]; then
    content="$(toolstore_txn_load_record "$project")" || return 1
    [[ "$(toolstore_txn_env_value "$content" COMPOSE_PROJECT_NAME)" == "$compose_project" ]] || return 1
    state="$(toolstore_txn_env_value "$content" STATE)"
    if [[ "$state" == "committed" ]]; then
      toolstore_txn_finish_committed "$project" || return 1
    elif [[ "$state" == "preparing" ]]; then
      toolstore_txn_complete_preparing "$project" || return 1
      return 0
    elif [[ "$state" == "preflight" ]]; then
      toolstore_txn_verify_preflight "$project" >/dev/null || return 1
      toolstore_txn_record_set "$record" CANDIDATE_IMAGE "$candidate_image" || return 1
      toolstore_txn_record_set "$record" CTL_IMAGE "$ctl_image" || return 1
      return 0
    elif [[ "$state" == "rolled_back" ]]; then
      toolstore_txn_retire_rolled_back "$project" || return 1
    elif [[ "$state" == "authoritative" &&
      "${TOOLSTORE_TXN_CONTINUE_REQUEST:-}" == "$(toolstore_txn_env_value "$content" REQUEST_ID)" ]]; then
      toolstore_txn_verify_backup "$project" >/dev/null || return 1
      toolstore_txn_record_set "$record" CANDIDATE_IMAGE "$candidate_image" || return 1
      toolstore_txn_record_set "$record" CTL_IMAGE "$ctl_image" || return 1
      return 0
    elif [[ "$state" == "authoritative" || "$state" == "candidate_active" ||
      "$state" == "reinstall_staged" || "$state" == "database_restored" ]]; then
      toolstore_txn_error "unfinished Tool Store transaction requires rollback before a new candidate"
      return 1
    else
      return 1
    fi
  fi
  mkdir -p -m 700 "${root}/requests" "${root}/history" || return 1
  chmod 700 "$root" "${root}/requests" "${root}/history" || return 1
  toolstore_txn_path_has_no_symlinks "$root" || return 1
  request_id="${TOOLSTORE_TXN_REQUEST_ID:-$(date -u +%Y%m%dT%H%M%SZ)-$$-${RANDOM}}"
  [[ "$request_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{5,127}$ ]] || return 1
  txn_dir="${root}/requests/${request_id}"
  [[ ! -e "$txn_dir" && ! -L "$txn_dir" ]] || return 1
  mkdir -m 700 "$txn_dir" "${txn_dir}/evidence" || return 1
  db="$(toolstore_txn_resolve_db_path "$env_file" "$project")" || return 1
  [[ -f "$db" && ! -L "$db" ]] || return 1
  snapshot_lines="$(toolstore_txn_snapshot_files "$project" "$txn_dir" "$env_file")" || return 1
  if [[ -n "${TOOLSTORE_TXN_LIBRARY_SOURCE:-}" ]]; then
    toolstore_txn_atomic_copy "$TOOLSTORE_TXN_LIBRARY_SOURCE" "${txn_dir}/toolstore_transaction.sh" || return 1
    toolstore_txn_atomic_copy "$TOOLSTORE_TXN_LIBRARY_SOURCE" "${root}/recovery-library.sh" || return 1
  fi
  initial="$(toolstore_txn_record_line FORMAT "$TOOLSTORE_TXN_FORMAT_VERSION")"$'\n'
  initial+="$(toolstore_txn_record_line STATE preparing)"$'\n'
  initial+="$(toolstore_txn_record_line REQUEST_ID "$request_id")"$'\n'
  initial+="$(toolstore_txn_record_line PROJECT_DIR "$project")"$'\n'
  initial+="$(toolstore_txn_record_line COMPOSE_PROJECT_NAME "$compose_project")"$'\n'
  initial+="$(toolstore_txn_record_line CANDIDATE_CONTAINER_ID '')"$'\n'
  initial+="$(toolstore_txn_record_line TXN_DIR "$txn_dir")"$'\n'
  initial+="$(toolstore_txn_record_line DB_PATH "$db")"$'\n'
  initial+="$(toolstore_txn_record_line DATA_DIR "${project}/data")"$'\n'
  initial+="$(toolstore_txn_record_line PREFLIGHT_BACKUP_PATH "${txn_dir}/preflight.db")"$'\n'
  initial+="$(toolstore_txn_record_line PREFLIGHT_METADATA_PATH "${txn_dir}/preflight.metadata.json")"$'\n'
  initial+="$(toolstore_txn_record_line BACKUP_PATH "${txn_dir}/backup.db")"$'\n'
  initial+="$(toolstore_txn_record_line BACKUP_METADATA_PATH "${txn_dir}/backup.metadata.json")"$'\n'
  initial+="$(toolstore_txn_record_line ARCHIVE_PATH "${txn_dir}/evidence/migrated.db")"$'\n'
  initial+="$(toolstore_txn_record_line STAGED_DATA_PATH "${txn_dir}/staged-data")"$'\n'
  initial+="$(toolstore_txn_record_line OLD_IMAGE "$old_image")"$'\n'
  initial+="$(toolstore_txn_record_line CANDIDATE_IMAGE "$candidate_image")"$'\n'
  initial+="$(toolstore_txn_record_line CTL_IMAGE "$ctl_image")"$'\n'
  initial+="$(toolstore_txn_record_line PREFLIGHT_SCHEMA '')"$'\n'
  initial+="$(toolstore_txn_record_line PREFLIGHT_HASH '')"$'\n'
  initial+="$(toolstore_txn_record_line OLD_SCHEMA '')"$'\n'
  initial+="$(toolstore_txn_record_line OLD_HASH '')"$'\n'
  initial+="$(toolstore_txn_record_line AUTHORITATIVE_AT '')"$'\n'
  initial+="$snapshot_lines"
  toolstore_txn_atomic_write "$record" "$initial" || return 1
  toolstore_txn_fault_point after_record || return $?
  toolstore_txn_complete_preparing "$project"
}

toolstore_txn_verify_backup() {
  local project_dir="$1" content state backup metadata ctl expected actual schema digest
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  case "$state" in
    authoritative|candidate_active|reinstall_staged|database_restored|rolled_back|committed) ;;
    *) return 1 ;;
  esac
  backup="$(toolstore_txn_env_value "$content" BACKUP_PATH)"
  metadata="$(toolstore_txn_env_value "$content" BACKUP_METADATA_PATH)"
  ctl="$(toolstore_txn_env_value "$content" CTL_IMAGE)"
  [[ -f "$backup" && ! -L "$backup" ]] || return 1
  toolstore_txn_mode_is_private "$backup" || return 1
  expected="$(toolstore_txn_load_private_file "$metadata")" || return 1
  actual="$(toolstore_txn_run_ctl "$ctl" verify "$backup")" || return 1
  toolstore_txn_metadata_same_backup "$expected" "$actual" || return 1
  schema="$(toolstore_txn_env_value "$content" OLD_SCHEMA)"
  digest="$(toolstore_txn_env_value "$content" OLD_HASH)"
  [[ "$schema" == "$(toolstore_txn_metadata_field "$actual" schema_version)" ]] || return 1
  [[ "$digest" == "$(toolstore_txn_metadata_field "$actual" sha256)" ]] || return 1
  printf '%s\n' "$actual"
}

toolstore_txn_mark_candidate() {
  local project_dir="$1" record content state
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  [[ "$state" == "authoritative" ]] || return 1
  toolstore_txn_verify_backup "$project_dir" >/dev/null || return 1
  toolstore_txn_record_set "$record" STATE candidate_active || return 1
  toolstore_txn_fault_point after_candidate_mark || return $?
}

toolstore_txn_restore_files() {
  local project_dir="$1" project content txn_dir config_snapshot name key state snapshot target hash
  project="$(toolstore_txn_project_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project")" || return 1
  txn_dir="$(toolstore_txn_env_value "$content" TXN_DIR)"
  mkdir -p "$project" || return 1
  [[ -d "$project" && ! -L "$project" ]] || return 1
  config_snapshot="$(toolstore_txn_env_value "$content" CONFIG_SNAPSHOT)"
  [[ "$(toolstore_txn_sha256 "$config_snapshot")" == "$(toolstore_txn_env_value "$content" CONFIG_SHA256)" ]] || return 1
  toolstore_txn_atomic_copy "$config_snapshot" "${project}/.env" || return 1
  for name in docker-compose.yml docker-compose.host.yml docker-compose.logdb.yml; do
    case "$name" in
      docker-compose.yml) key=COMPOSE_BASE ;;
      docker-compose.host.yml) key=COMPOSE_HOST ;;
      docker-compose.logdb.yml) key=COMPOSE_LOGDB ;;
    esac
    state="$(toolstore_txn_env_value "$content" "${key}_STATE")"
    snapshot="$(toolstore_txn_env_value "$content" "${key}_SNAPSHOT")"
    target="${project}/${name}"
    if [[ "$state" == "present" ]]; then
      hash="$(toolstore_txn_env_value "$content" "${key}_SHA256")"
      [[ "$(toolstore_txn_sha256 "$snapshot")" == "$hash" ]] || return 1
      toolstore_txn_atomic_copy "$snapshot" "$target" || return 1
    elif [[ "$state" == "absent" ]]; then
      [[ ! -d "$target" || -L "$target" ]] || return 1
      rm -f -- "$target" || return 1
    else
      return 1
    fi
  done
  toolstore_txn_sync_file "$project"
}

toolstore_txn_restore_config_only() {
  local project_dir="$1" project content config_snapshot
  project="$(toolstore_txn_project_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project")" || return 1
  mkdir -p "$project" || return 1
  [[ -d "$project" && ! -L "$project" ]] || return 1
  config_snapshot="$(toolstore_txn_env_value "$content" CONFIG_SNAPSHOT)"
  [[ "$(toolstore_txn_sha256 "$config_snapshot")" == "$(toolstore_txn_env_value "$content" CONFIG_SHA256)" ]] || return 1
  toolstore_txn_atomic_copy "$config_snapshot" "${project}/.env" || return 1
  toolstore_txn_sync_file "$project"
}

toolstore_txn_stage_data() {
  local project_dir="$1" record content state data staged
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  [[ "$state" == "authoritative" ]] || return 1
  toolstore_txn_verify_backup "$project_dir" >/dev/null || return 1
  data="$(toolstore_txn_env_value "$content" DATA_DIR)"
  staged="$(toolstore_txn_env_value "$content" STAGED_DATA_PATH)"
  [[ -d "$data" && ! -L "$data" ]] || return 1
  toolstore_txn_path_has_no_symlinks "$data" || return 1
  [[ ! -e "$staged" && ! -L "$staged" ]] || return 1
  mv -T -- "$data" "$staged" || return 1
  toolstore_txn_sync_file "$(dirname -- "$data")" || return 1
  toolstore_txn_sync_file "$(dirname -- "$staged")" || return 1
  toolstore_txn_record_set "$record" STATE reinstall_staged || return 1
  toolstore_txn_fault_point after_data_stage || return $?
}

toolstore_txn_unstage_data() {
  local project_dir="$1" record content state data staged
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  [[ "$state" == "reinstall_staged" ]] || return 0
  data="$(toolstore_txn_env_value "$content" DATA_DIR)"
  staged="$(toolstore_txn_env_value "$content" STAGED_DATA_PATH)"
  mkdir -p "$(dirname -- "$data")" || return 1
  if [[ -e "$staged" || -L "$staged" ]]; then
    [[ -d "$staged" && ! -L "$staged" && ! -e "$data" && ! -L "$data" ]] || return 1
    mv -T -- "$staged" "$data" || return 1
    toolstore_txn_sync_file "$(dirname -- "$data")" || return 1
  else
    [[ -d "$data" && ! -L "$data" ]] || return 1
  fi
  toolstore_txn_record_set "$record" STATE authoritative || return 1
  toolstore_txn_fault_point after_data_unstage || return $?
}

toolstore_txn_restore_database() {
  local project_dir="$1" record content ctl backup metadata db request txn_dir archive temp
  local expected restored live schema old_schema live_hash restored_hash sidecar
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  expected="$(toolstore_txn_verify_backup "$project_dir")" || return 1
  ctl="$(toolstore_txn_env_value "$content" CTL_IMAGE)"
  backup="$(toolstore_txn_env_value "$content" BACKUP_PATH)"
  db="$(toolstore_txn_env_value "$content" DB_PATH)"
  request="$(toolstore_txn_env_value "$content" REQUEST_ID)"
  txn_dir="$(toolstore_txn_env_value "$content" TXN_DIR)"
  archive="$(toolstore_txn_env_value "$content" ARCHIVE_PATH)"
  temp="$(dirname -- "$db")/.$(basename -- "$db").restore-${request}"
  mkdir -p "$(dirname -- "$db")" || return 1
  toolstore_txn_path_has_no_symlinks "$(dirname -- "$db")" || return 1
  if [[ -e "$temp" || -L "$temp" ]]; then
    [[ -f "$temp" && ! -L "$temp" ]] || return 1
    restored="$(toolstore_txn_run_ctl "$ctl" verify "$temp")" || return 1
  else
    restored="$(toolstore_txn_run_ctl "$ctl" restore "$backup" "$temp")" || return 1
  fi
  toolstore_txn_metadata_valid "$restored" || return 1
  toolstore_txn_metadata_same_database_truth "$expected" "$restored" || return 1
  toolstore_txn_fault_point after_restore_temp || return $?

  if [[ -e "$db" || -L "$db" ]]; then
    [[ -f "$db" && ! -L "$db" ]] || return 1
    live="$(toolstore_txn_run_ctl "$ctl" verify "$db" 2>/dev/null || true)"
    live_hash="$(toolstore_txn_metadata_field "$live" sha256)"
    if [[ "$live_hash" == "$(toolstore_txn_metadata_field "$expected" sha256)" ]] &&
      toolstore_txn_metadata_same_database_truth "$expected" "$live"; then
      rm -f -- "$temp" || return 1
      toolstore_txn_record_set "$record" RESTORED_HASH "$live_hash" || return 1
      toolstore_txn_record_set "$record" STATE database_restored || return 1
      return 0
    fi
    [[ ! -e "$archive" && ! -L "$archive" ]] || return 1
    mv -T -- "$db" "$archive" || return 1
    for sidecar in -wal -shm; do
      if [[ -e "${db}${sidecar}" || -L "${db}${sidecar}" ]]; then
        [[ -f "${db}${sidecar}" && ! -L "${db}${sidecar}" && ! -e "${archive}${sidecar}" ]] || return 1
        mv -T -- "${db}${sidecar}" "${archive}${sidecar}" || return 1
      fi
    done
    toolstore_txn_sync_file "$(dirname -- "$archive")" || return 1
  elif [[ ! -e "$archive" ]]; then
    return 1
  fi
  toolstore_txn_fault_point after_live_archive || return $?
  [[ ! -e "$db" && ! -L "$db" && -f "$temp" && ! -L "$temp" ]] || return 1
  mv -T -- "$temp" "$db" || return 1
  chmod 600 "$db" || return 1
  toolstore_txn_sync_file "$db" || return 1
  toolstore_txn_sync_file "$(dirname -- "$db")" || return 1
  restored="$(toolstore_txn_run_ctl "$ctl" verify "$db")" || return 1
  toolstore_txn_metadata_same_database_truth "$expected" "$restored" || return 1
  restored_hash="$(toolstore_txn_metadata_field "$restored" sha256)"
  toolstore_txn_record_set "$record" RESTORED_HASH "$restored_hash" || return 1
  toolstore_txn_record_set "$record" STATE database_restored || return 1
  toolstore_txn_fault_point after_database_activate || return $?
}

toolstore_txn_verify_candidate() {
  local project_dir="$1" content expected ctl db txn_dir snapshot actual old_schema new_schema
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" STATE)" == "candidate_active" ]] || return 1
  expected="$(toolstore_txn_verify_backup "$project_dir")" || return 1
  ctl="$(toolstore_txn_env_value "$content" CTL_IMAGE)"
  db="$(toolstore_txn_env_value "$content" DB_PATH)"
  txn_dir="$(toolstore_txn_env_value "$content" TXN_DIR)"
  snapshot="${txn_dir}/candidate-check.db"
  if [[ -e "$snapshot" || -L "$snapshot" ]]; then
    [[ -f "$snapshot" && ! -L "$snapshot" ]] || return 1
    rm -f -- "$snapshot" || return 1
    toolstore_txn_sync_file "$txn_dir" || return 1
  fi
  actual="$(toolstore_txn_run_ctl "$ctl" backup "$db" "$snapshot")" || return 1
  toolstore_txn_mode_is_private "$snapshot" || return 1
  toolstore_txn_metadata_valid "$actual" || return 1
  old_schema="$(toolstore_txn_metadata_field "$expected" schema_version)"
  new_schema="$(toolstore_txn_metadata_field "$actual" schema_version)"
  (( new_schema >= old_schema )) || return 1
  toolstore_txn_core_counts_match "$expected" "$actual"
}

toolstore_txn_mark_rolled_back() {
  local project_dir="$1" record content
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" STATE)" == "database_restored" ]] || return 1
  toolstore_txn_record_set "$record" STATE rolled_back
}

# A preflight snapshot was taken while the old service could still accept
# writes, so it is evidence only and must never be promoted into a rollback
# anchor. Retire the pointer after the old service is healthy again.
toolstore_txn_retire_preflight() {
  local project_dir="$1" record content state root request history
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  [[ "$state" == "preparing" || "$state" == "preflight" ]] || return 1
  root="$(toolstore_txn_root "$project_dir")" || return 1
  request="$(toolstore_txn_env_value "$content" REQUEST_ID)"
  history="${root}/history/${request}.preflight.env"
  if [[ ! -e "$history" && ! -L "$history" ]]; then
    toolstore_txn_atomic_write "$history" "$content" || return 1
  fi
  rm -f -- "$record" || return 1
  toolstore_txn_sync_file "$root"
}

# A completed rollback anchor is evidence, not a reusable backup: the restored
# old service may have accepted new writes since recovery. Retire the active
# pointer without deleting any backup/config/database evidence so the next
# candidate must take a fresh Online Backup.
toolstore_txn_retire_rolled_back() {
  local project_dir="$1" record content root request history
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" STATE)" == "rolled_back" ]] || return 1
  root="$(toolstore_txn_root "$project_dir")" || return 1
  request="$(toolstore_txn_env_value "$content" REQUEST_ID)"
  history="${root}/history/${request}.rolled-back.env"
  if [[ ! -e "$history" && ! -L "$history" ]]; then
    toolstore_txn_atomic_write "$history" "$content" || return 1
  fi
  rm -f -- "$record" || return 1
  toolstore_txn_sync_file "$root"
}

toolstore_txn_finish_committed() {
  local project_dir="$1" record content root txn_dir request history path cleanup_ok=true
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  [[ "$(toolstore_txn_env_value "$content" STATE)" == "committed" ]] || return 1
  root="$(toolstore_txn_root "$project_dir")" || return 1
  txn_dir="$(toolstore_txn_env_value "$content" TXN_DIR)"
  request="$(toolstore_txn_env_value "$content" REQUEST_ID)"
  history="${root}/history/${request}.env"
  if [[ ! -e "$history" && ! -L "$history" ]]; then
    toolstore_txn_atomic_write "$history" "$content" || return 1
  fi
  for path in preflight.db preflight.metadata.json backup.db backup.metadata.json candidate-check.db config.env docker-compose.yml docker-compose.host.yml docker-compose.logdb.yml toolstore_transaction.sh; do
    if [[ -e "${txn_dir}/${path}" || -L "${txn_dir}/${path}" ]]; then
      [[ ! -d "${txn_dir}/${path}" || -L "${txn_dir}/${path}" ]] || continue
      if ! rm -f -- "${txn_dir}/${path}"; then
        toolstore_txn_warn "cannot clean committed anchor ${txn_dir}/${path}"
        cleanup_ok=false
      fi
    fi
  done
  if ! toolstore_txn_sync_file "$txn_dir"; then
    toolstore_txn_warn "cannot sync committed transaction evidence directory"
    cleanup_ok=false
  fi
  if [[ "$cleanup_ok" != "true" ]]; then
    # STATE=committed is the durable commit point. Keep active.env so the next
    # invocation retries bounded cleanup without ever rolling back the healthy
    # candidate.
    return 0
  fi
  rm -f -- "$record" || return 1
  toolstore_txn_sync_file "$root" || toolstore_txn_warn "commit point reached but transaction root fsync failed"
  if [[ -e "${root}/recovery-library.sh" || -L "${root}/recovery-library.sh" ]]; then
    rm -f -- "${root}/recovery-library.sh" || toolstore_txn_warn "cannot clean committed recovery library"
  fi
}

toolstore_txn_commit() {
  local project_dir="$1" record content state
  record="$(toolstore_txn_active_record_path "$project_dir")" || return 1
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  [[ "$state" == "candidate_active" ]] || return 1
  toolstore_txn_verify_candidate "$project_dir" || return 1
  toolstore_txn_fault_point before_commit || return $?
  toolstore_txn_record_set "$record" COMMITTED_AT "$(date -u +%Y-%m-%dT%H:%M:%SZ)" || return 1
  toolstore_txn_record_set "$record" STATE committed || return 1
  toolstore_txn_fault_point after_commit_marker || return $?
}
