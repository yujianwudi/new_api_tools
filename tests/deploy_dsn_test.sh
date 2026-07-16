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

assert_eq 'NewAPI PORT accepts the lowest TCP port' 'ok' \
  bash -c 'source "$1"; is_valid_tcp_port 1 && printf ok' _ "${REPO_ROOT}/deploy.sh"
assert_eq 'NewAPI PORT accepts the highest TCP port' 'ok' \
  bash -c 'source "$1"; is_valid_tcp_port 65535 && printf ok' _ "${REPO_ROOT}/deploy.sh"
assert_rejected 'NewAPI PORT rejects zero' is_valid_tcp_port 0
assert_rejected 'NewAPI PORT rejects values above 65535' is_valid_tcp_port 65536
assert_rejected 'NewAPI PORT rejects non-decimal input' is_valid_tcp_port '3000/tcp'

fake_compose_version() {
  local mode="$1"
  shift
  [[ "${1:-} ${2:-}" == 'compose version' ]] || return 1
  case "$mode" in
    v224)
      [[ "${3:-}" == '--short' ]] && printf '2.24.0\n' || printf 'Docker Compose version v2.24.0\n'
      ;;
    v223)
      [[ "${3:-}" == '--short' ]] && printf '2.23.3\n' || printf 'Docker Compose version v2.23.3\n'
      ;;
    *) return 1 ;;
  esac
}

deploy_compose_detection() (
  local mode="$1"
  log_warn() { :; }
  docker() { fake_compose_version "$mode" "$@"; }
  detect_docker_compose
  printf '%s|%s\n' "$DOCKER_COMPOSE" "$DOCKER_COMPOSE_V2_VERSION"
)

assert_eq 'deploy requires Docker Compose v2.24 or newer' 'docker compose|2.24.0' \
  deploy_compose_detection v224
assert_rejected 'deploy rejects Docker Compose older than v2.24' deploy_compose_detection v223
assert_rejected 'deploy rejects legacy docker-compose v1' deploy_compose_detection legacy

install_compose_detection() (
  local mode="$1"
  # shellcheck source=../install.sh
  source "${REPO_ROOT}/install.sh"
  log_warn() { :; }
  log_success() { :; }
  command() {
    if [[ "${1:-}" == '-v' ]]; then
        case "${2:-}" in git|docker|flock|id|sha256sum|stat|sync|docker-compose) return 0 ;; esac
    fi
    builtin command "$@"
  }
  docker() { fake_compose_version "$mode" "$@"; }
  check_requirements
  printf '%s|%s\n' "$DOCKER_COMPOSE" "$DOCKER_COMPOSE_V2_VERSION"
)

assert_eq 'installer requires Docker Compose v2.24 or newer' 'docker compose|2.24.0' \
  install_compose_detection v224
assert_rejected 'installer rejects Docker Compose older than v2.24' install_compose_detection v223
assert_rejected 'installer rejects legacy docker-compose v1' install_compose_detection legacy

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

# Most unit fixtures below intentionally have no running installation. Tests
# that exercise a real/existing-container branch override this helper locally.
list_install_container_names() { :; }

resolve_install_image_in_subshell() (
  resolve_install_image "$@"
)

test_commit='0123456789abcdef0123456789abcdef01234567'
assert_eq \
  'release ref accepts an explicit immutable manifest digest' \
  'ghcr.io/yujianwudi/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  resolve_install_image_in_subshell 'v0.2.0' "$test_commit" \
  'ghcr.io/yujianwudi/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
assert_rejected \
  'release ref without an explicit manifest digest fails closed' \
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
  'ghcr.io/yujianwudi/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' \
  resolve_install_image_in_subshell 'feature/test' "$test_commit" \
  'ghcr.io/yujianwudi/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
assert_rejected \
  'explicit install digest from an untrusted repository is rejected' \
  resolve_install_image_in_subshell 'feature/test' "$test_commit" \
  'ghcr.io/example/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
assert_rejected \
  'image references containing whitespace are rejected' \
  resolve_install_image_in_subshell 'main' "$test_commit" 'ghcr.io/example/new_api_tools:bad value'

resolved_test_image='ghcr.io/yujianwudi/new_api_tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
previous_test_image='ghcr.io/yujianwudi/new_api_tools@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'

fake_repo_digest_inspect() {
  local digest="$1"
  shift
  if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' && "${3:-}" == '--format' && "${4:-}" == *RepoDigests* ]]; then
    [[ -n "$digest" ]] && printf '%s\n' "$digest"
    return 0
  fi
  return 1
}

resolve_install_digest_fixture() (
  local revision="$1"
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' ]]; then
      case "${4:-}" in
        *RepoDigests*) printf '%s\n' "$resolved_test_image" ;;
        *org.opencontainers.image.revision*) printf '%s\n' "$revision" ;;
        *) return 1 ;;
      esac
      return 0
    fi
    return 1
  }
  resolve_install_image_digest 'ghcr.io/yujianwudi/new_api_tools:0123456' "$test_commit"
)

assert_eq \
  'derived install tag resolves to an immutable digest and validates its source revision' \
  "$resolved_test_image" \
  resolve_install_digest_fixture "$test_commit"
assert_rejected \
  'derived install tag rejects a mismatched OCI source revision' \
  resolve_install_digest_fixture 'ffffffffffffffffffffffffffffffffffffffff'

preserve_explicit_install_digest() (
  docker() { return 99; }
  resolve_install_image_digest "$resolved_test_image" ''
)

assert_eq \
  'explicit install digest without an expected revision avoids mutable-tag resolution' \
  "$resolved_test_image" \
  preserve_explicit_install_digest

resolve_explicit_install_digest_fixture() (
  local revision="$1"
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' && "${4:-}" == *org.opencontainers.image.revision* ]]; then
      printf '%s\n' "$revision"
      return 0
    fi
    return 1
  }
  resolve_install_image_digest "$resolved_test_image" "$test_commit"
)

assert_eq \
  'explicit install digest validates the expected OCI source revision' \
  "$resolved_test_image" \
  resolve_explicit_install_digest_fixture "$test_commit"
assert_rejected \
  'explicit install digest rejects a mismatched expected OCI source revision' \
  resolve_explicit_install_digest_fixture 'ffffffffffffffffffffffffffffffffffffffff'

resolve_deploy_digest_fixture() (
  local revision="$1"
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' ]]; then
      case "${4:-}" in
        *RepoDigests*) printf '%s\n' "$resolved_test_image" ;;
        *org.opencontainers.image.revision*) printf '%s\n' "$revision" ;;
        *) return 1 ;;
      esac
      return 0
    fi
    return 1
  }
  resolve_deploy_image_digest 'ghcr.io/yujianwudi/new_api_tools:0.2.0' "$test_commit"
)

assert_eq \
  'derived deploy tag resolves to an immutable digest and validates its source revision' \
  "$resolved_test_image" \
  resolve_deploy_digest_fixture "$test_commit"
assert_rejected \
  'derived deploy tag rejects a mismatched OCI source revision' \
  resolve_deploy_digest_fixture 'ffffffffffffffffffffffffffffffffffffffff'

resolve_explicit_deploy_digest_fixture() (
  local revision="$1"
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' && "${4:-}" == *org.opencontainers.image.revision* ]]; then
      printf '%s\n' "$revision"
      return 0
    fi
    return 1
  }
  resolve_deploy_image_digest "$resolved_test_image" "$test_commit"
)

assert_eq \
  'explicit deploy digest validates the expected OCI source revision' \
  "$resolved_test_image" \
  resolve_explicit_deploy_digest_fixture "$test_commit"
assert_rejected \
  'explicit deploy digest rejects a mismatched expected OCI source revision' \
  resolve_explicit_deploy_digest_fixture 'ffffffffffffffffffffffffffffffffffffffff'

resolve_deploy_identity_fixture() (
  local mode="$1" exact_tag=''
  log_info() { :; }
  git() {
    case "${3:-}" in
      rev-parse)
        if [[ "${4:-}" == '--is-inside-work-tree' ]]; then return 0; fi
        if [[ "${4:-}" == '--verify' && "${5:-}" == 'HEAD' ]]; then
          printf '%s\n' "$test_commit"
          return 0
        fi
        ;;
      describe)
        [[ -n "$exact_tag" ]] && printf '%s\n' "$exact_tag"
        return 0
        ;;
    esac
    return 1
  }

  REQUESTED_NEWAPI_TOOLS_IMAGE=''
  REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION=''
  case "$mode" in
    explicit-release)
      REQUESTED_NEWAPI_TOOLS_IMAGE="$resolved_test_image"
      REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="$test_commit"
      ;;
    explicit-missing-revision)
      REQUESTED_NEWAPI_TOOLS_IMAGE="$resolved_test_image"
      ;;
    explicit-mutable)
      REQUESTED_NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.0'
      REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="$test_commit"
      ;;
    explicit-untrusted)
      REQUESTED_NEWAPI_TOOLS_IMAGE='ghcr.io/example/new_api_tools@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
      REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="$test_commit"
      ;;
    tag-checkout)
      exact_tag='v0.5.0'
      ;;
  esac

  resolve_deploy_image
  printf '%s|%s|%s\n' "$NEWAPI_TOOLS_IMAGE" "$NEWAPI_TOOLS_EXPECTED_REVISION" "$NEWAPI_TOOLS_IMAGE_DERIVED"
)

assert_eq \
  'fresh release deploy preserves explicit digest and expected revision from install' \
  "${resolved_test_image}|${test_commit}|false" \
  resolve_deploy_identity_fixture explicit-release
assert_eq \
  'development checkout derives the short-SHA tag and revision together' \
  "ghcr.io/yujianwudi/new_api_tools:0123456|${test_commit}|true" \
  resolve_deploy_identity_fixture derived-development
assert_rejected \
  'explicit deploy digest without expected revision fails closed' \
  resolve_deploy_identity_fixture explicit-missing-revision
assert_rejected \
  'explicit deploy mutable tag fails closed' \
  resolve_deploy_identity_fixture explicit-mutable
assert_rejected \
  'explicit deploy image from an untrusted repository fails closed' \
  resolve_deploy_identity_fixture explicit-untrusted
assert_rejected \
  'release tag checkout without explicit digest and revision fails closed' \
  resolve_deploy_identity_fixture tag-checkout

resolve_deploy_digest_candidates_fixture() (
  local mode="$1"
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' ]]; then
      case "${4:-}" in
        *RepoDigests*)
          case "$mode" in
            exact)
              printf '%s\n' \
                'ghcr.io/example/other@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc' \
                "$resolved_test_image"
              ;;
            none)
              printf '%s\n' 'ghcr.io/example/other@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'
              ;;
            multiple)
              printf '%s\n' \
                "$resolved_test_image" \
                'ghcr.io/yujianwudi/new_api_tools@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd'
              ;;
          esac
          ;;
        *org.opencontainers.image.revision*) printf '%s\n' "$test_commit" ;;
        *) return 1 ;;
      esac
      return 0
    fi
    return 1
  }
  resolve_deploy_image_digest 'ghcr.io/yujianwudi/new_api_tools:0.2.0' "$test_commit"
)

assert_eq \
  'deploy digest resolution ignores RepoDigests belonging to other repositories' \
  "$resolved_test_image" \
  resolve_deploy_digest_candidates_fixture exact
assert_rejected \
  'deploy digest resolution rejects zero target-repository matches' \
  resolve_deploy_digest_candidates_fixture none
assert_rejected \
  'deploy digest resolution rejects multiple target-repository matches' \
  resolve_deploy_digest_candidates_fixture multiple

migrate_newapi_baseurl_fixture() (
  local mode="$1" fixture env_file
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  case "$mode" in
    duplicate)
      printf '%s\n' \
        'NEWAPI_BASEURL=http://stale.example:3000' \
        'NEWAPI_BASEURL=' \
        'NEWAPI_CONTAINER=new-api-test' \
        'NEWAPI_NETWORK_MODE=bridge' > "$env_file"
      ;;
    whitespace)
      printf '%s\n' \
        "NEWAPI_BASEURL='   '" \
        'NEWAPI_CONTAINER=new-api-test' \
        'NEWAPI_NETWORK_MODE=bridge' > "$env_file"
      ;;
    *)
      printf '%s\n' \
        'NEWAPI_BASEURL=' \
        'NEWAPI_CONTAINER=new-api-test' \
        'NEWAPI_NETWORK_MODE=bridge' > "$env_file"
      ;;
  esac

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  migrate_image_env_file() { :; }
  openssl() { printf 'eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee\n'; }
  docker() {
    if [[ "$mode" == 'detected' && "${1:-}" == 'inspect' ]]; then
      printf 'PORT=3100\n'
      return 0
    fi
    return 1
  }

  migrate_env_file "$fixture"
  printf '%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_BASEURL')" \
    "$(grep -c '^NEWAPI_BASEURL=' "$env_file" || true)"
)

assert_eq \
  'an empty NEWAPI_BASEURL is replaced when the NewAPI endpoint can be detected' \
  'http://new-api-test:3100|1' \
  migrate_newapi_baseurl_fixture detected
assert_eq \
  'an undetectable NEWAPI_BASEURL is not persisted as an empty configured value' \
  '|0' \
  migrate_newapi_baseurl_fixture unavailable
