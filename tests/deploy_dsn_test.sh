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

if (( failures > 0 )); then
  printf '%d DSN parser test(s) failed\n' "$failures" >&2
  exit 1
fi

printf 'all DSN parser tests passed\n'
