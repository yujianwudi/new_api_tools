#!/usr/bin/env bash
set -euo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${TEST_DIR}/.." && pwd)"

# deploy.sh guards main(), so its parser helpers can be tested without Docker.
# shellcheck source=../deploy.sh
source "${REPO_ROOT}/deploy.sh"

failures=0

assert_eq() {
  local description="$1" expected="$2" actual
  shift 2
  if ! actual="$("$@")"; then
    printf 'not ok - %s (command failed)\n' "$description" >&2
    failures=$((failures + 1))
  elif [[ "$actual" != "$expected" ]]; then
    printf 'not ok - %s (expected %q, got %q)\n' "$description" "$expected" "$actual" >&2
    failures=$((failures + 1))
  else
    printf 'ok - %s\n' "$description"
  fi
}

assert_rejected() {
  local description="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    printf 'not ok - %s (input was accepted)\n' "$description" >&2
    failures=$((failures + 1))
  else
    printf 'ok - %s\n' "$description"
  fi
}

safe_url='postgresql://alice:secret@db.internal:5432/newapi?sslmode=require&application_name=newapi_tools'
assert_eq 'safe PostgreSQL URL keeps authority host' 'db.internal' extract_dsn_host "$safe_url"
assert_eq 'safe PostgreSQL URL keeps authority port' '5432' extract_dsn_port "$safe_url"
assert_eq \
  'safe PostgreSQL URL rewrite preserves non-routing query options' \
  'postgresql://alice:secret@postgres:6432/newapi?sslmode=require&application_name=newapi_tools' \
  rewrite_dsn_host_port "$safe_url" 'postgres' '6432'

assert_rejected \
  'PostgreSQL URL query host cannot override rewritten authority' \
  detect_dsn_format 'postgresql://alice:secret@db.internal:5432/newapi?host=attacker.internal'
assert_rejected \
  'PostgreSQL URL query hostaddr cannot bypass host rewrite' \
  detect_dsn_format 'postgres://alice:secret@db.internal:5432/newapi?sslmode=require&hostaddr=203.0.113.10'
assert_rejected \
  'PostgreSQL URL query port is rejected case-insensitively' \
  detect_dsn_format 'postgresql://alice:secret@db.internal:5432/newapi?PORT=6543'
assert_rejected \
  'percent-encoded PostgreSQL routing key is rejected fail-closed' \
  detect_dsn_format 'postgresql://alice:secret@db.internal:5432/newapi?h%6fst=attacker.internal'
assert_rejected \
  'rewrite also rejects PostgreSQL URL target overrides' \
  rewrite_dsn_host_port 'postgresql://alice:secret@db.internal:5432/newapi?host=attacker.internal' 'postgres' '5432'

safe_keyword='host=db.internal port=5432 user=alice password=secret dbname=newapi sslmode=require'
assert_eq \
  'safe PostgreSQL keyword DSN rewrites host and port' \
  'host=postgres port=6432 user=alice password=secret dbname=newapi sslmode=require' \
  rewrite_dsn_host_port "$safe_keyword" 'postgres' '6432'
assert_rejected \
  'PostgreSQL keyword hostaddr cannot bypass host rewrite' \
  detect_dsn_format 'host=db.internal hostaddr=203.0.113.10 port=5432 dbname=newapi'
assert_rejected \
  'PostgreSQL keyword hostaddr is rejected case-insensitively' \
  detect_dsn_format 'host=db.internal HOSTADDR=203.0.113.10 port=5432 dbname=newapi'

safe_mysql_url='mysql://alice:secret@db.internal:3307/newapi?charset=utf8mb4&parseTime=true'
assert_eq 'MySQL URL extracts custom port' '3307' extract_dsn_port "$safe_mysql_url"
assert_eq \
  'MySQL URL rewrite preserves query options' \
  'mysql://alice:secret@mysql.internal:4407/newapi?charset=utf8mb4&parseTime=true' \
  rewrite_dsn_host_port "$safe_mysql_url" 'mysql.internal' '4407'