assert_eq \
  'the last duplicate NEWAPI_BASEURL value controls migration and empty duplicates are removed' \
  '|0' \
  migrate_newapi_baseurl_fixture duplicate
assert_eq \
  'a whitespace-only NEWAPI_BASEURL is not treated as configured' \
  '|0' \
  migrate_newapi_baseurl_fixture whitespace

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
      remote)
        if [[ "${2:-}" == 'get-url' && "${3:-}" == 'origin' ]]; then
          printf '%s\n' 'https://github.com/yujianwudi/new_api_tools.git'
          return 0
        fi
        return 1
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
  REQUESTED_NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  if [[ "$ref" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="$tag_commit"
  else
    REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="$remote_commit"
  fi
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

checkout_release_identity_rejected() (
  local mode="$1" tag_commit='2222222222222222222222222222222222222222'
  log_success() { :; }
  git() {
    case "${1:-}" in
      fetch|reset) return 0 ;;
      remote)
        printf '%s\n' 'https://github.com/yujianwudi/new_api_tools.git'
        ;;
      show-ref)
        [[ "${4:-}" == 'refs/tags/v0.2.0' ]]
        ;;
      rev-parse)
        printf '%s\n' "$tag_commit"
        ;;
      *) return 1 ;;
    esac
  }

  INSTALL_REF='v0.2.0'
  REQUESTED_NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="$tag_commit"
  case "$mode" in
    missing-image) REQUESTED_NEWAPI_TOOLS_IMAGE='' ;;
    mutable-image) REQUESTED_NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.2.0' ;;
    missing-revision) REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION='' ;;
    mismatched-revision) REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION='ffffffffffffffffffffffffffffffffffffffff' ;;
  esac
  checkout_install_ref
)

assert_rejected \
  'release checkout requires an explicit immutable manifest digest' \
  checkout_release_identity_rejected missing-image
assert_rejected \
  'release checkout rejects a mutable image tag' \
  checkout_release_identity_rejected mutable-image
assert_rejected \
  'release checkout requires an expected release commit' \
  checkout_release_identity_rejected missing-revision
assert_rejected \
  'release checkout rejects a moved tag target' \
  checkout_release_identity_rejected mismatched-revision

verify_untrusted_install_origin() (
  git() {
    if [[ "${1:-}" == 'remote' && "${2:-}" == 'get-url' ]]; then
      printf '%s\n' 'https://github.com/attacker/new_api_tools.git'
      return 0
    fi
    return 1
  }
  verify_install_origin
)

assert_rejected \
  'an existing installation with an untrusted origin is rejected before update' \
  verify_untrusted_install_origin

checkout_missing_release_tag() (
  log_success() { :; }
  git() {
    case "${1:-}" in
      remote)
        printf '%s\n' 'https://github.com/yujianwudi/new_api_tools.git'
        ;;
      fetch)
        return 0
        ;;
      show-ref)
        return 1
        ;;
      *)
        return 1
        ;;
    esac
  }
  INSTALL_REF='v9.9.9'
  checkout_install_ref
)

assert_rejected \
  'a release ref without the immutable tag never falls back to a same-name branch' \
  checkout_missing_release_tag

image_env_migration_result() (
  local fixture env_file before
  log_info() { :; }
  docker() { fake_repo_digest_inspect "$previous_test_image" "$@"; }
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
  "${previous_test_image}|0|600" \
  image_env_migration_result

legacy_image_waits_for_candidate_validation() (
  local fixture env_file
  log_info() { :; }
  docker() { fake_repo_digest_inspect "$previous_test_image" "$@"; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_VERSION=0.1.0\n' > "$env_file"
  migrate_image_env_file "$env_file" 'ghcr.io/yujianwudi/new_api_tools:0123456'
  printf '%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')" \
    "$NEWAPI_TOOLS_IMAGE"
)

assert_eq \
  'legacy image migration anchors the installed tag while exporting the update candidate' \
  "${previous_test_image}|ghcr.io/yujianwudi/new_api_tools:0123456" \
  legacy_image_waits_for_candidate_validation

verified_image_replaces_legacy() (
  local fixture env_file
  log_info() { :; }
  docker() { fake_repo_digest_inspect "$previous_test_image" "$@"; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_VERSION=0.1.0\n' > "$env_file"

  migrate_image_env_file "$env_file" 'ghcr.io/yujianwudi/new_api_tools:0123456'
  migrate_image_env_file "$env_file" "$resolved_test_image" true
  printf '%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')" \
    "$(grep -c '^NEWAPI_TOOLS_VERSION=' "$env_file" || true)"
)

assert_eq \
  'a verified immutable image replaces the migrated legacy tag' \
  "${resolved_test_image}|0" \
  verified_image_replaces_legacy

mutable_image_survives_mutable_migration() (
  local fixture env_file current_image='ghcr.io/yujianwudi/new_api_tools:0.2.0'
  local requested_image='ghcr.io/yujianwudi/new_api_tools:0.5.0'
  log_info() { :; }
  docker() { fake_repo_digest_inspect "$previous_test_image" "$@"; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$current_image" > "$env_file"

  migrate_image_env_file "$env_file" "$requested_image"
  printf '%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')" \
    "$NEWAPI_TOOLS_IMAGE"
)

assert_eq \
  'a mutable installed image is anchored before candidate pull validation' \
  "${previous_test_image}|ghcr.io/yujianwudi/new_api_tools:0.5.0" \
  mutable_image_survives_mutable_migration

same_mutable_image_is_anchored() (
  local fixture env_file current_image='ghcr.io/yujianwudi/new_api_tools:0.2.0'
  log_info() { :; }
  docker() { fake_repo_digest_inspect "$previous_test_image" "$@"; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$current_image" > "$env_file"

  migrate_image_env_file "$env_file" "$current_image"
  printf '%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')" \
    "$NEWAPI_TOOLS_IMAGE"
)

assert_eq \
  'a same-tag update anchors the already-local image before pulling that tag again' \
  "${previous_test_image}|ghcr.io/yujianwudi/new_api_tools:0.2.0" \
  same_mutable_image_is_anchored

unresolved_mutable_image_fails_closed() (
  local fixture env_file status current_image='ghcr.io/yujianwudi/new_api_tools:0.2.0'
  log_info() { :; }
  docker() { fake_repo_digest_inspect '' "$@"; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$current_image" > "$env_file"

  set +e
  (migrate_image_env_file "$env_file" 'ghcr.io/yujianwudi/new_api_tools:0.5.0') >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')"
)

assert_eq \
  'an unresolved mutable installed image aborts without changing the rollback value' \
  '1|ghcr.io/yujianwudi/new_api_tools:0.2.0' \
  unresolved_mutable_image_fails_closed

ambiguous_mutable_image_fails_closed() (
  local fixture env_file status current_image='ghcr.io/yujianwudi/new_api_tools:0.2.0'
  log_info() { :; }
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' && "${4:-}" == *RepoDigests* ]]; then
      printf '%s\n%s\n' "$previous_test_image" "$resolved_test_image"
      return 0
    fi
    return 1
  }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$current_image" > "$env_file"

  set +e
  (migrate_image_env_file "$env_file" 'ghcr.io/yujianwudi/new_api_tools:0.5.0') >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')"
)

assert_eq \
  'multiple same-repository RepoDigests abort without changing the rollback value' \
  '1|ghcr.io/yujianwudi/new_api_tools:0.2.0' \
  ambiguous_mutable_image_fails_closed

immutable_image_survives_mutable_migration() (
  local fixture env_file requested_image='ghcr.io/yujianwudi/new_api_tools:0.5.0'
  log_info() { :; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "$env_file"

  migrate_image_env_file "$env_file" "$requested_image"
  printf '%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')" \
    "$NEWAPI_TOOLS_IMAGE"
)

assert_eq \
  'a mutable update candidate never downgrades the persisted immutable image before pull validation' \
  "${previous_test_image}|ghcr.io/yujianwudi/new_api_tools:0.5.0" \
  immutable_image_survives_mutable_migration

immutable_image_survives_unverified_digest_migration() (
  local fixture env_file
  log_info() { :; }
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "$env_file"

  migrate_image_env_file "$env_file" "$resolved_test_image"
  printf '%s|%s\n' \
    "$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')" \
    "$NEWAPI_TOOLS_IMAGE"
)

assert_eq \
  'a different immutable candidate does not replace the verified pin before pull validation' \
  "${previous_test_image}|${resolved_test_image}" \
  immutable_image_survives_unverified_digest_migration

deploy_start_order() (
  local candidate_up_status="${1:-0}" rollback_up_status="${2:-0}"
  local order_file status
  order_file="$(mktemp)"
  trap 'rm -f "$order_file"' EXIT

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$previous_test_image"; }
  resolve_deploy_running_compose_project() { printf 'test-project\n'; }
  verify_deploy_application_health() { :; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s+%s\n' "${args[i + 1]:-}" "${args[i + 2]:-}" >> "$order_file"
          return 0
          ;;
        down)
          printf 'down\n' >> "$order_file"
          return 0
          ;;
        up)
          if [[ " ${args[*]} " == *' --wait '* && " ${args[*]} " == *' --wait-timeout 180 '* ]]; then
            printf 'up-wait\n' >> "$order_file"
            if (( $(grep -c '^up-wait$' "$order_file") == 1 )); then
              return "$candidate_up_status"
            fi
            return "$rollback_up_status"
          else
            printf 'up-start\n' >> "$order_file"
            return 0
          fi
          ;;
      esac
    done
    return 0
  }
  docker() {
    case "${1:-}" in
      ps) printf 'newapi-tools\n' ;;
      image)
        if [[ "${2:-}" == 'inspect' && "${4:-}" == *RepoDigests* ]]; then
          printf 'pin\n' >> "$order_file"
          printf '%s\n' "$resolved_test_image"
        fi
        ;;
    esac
  }

  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILES=(-f "$REPO_ROOT/docker-compose.yml")
  ENV_FILE="${install_fixture}/.env"
  rm -f -- "${ENV_FILE}.rollback"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "$ENV_FILE"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.2.0'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  USE_HOST_MODE=true
  USE_BRIDGE_MODE=false
  LOG_NETWORK=''
  FRONTEND_BIND='127.0.0.1'
  FRONTEND_PORT='1145'
  ADMIN_PASSWORD='test-password'
  AUTO_GENERATED_PASSWORD=false

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s|%s\n' \
    "$status" "$(paste -sd, "$order_file")" \
    "$(env_file_value "$ENV_FILE" 'NEWAPI_TOOLS_IMAGE')" \
    "$([[ -e "${ENV_FILE}.rollback" || -L "${ENV_FILE}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'deployment waits for a healthy candidate before committing its digest' \
  "0|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait|${resolved_test_image}|absent" \
  deploy_start_order
assert_eq \
  'deployment restores the old digest and healthy old service when the candidate is unhealthy' \
  "1|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait,down,up-start,up-wait|${previous_test_image}|present" \
  deploy_start_order 42 0

deploy_pull_failure_actions() (
  local order_file status
  order_file="$(mktemp)"
  trap 'rm -f "$order_file"' EXIT

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$previous_test_image"; }
  resolve_deploy_running_compose_project() { printf 'test-project\n'; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s+%s\n' "${args[i + 1]:-}" "${args[i + 2]:-}" >> "$order_file"
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
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "$ENV_FILE"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.2.0'

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s\n' "$status" "$(paste -sd, "$order_file")" "$(env_file_value "$ENV_FILE" 'NEWAPI_TOOLS_IMAGE')"
)

assert_eq \
  'deployment leaves the old container running when the app or any dependency pull fails' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  deploy_pull_failure_actions

deploy_revision_mismatch_actions() (
  local order_file status
  order_file="$(mktemp)"
  trap 'rm -f "$order_file"' EXIT

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$previous_test_image"; }
  resolve_deploy_running_compose_project() { printf 'test-project\n'; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s+%s\n' "${args[i + 1]:-}" "${args[i + 2]:-}" >> "$order_file"
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
    case "${1:-}" in
      ps) printf 'newapi-tools\n' ;;
      image)
        if [[ "${2:-}" == 'inspect' ]]; then
          case "${4:-}" in
            *RepoDigests*) printf '%s\n' "$resolved_test_image" ;;
            *org.opencontainers.image.revision*) printf '%s\n' 'ffffffffffffffffffffffffffffffffffffffff' ;;
          esac
        fi
        ;;
    esac
  }

  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILES=(-f "$REPO_ROOT/docker-compose.yml")
  ENV_FILE="${install_fixture}/.env"
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "$ENV_FILE"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.2.0'
  NEWAPI_TOOLS_EXPECTED_REVISION="$test_commit"

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s\n' "$status" "$(paste -sd, "$order_file")" "$(env_file_value "$ENV_FILE" 'NEWAPI_TOOLS_IMAGE')"
)

assert_eq \
  'deployment leaves the old container running when OCI source revision validation fails' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  deploy_revision_mismatch_actions

