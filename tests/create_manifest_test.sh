#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="$repo_root/scripts/ci/create-manifest.sh"
work_dir="$(mktemp -d)"
trap 'rm -rf -- "$work_dir"' EXIT

mock_bin="$work_dir/bin"
digest_dir="$work_dir/digests"
call_log="$work_dir/docker.args"
mkdir -p "$mock_bin" "$digest_dir"

cat > "$mock_bin/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$@" > "$CALL_LOG"
EOF
chmod +x "$mock_bin/docker"

registry='ghcr.io'
image_name='yujianwudi/new_api_tools'
digest_a='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
digest_b='bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
digest_c='cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc'
metadata='{"tags":["ghcr.io/yujianwudi/new_api_tools:latest","ghcr.io/yujianwudi/new_api_tools:sha"]}'

reset_case() {
  rm -rf -- "$digest_dir"
  mkdir -p "$digest_dir"
  rm -f -- "$call_log"
}

run_manifest() {
  PATH="$mock_bin:$PATH" \
    CALL_LOG="$call_log" \
    REGISTRY="$registry" \
    IMAGE_NAME="$image_name" \
    DOCKER_METADATA_OUTPUT_JSON="$1" \
    bash "$script" "$digest_dir"
}

expect_failure() {
  local label="$1" case_metadata="$2"
  if run_manifest "$case_metadata" >/dev/null 2>&1; then
    printf 'expected manifest case to fail: %s\n' "$label" >&2
    exit 1
  fi
  [[ ! -e "$call_log" ]] || {
    printf 'failed manifest case reached docker: %s\n' "$label" >&2
    exit 1
  }
}

touch "$digest_dir/$digest_a" "$digest_dir/$digest_b"
run_manifest "$metadata"
mapfile -t actual_args < "$call_log"
expected_args=(
  buildx
  imagetools
  create
  -t
  'ghcr.io/yujianwudi/new_api_tools:latest'
  -t
  'ghcr.io/yujianwudi/new_api_tools:sha'
  "ghcr.io/yujianwudi/new_api_tools@sha256:${digest_a}"
  "ghcr.io/yujianwudi/new_api_tools@sha256:${digest_b}"
)
[[ "${actual_args[*]}" == "${expected_args[*]}" ]] || {
  printf 'manifest arguments were not passed as a closed array\n' >&2
  exit 1
}

reset_case
expect_failure 'zero digest files' "$metadata"

reset_case
touch "$digest_dir/$digest_a"
expect_failure 'one digest file' "$metadata"

reset_case
touch "$digest_dir/$digest_a" "$digest_dir/$digest_b" "$digest_dir/$digest_c"
expect_failure 'three digest files' "$metadata"

reset_case
touch "$digest_dir/$digest_a" "$digest_dir/not-a-digest"
expect_failure 'invalid digest file' "$metadata"

reset_case
touch "$digest_dir/$digest_a" "$digest_dir/$digest_b"
expect_failure 'empty metadata tags' '{"tags":[]}'
expect_failure 'unexpected metadata tag' '{"tags":["ghcr.io/other/image:latest"]}'

printf 'PASS: manifest creation is array-safe and fails closed\n'