safe_mysql_go='alice:secret@tcp(db.internal:3308)/newapi?charset=utf8mb4&parseTime=True&loc=Local'
assert_eq 'MySQL Go DSN extracts host' 'db.internal' extract_dsn_host "$safe_mysql_go"
assert_eq 'MySQL Go DSN extracts custom port' '3308' extract_dsn_port "$safe_mysql_go"
assert_eq \
  'MySQL Go DSN rewrite preserves query options' \
  'alice:secret@tcp(mysql.internal:4408)/newapi?charset=utf8mb4&parseTime=True&loc=Local' \
  rewrite_dsn_host_port "$safe_mysql_go" 'mysql.internal' '4408'

safe_ipv6_url='postgresql://alice:secret@[2001:db8::10]:6543/newapi?sslmode=require'
assert_eq 'bracketed IPv6 URL extracts host' '2001:db8::10' extract_dsn_host "$safe_ipv6_url"
assert_eq 'bracketed IPv6 URL extracts custom port' '6543' extract_dsn_port "$safe_ipv6_url"
assert_eq \
  'IPv6 URL rewrite retains brackets and query options' \
  'postgresql://alice:secret@[2001:db8::20]:7654/newapi?sslmode=require' \
  rewrite_dsn_host_port "$safe_ipv6_url" '2001:db8::20' '7654'

detected_port_for_network() (
  local network_mode="$1" network="$2" dsn="$3" expected_db_service="$4"

  log_info() { :; }
  log_success() { :; }
  log_warn() { :; }
  detect_newapi_redis_mutation_safety() { NEWAPI_REDIS_DISABLED=false; }
  detect_networks_for_container() { printf '%s\n' "$network"; }
  container_is_on_network() { return 0; }
  docker_inspect_label() {
    case "$2" in
      com.docker.compose.project) printf 'test-project\n' ;;
      com.docker.compose.project.config_files) printf '\n' ;;
    esac
  }
  docker_inspect_env_value() {
    case "$2" in
      SQL_DSN) printf '%s\n' "$dsn" ;;
      POSTGRES_USER|MYSQL_USER) printf 'alice\n' ;;
      POSTGRES_DB|MYSQL_DATABASE) printf 'newapi\n' ;;
      POSTGRES_PASSWORD|MYSQL_PASSWORD) printf 'secret\n' ;;
    esac
  }
  detect_db_container_by_compose_service() {
    if [[ "$2" == "$expected_db_service" ]]; then
      printf 'database-container\n'
    fi
    return 0
  }
  docker() {
    if [[ "${1:-}" == 'inspect' && "${2:-}" == '-f' && "${3:-}" == '{{.HostConfig.NetworkMode}}' ]]; then
      printf '%s\n' "$network_mode"
    fi
    return 0
  }

  NEWAPI_CONTAINER='new-api-test'
  NEWAPI_NETWORK=''
  COMPOSE_FILE_OVERRIDE=''
  detect_environment
  printf '%s\n' "$DB_PORT"
)

assert_eq \
  'custom-network deployment preserves PostgreSQL DSN custom port' \
  '6432' \
  detected_port_for_network 'custom-network' 'custom-network' \
  'postgresql://alice:secret@db.internal:6432/newapi?sslmode=require' 'postgres'
assert_eq \
  'default-bridge deployment preserves MySQL DSN custom port' \
  '4407' \
  detected_port_for_network 'bridge' 'bridge' \
  'alice:secret@tcp(172.17.0.3:4407)/newapi?charset=utf8mb4&parseTime=True' 'mysql'

deploy_host_overlay() (
  COMPOSE_HOST_FILE="$1"
  DOCKER_COMPOSE='docker compose'
  DOCKER_COMPOSE_V2_VERSION='2.24.0'
  configure_host_compose_files
  printf '%s\n' "${COMPOSE_FILES[*]}"
)

missing_overlay="${TEST_DIR}/missing-docker-compose.host.yml"
assert_rejected \
  'deploy host mode rejects a missing Compose overlay' \
  deploy_host_overlay "$missing_overlay"
assert_eq \
  'deploy host mode includes the required Compose overlay' \
  "-f ${REPO_ROOT}/docker-compose.yml -f ${REPO_ROOT}/docker-compose.host.yml" \
  deploy_host_overlay "${REPO_ROOT}/docker-compose.host.yml"