install_quick_update_actions() (
  local pull_status="$1"
  local requested_image="${2:-ghcr.io/yujianwudi/new_api_tools:0.2.0}"
  local existing_image="${3:-$previous_test_image}"
  local anchor_mode="${4:-available}"
  local candidate_up_status="${5:-0}" rollback_up_status="${6:-0}"
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  printf 'FRONTEND_PORT=1145\nCOMPOSE_PROJECT_NAME=old-project\nNEWAPI_TOOLS_IMAGE=%s\n' \
    "$existing_image" > "${fixture}/.env"
  : > "${fixture}/docker-compose.yml"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  check_and_update_configs() { :; }
  migrate_env_file() { migrate_image_env_file "${1}/.env" "$NEWAPI_TOOLS_IMAGE"; }
  sync_newapi_mutation_safety_config() { :; }
  check_server_host_security() { :; }
  download_geoip_database() { :; }
  setup_compose_files() { COMPOSE_FILE=''; }
  restore_runtime_network_connections() { :; }
  detect_env_details() { :; }
  show_frontend_access() { :; }
  verify_install_application_health() { :; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s+%s\n' "${args[i + 1]:-}" "${args[i + 2]:-}" >> "$order_file"
          return "$pull_status"
          ;;
        down)
          printf 'down\n' >> "$order_file"
          return 0
          ;;
        up)
          if [[ " ${args[*]} " == *' --wait '* && " ${args[*]} " == *' --wait-timeout 180 '* ]]; then
            printf 'up-wait\n' >> "$order_file"
            if (( $(grep -c '^up-wait$' "$order_file") == 1 )); then
              return "$candidate_up_status"
            fi
            return "$rollback_up_status"
          else
            printf 'up-start\n' >> "$order_file"
            return 0
          fi
          ;;
      esac
    done
    return 0
  }
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' && "${4:-}" == *RepoDigests* ]]; then
      if grep -q '^pull:' "$order_file"; then
        printf 'pin\n' >> "$order_file"
        printf '%s\n' "$resolved_test_image"
      elif [[ "$anchor_mode" == 'available' ]]; then
        printf '%s\n' "$previous_test_image"
      fi
    fi
  }

  PROJECT_DIR="$fixture"
  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILE=''
  ENV_FRONTEND_BIND='127.0.0.1'
  NEWAPI_TOOLS_IMAGE="$requested_image"
  NEWAPI_TOOLS_EXPECTED_REVISION=''

  set +e
  (quick_update >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s\n' "$status" "$(paste -sd, "$order_file")" "$(env_file_value "${fixture}/.env" 'NEWAPI_TOOLS_IMAGE')"
)

assert_eq \
  'quick update commits the pulled digest only after the candidate is healthy' \
  "0|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait|${resolved_test_image}" \
  install_quick_update_actions 0
assert_eq \
  'quick update restores the old digest and healthy old service when the candidate is unhealthy' \
  "1|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait,down,up-start,up-wait|${previous_test_image}" \
  install_quick_update_actions 0 'ghcr.io/yujianwudi/new_api_tools:0.2.0' "$previous_test_image" available 42 0
assert_eq \
  'quick update never stops services after a targeted pull failure' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_quick_update_actions 23
assert_eq \
  'quick update preserves the verified digest when a different digest cannot be pulled' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_quick_update_actions 23 "$resolved_test_image"
assert_eq \
  'quick update preserves a legacy mutable image when the replacement pull fails' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_quick_update_actions 23 'ghcr.io/yujianwudi/new_api_tools:0.5.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0'
assert_eq \
  'quick update replaces a legacy mutable image only after successful digest validation' \
  "0|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait|${resolved_test_image}" \
  install_quick_update_actions 0 'ghcr.io/yujianwudi/new_api_tools:0.5.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0'
assert_eq \
  'quick update anchors a same-name mutable tag before a failed refresh pull' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_quick_update_actions 23 'ghcr.io/yujianwudi/new_api_tools:0.2.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0'
assert_eq \
  'quick update aborts before pull when the installed mutable tag has no unique local RepoDigest' \
  '1||ghcr.io/yujianwudi/new_api_tools:0.2.0' \
  install_quick_update_actions 0 'ghcr.io/yujianwudi/new_api_tools:0.5.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0' missing

install_interactive_update_actions() (
  local pull_status="$1"
  local requested_image="${2:-ghcr.io/yujianwudi/new_api_tools:0.2.0}"
  local existing_image="${3:-$previous_test_image}"
  local anchor_mode="${4:-available}"
  local candidate_up_status="${5:-0}" rollback_up_status="${6:-0}"
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  printf 'FRONTEND_PORT=1145\nCOMPOSE_PROJECT_NAME=old-project\nNEWAPI_TOOLS_IMAGE=%s\n' \
    "$existing_image" > "${fixture}/.env"
  : > "${fixture}/docker-compose.yml"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  download_geoip_database() { :; }
  migrate_env_file() { migrate_image_env_file "${1}/.env" "$NEWAPI_TOOLS_IMAGE"; }
  sync_newapi_mutation_safety_config() { :; }
  check_server_host_security() { :; }
  setup_compose_files() { COMPOSE_FILE=''; }
  restore_runtime_network_connections() { :; }
  detect_env_details() { ENV_FRONTEND_BIND='127.0.0.1'; }
  show_frontend_access() { :; }
  hostname() { printf '127.0.0.1\n'; }
  verify_install_application_health() { :; }
  fake_compose() {
    local -a args=("$@")
    local i
    for ((i = 0; i < ${#args[@]}; i++)); do
      case "${args[i]}" in
        pull)
          printf 'pull:%s+%s\n' "${args[i + 1]:-}" "${args[i + 2]:-}" >> "$order_file"
          return "$pull_status"
          ;;
        down)
          printf 'down\n' >> "$order_file"
          return 0
          ;;
        up)
          if [[ " ${args[*]} " == *' --wait '* && " ${args[*]} " == *' --wait-timeout 180 '* ]]; then
            printf 'up-wait\n' >> "$order_file"
            if (( $(grep -c '^up-wait$' "$order_file") == 1 )); then
              return "$candidate_up_status"
            fi
            return "$rollback_up_status"
          else
            printf 'up-start\n' >> "$order_file"
            return 0
          fi
          ;;
      esac
    done
    return 0
  }
  docker() {
    if [[ "${1:-}" == 'image' && "${2:-}" == 'inspect' && "${4:-}" == *RepoDigests* ]]; then
      if grep -q '^pull:' "$order_file"; then
        printf 'pin\n' >> "$order_file"
        printf '%s\n' "$resolved_test_image"
      elif [[ "$anchor_mode" == 'available' ]]; then
        printf '%s\n' "$previous_test_image"
      fi
    fi
  }

  PROJECT_DIR="$fixture"
  DOCKER_COMPOSE='fake_compose'
  NEWAPI_TOOLS_IMAGE="$requested_image"
  NEWAPI_TOOLS_EXPECTED_REVISION=''

  set +e
  (do_update_interactive "$fixture" >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s\n' "$status" "$(paste -sd, "$order_file")" "$(env_file_value "${fixture}/.env" 'NEWAPI_TOOLS_IMAGE')"
)

assert_eq \
  'interactive update commits the pulled digest only after the candidate is healthy' \
  "0|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait|${resolved_test_image}" \
  install_interactive_update_actions 0
assert_eq \
  'interactive update restores the old digest and healthy old service when the candidate is unhealthy' \
  "1|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait,down,up-start,up-wait|${previous_test_image}" \
  install_interactive_update_actions 0 'ghcr.io/yujianwudi/new_api_tools:0.2.0' "$previous_test_image" available 42 0
assert_eq \
  'interactive update leaves the old service running after pull failure' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_interactive_update_actions 23
assert_eq \
  'interactive update preserves the verified digest when a different digest cannot be pulled' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_interactive_update_actions 23 "$resolved_test_image"
assert_eq \
  'interactive update preserves a legacy mutable image when the replacement pull fails' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_interactive_update_actions 23 'ghcr.io/yujianwudi/new_api_tools:0.5.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0'
assert_eq \
  'interactive update replaces a legacy mutable image only after successful digest validation' \
  "0|pull:--include-deps+newapi-tools,pin,down,up-start,up-wait|${resolved_test_image}" \
  install_interactive_update_actions 0 'ghcr.io/yujianwudi/new_api_tools:0.5.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0'
assert_eq \
  'interactive update anchors a same-name mutable tag before a failed refresh pull' \
  "1|pull:--include-deps+newapi-tools|${previous_test_image}" \
  install_interactive_update_actions 23 'ghcr.io/yujianwudi/new_api_tools:0.2.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0'
assert_eq \
  'interactive update aborts before pull when the installed mutable tag has no unique local RepoDigest' \
  '1||ghcr.io/yujianwudi/new_api_tools:0.2.0' \
  install_interactive_update_actions 0 'ghcr.io/yujianwudi/new_api_tools:0.5.0' 'ghcr.io/yujianwudi/new_api_tools:0.2.0' missing

deploy_preactivation_failure_restores_full_env() (
  local stage="$1" fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT

  local old_content
  old_content="$(printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'ADMIN_PASSWORD=old-secret' \
    'FRONTEND_BIND=127.0.0.1' \
    'COMPOSE_PROJECT_NAME=old-project' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' \
    "SQL_DSN='host=old-db password=old-db-secret'" \
    'OLD_ONLY=preserve-me')"
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.0' \
    'ADMIN_PASSWORD=new-secret' \
    'FRONTEND_BIND=0.0.0.0' \
    'COMPOSE_PROJECT_NAME=new-project' \
    'NEWAPI_NETWORK_MODE=host' \
    'NEWAPI_NETWORK=' \
    'NEW_ONLY=remove-me' > "${fixture}/.env"

  download_geoip_database() { [[ "$stage" != 'precheck' ]]; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  connect_deploy_runtime_networks() { :; }
  verify_deploy_application_health() { :; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$previous_test_image"; }
  resolve_deploy_running_compose_project() { printf 'test-project\n'; }
  pin_deploy_image_after_pull() {
    printf 'pin\n' >> "$order_file"
    [[ "$stage" != 'pin' ]] || return 1
    NEWAPI_TOOLS_IMAGE="$resolved_test_image"
    export NEWAPI_TOOLS_IMAGE
  }
  fake_compose() {
    local arg
    for arg in "$@"; do
      case "$arg" in
        pull)
          printf 'pull\n' >> "$order_file"
          [[ "$stage" != 'pull' ]] || return 23
          return 0
          ;;
        down)
          if [[ "$stage" == 'down' ]]; then
            printf 'down-fail\n' >> "$order_file"
            return 41
          fi
          printf 'down\n' >> "$order_file"
          return 0
          ;;
        up)
          if [[ " $* " == *' --wait '* ]]; then
            printf 'up-wait\n' >> "$order_file"
          else
            printf 'up-start\n' >> "$order_file"
          fi
          return 0
          ;;
      esac
    done
  }
  docker() {
    if [[ "${1:-}" == 'ps' ]]; then
      printf 'newapi-tools\n'
    fi
  }

  SCRIPT_DIR="$REPO_ROOT"
  ENV_FILE="${fixture}/.env"
  COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
  COMPOSE_HOST_FILE="${REPO_ROOT}/docker-compose.host.yml"
  COMPOSE_LOGDB_FILE="${REPO_ROOT}/docker-compose.logdb.yml"
  COMPOSE_FILES=(-f "$COMPOSE_FILE" -f "$COMPOSE_HOST_FILE")
  DOCKER_COMPOSE='fake_compose'
  DEPLOY_ROLLBACK_ENV_AVAILABLE=true
  DEPLOY_ROLLBACK_ENV_CONTENT="$old_content"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.0'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  USE_HOST_MODE=true
  USE_BRIDGE_MODE=false
  NEWAPI_CONTAINER='new-api'
  NEWAPI_NETWORK=''
  ORIGINAL_NETWORK=''
  LOG_NETWORK=''
  FRONTEND_BIND='127.0.0.1'
  FRONTEND_PORT='1145'

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
    "$status" \
    "$(paste -sd, "$order_file")" \
    "$(env_file_value "$ENV_FILE" 'ADMIN_PASSWORD')" \
    "$(env_file_value "$ENV_FILE" 'OLD_ONLY')" \
    "$(grep -c '^NEW_ONLY=' "$ENV_FILE" || true)" \
    "$([[ -f "${ENV_FILE}.rollback" ]] && printf present || printf absent)" \
    "$(env_file_value "${ENV_FILE}.rollback" 'ADMIN_PASSWORD')" \
    "$(env_file_value "${ENV_FILE}.rollback" 'SQL_DSN')" \
    "$(env_file_value "${ENV_FILE}.rollback" 'NEWAPI_NETWORK')" \
    "$(stat -c '%a' "${ENV_FILE}.rollback")"
)

assert_eq \
  'deploy preflight failure restores the complete old dotenv before pull' \
  '1||old-secret|preserve-me|0|present|old-secret|host=old-db password=old-db-secret|old-network|600' \
  deploy_preactivation_failure_restores_full_env precheck
assert_eq \
  'deploy pull failure restores the complete old dotenv after generate_env_file replacement' \
  '1|pull|old-secret|preserve-me|0|present|old-secret|host=old-db password=old-db-secret|old-network|600' \
  deploy_preactivation_failure_restores_full_env pull
assert_eq \
  'deploy digest pin failure restores the complete old dotenv before any stop' \
  '1|pull,pin|old-secret|preserve-me|0|present|old-secret|host=old-db password=old-db-secret|old-network|600' \
  deploy_preactivation_failure_restores_full_env pin