install_host_compose_files() (
  local project_dir="$1"
  # shellcheck source=../install.sh
  source "${REPO_ROOT}/install.sh"
  DOCKER_COMPOSE='docker compose'
  DOCKER_COMPOSE_V2_VERSION='2.24.0'
  setup_compose_files "$project_dir"
  printf '%s\n' "${COMPOSE_FILE:-}"
)

install_fixture="$(mktemp -d)"
trap 'rm -rf "$install_fixture"' EXIT
printf 'NEWAPI_NETWORK=\n' > "${install_fixture}/.env"
: > "${install_fixture}/docker-compose.yml"
assert_rejected \
  'install host-mode update rejects a missing Compose overlay' \
  install_host_compose_files "$install_fixture"
: > "${install_fixture}/docker-compose.host.yml"
assert_eq \
  'install host-mode update includes the required Compose overlay' \
  "${install_fixture}/docker-compose.yml:${install_fixture}/docker-compose.host.yml" \
  install_host_compose_files "$install_fixture"

# shellcheck source=../install.sh
source "${REPO_ROOT}/install.sh"

resolve_install_image_in_subshell() (
  resolve_install_image "$@"
)

test_commit='0123456789abcdef0123456789abcdef01234567'
assert_eq \
  'release ref selects the matching semver image tag' \
  'ghcr.io/yujianwudi/new_api_tools:0.2.0' \
  resolve_install_image_in_subshell 'v0.2.0' "$test_commit" ''
assert_eq \
  'main ref selects the current seven-character commit tag' \
  'ghcr.io/yujianwudi/new_api_tools:0123456' \
  resolve_install_image_in_subshell 'main' "$test_commit" ''
assert_rejected \
  'custom ref without an explicit image fails closed' \
  resolve_install_image_in_subshell 'feature/test' "$test_commit" ''
assert_eq \
  'explicit digest overrides a custom ref' \
  'ghcr.io/example/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  resolve_install_image_in_subshell 'feature/test' "$test_commit" \
  'ghcr.io/example/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
assert_rejected \
  'image references containing whitespace are rejected' \
  resolve_install_image_in_subshell 'main' "$test_commit" 'ghcr.io/example/new_api_tools:bad value'

checkout_commit_for_ref() (
  local ref="$1"
  local remote_commit='1111111111111111111111111111111111111111'
  local tag_commit='2222222222222222222222222222222222222222'

  log_success() { :; }
  git() {
    case "${1:-}" in
      fetch)
        return 0
        ;;
      show-ref)
        case "${4:-}" in
          "refs/tags/${ref}"|"refs/remotes/origin/${ref}") return 0 ;;
        esac
        return 1
        ;;
      rev-parse)
        case "${3:-}" in
          "refs/tags/${ref}^{commit}") printf '%s\n' "$tag_commit" ;;
          "refs/remotes/origin/${ref}^{commit}") printf '%s\n' "$remote_commit" ;;
          *) return 1 ;;
        esac
        ;;
      reset)
        return 0
        ;;
      *)
        return 1
        ;;
    esac
  }

  INSTALL_REF="$ref"
  REQUESTED_NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:test'
  checkout_install_ref
  printf '%s\n' "$INSTALL_COMMIT"
)

assert_eq \
  'main always resolves the fetched origin/main even when a local main tag exists' \
  '1111111111111111111111111111111111111111' \
  checkout_commit_for_ref 'main'
assert_eq \
  'a non-semver ref prefers the fetched remote branch over a colliding tag' \
  '1111111111111111111111111111111111111111' \
  checkout_commit_for_ref 'feature/test'
assert_eq \
  'a semver release ref prefers its immutable tag over a colliding branch' \
  '2222222222222222222222222222222222222222' \
  checkout_commit_for_ref 'v0.2.0'

image_env_migration_result() (
  local fixture env_file before
  log_info() { :; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'FOO=bar\nNEWAPI_TOOLS_VERSION=0.1.0\n' > "$env_file"

  migrate_image_env_file "$env_file" ''
  before="$(cksum < "$env_file")"
  migrate_image_env_file "$env_file" ''
  [[ "$(cksum < "$env_file")" == "$before" ]] || return 1

  printf '%s|%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')" \
    "$(grep -c '^NEWAPI_TOOLS_VERSION=' "$env_file" || true)" \
    "$(stat -c '%a' "$env_file")"
)