assert_eq \
  'deploy treats an initial down error as partial removal and rebuilds the old service' \
  '1|pull,pin,down-fail,up-start,up-wait|old-secret|preserve-me|0|present|old-secret|host=old-db password=old-db-secret|old-network|600' \
  deploy_preactivation_failure_restores_full_env down

deploy_first_install_commits_without_rollback_snapshot() (
  local fixture order_file status start_output
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT

  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.1' \
    'COMPOSE_PROJECT_NAME=first-install' > "${fixture}/.env"

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  connect_deploy_runtime_networks() { :; }
  verify_deploy_application_health() { :; }
  pin_deploy_image_after_pull() {
    printf 'pin\n' >> "$order_file"
    NEWAPI_TOOLS_IMAGE="$resolved_test_image"
    export NEWAPI_TOOLS_IMAGE
  }
  fake_compose() {
    local arg
    for arg in "$@"; do
      case "$arg" in
        pull)
          printf 'pull\n' >> "$order_file"
          return 0
          ;;
        up)
          if [[ " $* " == *' --wait '* ]]; then
            printf 'up-wait\n' >> "$order_file"
          else
            printf 'up-start\n' >> "$order_file"
          fi
          return 0
          ;;
      esac
    done
  }
  docker() {
    if [[ "${1:-}" == 'ps' ]]; then
      return 0
    fi
  }

  SCRIPT_DIR="$REPO_ROOT"
  ENV_FILE="${fixture}/.env"
  COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
  COMPOSE_HOST_FILE="${REPO_ROOT}/docker-compose.host.yml"
  COMPOSE_LOGDB_FILE="${REPO_ROOT}/docker-compose.logdb.yml"
  COMPOSE_FILES=(-f "$COMPOSE_FILE")
  DOCKER_COMPOSE='fake_compose'
  DEPLOY_ROLLBACK_ENV_AVAILABLE=false
  DEPLOY_ROLLBACK_ENV_CONTENT=''
  DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=false
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.1'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  FRONTEND_BIND='127.0.0.1'
  FRONTEND_PORT='1145'
  ADMIN_PASSWORD='test-password'
  AUTO_GENERATED_PASSWORD=false
  USE_HOST_MODE=true
  USE_BRIDGE_MODE=false
  NEWAPI_CONTAINER='new-api'
  NEWAPI_NETWORK=''
  ORIGINAL_NETWORK=''
  LOG_NETWORK=''

  set +e
  start_output="$(start_services 2>&1)"
  status=$?
  set -e
  if [[ "$status" -ne 0 ]]; then
    printf '%s\n' "$start_output" >&2
    return "$status"
  fi
  printf '%s|%s|%s|%s\n' \
    "$status" \
    "$(paste -sd, "$order_file")" \
    "$(env_file_value "$ENV_FILE" 'NEWAPI_TOOLS_IMAGE')" \
    "$([[ -e "${ENV_FILE}.rollback" || -L "${ENV_FILE}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'deploy first install commits a healthy candidate without requiring a nonexistent rollback snapshot' \
  "0|pull,pin,up-start,up-wait|${resolved_test_image}|absent" \
  deploy_first_install_commits_without_rollback_snapshot

deploy_rollback_ignores_candidate_exports() (
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  local old_content
  old_content="$(printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'ADMIN_PASSWORD=old-secret' \
    'FRONTEND_BIND=127.0.0.1' \
    'COMPOSE_PROJECT_NAME=old-project' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network')"
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.0' \
    'ADMIN_PASSWORD=new-secret' \
    'FRONTEND_BIND=0.0.0.0' \
    'COMPOSE_PROJECT_NAME=new-project' \
    'NEWAPI_NETWORK_MODE=host' \
    'NEWAPI_NETWORK=' > "${fixture}/.env"

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  connect_deploy_runtime_networks() { :; }
  verify_deploy_application_health() { :; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$previous_test_image"; }
  resolve_deploy_running_compose_project() { printf 'old-project\n'; }
  pin_deploy_image_after_pull() {
    printf 'pin\n' >> "$order_file"
    NEWAPI_TOOLS_IMAGE="$resolved_test_image"
    export NEWAPI_TOOLS_IMAGE
  }
  fake_compose() {
    local -a args=("$@")
    local i env_path='' command_name='' admin bind project overlay='base'
    for ((i = 0; i < ${#args[@]}; i++)); do
      [[ "${args[i]}" == '--env-file' ]] && env_path="${args[i + 1]}"
      case "${args[i]}" in pull|down|up) command_name="${args[i]}"; break ;; esac
    done
    [[ " ${args[*]} " == *'docker-compose.host.yml'* ]] && overlay='host'
    if [[ -n "${ADMIN_PASSWORD+x}" ]]; then admin="$ADMIN_PASSWORD"; else admin="$(env_file_value "$env_path" ADMIN_PASSWORD)"; fi
    if [[ -n "${FRONTEND_BIND+x}" ]]; then bind="$FRONTEND_BIND"; else bind="$(env_file_value "$env_path" FRONTEND_BIND)"; fi
    if [[ -n "${COMPOSE_PROJECT_NAME+x}" ]]; then project="$COMPOSE_PROJECT_NAME"; else project="$(env_file_value "$env_path" COMPOSE_PROJECT_NAME)"; fi
    case "$command_name" in
      pull) printf 'pull\n' >> "$order_file" ;;
      down) printf 'down:%s:%s:%s:%s\n' "$admin" "$bind" "$project" "$overlay" >> "$order_file" ;;
      up)
        if [[ " ${args[*]} " == *' --wait '* ]]; then
          printf 'wait:%s:%s:%s:%s\n' "$admin" "$bind" "$project" "$overlay" >> "$order_file"
          if (( $(grep -c '^wait:' "$order_file") > 1 )); then return 0; fi
          return 42
        fi
        printf 'up:%s:%s:%s:%s\n' "$admin" "$bind" "$project" "$overlay" >> "$order_file"
        ;;
    esac
  }
  docker() { [[ "${1:-}" == 'ps' ]] && printf 'newapi-tools\n'; }

  SCRIPT_DIR="$REPO_ROOT"
  ENV_FILE="${fixture}/.env"
  COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
  COMPOSE_HOST_FILE="${REPO_ROOT}/docker-compose.host.yml"
  COMPOSE_LOGDB_FILE="${REPO_ROOT}/docker-compose.logdb.yml"
  COMPOSE_FILES=(-f "$COMPOSE_FILE" -f "$COMPOSE_HOST_FILE")
  DOCKER_COMPOSE='fake_compose'
  DEPLOY_ROLLBACK_ENV_AVAILABLE=true
  DEPLOY_ROLLBACK_ENV_CONTENT="$old_content"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.0'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  USE_HOST_MODE=true
  USE_BRIDGE_MODE=false
  NEWAPI_CONTAINER='new-api'
  NEWAPI_NETWORK=''
  ORIGINAL_NETWORK=''
  LOG_NETWORK=''
  export ADMIN_PASSWORD='new-secret' FRONTEND_BIND='0.0.0.0' COMPOSE_PROJECT_NAME='new-project'

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(paste -sd, "$order_file")"
)

assert_eq \
  'rollback clears candidate exports and rebuilds the old Compose overlay context' \
  '1|pull,pin,down:new-secret:0.0.0.0:old-project:host,up:new-secret:0.0.0.0:old-project:host,wait:new-secret:0.0.0.0:old-project:host,down:old-secret:127.0.0.1:old-project:base,up:old-secret:127.0.0.1:old-project:base,wait:old-secret:127.0.0.1:old-project:base' \
  deploy_rollback_ignores_candidate_exports

deploy_custom_project_identity_transaction() (
  local explicit_project="$1" fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  local old_content
  old_content="$(printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'ADMIN_PASSWORD=old-secret' \
    'FRONTEND_BIND=127.0.0.1' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network')"
  if [[ "$explicit_project" == 'true' ]]; then
    old_content+=$'\nCOMPOSE_PROJECT_NAME=old-project'
  fi
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.0' \
    'ADMIN_PASSWORD=new-secret' \
    'FRONTEND_BIND=127.0.0.1' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' > "${fixture}/.env"

  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  connect_deploy_runtime_networks() { :; }
  verify_deploy_application_health() { :; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$previous_test_image"; }
  pin_deploy_image_after_pull() {
    printf 'pin\n' >> "$order_file"
    NEWAPI_TOOLS_IMAGE="$resolved_test_image"
    export NEWAPI_TOOLS_IMAGE
  }
  fake_compose() {
    local -a args=("$@")
    local i project='' command_name=''
    for ((i = 0; i < ${#args[@]}; i++)); do
      [[ "${args[i]}" == '-p' ]] && project="${args[i + 1]}"
      case "${args[i]}" in pull|down|up) command_name="${args[i]}"; break ;; esac
    done
    printf '%s:%s%s\n' "$command_name" "$project" \
      "$([[ " ${args[*]} " == *' --wait '* ]] && printf ':wait' || true)" >> "$order_file"
    [[ "$project" == 'old-project' ]] || return 73
    return 0
  }
  docker() {
    case "${1:-}" in
      ps) printf 'newapi-tools\n'; return 0 ;;
      inspect)
        if [[ "${3:-}" == *com.docker.compose.project* ]]; then
          printf 'old-project\n'
          return 0
        fi
        ;;
    esac
    return 1
  }

  ENV_FILE="${fixture}/.env"
  COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
  COMPOSE_HOST_FILE="${REPO_ROOT}/docker-compose.host.yml"
  COMPOSE_LOGDB_FILE="${REPO_ROOT}/docker-compose.logdb.yml"
  COMPOSE_FILES=(-f "$COMPOSE_FILE")
  DOCKER_COMPOSE='fake_compose'
  DEPLOY_ROLLBACK_ENV_AVAILABLE=true
  DEPLOY_ROLLBACK_ENV_CONTENT="$old_content"
  DEPLOY_ENV_GENERATED_THIS_RUN=true
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.0'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  USE_HOST_MODE=false
  USE_BRIDGE_MODE=false
  NEWAPI_CONTAINER='new-api'
  NEWAPI_NETWORK='old-network'
  ORIGINAL_NETWORK=''
  LOG_NETWORK=''
  FRONTEND_BIND='127.0.0.1'
  FRONTEND_PORT='1145'
  ADMIN_PASSWORD='new-secret'
  AUTO_GENERATED_PASSWORD=false

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s|%s|%s\n' \
    "$status" "$(paste -sd, "$order_file")" \
    "$(env_file_value "$ENV_FILE" COMPOSE_PROJECT_NAME)" \
    "$(env_file_value "$ENV_FILE" NEWAPI_TOOLS_IMAGE)" \
    "$([[ -e "${ENV_FILE}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'candidate down/up stays in the explicit old Compose project and commits that identity' \
  "0|pull:old-project,pin,down:old-project,up:old-project,up:old-project:wait|old-project|${resolved_test_image}|absent" \
  deploy_custom_project_identity_transaction true
assert_eq \
  'an inline-only historical Compose project is recovered from the running container label' \
  "0|pull:old-project,pin,down:old-project,up:old-project,up:old-project:wait|old-project|${resolved_test_image}|absent" \
  deploy_custom_project_identity_transaction false

deploy_compose_project_conflict() (
  docker() {
    [[ "${1:-}" == 'inspect' ]] || return 1
    printf 'old-project\n'
  }
  resolve_deploy_running_compose_project newapi-tools 'COMPOSE_PROJECT_NAME=other-project'
)

assert_rejected \
  'a configured Compose project that conflicts with the running container label fails closed' \
  deploy_compose_project_conflict

deploy_first_install_cleanup() (
  local failure_mode="$1" fixture order_file status wait_count=0
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.0' \
    'ADMIN_PASSWORD=new-secret' \
    'FRONTEND_BIND=127.0.0.1' > "${fixture}/.env"
  download_geoip_database() { :; }
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  connect_deploy_runtime_networks() { :; }
  verify_deploy_application_health() { :; }
  pin_deploy_image_after_pull() {
    printf 'pin\n' >> "$order_file"
    NEWAPI_TOOLS_IMAGE="$resolved_test_image"
    export NEWAPI_TOOLS_IMAGE
  }
  if [[ "$failure_mode" == 'commit' ]]; then
    persist_deploy_image_env() { return 1; }
  fi
  fake_compose() {
    local arg
    for arg in "$@"; do
      case "$arg" in
        pull) printf 'pull\n' >> "$order_file"; return 0 ;;
        down)
          printf 'down-cleanup\n' >> "$order_file"
          [[ "$failure_mode" != 'wait-down-fail' ]]
          return
          ;;
        rm) printf 'rm-services\n' >> "$order_file"; return 0 ;;
        up)
          if [[ " $* " == *' --wait '* ]]; then
            wait_count=$((wait_count + 1))
            printf 'up-wait\n' >> "$order_file"
            [[ "$failure_mode" != 'wait' && "$failure_mode" != 'wait-down-fail' ]]
            return
          fi
          printf 'up-start\n' >> "$order_file"
          return 0
          ;;
      esac
    done
  }
  docker() {
    case "${1:-}" in
      ps) return 0 ;;
      rm) printf 'rm-known\n' >> "$order_file"; return 0 ;;
    esac
  }

  SCRIPT_DIR="$REPO_ROOT"
  ENV_FILE="${fixture}/.env"
  COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
  COMPOSE_HOST_FILE="${REPO_ROOT}/docker-compose.host.yml"
  COMPOSE_LOGDB_FILE="${REPO_ROOT}/docker-compose.logdb.yml"
  COMPOSE_FILES=(-f "$COMPOSE_FILE")
  DOCKER_COMPOSE='fake_compose'
  DEPLOY_ROLLBACK_ENV_AVAILABLE=false
  DEPLOY_ROLLBACK_ENV_CONTENT=''
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.0'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  USE_HOST_MODE=true
  USE_BRIDGE_MODE=false
  NEWAPI_CONTAINER='new-api'
  NEWAPI_NETWORK=''
  ORIGINAL_NETWORK=''
  LOG_NETWORK=''
  FRONTEND_BIND='127.0.0.1'
  FRONTEND_PORT='1145'
  ADMIN_PASSWORD='new-secret'
  AUTO_GENERATED_PASSWORD=false

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(paste -sd, "$order_file")"
)

assert_eq \
  'a failed first-install health wait is torn down and leaves no restart-loop app container' \
  '1|pull,pin,up-start,up-wait,down-cleanup,rm-known' \
  deploy_first_install_cleanup wait
assert_eq \
  'a failed first-install image persistence is torn down after candidate health' \
  '1|pull,pin,up-start,up-wait,down-cleanup,rm-known' \
  deploy_first_install_cleanup commit
assert_eq \
  'a first-install cleanup down failure falls back to service-scoped removal' \
  '1|pull,pin,up-start,up-wait,down-cleanup,rm-services,rm-known' \
  deploy_first_install_cleanup wait-down-fail

health_probe_result() (
  local scenario="$1" mode="$2"
  docker() {
    [[ "${1:-}" == 'exec' ]] || return 1
    case "${*: -1}" in
      */readyz)
        case "$scenario" in
          v05-ready) printf '%s\n' '{"status":"ready","checks":{"main_database":"ok","tool_store":"ok"}}' ;;
          v05-not-ready) printf '%s\n' '{"status":"not_ready","checks":{"main_database":"ok","tool_store":"unavailable"}}' ;;
          *) printf '%s\n' '<!doctype html><html>legacy SPA</html>' ;;
        esac
        ;;
      */api/health/db)
        case "$scenario" in
          legacy-db-ready|v05-not-ready) printf '%s\n' '{"success":true,"status":"connected"}' ;;
          *) printf '%s\n' '<!doctype html><html>not a health response</html>' ;;
        esac
        ;;
    esac
  }
  if verify_deploy_application_health "$mode"; then printf 'ready\n'; else printf 'rejected\n'; fi
)

assert_eq 'v0.5 candidate health requires semantic readiness including Tool Store' 'ready' \
  health_probe_result v05-ready candidate
assert_eq 'a candidate cannot downgrade to the legacy DB-only health contract' 'rejected' \
  health_probe_result legacy-db-ready candidate
assert_eq 'legacy v0.2 rollback accepts a semantic connected database response' 'ready' \
  health_probe_result legacy-db-ready rollback
assert_eq 'legacy SPA 200 responses never count as healthy by themselves' 'rejected' \
  health_probe_result legacy-spa rollback
assert_eq 'v0.5 not-ready JSON is authoritative and cannot fall back to DB-only health' 'rejected' \
  health_probe_result v05-not-ready rollback

running_container_digest_anchor() (
  local container_image_id="sha256:$(printf 'c%.0s' {1..64})"
  docker() {
    if [[ "${1:-}" == 'inspect' ]]; then
      printf '%s\n' "$container_image_id"
      return 0
    elif [[ "${1:-} ${2:-}" == 'image inspect' && "${5:-}" == "$container_image_id" ]]; then
      printf '%s\n' "$previous_test_image"
      return 0
    fi
    return 1
  }
  resolve_deploy_running_image_digest newapi-tools 'ghcr.io/yujianwudi/new_api_tools:0.2.0'
)

assert_eq \
  'rollback anchor follows the image ID of the running container instead of a moved mutable tag' \
  "$previous_test_image" \
  running_container_digest_anchor

install_running_container_anchor() (
  local fixture container_image_id
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  container_image_id="sha256:$(printf 'd%.0s' {1..64})"
  printf '%s\n' 'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.2.0' > "${fixture}/.env"
  log_info() { :; }
  list_install_container_names() { printf 'newapi-tools\n'; }
  docker() {
    case "${1:-} ${2:-}" in
      'inspect --format') printf '%s\n' "$container_image_id"; return 0 ;;
      'image inspect')
        [[ "${5:-}" == "$container_image_id" ]] || return 1
        printf '%s\n' "$previous_test_image"
        return 0
        ;;
    esac
    return 1
  }
  migrate_image_env_file "${fixture}/.env" 'ghcr.io/yujianwudi/new_api_tools:0.5.0'
  env_file_value "${fixture}/.env" NEWAPI_TOOLS_IMAGE
)

assert_eq \
  'installer migration anchors the running container image rather than the current mutable tag target' \
  "$previous_test_image" \
  install_running_container_anchor

install_initial_down_failure_recovers_old() (
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "${fixture}/.env"
  : > "${fixture}/docker-compose.yml"
  log_info() { :; }
  log_error() { :; }
  setup_compose_files() { :; }
  restore_runtime_network_connections() { :; }
  verify_install_application_health() { :; }
  fake_compose() {
    local arg
    for arg in "$@"; do
      case "$arg" in
        down) printf 'down-partial-failure\n' >> "$order_file"; return 41 ;;
        up)
          if [[ " $* " == *' --wait '* ]]; then printf 'up-wait\n'; else printf 'up-start\n'; fi >> "$order_file"
          return 0
          ;;
      esac
    done
  }
  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILE=''
  NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  export NEWAPI_TOOLS_IMAGE
  set +e
  (restart_install_services_transactionally "${fixture}/.env" "$fixture" >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(paste -sd, "$order_file")"
)

assert_eq \
  'installer initial down failure still starts and verifies the immutable old service' \
  '1|down-partial-failure,up-start,up-wait' \
  install_initial_down_failure_recovers_old