assert_eq \
  'legacy image version migrates once to a full image reference with mode 600' \
  'ghcr.io/yujianwudi/new_api_tools:0.1.0|0|600' \
  image_env_migration_result

selected_image_overrides_legacy() (
  local fixture env_file
  log_info() { :; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_VERSION=0.1.0\n' > "$env_file"
  migrate_image_env_file "$env_file" 'ghcr.io/yujianwudi/new_api_tools:0123456'
  env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE'
)

assert_eq \
  'the image resolved for the current install ref overrides a legacy version' \
  'ghcr.io/yujianwudi/new_api_tools:0123456' \
  selected_image_overrides_legacy

deploy_start_order() (
  local order_file
  order_file="$(mktemp)"
  trap 'rm -f "$order_file"' EXIT

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s\n' "${args[i + 1]:-}" >> "$order_file"
          return 0
          ;;
        down|up)
          printf '%s\n' "${args[i]}" >> "$order_file"
          return 0
          ;;
      esac
    done
    return 0
  }
  docker() {
    if [[ "${1:-}" == 'ps' ]]; then
      printf 'newapi-tools\n'
    fi
  }

  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILES=(-f "$REPO_ROOT/docker-compose.yml")
  ENV_FILE="${install_fixture}/.env"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.2.0'
  USE_HOST_MODE=true
  USE_BRIDGE_MODE=false
  LOG_NETWORK=''
  FRONTEND_BIND='127.0.0.1'
  FRONTEND_PORT='1145'
  ADMIN_PASSWORD='test-password'
  AUTO_GENERATED_PASSWORD=false

  start_services >/dev/null
  paste -sd, "$order_file"
)

assert_eq \
  'deployment pulls only newapi-tools before stopping the old container' \
  'pull:newapi-tools,down,up' \
  deploy_start_order

deploy_pull_failure_actions() (
  local order_file status
  order_file="$(mktemp)"
  trap 'rm -f "$order_file"' EXIT

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s\n' "${args[i + 1]:-}" >> "$order_file"
          return 23
          ;;
        down|up)
          printf '%s\n' "${args[i]}" >> "$order_file"
          return 0
          ;;
      esac
    done
    return 0
  }
  docker() {
    if [[ "${1:-}" == 'ps' ]]; then
      printf 'newapi-tools\n'
    fi
  }

  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILES=(-f "$REPO_ROOT/docker-compose.yml")
  ENV_FILE="${install_fixture}/.env"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.2.0'

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(paste -sd, "$order_file")"
)

assert_eq \
  'deployment leaves the old container running when the targeted pull fails' \
  '1|pull:newapi-tools' \
  deploy_pull_failure_actions

install_quick_update_actions() (
  local pull_status="$1"
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  printf 'FRONTEND_PORT=1145\n' > "${fixture}/.env"
  : > "${fixture}/docker-compose.yml"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  check_and_update_configs() { :; }
  migrate_env_file() { :; }
  sync_newapi_mutation_safety_config() { :; }
  check_server_host_security() { :; }
  download_geoip_database() { :; }
  setup_compose_files() { COMPOSE_FILE=''; }
  restore_runtime_network_connections() { :; }
  detect_env_details() { :; }
  show_frontend_access() { :; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s\n' "${args[i + 1]:-}" >> "$order_file"
          return "$pull_status"
          ;;
        down|up)
          printf '%s\n' "${args[i]}" >> "$order_file"
          return 0
          ;;
      esac
    done
    return 0
  }

  PROJECT_DIR="$fixture"
  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILE=''
  ENV_FRONTEND_BIND='127.0.0.1'

  set +e
  (quick_update >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(paste -sd, "$order_file")"
)

assert_eq \
  'quick update pulls only newapi-tools before restarting services' \
  '0|pull:newapi-tools,down,up' \
  install_quick_update_actions 0
assert_eq \
  'quick update never stops services after a targeted pull failure' \
  '1|pull:newapi-tools' \
  install_quick_update_actions 23

if (( failures > 0 )); then
  printf '%d deployment test(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'all deployment tests passed\n'