install_custom_project_identity_transaction() (
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "${fixture}/.env"
  : > "${fixture}/docker-compose.yml"
  log_info() { :; }
  log_success() { :; }
  list_install_container_names() { printf 'newapi-tools\n'; }
  restore_runtime_network_connections() { :; }
  verify_install_application_health() { :; }
  setup_compose_files() { :; }
  docker() {
    [[ "${1:-}" == 'inspect' ]] || return 1
    printf 'old-project\n'
  }
  fake_compose() {
    local -a args=("$@")
    local i project='' command_name=''
    for ((i = 0; i < ${#args[@]}; i++)); do
      [[ "${args[i]}" == '-p' ]] && project="${args[i + 1]}"
      case "${args[i]}" in down|up) command_name="${args[i]}"; break ;; esac
    done
    printf '%s:%s%s\n' "$command_name" "$project" \
      "$([[ " ${args[*]} " == *' --wait '* ]] && printf ':wait' || true)" >> "$order_file"
    [[ "$project" == 'old-project' ]]
  }
  DOCKER_COMPOSE='fake_compose'
  COMPOSE_FILE=''
  NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  export NEWAPI_TOOLS_IMAGE
  set +e
  (restart_install_services_transactionally "${fixture}/.env" "$fixture" >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s|%s\n' \
    "$status" "$(paste -sd, "$order_file")" \
    "$(env_file_value "${fixture}/.env" COMPOSE_PROJECT_NAME)" \
    "$(env_file_value "${fixture}/.env" NEWAPI_TOOLS_IMAGE)"
)

assert_eq \
  'installer candidate activation stays in the running container Compose project' \
  "0|down:old-project,up:old-project,up:old-project:wait|old-project|${resolved_test_image}" \
  install_custom_project_identity_transaction

deploy_legacy_context_without_network_key() (
  local fixture
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$previous_test_image" > "${fixture}/.env"
  COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
  COMPOSE_HOST_FILE="${REPO_ROOT}/docker-compose.host.yml"
  COMPOSE_LOGDB_FILE="${REPO_ROOT}/docker-compose.logdb.yml"
  NEWAPI_CONTAINER='new-api'
  configure_deploy_context_from_env "${fixture}/.env"
  printf '%s|%s|%s\n' "$USE_HOST_MODE" "$USE_BRIDGE_MODE" "${#COMPOSE_FILES[@]}"
)

assert_eq \
  'a legacy dotenv without NEWAPI_NETWORK uses the base Compose fallback rather than host mode' \
  'false|false|2' \
  deploy_legacy_context_without_network_key

deploy_container_listing_failure_is_fail_closed() (
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  local old_content
  old_content="$(printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'ADMIN_PASSWORD=old-secret' \
    'OLD_ONLY=preserve-me')"
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.0' \
    'ADMIN_PASSWORD=new-secret' \
    'NEW_ONLY=remove-me' > "${fixture}/.env"
  download_geoip_database() { :; }
  log_info() { :; }
  fake_compose() { printf 'compose-called\n' >> "$order_file"; return 0; }
  docker() {
    [[ "${1:-}" == 'ps' ]] && return 55
    printf 'docker-mutation-called\n' >> "$order_file"
    return 1
  }
  ENV_FILE="${fixture}/.env"
  COMPOSE_FILE="${REPO_ROOT}/docker-compose.yml"
  COMPOSE_HOST_FILE="${REPO_ROOT}/docker-compose.host.yml"
  COMPOSE_LOGDB_FILE="${REPO_ROOT}/docker-compose.logdb.yml"
  COMPOSE_FILES=(-f "$COMPOSE_FILE")
  DOCKER_COMPOSE='fake_compose'
  DEPLOY_ROLLBACK_ENV_AVAILABLE=true
  DEPLOY_ROLLBACK_ENV_CONTENT="$old_content"
  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.0'

  set +e
  (start_services >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s|%s\n' \
    "$status" "$(paste -sd, "$order_file")" \
    "$(env_file_value "$ENV_FILE" ADMIN_PASSWORD)" \
    "$(grep -c '^NEW_ONLY=' "$ENV_FILE" || true)"
)

assert_eq \
  'deploy aborts before pull or cleanup when Docker container enumeration fails' \
  '1||old-secret|0' \
  deploy_container_listing_failure_is_fail_closed

install_container_listing_failure_is_fail_closed() (
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  printf '%s\n' 'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.2.0' > "${fixture}/.env"
  log_info() { :; }
  list_install_container_names() { return 55; }
  docker() { printf 'inspect-called\n' >> "$order_file"; return 1; }
  set +e
  (migrate_image_env_file "${fixture}/.env" 'ghcr.io/yujianwudi/new_api_tools:0.5.0' >/dev/null 2>&1)
  status=$?
  set -e
  printf '%s|%s|%s\n' \
    "$status" "$(paste -sd, "$order_file")" \
    "$(env_file_value "${fixture}/.env" NEWAPI_TOOLS_IMAGE)"
)

assert_eq \
  'installer aborts without tag inspection or dotenv mutation when container enumeration fails' \
  '1||ghcr.io/yujianwudi/new_api_tools:0.2.0' \
  install_container_listing_failure_is_fail_closed

deploy_capture_persists_complete_snapshot() (
  local fixture
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.2.0' \
    'ADMIN_PASSWORD=old-secret' \
    "SQL_DSN='host=old-db password=old-db-secret'" \
    "LOG_SQL_DSN='host=old-log password=old-log-secret'" \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' > "$ENV_FILE"
  log_warn() { :; }
  resolve_deploy_running_compose_project() { printf 'old-project\n'; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$previous_test_image"; }

  capture_deploy_rollback_env 'newapi-tools'

  printf '%s|%s|%s|%s|%s|%s|%s|%s|%s\n' \
    "$(env_file_value "$ENV_FILE" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "${ENV_FILE}.rollback" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "${ENV_FILE}.rollback" COMPOSE_PROJECT_NAME)" \
    "$(env_file_value "${ENV_FILE}.rollback" ADMIN_PASSWORD)" \
    "$(env_file_value "${ENV_FILE}.rollback" SQL_DSN)" \
    "$(env_file_value "${ENV_FILE}.rollback" LOG_SQL_DSN)" \
    "$(env_file_value "${ENV_FILE}.rollback" NEWAPI_NETWORK)" \
    "$(stat -c '%a' "${ENV_FILE}.rollback")" \
    "$DEPLOY_ROLLBACK_ENV_AVAILABLE"
)

assert_eq \
  'deploy atomically captures a mode-600 complete rollback dotenv before regeneration' \
  "ghcr.io/yujianwudi/new_api_tools:0.2.0|${previous_test_image}|old-project|old-secret|host=old-db password=old-db-secret|host=old-log password=old-log-secret|old-network|600|true" \
  deploy_capture_persists_complete_snapshot

deploy_capture_rejects_permissive_snapshot() (
  local fixture
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${resolved_test_image}" \
    'ADMIN_PASSWORD=candidate-secret' > "$ENV_FILE"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "${ENV_FILE}.rollback"
  chmod 640 "${ENV_FILE}.rollback"
  log_warn() { :; }
  resolve_deploy_running_compose_project() { printf 'candidate-project\n'; }
  resolve_deploy_running_image_digest() { printf '%s\n' "$resolved_test_image"; }
  capture_deploy_rollback_env 'newapi-tools'
)

assert_rejected \
  'deploy rejects a preexisting rollback snapshot unless its permissions are exactly 0600' \
  deploy_capture_rejects_permissive_snapshot

deploy_recovers_snapshot_after_candidate_start_crash() (
  local fixture order_file
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${resolved_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' > "$ENV_FILE"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' \
    "SQL_DSN='host=old-db password=old-db-secret'" \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' > "${ENV_FILE}.rollback"
  chmod 600 "${ENV_FILE}.rollback"

  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  resolve_deploy_running_compose_project() {
    printf 'resolver-project-called\n' >> "$order_file"
    printf 'candidate-project\n'
  }
  resolve_deploy_running_image_digest() {
    printf 'resolver-image-called\n' >> "$order_file"
    printf '%s\n' "$resolved_test_image"
  }
  configure_deploy_context_from_env() {
    printf 'configure:%s:%s\n' \
      "$(env_file_value "$1" ADMIN_PASSWORD)" \
      "$(env_file_value "$1" NEWAPI_TOOLS_IMAGE)" >> "$order_file"
  }
  run_deploy_compose() {
    printf '%s:%s\n' "$3" "$2" >> "$order_file"
  }
  start_deploy_services_and_wait() {
    printf 'start:%s:%s:%s\n' \
      "$1" "$2" "$(env_file_value "$ENV_FILE" ADMIN_PASSWORD)" >> "$order_file"
  }

  capture_deploy_rollback_env 'newapi-tools'
  local captured_image captured_project
  captured_image="$(env_file_value "${ENV_FILE}.rollback" NEWAPI_TOOLS_IMAGE)"
  captured_project="$(env_file_value "${ENV_FILE}.rollback" COMPOSE_PROJECT_NAME)"
  recover_preexisting_deploy_rollback_snapshot

  printf '%s|%s|%s|%s|%s|%s|%s\n' \
    "$captured_image" \
    "$captured_project" \
    "$(env_file_value "${ENV_FILE}.rollback" ADMIN_PASSWORD)" \
    "$(env_file_value "$ENV_FILE" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "$ENV_FILE" ADMIN_PASSWORD)" \
    "$DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING" \
    "$(paste -sd, "$order_file")"
)

assert_eq \
  'a crash after candidate start restores the authoritative old snapshot without adopting candidate identity' \
  "${previous_test_image}|old-project|old-secret|${previous_test_image}|old-secret|false|configure:old-secret:${previous_test_image},down:${previous_test_image},start:rollback:${previous_test_image}:old-secret" \
  deploy_recovers_snapshot_after_candidate_start_crash

deploy_recovers_snapshot_after_down_crash_without_container() (
  local fixture order_file
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' > "${ENV_FILE}.rollback"
  chmod 600 "${ENV_FILE}.rollback"

  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  configure_deploy_context_from_env() {
    printf 'configure:%s\n' "$(env_file_value "$1" NEWAPI_TOOLS_IMAGE)" >> "$order_file"
  }
  run_deploy_compose() {
    printf '%s:%s\n' "$3" "$2" >> "$order_file"
  }
  start_deploy_services_and_wait() {
    printf 'start:%s:%s\n' "$1" "$2" >> "$order_file"
  }

  capture_deploy_rollback_env ''
  recover_preexisting_deploy_rollback_snapshot
  printf '%s|%s|%s|%s\n' \
    "$DEPLOY_ROLLBACK_ENV_AVAILABLE" \
    "$DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING" \
    "$(env_file_value "$ENV_FILE" NEWAPI_TOOLS_IMAGE)" \
    "$(paste -sd, "$order_file")"
)

assert_eq \
  'a crash after compose down restores the old snapshot even when no tool container remains' \
  "true|false|${previous_test_image}|configure:${previous_test_image},down:${previous_test_image},start:rollback:${previous_test_image}" \
  deploy_recovers_snapshot_after_down_crash_without_container

deploy_snapshot_symlink_directory_race_is_safe() (
  local fixture called=0 status target_kind attacker_files
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  mkdir "${fixture}/attacker"
  sync() {
    called=$((called + 1))
    if [[ "$called" == "1" ]]; then
      ln -s "${fixture}/attacker" "${ENV_FILE}.rollback"
    fi
    return 0
  }
  set +e
  persist_deploy_rollback_snapshot 'ADMIN_PASSWORD=top-secret' 2>/dev/null
  status=$?
  set -e
  target_kind="$([[ -f "${ENV_FILE}.rollback" && ! -L "${ENV_FILE}.rollback" ]] && printf regular || printf unsafe)"
  attacker_files="$(find "${fixture}/attacker" -type f | wc -l | tr -d '[:space:]')"
  printf '%s|%s|%s|%s|%s\n' \
    "$status" "$target_kind" "$attacker_files" \
    "$(env_file_value "${ENV_FILE}.rollback" ADMIN_PASSWORD)" \
    "$(stat -c '%a' "${ENV_FILE}.rollback")"
)

assert_eq \
  'rollback snapshot rename does not follow a concurrently inserted symlink-to-directory' \
  '0|regular|0|top-secret|600' \
  deploy_snapshot_symlink_directory_race_is_safe

deploy_snapshot_directory_race_fails_closed() (
  local fixture called=0 status
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  sync() {
    called=$((called + 1))
    if [[ "$called" == "1" ]]; then
      mkdir "${ENV_FILE}.rollback"
    fi
    return 0
  }
  set +e
  persist_deploy_rollback_snapshot 'ADMIN_PASSWORD=top-secret' 2>/dev/null
  status=$?
  set -e
  printf '%s|%s|%s\n' \
    "$status" \
    "$([[ -d "${ENV_FILE}.rollback" ]] && printf directory || printf other)" \
    "$(find "${ENV_FILE}.rollback" -type f | wc -l | tr -d '[:space:]')"
)

assert_eq \
  'rollback snapshot persistence fails closed when a directory is inserted before rename' \
  '1|directory|0' \
  deploy_snapshot_directory_race_fails_closed

install_recovers_persistent_snapshot() (
  local container_mode="$1" fixture order_file
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  local env_file="${fixture}/.env"
  : > "${fixture}/docker-compose.yml"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${resolved_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' > "$env_file"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' > "${env_file}.rollback"
  chmod 600 "${env_file}.rollback"

  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  list_install_container_names() {
    printf 'container-list-called\n' >> "$order_file"
    [[ "$container_mode" == 'candidate' ]] && printf 'newapi-tools\n'
  }
  resolve_install_running_compose_project() {
    printf 'project-resolver-called\n' >> "$order_file"
    return 1
  }
  resolve_install_running_image_digest() {
    printf 'image-resolver-called\n' >> "$order_file"
    return 1
  }
  setup_compose_files() { unset COMPOSE_FILE; }
  run_install_compose() {
    printf '%s:%s\n' "${*: -1}" "$3" >> "$order_file"
  }
  start_install_services_and_wait() {
    printf 'start:%s:%s\n' "$3" "$4" >> "$order_file"
  }

  NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  export NEWAPI_TOOLS_IMAGE
  prepare_install_rollback_transaction "$env_file" "$fixture"
  printf '%s|%s|%s|%s|%s|%s\n' \
    "$INSTALL_ROLLBACK_ENV_AVAILABLE" \
    "$INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING" \
    "$(env_file_value "$env_file" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "$env_file" ADMIN_PASSWORD)" \
    "$NEWAPI_TOOLS_IMAGE" \
    "$(paste -sd, "$order_file")"
)

assert_eq \
  'installer restores a persistent old snapshot after candidate-start-before-commit crash' \
  "true|false|${previous_test_image}|old-secret|${resolved_test_image}|down:${previous_test_image},start:rollback:${previous_test_image}" \
  install_recovers_persistent_snapshot candidate

assert_eq \
  'installer restores a persistent old snapshot when a crash after down left no container' \
  "true|false|${previous_test_image}|old-secret|${resolved_test_image}|down:${previous_test_image},start:rollback:${previous_test_image}" \
  install_recovers_persistent_snapshot none

install_recovers_before_management_menu() (
  local fixture order_file target_dir
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  target_dir="${fixture}/new_api_tools"
  mkdir -p "$target_dir"
  : > "${target_dir}/docker-compose.yml"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${resolved_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' > "${target_dir}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "${target_dir}/.env.rollback"
  chmod 600 "${target_dir}/.env.rollback"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  setup_compose_files() { unset COMPOSE_FILE; }
  run_install_compose() { printf 'recover-down\n' >> "$order_file"; }
  start_install_services_and_wait() { printf 'recover-start\n' >> "$order_file"; }
  show_management_menu() { printf 'menu\n' >> "$order_file"; }
  docker() {
    [[ "${1:-} ${2:-}" == 'ps --format' ]] && printf 'newapi-tools\n'
  }

  INSTALL_DIR="$fixture"
  unset NEWAPI_TOOLS_IMAGE
  check_existing_installation
  prepare_install_rollback_transaction "${target_dir}/.env" "$target_dir"
  printf '%s|%s|%s|%s\n' \
    "$(env_file_value "${target_dir}/.env" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "${target_dir}/.env" ADMIN_PASSWORD)" \
    "$([[ -n "${NEWAPI_TOOLS_IMAGE+x}" ]] && printf set || printf unset)" \
    "$(paste -sd, "$order_file")"
)

assert_eq \
  'installer restores an interrupted transaction before exposing management actions' \
  "${previous_test_image}|old-secret|unset|recover-down,recover-start,menu" \
  install_recovers_before_management_menu

install_persistent_transaction_result() (
  local candidate_status="$1" fixture status
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  local env_file="${fixture}/.env"
  : > "${fixture}/docker-compose.yml"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' > "$env_file"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "${env_file}.rollback"
  chmod 600 "${env_file}.rollback"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  list_install_container_names() { printf 'newapi-tools\n'; }
  setup_compose_files() { unset COMPOSE_FILE; }
  run_install_compose() { :; }
  start_install_services_and_wait() {
    [[ "$3" == 'candidate' ]] && return "$candidate_status"
    return 0
  }

  INSTALL_ROLLBACK_ENV_AVAILABLE=true
  INSTALL_ROLLBACK_ENV_CONTENT="$(<"${env_file}.rollback")"
  INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING=false
  INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE='old-project'
  NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  export NEWAPI_TOOLS_IMAGE
  set +e
  (restart_install_services_transactionally "$env_file" "$fixture") >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s|%s|%s\n' \
    "$status" \
    "$(env_file_value "$env_file" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "$env_file" ADMIN_PASSWORD)" \
    "$([[ -e "${env_file}.rollback" || -L "${env_file}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'installer durably commits a healthy candidate before clearing its rollback snapshot' \
  "0|${resolved_test_image}|candidate-secret|absent" \
  install_persistent_transaction_result 0

assert_eq \
  'installer retains the rollback snapshot and restores the full old dotenv after candidate failure' \
  "1|${previous_test_image}|old-secret|present" \
  install_persistent_transaction_result 42

deploy_fresh_preseeded_env_has_no_false_rollback() (
  local fixture before after
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  cp "${REPO_ROOT}/.env.example" "$ENV_FILE"
  before="$(sha256sum "$ENV_FILE" | awk '{print $1}')"
  capture_deploy_rollback_env ''
  after="$(sha256sum "$ENV_FILE" | awk '{print $1}')"
  printf '%s|%s|%s\n' \
    "$DEPLOY_ROLLBACK_ENV_AVAILABLE" \
    "$([[ -e "${ENV_FILE}.rollback" || -L "${ENV_FILE}.rollback" ]] && printf present || printf absent)" \
    "$([[ "$before" == "$after" ]] && printf unchanged || printf changed)"
)

assert_eq \
  'deploy treats a copied fresh .env.example with empty image identity as first-install input' \
  'false|absent|unchanged' \
  deploy_fresh_preseeded_env_has_no_false_rollback

install_fresh_preseeded_env_has_no_false_rollback() (
  local fixture env_file before after
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  cp "${REPO_ROOT}/.env.example" "$env_file"
  before="$(sha256sum "$env_file" | awk '{print $1}')"
  list_install_container_names() { return 0; }
  capture_install_rollback_env "$env_file"
  after="$(sha256sum "$env_file" | awk '{print $1}')"
  printf '%s|%s|%s\n' \
    "$INSTALL_ROLLBACK_ENV_AVAILABLE" \
    "$([[ -e "${env_file}.rollback" || -L "${env_file}.rollback" ]] && printf present || printf absent)" \
    "$([[ "$before" == "$after" ]] && printf unchanged || printf changed)"
)

assert_eq \
  'installer treats a copied fresh .env.example with empty image identity as first-install input' \
  'false|absent|unchanged' \
  install_fresh_preseeded_env_has_no_false_rollback

deploy_stopped_installation_restores_full_env() (
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' \
    'OLD_ONLY=preserve-me' > "$ENV_FILE"
  chmod 600 "$ENV_FILE"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  resolve_deploy_image_digest() {
    printf 'mutable-resolver-called\n' >> "$order_file"
    return 1
  }
  capture_deploy_rollback_env ''

  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.1' \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' \
    'NEWAPI_NETWORK_MODE=host' \
    'NEWAPI_NETWORK=' \
    'NEW_ONLY=remove-me' > "$ENV_FILE"
  chmod 600 "$ENV_FILE"

  download_geoip_database() { :; }
  list_deploy_container_names() { return 0; }
  pin_deploy_image_after_pull() {
    printf 'pin\n' >> "$order_file"
    NEWAPI_TOOLS_IMAGE="$resolved_test_image"
    export NEWAPI_TOOLS_IMAGE
  }
  run_deploy_compose() {
    printf 'compose:%s:%s\n' "$3" "$2" >> "$order_file"
  }
  configure_deploy_context_from_env() { :; }
  start_deploy_services_and_wait() {
    printf 'start:%s:%s\n' "$1" "$2" >> "$order_file"
    [[ "$1" == 'rollback' ]]
  }

  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.1'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=false
  set +e
  (start_services) >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s|%s|%s|%s|%s|%s\n' \
    "$status" \
    "$(env_file_value "$ENV_FILE" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "$ENV_FILE" ADMIN_PASSWORD)" \
    "$(env_file_value "$ENV_FILE" NEWAPI_NETWORK)" \
    "$(env_file_value "$ENV_FILE" OLD_ONLY)" \
    "$(grep -c '^NEW_ONLY=' "$ENV_FILE" || true)" \
    "$([[ -e "${ENV_FILE}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'deploy treats a safe stopped installation as rollback-capable and restores its complete dotenv' \
  "1|${previous_test_image}|old-secret|old-network|preserve-me|0|present" \
  deploy_stopped_installation_restores_full_env

deploy_stopped_installation_commits_authoritative_project() (
  local fixture status
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  capture_deploy_rollback_env ''

  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.1' \
    'ADMIN_PASSWORD=candidate-secret' \
    'FRONTEND_BIND=127.0.0.1' \
    'FRONTEND_PORT=1145' > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  download_geoip_database() { :; }
  list_deploy_container_names() { return 0; }
  run_deploy_compose() { :; }
  pin_deploy_image_after_pull() {
    NEWAPI_TOOLS_IMAGE="$resolved_test_image"
    export NEWAPI_TOOLS_IMAGE
  }
  start_deploy_services_and_wait() { :; }

  NEWAPI_TOOLS_IMAGE='ghcr.io/yujianwudi/new_api_tools:0.5.1'
  NEWAPI_TOOLS_EXPECTED_REVISION=''
  FRONTEND_BIND='127.0.0.1'
  FRONTEND_PORT='1145'
  ADMIN_PASSWORD='candidate-secret'
  AUTO_GENERATED_PASSWORD=false
  set +e
  (start_services) >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s|%s|%s\n' \
    "$status" \
    "$(env_file_value "$ENV_FILE" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "$ENV_FILE" COMPOSE_PROJECT_NAME)" \
    "$([[ -e "${ENV_FILE}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'deploy durably keeps the stopped installation project when a healthy candidate commits' \
  "0|${resolved_test_image}|old-project|absent" \
  deploy_stopped_installation_commits_authoritative_project

deploy_stopped_mutable_image_is_anchored_locally() (
  local fixture resolver_file
  fixture="$(mktemp -d)"
  resolver_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$resolver_file"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.0' \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  resolve_deploy_image_digest() {
    printf 'called\n' >> "$resolver_file"
    [[ "$1" == 'ghcr.io/yujianwudi/new_api_tools:0.5.0' && -z "$2" ]] || return 1
    printf '%s\n' "$previous_test_image"
  }
  capture_deploy_rollback_env ''
  printf '%s|%s|%s\n' \
    "$(wc -l < "$resolver_file" | tr -d '[:space:]')" \
    "$(env_file_value "${ENV_FILE}.rollback" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "${ENV_FILE}.rollback" COMPOSE_PROJECT_NAME)"
)

assert_eq \
  'deploy anchors a stopped mutable image to one local same-repository digest before migration' \
  "1|${previous_test_image}|old-project" \
  deploy_stopped_mutable_image_is_anchored_locally

deploy_stopped_installation_without_project_is_rejected() (
  local fixture
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'ADMIN_PASSWORD=old-secret' > "$ENV_FILE"
  chmod 600 "$ENV_FILE"
  capture_deploy_rollback_env ''
)

assert_rejected \
  'deploy refuses to guess the Compose project for a stopped installation' \
  deploy_stopped_installation_without_project_is_rejected

install_stopped_installation_restores_full_env() (
  local fixture order_file status env_file
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  env_file="${fixture}/.env"
  : > "${fixture}/docker-compose.yml"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' \
    'NEWAPI_NETWORK_MODE=custom' \
    'NEWAPI_NETWORK=old-network' \
    'OLD_ONLY=preserve-me' > "$env_file"
  chmod 600 "$env_file"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  list_install_container_names() { return 0; }
  capture_install_rollback_env "$env_file"

  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' \
    'NEWAPI_NETWORK_MODE=host' \
    'NEWAPI_NETWORK=' \
    'NEW_ONLY=remove-me' > "$env_file"
  chmod 600 "$env_file"
  setup_compose_files() { unset COMPOSE_FILE; }
  run_install_compose() { :; }
  start_install_services_and_wait() {
    printf 'start:%s:%s\n' "$3" "$4" >> "$order_file"
    [[ "$3" == 'rollback' ]]
  }

  NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  export NEWAPI_TOOLS_IMAGE
  set +e
  (restart_install_services_transactionally "$env_file" "$fixture") >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s|%s|%s|%s|%s|%s\n' \
    "$status" \
    "$(env_file_value "$env_file" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "$env_file" ADMIN_PASSWORD)" \
    "$(env_file_value "$env_file" NEWAPI_NETWORK)" \
    "$(env_file_value "$env_file" OLD_ONLY)" \
    "$(grep -c '^NEW_ONLY=' "$env_file" || true)" \
    "$([[ -e "${env_file}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'installer snapshots a stopped installation before migration and restores every old setting' \
  "1|${previous_test_image}|old-secret|old-network|preserve-me|0|present" \
  install_stopped_installation_restores_full_env

install_stopped_mutable_image_is_anchored_locally() (
  local fixture resolver_file env_file
  fixture="$(mktemp -d)"
  resolver_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$resolver_file"' EXIT
  env_file="${fixture}/.env"
  printf '%s\n' \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools:0.5.0' \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "$env_file"
  chmod 600 "$env_file"
  list_install_container_names() { return 0; }
  resolve_install_image_digest() {
    printf 'called\n' >> "$resolver_file"
    [[ "$1" == 'ghcr.io/yujianwudi/new_api_tools:0.5.0' && -z "$2" ]] || return 1
    printf '%s\n' "$previous_test_image"
  }
  capture_install_rollback_env "$env_file"
  printf '%s|%s|%s\n' \
    "$(wc -l < "$resolver_file" | tr -d '[:space:]')" \
    "$(env_file_value "${env_file}.rollback" NEWAPI_TOOLS_IMAGE)" \
    "$(env_file_value "${env_file}.rollback" COMPOSE_PROJECT_NAME)"
)

assert_eq \
  'installer anchors a stopped mutable image to one local same-repository digest before migration' \
  "1|${previous_test_image}|old-project" \
  install_stopped_mutable_image_is_anchored_locally

install_stopped_installation_without_project_is_rejected() (
  local fixture env_file
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'ADMIN_PASSWORD=old-secret' > "$env_file"
  chmod 600 "$env_file"
  list_install_container_names() { return 0; }
  capture_install_rollback_env "$env_file"
)

assert_rejected \
  'installer refuses to guess the Compose project for a stopped installation' \
  install_stopped_installation_without_project_is_rejected

deploy_uninstall_discards_crash_snapshot() (
  local fixture order_file
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${resolved_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' > "$ENV_FILE"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' \
    "SQL_DSN='host=old-db password=old-db-secret'" > "${ENV_FILE}.rollback"
  chmod 600 "$ENV_FILE" "${ENV_FILE}.rollback"

  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  configure_deploy_context_from_env() { :; }
  run_deploy_compose() {
    printf '%s:%s:%s\n' \
      "$3" "$2" "$(env_file_value "$1" ADMIN_PASSWORD)" >> "$order_file"
  }
  start_deploy_services_and_wait() { printf 'unexpected-recovery\n' >> "$order_file"; }

  uninstall
  capture_deploy_rollback_env ''
  recover_preexisting_deploy_rollback_snapshot
  printf '%s|%s|%s|%s|%s\n' \
    "$([[ -e "$ENV_FILE" || -L "$ENV_FILE" ]] && printf present || printf absent)" \
    "$([[ -e "${ENV_FILE}.rollback" || -L "${ENV_FILE}.rollback" ]] && printf present || printf absent)" \
    "$DEPLOY_ROLLBACK_ENV_AVAILABLE" \
    "$DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING" \
    "$(paste -sd, "$order_file")"
)

assert_eq \
  'deploy uninstall durably discards a crash snapshot so the next deploy cannot resurrect old secrets or services' \
  "absent|absent|false|false|down:${previous_test_image}:old-secret" \
  deploy_uninstall_discards_crash_snapshot

deploy_uninstall_cleanup_failure_keeps_recovery_files() (
  local fixture status
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${resolved_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=candidate-secret' > "$ENV_FILE"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "${ENV_FILE}.rollback"
  chmod 600 "$ENV_FILE" "${ENV_FILE}.rollback"
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  configure_deploy_context_from_env() { :; }
  run_deploy_compose() { return 42; }
  set +e
  (uninstall) >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s|%s\n' \
    "$status" \
    "$([[ -f "$ENV_FILE" ]] && printf present || printf absent)" \
    "$([[ -f "${ENV_FILE}.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'deploy uninstall never deletes recovery secrets or reports success after service cleanup fails' \
  '1|present|present' \
  deploy_uninstall_cleanup_failure_keeps_recovery_files

install_purge_discards_project_snapshot_durably() (
  local fixture project_dir order_file
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  project_dir="${fixture}/new_api_tools"
  mkdir -p "$project_dir"
  : > "${project_dir}/docker-compose.yml"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "${project_dir}/.env"
  cp "${project_dir}/.env" "${project_dir}/.env.rollback"
  chmod 600 "${project_dir}/.env" "${project_dir}/.env.rollback"

  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  run_install_compose() { printf 'down\n' >> "$order_file"; }
  docker() { return 0; }
  (do_purge_interactive "$project_dir" <<< 'DELETE') >/dev/null 2>&1
  printf '%s|%s\n' \
    "$([[ -e "$project_dir" || -L "$project_dir" ]] && printf present || printf absent)" \
    "$(paste -sd, "$order_file")"
)

assert_eq \
  'installer purge removes the project rollback snapshot only after successful service cleanup and parent sync' \
  'absent|down' \
  install_purge_discards_project_snapshot_durably

install_purge_cleanup_failure_keeps_project_snapshot() (
  local fixture project_dir status
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project_dir="${fixture}/new_api_tools"
  mkdir -p "$project_dir"
  : > "${project_dir}/docker-compose.yml"
  printf '%s\n' \
    "NEWAPI_TOOLS_IMAGE=${previous_test_image}" \
    'COMPOSE_PROJECT_NAME=old-project' \
    'ADMIN_PASSWORD=old-secret' > "${project_dir}/.env"
  cp "${project_dir}/.env" "${project_dir}/.env.rollback"
  chmod 600 "${project_dir}/.env" "${project_dir}/.env.rollback"
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  run_install_compose() { return 42; }
  docker() { return 0; }
  set +e
  (do_purge_interactive "$project_dir" <<< 'DELETE') >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s|%s\n' \
    "$status" \
    "$([[ -d "$project_dir" ]] && printf present || printf absent)" \
    "$([[ -f "${project_dir}/.env.rollback" ]] && printf present || printf absent)"
)

assert_eq \
  'installer purge retains the full project and crash snapshot when service cleanup fails' \
  '1|present|present' \
  install_purge_cleanup_failure_keeps_project_snapshot

deploy_uninstall_without_config_rejects_live_container() (
  local fixture
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  list_deploy_container_names() { printf 'newapi-tools\n'; }
  uninstall
)

assert_rejected \
  'deploy uninstall refuses to report success when a live project container has no auditable config' \
  deploy_uninstall_without_config_rejects_live_container

deploy_durable_delete_sync_failure_is_rejected() (
  local fixture target
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  target="${fixture}/secret.env"
  printf 'secret\n' > "$target"
  sync() { return 1; }
  durable_remove_deploy_file "$target"
)

assert_rejected \
  'deploy durable config deletion reports a parent-directory sync failure' \
  deploy_durable_delete_sync_failure_is_rejected

install_durable_tree_sync_failure_is_rejected() (
  local fixture target
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  target="${fixture}/new_api_tools"
  mkdir -p "$target"
  printf 'secret\n' > "${target}/.env.rollback"
  sync() { return 1; }
  durable_remove_install_tree "$target"
)

assert_rejected \
  'installer durable project deletion reports a parent-directory sync failure' \
  install_durable_tree_sync_failure_is_rejected

deploy_generate_env_atomic_result() (
  local mode="$1" fixture status target_kind tmp_count file_mode image password
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  ENV_FILE="${fixture}/.env"
  SOURCE_SQL_DSN=''
  DB_ENGINE='postgres'
  DB_DNS='db.internal'
  DB_PORT='5432'
  DB_USER='newapi'
  DB_PASSWORD='db-secret'
  DB_NAME='newapi'
  NEWAPI_CONTAINER='new-api'
  NEWAPI_NETWORK='old-network'
  ORIGINAL_NETWORK='bridge'
  LOG_NETWORK=''
  LOG_SQL_DSN_FINAL=''
  NEWAPI_TOOLS_IMAGE="$resolved_test_image"
  NEWAPI_ADMIN_ACCESS_TOKEN=''
  NEWAPI_ADMIN_USER_ID='0'
  NEWAPI_REDIS_DISABLED=false
  ADMIN_PASSWORD='admin-secret'
  API_KEY='api-secret'
  API_KEY_ROLE='admin'
  FRONTEND_PORT='1145'
  FRONTEND_BIND='127.0.0.1'
  TRUSTED_PROXY_CIDRS='127.0.0.1/32,::1/128'
  LOG_FRESHNESS_MAX_SECONDS='900'
  USE_HOST_MODE=false
  USE_BRIDGE_MODE=false
  DEPLOY_ENV_GENERATED_THIS_RUN=false
  unset NEWAPI_BASEURL OBSERVABILITY_TOKEN
  log_info() { :; }
  log_success() { :; }
  log_error() { :; }
  docker_inspect_env_value() { return 1; }
  openssl() { printf '%064d\n' 0; }
  if [[ "$mode" == 'rename-fail' ]]; then
    mv() { return 1; }
  fi

  set +e
  (generate_env_file) >/dev/null 2>&1
  status=$?
  set -e
  target_kind="$([[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] && printf regular || printf absent)"
  tmp_count="$(find "$fixture" -maxdepth 1 -name '.env.tmp.*' -print | wc -l | tr -d '[:space:]')"
  file_mode="$([[ -f "$ENV_FILE" ]] && stat -c '%a' "$ENV_FILE" || printf none)"
  image="$(env_file_value "$ENV_FILE" NEWAPI_TOOLS_IMAGE)"
  password="$(env_file_value "$ENV_FILE" ADMIN_PASSWORD)"
  printf '%s|%s|%s|%s|%s|%s\n' \
    "$status" "$target_kind" "$tmp_count" "$file_mode" "$image" "$password"
)

assert_eq \
  'fresh deploy generation atomically publishes one complete mode-600 dotenv' \
  "0|regular|0|600|${resolved_test_image}|admin-secret" \
  deploy_generate_env_atomic_result success

assert_eq \
  'fresh deploy generation leaves no partial dotenv or temp file when rename fails' \
  '1|absent|0|none||' \
  deploy_generate_env_atomic_result rename-fail

install_migration_rename_failure_preserves_old_dotenv() (
  local fixture env_file before after status tmp_count
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf '%s\n' \
    'NEWAPI_BASEURL=' \
    'NEWAPI_CONTAINER=new-api-test' \
    'NEWAPI_NETWORK_MODE=bridge' \
    'ADMIN_PASSWORD=old-secret' > "$env_file"
  chmod 640 "$env_file"
  before="$(sha256sum "$env_file" | awk '{print $1}')"
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  migrate_image_env_file() { :; }
  openssl() { printf '%064d\n' 0; }
  docker() { return 1; }
  mv() { return 1; }
  set +e
  (migrate_env_file "$fixture") >/dev/null 2>&1
  status=$?
  set -e
  after="$(sha256sum "$env_file" | awk '{print $1}')"
  tmp_count="$(find "$fixture" -maxdepth 1 -name '.env.tmp.*' -print | wc -l | tr -d '[:space:]')"
  printf '%s|%s|%s|%s\n' \
    "$status" \
    "$([[ "$before" == "$after" ]] && printf unchanged || printf changed)" \
    "$(stat -c '%a' "$env_file")" \
    "$tmp_count"
)

assert_eq \
  'installer migration never exposes a partially appended dotenv when rename fails' \
  '1|unchanged|640|0' \
  install_migration_rename_failure_preserves_old_dotenv

install_safety_rewrite_rename_failure_preserves_old_dotenv() (
  local fixture env_file before after status tmp_count
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  env_file="${fixture}/.env"
  printf '%s\n' \
    'NEWAPI_CONTAINER=new-api-test' \
    'ADMIN_PASSWORD=old-secret' > "$env_file"
  chmod 640 "$env_file"
  before="$(sha256sum "$env_file" | awk '{print $1}')"
  log_info() { :; }
  log_warn() { :; }
  log_success() { :; }
  log_error() { :; }
  docker() { printf 'REDIS_CONN_STRING=\n'; }
  mv() { return 1; }
  set +e
  (sync_newapi_mutation_safety_config "$fixture") >/dev/null 2>&1
  status=$?
  set -e
  after="$(sha256sum "$env_file" | awk '{print $1}')"
  tmp_count="$(find "$fixture" -maxdepth 1 -name '.env.tmp.*' -print | wc -l | tr -d '[:space:]')"
  printf '%s|%s|%s|%s\n' \
    "$status" \
    "$([[ "$before" == "$after" ]] && printf unchanged || printf changed)" \
    "$(stat -c '%a' "$env_file")" \
    "$tmp_count"
)

assert_eq \
  'installer safety synchronization keeps the old dotenv intact when atomic rename fails' \
  '1|unchanged|640|0' \
  install_safety_rewrite_rename_failure_preserves_old_dotenv

project_lock_path_result() (
  local fixture project absent_path existing_path relationship
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project="${fixture}/new_api_tools"
  absent_path="$(project_state_lock_path "$project")"
  mkdir "$project"
  existing_path="$(project_state_lock_path "$project")"
  [[ "$absent_path" == "$existing_path" ]] && relationship='same' || relationship='different'
  printf '%s|%s\n' "$relationship" "$(basename -- "$existing_path")"
)

assert_eq \
  'first install and existing checkout resolve the same sibling state lock' \
  'same|.new_api_tools.state.lock' \
  project_lock_path_result

project_lock_serialization_result() (
  local fixture project ready release holder blocked_after_purge acquired_after_release
  local lock_path lock_mode lock_empty
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project="${fixture}/new_api_tools"
  mkdir "$project"
  ready="${fixture}/ready"
  release="${fixture}/release"
  mkfifo "$ready" "$release"
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }

  (
    acquire_deploy_state_lock "$project"
    printf 'ready\n' > "$ready"
    read -r _ < "$release"
  ) &
  holder=$!
  read -r _ < "$ready"

  if (acquire_install_state_lock "$project") >/dev/null 2>&1; then
    blocked_after_purge='not-blocked-before-purge'
  else
    blocked_after_purge='blocked-before-purge'
  fi

  rm -rf -- "$project"
  if (acquire_install_state_lock "$project") >/dev/null 2>&1; then
    blocked_after_purge="${blocked_after_purge},not-blocked-after-purge"
  else
    blocked_after_purge="${blocked_after_purge},blocked-after-purge"
  fi

  printf 'release\n' > "$release"
  wait "$holder"
  if (acquire_install_state_lock "$project") >/dev/null 2>&1; then
    acquired_after_release='released'
  else
    acquired_after_release='still-blocked'
  fi

  lock_path="$(project_state_lock_path "$project")"
  lock_mode="$(stat -c '%a' -- "$lock_path")"
  [[ ! -s "$lock_path" ]] && lock_empty='empty' || lock_empty='not-empty'
  printf '%s|%s|%s|%s\n' \
    "$blocked_after_purge" "$acquired_after_release" "$lock_mode" "$lock_empty"
)

assert_eq \
  'deploy/install mutual exclusion survives project purge and leaves a secret-free lock' \
  'blocked-before-purge,blocked-after-purge|released|600|empty' \
  project_lock_serialization_result

project_lock_exec_handoff_result() (
  local fixture project
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project="${fixture}/new_api_tools"
  mkdir "$project"
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }
  acquire_install_state_lock "$project"
  bash -c '
    set -euo pipefail
    source "$1"
    log_error() { :; }
    acquire_deploy_state_lock "$2"
    [[ "$DEPLOY_STATE_LOCK_FD" == "$NEWAPI_TOOLS_STATE_LOCK_FD" ]]
    printf adopted
  ' _ "${REPO_ROOT}/deploy.sh" "$project"
)

assert_eq \
  'installer hands the held flock fd to exec deploy without a self-deadlock window' \
  'adopted' \
  project_lock_exec_handoff_result

project_lock_crash_release_result() (
  local fixture project ready crash holder result
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project="${fixture}/new_api_tools"
  ready="${fixture}/ready"
  crash="${fixture}/crash"
  mkfifo "$ready" "$crash"
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }

  (
    acquire_deploy_state_lock "$project"
    printf 'ready\n' > "$ready"
    read -r _ < "$crash"
    kill -KILL "$BASHPID"
  ) &
  holder=$!
  read -r _ < "$ready"
  printf 'crash\n' > "$crash"
  wait "$holder" 2>/dev/null || true
  if (acquire_install_state_lock "$project") >/dev/null 2>&1; then
    result='released-after-crash'
  else
    result='still-locked-after-crash'
  fi
  printf '%s\n' "$result"
)

assert_eq \
  'kernel releases the project state lock when the maintenance process crashes' \
  'released-after-crash' \
  project_lock_crash_release_result

project_lock_symlink_result() (
  local fixture project lock_path victim status
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project="${fixture}/new_api_tools"
  mkdir "$project"
  lock_path="$(project_state_lock_path "$project")"
  victim="${fixture}/victim"
  printf 'victim-content\n' > "$victim"
  ln -s "$victim" "$lock_path"
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }
  set +e
  (acquire_deploy_state_lock "$project") >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(<"$victim")"
)

assert_eq \
  'project state lock refuses a symlink without touching its target' \
  '1|victim-content' \
  project_lock_symlink_result

project_lock_mode_repair_result() (
  local fixture project lock_path
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project="${fixture}/new_api_tools"
  mkdir "$project"
  lock_path="$(project_state_lock_path "$project")"
  : > "$lock_path"
  chmod 666 "$lock_path"
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }
  acquire_deploy_state_lock "$project"
  printf '%s|%s\n' \
    "$(stat -c '%a' -- "$lock_path")" \
    "$([[ "$(stat -c '%u' -- "$lock_path")" == "$(id -u)" ]] && printf owner-ok || printf owner-bad)"
)

assert_eq \
  'current-user world-writable state lock is repaired and verified as mode 600' \
  '600|owner-ok' \
  project_lock_mode_repair_result

project_lock_foreign_owner_result() (
  local fixture project lock_path status
  fixture="$(mktemp -d)"
  trap 'rm -rf "$fixture"' EXIT
  project="${fixture}/new_api_tools"
  mkdir "$project"
  lock_path="$(project_state_lock_path "$project")"
  : > "$lock_path"
  chmod 600 "$lock_path"
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH
  log_error() { :; }
  state_lock_path_owner() { printf '%s\n' "$(( $(id -u) + 1 ))"; }
  set +e
  (acquire_install_state_lock "$project") >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s\n' "$status" "$(stat -c '%a' -- "$lock_path")"
)

assert_eq \
  'state lock reported as foreign-owned is rejected before opening' \
  '1|600' \
  project_lock_foreign_owner_result

project_lock_entrypoint_wiring() {
  local deploy_count install_count
  deploy_count="$(grep -cF 'acquire_deploy_state_lock "$SCRIPT_DIR"' "${REPO_ROOT}/deploy.sh")"
  install_count="$(grep -cF 'acquire_install_state_lock "${INSTALL_DIR}/${PROJECT_NAME}"' "${REPO_ROOT}/install.sh")"
  printf '%s|%s\n' "$deploy_count" "$install_count"
}

assert_eq \
  'mutating deploy and installer entrypoints acquire the shared project state lock' \
  '2|1' \
  project_lock_entrypoint_wiring

unsafe_dotenv_write_paths() {
  grep -En \
    'cat[[:space:]]*>[[:space:]]*"\$ENV_FILE"|>>[[:space:]]*("\$(ENV_FILE|env_file)"|\.env)|sed[[:space:]]+-i[^#]*(ENV_FILE|env_file|\.env)|rm[[:space:]]+-f[[:space:]]+("\$(ENV_FILE|env_file)"|\.env)' \
    "${REPO_ROOT}/deploy.sh" "${REPO_ROOT}/install.sh" || true
}

assert_eq \
  'deploy and installer contain no truncate/append/sed/rm direct dotenv write path' \
  '' \
  unsafe_dotenv_write_paths

deploy_existing_container_without_env_is_fail_closed() (
  local fixture order_file status
  fixture="$(mktemp -d)"
  order_file="$(mktemp)"
  trap 'rm -rf "$fixture"; rm -f "$order_file"' EXIT
  ENV_FILE="${fixture}/.env"
  generate_env_file() { printf 'generate-called\n' >> "$order_file"; }
  start_services() { printf 'start-called\n' >> "$order_file"; }
  set +e
  (capture_deploy_rollback_env 'newapi-tools'; generate_env_file; start_services) >/dev/null 2>&1
  status=$?
  set -e
  printf '%s|%s|%s\n' \
    "$status" "$(paste -sd, "$order_file")" "$([[ -e "$ENV_FILE" ]] && printf present || printf absent)"
)

assert_eq \
  'an existing container without an old dotenv aborts before generate or service mutation' \
  '1||absent' \
  deploy_existing_container_without_env_is_fail_closed

if (( failures > 0 )); then
  printf '%d deployment test(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'all deployment tests passed\n'
