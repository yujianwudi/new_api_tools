#!/usr/bin/env bash
set -euo pipefail

#######################################
# NewAPI Middleware Tool - 一键部署脚本
# 
# 功能:
#   1. 自动检测 NewAPI 容器和数据库配置
#   2. 交互式配置前端密码和 API Key
#   3. 生成 .env 配置文件
#   4. 启动 Docker Compose 服务
#
# 使用方法:
#   ./deploy.sh              # 交互式部署
#   ./deploy.sh --uninstall  # 卸载服务
#   ./deploy.sh --status     # 查看状态
#######################################

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${SCRIPT_DIR}/.env"
COMPOSE_FILE="${SCRIPT_DIR}/docker-compose.yml"
COMPOSE_HOST_FILE="${SCRIPT_DIR}/docker-compose.host.yml"
COMPOSE_LOGDB_FILE="${SCRIPT_DIR}/docker-compose.logdb.yml"
SOURCE_SQL_DSN=""
NEWAPI_TOOLS_IMAGE_REPOSITORY="ghcr.io/yujianwudi/new_api_tools"
NEWAPI_TOOLS_SIGNING_REPOSITORY="yujianwudi/new_api_tools"
NEWAPI_TOOLS_COSIGN_IMAGE="ghcr.io/sigstore/cosign/cosign:v3.1.2@sha256:d91bc4e7e95e8d2f549c747a72dc174f90579e410a1695f57f686674f84ce849"
REQUESTED_NEWAPI_TOOLS_IMAGE="${NEWAPI_TOOLS_IMAGE:-}"
REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="${NEWAPI_TOOLS_EXPECTED_REVISION:-}"
REQUESTED_NEWAPI_TOOLS_RELEASE_TAG="${NEWAPI_TOOLS_RELEASE_TAG:-}"
NEWAPI_TOOLS_IMAGE_DERIVED=false
NEWAPI_TOOLS_EXPECTED_REVISION=""
NEWAPI_TOOLS_RELEASE_TAG=""
DEPLOY_ROLLBACK_ENV_AVAILABLE=false
DEPLOY_ROLLBACK_ENV_CONTENT=""
DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=false
DEPLOY_ENV_GENERATED_THIS_RUN=false
DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE=""
DEPLOY_STATE_LOCK_FD=""
DEPLOY_STATE_LOCK_PATH=""

# 由 detect_environment() 设置：host 模式下需要追加 overlay compose 文件
COMPOSE_FILES=("-f" "$COMPOSE_FILE")

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die() { log_error "$*"; exit 1; }

TOOLSTORE_TXN_LIBRARY_SOURCE="${SCRIPT_DIR}/scripts/toolstore_transaction.sh"
[[ -f "$TOOLSTORE_TXN_LIBRARY_SOURCE" && ! -L "$TOOLSTORE_TXN_LIBRARY_SOURCE" ]] ||
  die "缺少安全的 Tool Store 事务库：${TOOLSTORE_TXN_LIBRARY_SOURCE}"
# shellcheck source=scripts/toolstore_transaction.sh
source "$TOOLSTORE_TXN_LIBRARY_SOURCE"

# Keep the lock beside the project directory instead of inside it. This gives
# first installs a stable lock before the target exists and prevents purge from
# unlinking the active lock inode while another process is still excluded.
project_state_lock_path() {
  local project_dir="$1" resolved parent base
  [[ -n "$project_dir" && "$project_dir" != "/" ]] || return 1
  if [[ -d "$project_dir" ]]; then
    resolved="$(cd -- "$project_dir" && pwd -P)" || return 1
    parent="$(dirname -- "$resolved")"
    base="$(basename -- "$resolved")"
  else
    parent="$(dirname -- "$project_dir")"
    base="$(basename -- "$project_dir")"
    [[ -d "$parent" ]] || return 1
    parent="$(cd -- "$parent" && pwd -P)" || return 1
  fi
  [[ -n "$base" && "$base" != "." && "$base" != ".." && "$base" != "/" ]] || return 1
  printf '%s/.%s.state.lock\n' "$parent" "$base"
}

state_lock_fd_matches_path() {
  local fd="$1" path="$2" fd_path fd_identity path_identity
  [[ "$fd" =~ ^[0-9]+$ && -f "$path" && ! -L "$path" ]] || return 1
  fd_path="/proc/${BASHPID}/fd/${fd}"
  [[ -e "$fd_path" ]] || return 1
  fd_identity="$(stat -Lc '%d:%i' -- "$fd_path" 2>/dev/null)" || return 1
  path_identity="$(stat -Lc '%d:%i' -- "$path" 2>/dev/null)" || return 1
  [[ "$fd_identity" == "$path_identity" ]]
}

state_lock_current_uid() { id -u; }
state_lock_path_owner() { stat -c '%u' -- "$1" 2>/dev/null; }

state_lock_fd_is_secure() {
  local fd="$1" path="$2" fd_path owner mode
  state_lock_fd_matches_path "$fd" "$path" || return 1
  fd_path="/proc/${BASHPID}/fd/${fd}"
  owner="$(stat -Lc '%u' -- "$fd_path" 2>/dev/null)" || return 1
  mode="$(stat -Lc '%a' -- "$fd_path" 2>/dev/null)" || return 1
  [[ "$owner" == "$(state_lock_current_uid)" && "$mode" == "600" ]]
}

acquire_deploy_state_lock() {
  local project_dir="$1" lock_path inherited_fd inherited_path old_umask
  lock_path="$(project_state_lock_path "$project_dir")" ||
    die "无法确定部署状态锁路径"

  inherited_fd="${NEWAPI_TOOLS_STATE_LOCK_FD:-}"
  inherited_path="${NEWAPI_TOOLS_STATE_LOCK_PATH:-}"
  if [[ "$inherited_path" == "$lock_path" ]] &&
    state_lock_fd_is_secure "$inherited_fd" "$lock_path"; then
    flock -n "$inherited_fd" || die "继承的部署状态锁已失效"
    DEPLOY_STATE_LOCK_FD="$inherited_fd"
    DEPLOY_STATE_LOCK_PATH="$lock_path"
    return 0
  fi
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH

  if [[ -e "$lock_path" || -L "$lock_path" ]]; then
    [[ -f "$lock_path" && ! -L "$lock_path" ]] ||
      die "部署状态锁不是安全的常规文件：${lock_path}"
    [[ "$(state_lock_path_owner "$lock_path")" == "$(state_lock_current_uid)" ]] ||
      die "部署状态锁不属于当前用户：${lock_path}"
  fi
  old_umask="$(umask)"
  umask 077
  if ! exec {DEPLOY_STATE_LOCK_FD}>>"$lock_path"; then
    umask "$old_umask"
    die "无法打开部署状态锁：${lock_path}"
  fi
  umask "$old_umask"
  state_lock_fd_matches_path "$DEPLOY_STATE_LOCK_FD" "$lock_path" ||
    die "部署状态锁在打开时发生替换：${lock_path}"
  [[ "$(stat -Lc '%u' -- "/proc/${BASHPID}/fd/${DEPLOY_STATE_LOCK_FD}" 2>/dev/null)" == \
    "$(state_lock_current_uid)" ]] ||
    die "部署状态锁打开后不属于当前用户：${lock_path}"
  chmod 600 "/proc/${BASHPID}/fd/${DEPLOY_STATE_LOCK_FD}" ||
    die "无法收紧部署状态锁权限：${lock_path}"
  state_lock_fd_is_secure "$DEPLOY_STATE_LOCK_FD" "$lock_path" ||
    die "部署状态锁权限或身份无效：${lock_path}"
  flock -n "$DEPLOY_STATE_LOCK_FD" ||
    die "另一个安装、部署或卸载进程正在操作该项目：${lock_path}"
  state_lock_fd_matches_path "$DEPLOY_STATE_LOCK_FD" "$lock_path" ||
    die "部署状态锁在加锁时发生替换：${lock_path}"

  DEPLOY_STATE_LOCK_PATH="$lock_path"
  export NEWAPI_TOOLS_STATE_LOCK_FD="$DEPLOY_STATE_LOCK_FD"
  export NEWAPI_TOOLS_STATE_LOCK_PATH="$DEPLOY_STATE_LOCK_PATH"
}

validate_newapi_tools_image() {
  local image="${1:-}"
  [[ -n "$image" ]] || die "NEWAPI_TOOLS_IMAGE 不能为空"
  (( ${#image} <= 512 )) || die "NEWAPI_TOOLS_IMAGE 过长"
  [[ ! "$image" =~ [[:space:][:cntrl:]] ]] ||
    die "NEWAPI_TOOLS_IMAGE 不能包含空白或控制字符"
  [[ "$image" != -* ]] || die "NEWAPI_TOOLS_IMAGE 格式无效"
  if [[ "$image" == *@* ]]; then
    [[ "$image" =~ ^[^@]+@sha256:[0-9a-f]{64}$ ]] ||
      die "NEWAPI_TOOLS_IMAGE digest 必须使用 repo@sha256:<64 位小写十六进制>"
  fi
}

is_immutable_newapi_tools_image() {
  [[ "${1:-}" =~ ^[^@]+@sha256:[0-9a-f]{64}$ ]]
}

is_valid_tcp_port() {
  local port="${1:-}"
  [[ "$port" =~ ^[0-9]{1,5}$ ]] || return 1
  (( 10#$port >= 1 && 10#$port <= 65535 ))
}

env_file_value() {
  local env_file="$1" key="$2" value
  value="$(awk -v k="$key" '
    index($0, k "=") == 1 { value = substr($0, length(k)+2); found = 1 }
    END { if (found) print value }
  ' "$env_file" 2>/dev/null || true)"
  value="${value%$'\r'}"
  if [[ ${#value} -ge 2 && "$value" == \'*\' ]]; then
    value="${value:1:${#value}-2}"
    value="${value//\\\'/\'}"
  elif [[ ${#value} -ge 2 && "$value" == \"*\" ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s\n' "$value"
}

env_content_value() {
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

validate_deploy_credential_separation_content() {
  local content="$1" index prior value
  local -a names=(
    ADMIN_PASSWORD
    API_KEY
    JWT_SECRET
    NEWAPI_ADMIN_ACCESS_TOKEN
    MODEL_PROBE_API_KEY
    OBSERVABILITY_TOKEN
  )
  local -a values=()

  for index in "${!names[@]}"; do
    value="$(env_content_value "$content" "${names[index]}")"
    value="${value#"${value%%[![:space:]]*}"}"
    value="${value%"${value##*[![:space:]]}"}"
    values[index]="$value"
    [[ -n "$value" ]] || continue
    for ((prior = 0; prior < index; prior++)); do
      if [[ -n "${values[prior]:-}" && "$value" == "${values[prior]}" ]]; then
        log_error "凭据变量 ${names[prior]} 与 ${names[index]} 不得复用同一非空值"
        return 1
      fi
    done
  done
}

validate_deploy_credential_separation_file() {
  local env_file="$1"
  [[ -f "$env_file" && ! -L "$env_file" && -r "$env_file" ]] || {
    log_error "凭据预检要求安全且可读的常规 .env 文件"
    return 1
  }
  validate_deploy_credential_separation_content "$(<"$env_file")"
}

atomic_write_deploy_dotenv() {
  local target="$1" content="$2" tmp mode
  local parent
  parent="$(dirname "$target")"

  # Refuse an existing directory. A symlink is safe to replace only because
  # mv -T treats the destination as a path entry rather than following a
  # symlink-to-directory. The final checks catch a concurrent replacement.
  [[ ! -d "$target" ]] || return 1
  tmp="$(umask 077; mktemp "${target}.tmp.XXXXXX")" || return 1
  if ! printf '%s\n' "$content" > "$tmp" ||
    ! chmod 600 "$tmp" ||
    ! sync -f "$tmp" ||
    ! mv -Tf -- "$tmp" "$target" ||
    ! sync -f "$parent"; then
    rm -f -- "$tmp"
    return 1
  fi

  [[ -f "$target" && ! -L "$target" ]] || return 1
  mode="$(stat -c '%a' -- "$target" 2>/dev/null)" || return 1
  [[ "$mode" == "600" ]]
}

persist_deploy_image_env() {
  local env_file="$1" image="$2" content
  [[ -f "$env_file" && ! -L "$env_file" ]] ||
    die "无法持久化部署镜像：配置文件不存在或不是安全的常规文件"
  validate_newapi_tools_image "$image"

  content="$(
    awk 'index($0, "NEWAPI_TOOLS_IMAGE=") == 1 { next } { print }' "$env_file"
    printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$(dotenv_quote "$image")"
  )"
  atomic_write_deploy_dotenv "$env_file" "$content"
}

persist_deploy_compose_project_env() {
  local env_file="$1" project_name="$2" content
  [[ -f "$env_file" && ! -L "$env_file" ]] ||
    die "无法持久化 Compose 项目标识：配置文件不存在或不是安全的常规文件"
  [[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
    die "COMPOSE_PROJECT_NAME 格式无效"
  content="$(
    awk 'index($0, "COMPOSE_PROJECT_NAME=") == 1 { next } { print }' "$env_file"
    printf 'COMPOSE_PROJECT_NAME=%s\n' "$(dotenv_quote "$project_name")"
  )"
  atomic_write_deploy_dotenv "$env_file" "$content"
}

restore_deploy_env_content() {
  local env_file="$1" content="$2" image="$3"
  [[ ! -d "$env_file" ]] || die "无法恢复部署配置：目标是目录"
  atomic_write_deploy_dotenv "$env_file" "$content"
  persist_deploy_image_env "$env_file" "$image"
}

restore_deploy_env_snapshot() {
  local env_file="$1" content="$2"
  atomic_write_deploy_dotenv "$env_file" "$content"
}

deploy_rollback_snapshot_path() {
  printf '%s.rollback\n' "$ENV_FILE"
}

normalize_deploy_rollback_env_content() {
  local content="$1" image="$2" project_name="$3"
  is_immutable_newapi_tools_image "$image" || return 1
  [[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || return 1
  printf '%s\n' "$content" |
    awk '
      index($0, "NEWAPI_TOOLS_IMAGE=") == 1 { next }
      index($0, "COMPOSE_PROJECT_NAME=") == 1 { next }
      { print }
    '
  printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$(dotenv_quote "$image")"
  printf 'COMPOSE_PROJECT_NAME=%s\n' "$(dotenv_quote "$project_name")"
}

persist_deploy_rollback_snapshot() {
  local content="$1" snapshot_file
  snapshot_file="$(deploy_rollback_snapshot_path)"
  atomic_write_deploy_dotenv "$snapshot_file" "$content"
}

load_deploy_rollback_snapshot() {
  local snapshot_file mode snapshot_fd fd_path path_identity fd_identity status
  snapshot_file="$(deploy_rollback_snapshot_path)"
  [[ -f "$snapshot_file" && ! -L "$snapshot_file" && -r "$snapshot_file" ]] || return 1
  exec {snapshot_fd}< "$snapshot_file" || return 1
  fd_path="/proc/${BASHPID}/fd/${snapshot_fd}"
  if [[ ! -e "$fd_path" ]] ||
    ! path_identity="$(stat -Lc '%d:%i' -- "$snapshot_file" 2>/dev/null)" ||
    ! fd_identity="$(stat -Lc '%d:%i' -- "$fd_path" 2>/dev/null)" ||
    [[ "$path_identity" != "$fd_identity" ]] ||
    [[ -L "$snapshot_file" ]] ||
    ! mode="$(stat -Lc '%a' -- "$fd_path" 2>/dev/null)" ||
    [[ "$mode" != "600" ]]; then
    exec {snapshot_fd}<&-
    return 1
  fi
  cat <&"$snapshot_fd"
  status=$?
  exec {snapshot_fd}<&-
  return "$status"
}

ensure_deploy_rollback_snapshot() {
  local snapshot_file content
  [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]] || return 0
  snapshot_file="$(deploy_rollback_snapshot_path)"
  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    content="$(load_deploy_rollback_snapshot)" || return 1
    DEPLOY_ROLLBACK_ENV_CONTENT="$content"
    return 0
  fi
  persist_deploy_rollback_snapshot "$DEPLOY_ROLLBACK_ENV_CONTENT"
}

restore_active_deploy_rollback_snapshot() {
  local fallback_content="${1:-}" content
  content="$fallback_content"
  if [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
    content="$(load_deploy_rollback_snapshot)" || return 1
  elif [[ -z "$fallback_content" ]]; then
    return 1
  fi
  restore_deploy_env_snapshot "$ENV_FILE" "$content"
}

remove_deploy_rollback_snapshot() {
  local snapshot_file parent
  snapshot_file="$(deploy_rollback_snapshot_path)"
  parent="$(dirname "$snapshot_file")"
  [[ -f "$snapshot_file" && ! -L "$snapshot_file" ]] || return 1
  rm -f -- "$snapshot_file" || return 1
  [[ ! -e "$snapshot_file" && ! -L "$snapshot_file" ]] || return 1
  if ! sync -f "$parent"; then
    # A cleanup failure must not discard the only durable rollback copy.
    persist_deploy_rollback_snapshot "$DEPLOY_ROLLBACK_ENV_CONTENT" || true
    return 1
  fi
}

start_and_finalize_committed_deploy_candidate() {
  local candidate_image="$1" snapshot_file rollback_content cleanup_ok=true
  snapshot_file="$(deploy_rollback_snapshot_path)"

  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    if rollback_content="$(load_deploy_rollback_snapshot)"; then
      DEPLOY_ROLLBACK_ENV_CONTENT="$rollback_content"
      DEPLOY_ROLLBACK_ENV_AVAILABLE=true
      if remove_deploy_rollback_snapshot; then
        DEPLOY_ROLLBACK_ENV_AVAILABLE=false
        DEPLOY_ROLLBACK_ENV_CONTENT=""
        DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=false
      else
        cleanup_ok=false
        log_warn "Tool Store 已提交；旧部署回滚快照未清理，仍将启动正式候选，禁止恢复旧 schema"
      fi
    else
      DEPLOY_ROLLBACK_ENV_CONTENT=""
      DEPLOY_ROLLBACK_ENV_AVAILABLE=false
      cleanup_ok=false
      log_warn "Tool Store 已提交；旧部署回滚快照不可读，已保留该文件和提交证据，仍将启动正式候选"
    fi
  else
    DEPLOY_ROLLBACK_ENV_AVAILABLE=false
    DEPLOY_ROLLBACK_ENV_CONTENT=""
    DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=false
  fi

  if ! start_deploy_services_and_wait candidate "$candidate_image"; then
    log_error "Tool Store 已提交且不能安全回滚；正式候选启动失败，提交证据与未清理文件均已保留"
    return 1
  fi

  if [[ "$cleanup_ok" == "true" ]]; then
    if ! toolstore_txn_finish_committed "$SCRIPT_DIR"; then
      cleanup_ok=false
      log_warn "正式候选已健康，但 Tool Store 提交证据清理返回失败"
    elif toolstore_txn_has_active "$SCRIPT_DIR"; then
      cleanup_ok=false
      log_warn "正式候选已健康，但 Tool Store 提交证据仍处于活动状态"
    fi
  fi

  if [[ "$cleanup_ok" != "true" ]]; then
    log_warn "正式候选已健康并保持运行；已保留 active.env 与未删除快照，下次运行 deploy.sh 将幂等重试清理，请勿手工恢复旧数据库、配置或镜像"
  fi
  log_success "隔离候选验证和正式晋升均已完成，部署镜像为 ${candidate_image}"
}

durable_remove_deploy_file() {
  local target="$1" parent removed=false
  parent="$(dirname "$target")"
  [[ ! -d "$target" || -L "$target" ]] || return 1
  if [[ -e "$target" || -L "$target" ]]; then
    rm -f -- "$target" || return 1
    removed=true
  fi
  [[ ! -e "$target" && ! -L "$target" ]] || return 1
  if [[ "$removed" == "true" ]]; then
    sync -f "$parent" || return 1
  fi
}

image_repository_without_tag() {
  local image="${1%@*}" final_component
  final_component="${image##*/}"
  if [[ "$final_component" == *:* ]]; then
    image="${image%:*}"
  fi
  printf '%s\n' "$image"
}

run_deploy_cosign() {
  local runner="${NEWAPI_TOOLS_COSIGN_RUNNER:-}"
  if [[ -n "$runner" ]]; then
    "$runner" "$@"
    return
  fi
  local policy_file="${NEWAPI_TOOLS_COSIGN_POLICY_FILE:-}"
  local -a policy_mount=()
  if [[ -n "$policy_file" ]]; then
    [[ "$policy_file" == /* && -f "$policy_file" && ! -L "$policy_file" ]] || return 1
    policy_mount=(--volume "${policy_file}:/tmp/newapi-tools-provenance-policy.cue:ro")
  fi
  docker run --rm \
    --read-only \
    --cap-drop=ALL \
    --security-opt=no-new-privileges:true \
    --user=65532:65532 \
    --tmpfs=/tmp:rw,noexec,nosuid,nodev,size=64m \
    --env=HOME=/tmp \
    --env=XDG_CACHE_HOME=/tmp \
    "${policy_mount[@]}" \
    "$NEWAPI_TOOLS_COSIGN_IMAGE" "$@"
}

verify_deploy_release_signature() {
  local image="$1" tag="$2" git_sha="$3" source_ref identity index
  local -a certificate_sha_args=()
  local -a workflow_files=(build.yml release-recovery.yml)
  local -a workflow_refs=("refs/tags/${tag}" refs/heads/main)
  local -a workflow_shas=("$git_sha" '')
  local -a workflow_names=(
    'Build and Push Docker Image'
    'Recover Release Image From Existing Tag'
  )
  local -a workflow_triggers=(push workflow_dispatch)

  [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || return 1
  [[ "$git_sha" =~ ^[0-9a-f]{40}$ ]] || return 1
  is_immutable_newapi_tools_image "$image" || return 1
  [[ "$(image_repository_without_tag "$image")" == "$NEWAPI_TOOLS_IMAGE_REPOSITORY" ]] || return 1
  source_ref="refs/tags/${tag}"

  for index in "${!workflow_files[@]}"; do
    identity="https://github.com/${NEWAPI_TOOLS_SIGNING_REPOSITORY}/.github/workflows/${workflow_files[index]}@${workflow_refs[index]}"
    certificate_sha_args=()
    if [[ -n "${workflow_shas[index]}" ]]; then
      certificate_sha_args=(--certificate-github-workflow-sha "${workflow_shas[index]}")
    fi
    if run_deploy_cosign verify \
      --certificate-identity "$identity" \
      --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
      --certificate-github-workflow-repository "$NEWAPI_TOOLS_SIGNING_REPOSITORY" \
      --certificate-github-workflow-ref "${workflow_refs[index]}" \
      "${certificate_sha_args[@]}" \
      --certificate-github-workflow-name "${workflow_names[index]}" \
      --certificate-github-workflow-trigger "${workflow_triggers[index]}" \
      -a "git_sha=${git_sha}" \
      -a "tag=${tag}" \
      "$image" >/dev/null; then
      return 0
    fi
  done
  log_error "发行镜像签名未匹配允许的 build.yml 或 release-recovery.yml 标签工作流身份"
  return 1
}

resolve_deploy_release_platform_digests() {
  local image="$1" descriptors platform digest
  local amd64_digest='' arm64_digest=''

  is_immutable_newapi_tools_image "$image" || return 1
  docker buildx version >/dev/null 2>&1 || {
    log_error "发行镜像平台 digest 校验需要 Docker Buildx"
    return 1
  }
  descriptors="$(docker buildx imagetools inspect "$image" --format \
    '{{range .Manifest.Manifests}}{{if .Platform}}{{.Platform.OS}}/{{.Platform.Architecture}}{{else}}unknown/unknown{{end}} {{.Digest}}{{println}}{{end}}')" || return 1
  while read -r platform digest; do
    [[ -n "$platform" ]] || continue
    case "$platform" in
      linux/amd64)
        [[ -z "$amd64_digest" && "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
        amd64_digest="$digest"
        ;;
      linux/arm64)
        [[ -z "$arm64_digest" && "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
        arm64_digest="$digest"
        ;;
      unknown/unknown)
        [[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || return 1
        ;;
      *) return 1 ;;
    esac
  done <<< "$descriptors"
  [[ -n "$amd64_digest" && -n "$arm64_digest" ]] || return 1
  printf '%s %s\n' "$amd64_digest" "$arm64_digest"
}

verify_deploy_release_provenance() {
  local image="$1" tag="$2" git_sha="$3" source_ref identity index
  local repository manifest_digest policy_file policy_argument platform_digests
  local amd64_digest arm64_digest extra
  local -a certificate_sha_args=()
  local -a workflow_files=(build.yml release-recovery.yml)
  local -a workflow_refs=("refs/tags/${tag}" refs/heads/main)
  local -a workflow_shas=("$git_sha" '')
  local -a workflow_names=(
    'Build and Push Docker Image'
    'Recover Release Image From Existing Tag'
  )
  local -a workflow_triggers=(push workflow_dispatch)

  [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || return 1
  [[ "$git_sha" =~ ^[0-9a-f]{40}$ ]] || return 1
  is_immutable_newapi_tools_image "$image" || return 1
  repository="$(image_repository_without_tag "$image")"
  [[ "$repository" == "$NEWAPI_TOOLS_IMAGE_REPOSITORY" ]] || return 1
  manifest_digest="${image##*@}"
  source_ref="refs/tags/${tag}"
  platform_digests="$(resolve_deploy_release_platform_digests "$image")" || return 1
  read -r amd64_digest arm64_digest extra <<< "$platform_digests"
  [[ "$amd64_digest" =~ ^sha256:[0-9a-f]{64}$ &&
     "$arm64_digest" =~ ^sha256:[0-9a-f]{64}$ && -z "${extra:-}" ]] || return 1

  for index in "${!workflow_files[@]}"; do
    identity="https://github.com/${NEWAPI_TOOLS_SIGNING_REPOSITORY}/.github/workflows/${workflow_files[index]}@${workflow_refs[index]}"
    policy_file="$(mktemp --suffix=.cue)" || return 1
    cat >"$policy_file" <<EOF
package newapi_tools_release

"_type": "https://in-toto.io/Statement/v1"
predicateType: "https://slsa.dev/provenance/v1"
subject: [{
  name: "${repository}"
  digest: sha256: "${manifest_digest#sha256:}"
}]
predicate: {
  buildDefinition: {
    buildType: "${identity}"
    externalParameters: {
      repository: "https://github.com/${NEWAPI_TOOLS_SIGNING_REPOSITORY}"
      ref: "${source_ref}"
      revision: "${git_sha}"
      tag: "${tag}"
      manifest_digest: "${manifest_digest}"
      platform_digests: close({
        "linux/amd64": "${amd64_digest}"
        "linux/arm64": "${arm64_digest}"
      })
    }
    resolvedDependencies: [{
      uri: "git+https://github.com/${NEWAPI_TOOLS_SIGNING_REPOSITORY}@${source_ref}"
      digest: gitCommit: "${git_sha}"
    }]
  }
  runDetails: builder: id: "${identity}"
}
EOF
    chmod 0444 "$policy_file" || { rm -f -- "$policy_file"; return 1; }
    policy_argument="$policy_file"
    if [[ -z "${NEWAPI_TOOLS_COSIGN_RUNNER:-}" ]]; then
      policy_argument='/tmp/newapi-tools-provenance-policy.cue'
    fi
    certificate_sha_args=()
    if [[ -n "${workflow_shas[index]}" ]]; then
      certificate_sha_args=(--certificate-github-workflow-sha "${workflow_shas[index]}")
    fi
    if NEWAPI_TOOLS_COSIGN_POLICY_FILE="$policy_file" run_deploy_cosign verify-attestation \
      --type slsaprovenance1 \
      --policy "$policy_argument" \
      --certificate-identity "$identity" \
      --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
      --certificate-github-workflow-repository "$NEWAPI_TOOLS_SIGNING_REPOSITORY" \
      --certificate-github-workflow-ref "${workflow_refs[index]}" \
      "${certificate_sha_args[@]}" \
      --certificate-github-workflow-name "${workflow_names[index]}" \
      --certificate-github-workflow-trigger "${workflow_triggers[index]}" \
      "$image" >/dev/null; then
      rm -f -- "$policy_file"
      return 0
    fi
    rm -f -- "$policy_file"
  done
  log_error "发行镜像缺少与 subject、平台 digest、tag 和 commit 绑定的受信 SLSA provenance"
  return 1
}

resolve_deploy_image_digest() {
  local image="$1" expected_revision="${2:-}" repository actual_revision
  local -a matching_digests=()
  validate_newapi_tools_image "$image"
  if is_immutable_newapi_tools_image "$image"; then
    matching_digests=("$image")
  else
    repository="$(image_repository_without_tag "$image")"
    mapfile -t matching_digests < <(
      docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image" 2>/dev/null |
        awk -v prefix="${repository}@sha256:" \
          'index($0, prefix) == 1 && $0 ~ /^.+@sha256:[0-9a-f]{64}$/ { print }'
    ) || die "无法读取已拉取镜像的 OCI digest"
    (( ${#matching_digests[@]} == 1 )) ||
      die "目标仓库 ${repository} 必须且只能匹配一个 RepoDigest，实际为 ${#matching_digests[@]} 个"
  fi

  if [[ -n "$expected_revision" ]]; then
    actual_revision="$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image" 2>/dev/null)" ||
      die "无法读取镜像源码版本标签"
    [[ "${actual_revision,,}" == "${expected_revision,,}" ]] ||
      die "镜像源码版本与当前 Git commit 不匹配，已拒绝部署"
  fi

  printf '%s\n' "${matching_digests[0]}"
}

# Resolve the image that the existing container is actually running, rather
# than trusting a mutable local tag which may already have moved after a pull.
resolve_deploy_running_image_digest() {
  local container="$1" configured_image="$2" image_id repository
  local -a matching_digests=()
  validate_newapi_tools_image "$configured_image"
  repository="$(image_repository_without_tag "$configured_image")"
  image_id="$(docker inspect --format '{{.Image}}' "$container" 2>/dev/null)" ||
    die "无法读取现有 ${container} 容器实际运行的镜像 ID"
  [[ "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]] ||
    die "现有 ${container} 容器的镜像 ID 格式无效"

  mapfile -t matching_digests < <(
    docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$image_id" 2>/dev/null |
      awk -v prefix="${repository}@sha256:" \
        'index($0, prefix) == 1 && $0 ~ /^.+@sha256:[0-9a-f]{64}$/ { print }'
  ) || die "无法读取现有容器镜像的 OCI digest"
  (( ${#matching_digests[@]} == 1 )) ||
    die "现有容器镜像在目标仓库 ${repository} 中必须且只能匹配一个 RepoDigest，实际为 ${#matching_digests[@]} 个"

  if is_immutable_newapi_tools_image "$configured_image" &&
    [[ "${matching_digests[0]}" != "$configured_image" ]]; then
    die "旧配置的不可变镜像与现有容器实际运行镜像不一致；拒绝建立错误回滚锚点"
  fi
  printf '%s\n' "${matching_digests[0]}"
}

resolve_deploy_running_compose_project() {
  local container="$1" rollback_content="$2" label_project configured_project
  label_project="$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' \
    "$container" 2>/dev/null)" || die "无法读取现有容器的 Compose project label"
  label_project="${label_project%$'\r'}"
  [[ "$label_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
    die "现有容器缺少有效的 com.docker.compose.project 标识；拒绝猜测部署身份"

  configured_project="$(env_content_value "$rollback_content" 'COMPOSE_PROJECT_NAME')"
  if [[ -n "$configured_project" ]]; then
    [[ "$configured_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "旧配置中的 COMPOSE_PROJECT_NAME 格式无效"
    [[ "$configured_project" == "$label_project" ]] ||
      die "旧配置的 COMPOSE_PROJECT_NAME 与运行容器 project label 不一致"
  fi
  printf '%s\n' "$label_project"
}

pin_deploy_image_after_pull() {
  local resolved
  if ! resolved="$(resolve_deploy_image_digest "$NEWAPI_TOOLS_IMAGE" "$NEWAPI_TOOLS_EXPECTED_REVISION")"; then
    log_error "候选镜像 digest 或源码版本验证失败；现有服务保持不变"
    return 1
  fi
  if [[ -n "$NEWAPI_TOOLS_RELEASE_TAG" ]] &&
    { ! verify_deploy_release_signature \
        "$resolved" "$NEWAPI_TOOLS_RELEASE_TAG" "$NEWAPI_TOOLS_EXPECTED_REVISION" ||
      ! verify_deploy_release_provenance \
        "$resolved" "$NEWAPI_TOOLS_RELEASE_TAG" "$NEWAPI_TOOLS_EXPECTED_REVISION"; }; then
    log_error "候选发行镜像的 Sigstore 身份、标签、commit、注解或 subject digest 验证失败；现有服务保持不变"
    return 1
  fi
  if [[ -z "$NEWAPI_TOOLS_RELEASE_TAG" ]]; then
    log_warn "开发部署仅验证 OCI revision，未执行发行 tag 的签名与 provenance 校验"
  fi
  NEWAPI_TOOLS_IMAGE="$resolved"
  export NEWAPI_TOOLS_IMAGE
  log_success "候选部署镜像已验证并固定为 ${resolved}"
}

deploy_release_tag_at_commit() {
  local commit="$1" tags tag release_tag=""
  tags="$(git -C "$SCRIPT_DIR" tag --points-at "$commit")" ||
    die "无法枚举部署 commit ${commit} 上的 Git tag"
  while IFS= read -r tag; do
    [[ -n "$tag" ]] || continue
    if [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
      [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
      die "部署 commit 上的发行标签必须使用无前导零的严格 vMAJOR.MINOR.PATCH 格式: ${tag}"
    fi
    [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || continue
    [[ "$(git -C "$SCRIPT_DIR" cat-file -t "refs/tags/${tag}" 2>/dev/null)" == "tag" ]] ||
      die "发行标签必须是 annotated tag: ${tag}"
    [[ -z "$release_tag" ]] ||
      die "同一部署 commit 不能同时关联多个发行标签: ${release_tag}, ${tag}"
    release_tag="$tag"
  done <<< "$tags"
  printf '%s\n' "$release_tag"
}

resolve_deploy_image() {
  local image="$REQUESTED_NEWAPI_TOOLS_IMAGE" git_commit="" resolved_exact_tag=""

  NEWAPI_TOOLS_IMAGE_DERIVED=false
  NEWAPI_TOOLS_EXPECTED_REVISION=""
  NEWAPI_TOOLS_RELEASE_TAG=""

  if [[ -n "$REQUESTED_NEWAPI_TOOLS_RELEASE_TAG" ]]; then
    [[ "$REQUESTED_NEWAPI_TOOLS_RELEASE_TAG" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
      die "NEWAPI_TOOLS_RELEASE_TAG 必须使用严格的 vMAJOR.MINOR.PATCH 格式"
    [[ -n "$image" ]] ||
      die "显式发行标签必须同时提供不可变 NEWAPI_TOOLS_IMAGE 和 NEWAPI_TOOLS_EXPECTED_REVISION"
  fi

  if command -v git >/dev/null 2>&1 &&
    git -C "$SCRIPT_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
    git_commit="$(git -C "$SCRIPT_DIR" rev-parse --verify HEAD)" ||
      die "无法解析当前 Git commit"
    if ! resolved_exact_tag="$(deploy_release_tag_at_commit "$git_commit")"; then
      die "无法安全解析部署 commit ${git_commit} 的发行标签身份"
    fi
  fi

  if [[ -n "$image" ]]; then
    [[ "$REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION" =~ ^[0-9a-fA-F]{40}$ ]] ||
      die "显式 NEWAPI_TOOLS_IMAGE 必须同时提供 40 位 NEWAPI_TOOLS_EXPECTED_REVISION"
    [[ "$(image_repository_without_tag "$image")" == "$NEWAPI_TOOLS_IMAGE_REPOSITORY" ]] ||
      die "NEWAPI_TOOLS_IMAGE 必须属于受信任仓库 ${NEWAPI_TOOLS_IMAGE_REPOSITORY}"
    is_immutable_newapi_tools_image "$image" ||
      die "显式部署镜像必须使用发行页核验过的 repo@sha256:<digest>"
    NEWAPI_TOOLS_EXPECTED_REVISION="${REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION,,}"
    if [[ -n "$REQUESTED_NEWAPI_TOOLS_RELEASE_TAG" ]]; then
      if [[ "$resolved_exact_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ &&
            "$resolved_exact_tag" != "$REQUESTED_NEWAPI_TOOLS_RELEASE_TAG" ]]; then
        die "显式发行标签与当前 checkout 的精确发行标签不一致"
      fi
      NEWAPI_TOOLS_RELEASE_TAG="$REQUESTED_NEWAPI_TOOLS_RELEASE_TAG"
    elif [[ "$resolved_exact_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
      [[ "${git_commit,,}" == "$NEWAPI_TOOLS_EXPECTED_REVISION" ]] ||
        die "当前发行标签 commit 与 NEWAPI_TOOLS_EXPECTED_REVISION 不一致"
      NEWAPI_TOOLS_RELEASE_TAG="$resolved_exact_tag"
    fi
    [[ -n "$NEWAPI_TOOLS_RELEASE_TAG" ]] ||
      die "显式 NEWAPI_TOOLS_IMAGE digest 必须提供 NEWAPI_TOOLS_RELEASE_TAG；开发部署请从未打 tag 的 Git checkout 派生镜像"
  fi

  if [[ -z "$image" && -n "$git_commit" ]]; then
    NEWAPI_TOOLS_IMAGE_DERIVED=true
    NEWAPI_TOOLS_EXPECTED_REVISION="$git_commit"
    if [[ "$resolved_exact_tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
      die "发行 tag ${resolved_exact_tag} 必须显式提供发行页中的不可变 NEWAPI_TOOLS_IMAGE digest 和 NEWAPI_TOOLS_EXPECTED_REVISION"
    fi
    image="${NEWAPI_TOOLS_IMAGE_REPOSITORY}:${git_commit:0:7}"
  fi

  [[ -n "$image" ]] ||
    die "无法从 Git 推导开发镜像；发行部署必须显式提供不可变 NEWAPI_TOOLS_IMAGE digest 和 NEWAPI_TOOLS_EXPECTED_REVISION"
  validate_newapi_tools_image "$image"
  NEWAPI_TOOLS_IMAGE="$image"
  export NEWAPI_TOOLS_IMAGE NEWAPI_TOOLS_RELEASE_TAG
  log_info "部署镜像: ${NEWAPI_TOOLS_IMAGE}"
}

need_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "缺少必要命令: $1"
}

list_deploy_container_names() {
  docker ps -a --format '{{.Names}}'
}

# 提取 docker compose v2 的语义版本号。
get_docker_compose_v2_version() {
  local output
  output="$(docker compose version --short 2>/dev/null || docker compose version 2>/dev/null || true)"
  if [[ "$output" =~ v?([0-9]+)\.([0-9]+)\.([0-9]+) ]]; then
    printf '%s.%s.%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}" "${BASH_REMATCH[3]}"
  fi
}

version_at_least() {
  local current="$1" required="$2"
  local current_major current_minor current_patch required_major required_minor required_patch
  IFS=. read -r current_major current_minor current_patch <<< "$current"
  IFS=. read -r required_major required_minor required_patch <<< "$required"

  (( 10#$current_major > 10#$required_major )) ||
    (( 10#$current_major == 10#$required_major && 10#$current_minor > 10#$required_minor )) ||
    (( 10#$current_major == 10#$required_major && 10#$current_minor == 10#$required_minor && 10#$current_patch >= 10#$required_patch ))
}

# docker-compose.host.yml 使用 !reset，只有 Docker Compose v2.24+ 能正确解析。
require_docker_compose_v224() {
  local reason="${1:-当前 Compose 配置}"
  if [[ "${DOCKER_COMPOSE:-}" != "docker compose" ]]; then
    die "${reason} 需要 Docker Compose v2.24+；检测到的是旧版 docker-compose，请安装/升级 Compose v2 插件"
  fi

  local version="${DOCKER_COMPOSE_V2_VERSION:-}"
  [[ -n "$version" ]] || version="$(get_docker_compose_v2_version)"
  [[ -n "$version" ]] || die "无法识别 Docker Compose v2 版本；${reason} 需要 v2.24+"
  version_at_least "$version" "2.24.0" || die "Docker Compose v${version} 过旧；${reason} 最低需要 v2.24.0"
}

configure_host_compose_files() {
  [[ -f "$COMPOSE_HOST_FILE" ]] ||
    die "host 模式需要 ${COMPOSE_HOST_FILE}，缺少该叠加层时无法安全移除基础 external network 配置"
  require_docker_compose_v224 "host 网络叠加层 ${COMPOSE_HOST_FILE} 的 !reset 语法"
  COMPOSE_FILES=("-f" "$COMPOSE_FILE" "-f" "$COMPOSE_HOST_FILE")
}

# 当前基础 Compose 文件使用 service_completed_successfully 镜像策略门禁，
# 并与 host overlay 的 !reset 语法统一要求 Docker Compose v2.24+。
detect_docker_compose() {
  if docker compose version >/dev/null 2>&1; then
    DOCKER_COMPOSE="docker compose"
    DOCKER_COMPOSE_V2_VERSION="$(get_docker_compose_v2_version)"
  else
    die "缺少 Docker Compose v2.24+；当前不可变镜像策略不支持旧版 docker-compose"
  fi
  require_docker_compose_v224 "当前不可变镜像策略门禁"
}

trim() { sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//'; }

dotenv_quote() {
	local value="${1-}" escaped
	[[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "环境变量值不能包含换行"
	escaped="$(printf '%s' "$value" | sed "s/'/\\\\'/g")"
	printf "'%s'" "$escaped"
}

# 生成 32 字符强密码（约 200 bits 熵）
# 字符集：62 字母数字 + 14 个安全特殊符号 = 76 chars
# 刻意排除：$ ` \ " ' = # ; & | < > [ ] / 空格 — 这些会破坏 .env / docker-compose / heredoc 解析
# 注意：- 必须放在 tr 字符类末尾，否则会被解读为范围操作符
generate_strong_password() {
  local alphabet='A-Za-z0-9!@%^*()_+:?,.-'
  local pw=""
  if [[ -r /dev/urandom ]]; then
    pw="$(LC_ALL=C tr -dc "$alphabet" </dev/urandom 2>/dev/null | head -c 32)"
  fi
  if [[ ${#pw} -lt 32 ]] && command -v openssl >/dev/null 2>&1; then
    pw="$(openssl rand 256 2>/dev/null | LC_ALL=C tr -dc "$alphabet" | head -c 32)"
  fi
  if [[ ${#pw} -lt 32 ]]; then
    log_warn "强密码生成失败，回退到 20 字符字母数字"
    pw="$(head -c 256 /dev/urandom 2>/dev/null | LC_ALL=C tr -dc 'A-Za-z0-9' | head -c 20)"
  fi
  echo "$pw"
}

first_csv() {
  echo "${1}" | sed 's/,.*$//'
}

#######################################
# Docker 环境检测函数 (来自 newapi_detect.sh)
#######################################

# DSN 解析只接受可以无歧义拆分的常见形式。所有错误都必须脱敏，禁止输出原始 DSN。
dsn_parse_error() {
  printf '无法安全解析数据库 DSN：%s（原始 DSN 已隐藏）\n' "$1" >&2
  return 1
}

dsn_validate_port() {
  local port="${1:-}"
  [[ -z "$port" ]] && return 0
  if [[ ! "$port" =~ ^[0-9]+$ || ${#port} -gt 5 ]] || (( 10#$port < 1 || 10#$port > 65535 )); then
    dsn_parse_error "端口必须是 1-65535 的数字"
  fi
}

dsn_validate_postgres_url_query() {
  local query="${1:-}" parameter key normalized_key

  [[ -n "$query" ]] || return 0
  while :; do
    parameter="${query%%&*}"
    key="${parameter%%=*}"

    # libpq URL-decodes query parameter names. Restrict names to its normal
    # identifier form so percent-encoding cannot disguise a target override.
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || {
      dsn_parse_error "PostgreSQL URL query 含无效或编码后的键名"
      return 1
    }
    normalized_key="${key,,}"
    case "$normalized_key" in
      host|hostaddr|port)
        dsn_parse_error "PostgreSQL URL query 不能覆盖 host、hostaddr 或 port"
        return 1
        ;;
    esac

    [[ "$query" == *'&'* ]] || break
    query="${query#*&}"
  done
}

dsn_url_component() {
  local dsn="$1" component="$2"
  local scheme rest authority path query="" userinfo="" hostport user="" password="" host="" port="" dbname

  [[ "$dsn" != *$'\n'* && "$dsn" != *$'\r'* && "$dsn" != *[[:space:]]* ]] || {
    dsn_parse_error "URL DSN 不能包含空白或换行"
    return 1
  }

  scheme="${dsn%%://*}"
  case "$scheme" in
    postgres|postgresql|mysql) ;;
    *) dsn_parse_error "不支持的 URL scheme"; return 1 ;;
  esac

  rest="${dsn#*://}"
  [[ "$rest" == */* ]] || { dsn_parse_error "URL DSN 缺少 /dbname"; return 1; }
  authority="${rest%%/*}"
  path="${rest#*/}"
  [[ -n "$authority" && -n "$path" && "$path" != *'#'* ]] || {
    dsn_parse_error "URL DSN 的 authority 或 dbname 无效"
    return 1
  }

  dbname="${path%%\?*}"
  if [[ "$path" == *'?'* ]]; then
    query="${path#*\?}"
    if [[ "$scheme" == "postgres" || "$scheme" == "postgresql" ]]; then
      dsn_validate_postgres_url_query "$query" || return 1
    fi
  fi
  [[ -n "$dbname" && "$dbname" != */* ]] || {
    dsn_parse_error "URL DSN 的 dbname 缺失或包含未转义的斜杠"
    return 1
  }

  hostport="$authority"
  if [[ "$authority" == *@* ]]; then
    userinfo="${authority%@*}"
    hostport="${authority##*@}"
    [[ -n "$userinfo" ]] || { dsn_parse_error "URL DSN 的用户信息为空"; return 1; }
    if [[ "$userinfo" == *:* ]]; then
      user="${userinfo%%:*}"
      password="${userinfo#*:}"
    else
      user="$userinfo"
    fi
    [[ -n "$user" ]] || { dsn_parse_error "URL DSN 的用户名为空"; return 1; }
  fi

  if [[ "$hostport" =~ ^\[([^][]+)\](:([0-9]+))?$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[3]:-}"
  elif [[ "$hostport" =~ ^([^:]+)(:([0-9]+))?$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[3]:-}"
  else
    dsn_parse_error "URL DSN 的 host:port 无法无歧义解析"
    return 1
  fi
  [[ -n "$host" && "$host" != *[[:space:]@/]* ]] || {
    dsn_parse_error "URL DSN 的 host 无效"
    return 1
  }
  dsn_validate_port "$port" || return 1

  case "$component" in
    validate) ;;
    engine) [[ "$scheme" == "mysql" ]] && printf 'mysql\n' || printf 'postgres\n' ;;
    host) printf '%s\n' "$host" ;;
    user) printf '%s\n' "$user" ;;
    password) printf '%s\n' "$password" ;;
    port) printf '%s\n' "$port" ;;
    dbname) printf '%s\n' "$dbname" ;;
    *) dsn_parse_error "内部请求了未知的 URL DSN 字段"; return 1 ;;
  esac
}

dsn_mysql_go_component() {
  local dsn="$1" component="$2"
  local user password address dbname host="" port=""

  [[ "$dsn" != *$'\n'* && "$dsn" != *$'\r'* && "$dsn" != *[[:space:]]* ]] || {
    dsn_parse_error "MySQL Go DSN 不能包含空白或换行"
    return 1
  }
  if [[ "$dsn" =~ ^([^:/@]+):(.*)@tcp\(([^()]*)\)/([^?]+)(\?.*)?$ ]]; then
    user="${BASH_REMATCH[1]}"
    password="${BASH_REMATCH[2]}"
    address="${BASH_REMATCH[3]}"
    dbname="${BASH_REMATCH[4]}"
  else
    dsn_parse_error "MySQL Go DSN 必须形如 user:password@tcp(host:port)/dbname"
    return 1
  fi

  if [[ "$address" =~ ^\[([^][]+)\]:([0-9]+)$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[2]}"
  elif [[ "$address" =~ ^([^:]+):([0-9]+)$ ]]; then
    host="${BASH_REMATCH[1]}"
    port="${BASH_REMATCH[2]}"
  else
    dsn_parse_error "MySQL Go DSN 的 tcp 地址必须是 host:port"
    return 1
  fi
  [[ -n "$host" && "$host" != *[[:space:]@/]* && -n "$dbname" && "$dbname" != */* ]] || {
    dsn_parse_error "MySQL Go DSN 的 host 或 dbname 无效"
    return 1
  }
  dsn_validate_port "$port" || return 1

  case "$component" in
    validate) ;;
    engine) printf 'mysql\n' ;;
    host) printf '%s\n' "$host" ;;
    user) printf '%s\n' "$user" ;;
    password) printf '%s\n' "$password" ;;
    port) printf '%s\n' "$port" ;;
    dbname) printf '%s\n' "$dbname" ;;
    *) dsn_parse_error "内部请求了未知的 MySQL Go DSN 字段"; return 1 ;;
  esac
}

dsn_postgres_keyword_component() {
  local dsn="$1" component="$2"
  local token key value normalized_key
  local host="" port="" user="" password="" dbname=""
  local seen_host=false seen_port=false seen_user=false seen_password=false seen_dbname=false
  local -a tokens=()

  [[ "$dsn" != *$'\n'* && "$dsn" != *$'\r'* ]] || {
    dsn_parse_error "PostgreSQL keyword DSN 不能包含换行"
    return 1
  }
  # libpq 的引号/反斜杠转义需要完整词法分析；这里宁可拒绝，也不能误拆凭证。
  [[ "$dsn" != *"'"* && "$dsn" != *'"'* && "$dsn" != *'\'* ]] || {
    dsn_parse_error "暂不支持带引号或反斜杠转义的 PostgreSQL keyword DSN"
    return 1
  }

  read -r -a tokens <<< "$dsn"
  (( ${#tokens[@]} > 0 )) || { dsn_parse_error "PostgreSQL keyword DSN 为空"; return 1; }
  for token in "${tokens[@]}"; do
    [[ "$token" == *=* ]] || {
      dsn_parse_error "PostgreSQL keyword DSN 含无法归属的空白值"
      return 1
    }
    key="${token%%=*}"
    value="${token#*=}"
    [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || {
      dsn_parse_error "PostgreSQL keyword DSN 含无效键名"
      return 1
    }
    normalized_key="${key,,}"
    case "$normalized_key" in
      host)
        [[ "$seen_host" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 host"; return 1; }
        seen_host=true; host="$value"
        ;;
      hostaddr)
        dsn_parse_error "PostgreSQL keyword DSN 不支持 hostaddr；它可能绕过 host 改写"
        return 1
        ;;
      port)
        [[ "$seen_port" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 port"; return 1; }
        seen_port=true; port="$value"
        ;;
      user)
        [[ "$seen_user" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 user"; return 1; }
        seen_user=true; user="$value"
        ;;
      password)
        [[ "$seen_password" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 password"; return 1; }
        seen_password=true; password="$value"
        ;;
      dbname)
        [[ "$seen_dbname" == false ]] || { dsn_parse_error "PostgreSQL keyword DSN 重复 dbname"; return 1; }
        seen_dbname=true; dbname="$value"
        ;;
    esac
  done

  [[ "$seen_host" == true && -n "$host" && "$host" != *[[:space:]@/]* ]] || {
    dsn_parse_error "PostgreSQL keyword DSN 缺少有效 host"
    return 1
  }
  dsn_validate_port "$port" || return 1

  case "$component" in
    validate) ;;
    engine) printf 'postgres\n' ;;
    host) printf '%s\n' "$host" ;;
    user) printf '%s\n' "$user" ;;
    password) printf '%s\n' "$password" ;;
    port) printf '%s\n' "$port" ;;
    dbname) printf '%s\n' "$dbname" ;;
    *) dsn_parse_error "内部请求了未知的 PostgreSQL keyword DSN 字段"; return 1 ;;
  esac
}

detect_dsn_format() {
  local dsn="${1:-}"
  [[ -n "$dsn" ]] || return 0
  if [[ "$dsn" =~ ^postgres(ql)?:// ]]; then
    dsn_url_component "$dsn" validate || return 1
    printf 'postgres_url\n'
  elif [[ "$dsn" =~ ^mysql:// ]]; then
    dsn_url_component "$dsn" validate || return 1
    printf 'mysql_url\n'
  elif [[ "$dsn" == *@tcp\(* ]]; then
    dsn_mysql_go_component "$dsn" validate || return 1
    printf 'mysql_go\n'
  elif [[ "$dsn" == *=* ]]; then
    dsn_postgres_keyword_component "$dsn" validate || return 1
    printf 'postgres_keyword\n'
  else
    dsn_parse_error "仅支持 PostgreSQL/MySQL URL、MySQL Go DSN 和 PostgreSQL keyword DSN"
  fi
}

extract_dsn_component() {
  local dsn="${1:-}" component="$2" format
  [[ -n "$dsn" ]] || return 0
  format="$(detect_dsn_format "$dsn")" || return 1
  case "$format" in
    postgres_url|mysql_url) dsn_url_component "$dsn" "$component" ;;
    mysql_go) dsn_mysql_go_component "$dsn" "$component" ;;
    postgres_keyword) dsn_postgres_keyword_component "$dsn" "$component" ;;
    *) dsn_parse_error "内部识别到了未知 DSN 格式"; return 1 ;;
  esac
}

extract_dsn_engine() { extract_dsn_component "${1:-}" engine; }
extract_dsn_host() { extract_dsn_component "${1:-}" host; }
extract_dsn_user() { extract_dsn_component "${1:-}" user; }
extract_dsn_password() { extract_dsn_component "${1:-}" password; }
extract_dsn_port() { extract_dsn_component "${1:-}" port; }
extract_dsn_dbname() { extract_dsn_component "${1:-}" dbname; }

dsn_host_port() {
  local host="$1" port="$2"
  [[ -n "$host" && "$host" != *[[:space:]@/]* ]] || {
    dsn_parse_error "改写后的数据库 host 无效"
    return 1
  }
  dsn_validate_port "$port" || return 1
  if [[ "$host" == *:* ]]; then
    printf '[%s]:%s' "$host" "$port"
  else
    printf '%s:%s' "$host" "$port"
  fi
}

# Preserve the original DSN representation, escaping and connection options;
# only the network host and port are replaced.
rewrite_dsn_host_port() {
  local dsn="$1" new_host="$2" new_port="$3" format address
  format="$(detect_dsn_format "$dsn")" || return 1
  address="$(dsn_host_port "$new_host" "$new_port")" || return 1

  case "$format" in
    postgres_url|mysql_url)
      local scheme rest authority path userinfo_prefix=""
      scheme="${dsn%%://*}"
      rest="${dsn#*://}"
      authority="${rest%%/*}"
      path="${rest#*/}"
      if [[ "$authority" == *@* ]]; then
        userinfo_prefix="${authority%@*}@"
      fi
      printf '%s://%s%s/%s\n' "$scheme" "$userinfo_prefix" "$address" "$path"
      ;;
    mysql_go)
      if [[ "$dsn" =~ ^(.+)@tcp\([^()]*\)/(.*)$ ]]; then
        printf '%s@tcp(%s)/%s\n' "${BASH_REMATCH[1]}" "$address" "${BASH_REMATCH[2]}"
      else
        dsn_parse_error "MySQL Go DSN 无法安全改写地址"
        return 1
      fi
      ;;
    postgres_keyword)
      local token key normalized_key seen_port=false i
      local -a tokens=() rewritten=()
      read -r -a tokens <<< "$dsn"
      for token in "${tokens[@]}"; do
        key="${token%%=*}"
        normalized_key="${key,,}"
        case "$normalized_key" in
          host) rewritten+=("${key}=${new_host}") ;;
          port) rewritten+=("${key}=${new_port}"); seen_port=true ;;
          *) rewritten+=("$token") ;;
        esac
      done
      [[ "$seen_port" == true ]] || rewritten+=("port=${new_port}")
      printf '%s' "${rewritten[0]}"
      for ((i = 1; i < ${#rewritten[@]}; i++)); do
        printf ' %s' "${rewritten[$i]}"
      done
      printf '\n'
      ;;
    *)
      dsn_parse_error "未知 DSN 格式"
      return 1
      ;;
  esac
}

detect_newapi_container() {
  local found=""
  # 按容器名匹配：new-api / new-api-master / new-api-my ...（不含 newapi-tools）
  found="$(docker ps --format '{{.Names}}' | awk 'tolower($0) ~ /(^|[-_])new-api([-_]|$)/ {print; exit}')"
  if [[ -n "$found" ]]; then echo "$found"; return 0; fi

  found="$(docker ps -q --filter 'label=com.docker.compose.service=new-api' | head -n 1 || true)"
  if [[ -n "$found" ]]; then echo "$found"; return 0; fi

  # 按镜像名匹配：允许 fork 后缀（new-api-my:latest 也能命中）
  found="$(docker ps --format '{{.ID}}\t{{.Image}}' | awk 'tolower($2) ~ /(^|\/)new-api([-_:]|$)/ {print $1; exit}')"
  if [[ -n "$found" ]]; then echo "$found"; return 0; fi

  return 1
}

docker_inspect_label() {
  local container="$1" key="$2"
  docker inspect -f "{{ index .Config.Labels \"$key\" }}" "$container" 2>/dev/null || true
}

docker_inspect_env_value() {
  local container="$1" var_name="$2"
  docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null |
    awk -v k="$var_name" 'index($0, k "=")==1 {print substr($0, length(k)+2); exit}'
}

# Direct database mutations are only safe when NewAPI explicitly declares an
# empty REDIS_CONN_STRING. A missing variable or an inspect failure is treated
# as unknown and therefore remains fail-closed.
detect_newapi_redis_mutation_safety() {
  NEWAPI_REDIS_DISABLED=false

  local env_lines redis_entry redis_value
  if ! env_lines="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$NEWAPI_CONTAINER" 2>/dev/null)"; then
    log_warn "无法读取 NewAPI 容器环境变量；NEWAPI_REDIS_DISABLED=false"
    log_warn "用户、Token、分组和 IP 记录等直接写库操作将被阻止，请改用 NewAPI 管理 API"
    return 0
  fi

  redis_entry="$(printf '%s\n' "$env_lines" | awk -F= '$1=="REDIS_CONN_STRING"{print; exit}')"
  if [[ -z "$redis_entry" ]]; then
    log_warn "NewAPI 未显式声明 REDIS_CONN_STRING，Redis 状态未知；NEWAPI_REDIS_DISABLED=false"
    log_warn "用户、Token、分组和 IP 记录等直接写库操作将被阻止，请改用 NewAPI 管理 API"
    return 0
  fi

  redis_value="${redis_entry#*=}"
  if [[ -z "$redis_value" ]]; then
    NEWAPI_REDIS_DISABLED=true
    log_success "NewAPI 明确配置 REDIS_CONN_STRING=（空），允许受保护的直接数据库写操作"
    return 0
  fi

  log_warn "检测到 NewAPI 已配置 Redis；NEWAPI_REDIS_DISABLED=false"
  log_warn "为避免缓存中的用户/Token 权限延迟失效，相关直接写库操作将被阻止，请改用 NewAPI 管理 API"
}

detect_networks_for_container() {
  local container="$1"
  docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$container" 2>/dev/null || true
}

container_is_on_network() {
  local container="$1" network="$2"
  docker inspect -f "{{ if (index .NetworkSettings.Networks \"$network\") }}yes{{ end }}" "$container" 2>/dev/null | grep -q '^yes$'
}

detect_db_container_by_compose_service() {
  local project="$1" service="$2"
  docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$service" | head -n 1 || true
}

detect_db_container_by_exposed_port() {
  local network="$1" port_tcp="$2"
  local cid
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    if docker inspect -f '{{json .Config.ExposedPorts}}' "$cid" 2>/dev/null | grep -q "\"$port_tcp\""; then
      echo "$cid"
      return 0
    fi
  done < <(docker ps -q --filter "network=$network" || true)
  return 0
}

# 获取容器在指定网络上的 IPv4 地址
get_container_ipv4() {
  local container="$1" network="$2"
  docker inspect -f "{{(index .NetworkSettings.Networks \"$network\").IPAddress}}" "$container" 2>/dev/null || true
}

# 判断一个字符串是否是 IPv4 字面量
is_ipv4_literal() {
  [[ "${1:-}" =~ ^[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}$ ]]
}

# 给定一个 IPv4，在所有运行中的容器里反查“谁在某个 docker 网络上持有这个 IP”。
# 命中则输出 "<网络名>\t<容器名>"（取第一个匹配），否则无输出。
# 用于：NewAPI 是 host 模式、但数据库其实是另一条 bridge 网络上的容器（如 1Panel 托管的 PG），
#       DSN 里硬编码了该容器的 bridge IP（172.x.x.x）——此时 newapi-tools 需要挂进那条网络。
find_container_by_network_ip() {
  local target_ip="${1:-}"
  [[ -z "$target_ip" ]] && return 0
  local cid name net ip
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##')"
    while IFS=$'\t' read -r net ip; do
      [[ -z "$net" ]] && continue
      if [[ "$ip" == "$target_ip" ]]; then
        printf '%s\t%s\n' "$net" "$name"
        return 0
      fi
    done < <(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{"\t"}}{{$v.IPAddress}}{{"\n"}}{{end}}' "$cid" 2>/dev/null)
  done < <(docker ps -q)
  return 0
}

# 给定一个宿主机端口，找出"把该端口发布到宿主机(0.0.0.0 / 127.0.0.1 / [::])"的容器，
# 并返回它所在的【用户自定义】网络、容器名、以及容器内端口："<网络名>\t<容器名>\t<容器内端口>"。
# 用于：NewAPI 是 host 模式、DSN host 写的是 127.0.0.1，但数据库其实是个容器、
#       端口只发布在宿主机回环上（1Panel / 宝塔默认就这么发布）。这种情况下
#       host.docker.internal(网关 IP) 连不通（docker-proxy 只绑回环），必须把
#       newapi-tools 挂进该容器的网络、用容器名直连。
# 注意：返回的是容器内端口（PortBindings 的 key 侧），而非宿主机映射端口——
#       例如 127.0.0.1:5433->5432 时，同网络直连要用 5432 而不是 5433。
# 排除默认 bridge / host / none：它们不支持按容器名做 DNS 解析。
find_container_by_published_port() {
  local requested_host="${1:-}" target_port="${2:-}"
  [[ -z "$target_port" ]] && return 0
  local cid name net selected_net cport binding_port host_ip host_port candidate match=""
  while IFS= read -r cid; do
    [[ -z "$cid" ]] && continue
    cport=""
    while IFS='|' read -r binding_port host_ip host_port; do
      [[ "$host_port" == "$target_port" ]] || continue
      case "$requested_host" in
        127.0.0.1) [[ "$host_ip" == "127.0.0.1" ]] || continue ;;
        ::1) [[ "$host_ip" == "::1" ]] || continue ;;
        localhost) [[ "$host_ip" == "127.0.0.1" || "$host_ip" == "::1" ]] || continue ;;
        *) continue ;;
      esac
      cport="${binding_port%/*}"
      break
    done < <(docker inspect -f '{{range $p,$bindings := .NetworkSettings.Ports}}{{range $bindings}}{{printf "%s|%s|%s\n" $p .HostIp .HostPort}}{{end}}{{end}}' "$cid" 2>/dev/null)
    [[ -z "$cport" ]] && continue
    name="$(docker inspect -f '{{.Name}}' "$cid" 2>/dev/null | sed 's#^/##')"
    selected_net=""
    while IFS= read -r net; do
      [[ -z "$net" ]] && continue
      case "$net" in bridge|host|none) continue ;; esac
      selected_net="$net"
      break
    done < <(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$cid" 2>/dev/null)
    [[ -n "$selected_net" ]] || continue
    candidate="${selected_net}"$'\t'"${name}"$'\t'"${cport}"
    if [[ -n "$match" && "$candidate" != "$match" ]]; then
      return 0
    fi
    match="$candidate"
  done < <(docker ps -q)
  [[ -n "$match" ]] && printf '%s\n' "$match"
  return 0
}

#######################################
# 检测 NewAPI 环境
#######################################
detect_environment() {
  log_info "正在检测 NewAPI 环境..."

  # 检测 NewAPI 容器
  NEWAPI_CONTAINER="${NEWAPI_CONTAINER:-}"
  if [[ -z "$NEWAPI_CONTAINER" ]]; then
    NEWAPI_CONTAINER="$(detect_newapi_container)" || die "找不到运行中的 NewAPI 容器（可设置环境变量 NEWAPI_CONTAINER=<容器名或ID> 手动指定）"
  fi
  log_success "找到 NewAPI 容器: $NEWAPI_CONTAINER"

  detect_newapi_redis_mutation_safety

  # 获取 compose 项目信息
  local compose_project compose_files user_compose_file
  compose_project="$(docker_inspect_label "$NEWAPI_CONTAINER" 'com.docker.compose.project' | trim)"
  compose_files="$(docker_inspect_label "$NEWAPI_CONTAINER" 'com.docker.compose.project.config_files' | trim)"

  user_compose_file="${COMPOSE_FILE_OVERRIDE:-}"
  if [[ -z "$user_compose_file" && -n "$compose_files" ]]; then
    user_compose_file="$(first_csv "$compose_files" | trim)"
  fi
  if [[ -n "$user_compose_file" && ! -r "$user_compose_file" ]]; then
    user_compose_file=""
  fi

  # 检测网络
  local networks network_mode
  networks="$(detect_networks_for_container "$NEWAPI_CONTAINER" | trim || true)"
  network_mode="$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$NEWAPI_CONTAINER" 2>/dev/null | trim || true)"

  ORIGINAL_NETWORK=""
  USE_BRIDGE_MODE=false
  USE_HOST_MODE=false

  if [[ "$network_mode" == "host" ]]; then
    # ===== Host 网络模式 =====
    # 注意：NewAPI 用 host 网络，不代表数据库也在宿主机上。
    # 常见反例：数据库是 1Panel / 另一套 compose 托管的容器，挂在某条 bridge 网络上，
    # DSN 里写的是它的 bridge IP（172.x）。这种 IP 只有 host 网络的 NewAPI 碰得到，
    # bridge 里的 newapi-tools 跨不过去。真正的网络方案要等解析完 DSN host 再定（见下方）。
    log_warn "检测到 NewAPI 使用 host 网络模式"
    USE_HOST_MODE=true
    NEWAPI_NETWORK=""
    ORIGINAL_NETWORK="host"
  else
    NEWAPI_NETWORK="${NEWAPI_NETWORK:-}"
    if [[ -z "$NEWAPI_NETWORK" ]]; then
      NEWAPI_NETWORK="$(echo "$networks" | head -n 1 | trim)"
    fi
    [[ -n "$NEWAPI_NETWORK" ]] || die "无法确定 NewAPI 容器的 Docker 网络"
    container_is_on_network "$NEWAPI_CONTAINER" "$NEWAPI_NETWORK" || die "容器 '$NEWAPI_CONTAINER' 未连接到网络 '$NEWAPI_NETWORK'"

    ORIGINAL_NETWORK="$NEWAPI_NETWORK"

    if [[ "$NEWAPI_NETWORK" == "bridge" ]]; then
      log_warn "检测到 NewAPI 使用默认 bridge 网络"
      log_warn "默认 bridge 网络不支持 Docker 服务发现，将使用 IPv4 地址模式"
      log_info ""
      log_info "提示：为获得更好的体验，建议将 NewAPI 部署在用户自定义网络中"
      log_info ""
      USE_BRIDGE_MODE=true

      # 创建一个用户自定义网络供 docker-compose 使用
      # 这样可以避免 "network-scoped alias" 错误
      if ! docker network inspect newapi-tools-network >/dev/null 2>&1; then
        log_info "创建网络 'newapi-tools-network' 供服务使用..."
        docker network create newapi-tools-network || die "创建网络失败"
      fi
      # 使用新创建的网络作为 NEWAPI_NETWORK（供 docker-compose.yml 使用）
      NEWAPI_NETWORK="newapi-tools-network"
      log_success "使用网络: $NEWAPI_NETWORK (数据库连接将使用 IPv4 地址)"
    else
      log_success "检测到网络: $NEWAPI_NETWORK"
    fi
  fi

  # 检测数据库 DSN
  local detected_dsn=""
  detected_dsn="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'SQL_DSN' || true)"
  [[ -z "$detected_dsn" ]] && detected_dsn="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'DATABASE_URL' || true)"
  [[ -z "$detected_dsn" ]] && detected_dsn="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'DB_DSN' || true)"
  SOURCE_SQL_DSN="$detected_dsn"

  DB_ENGINE=""
  DB_DNS=""
  DB_PORT=""
  if [[ -n "$detected_dsn" ]]; then
    DB_ENGINE="$(extract_dsn_engine "$detected_dsn")" ||
      die "NewAPI 数据库 DSN 不受支持或格式不安全；原始 DSN 已隐藏"
    DB_DNS="$(extract_dsn_host "$detected_dsn")" ||
      die "无法从 NewAPI 数据库 DSN 安全解析 host；原始 DSN 已隐藏"
    DB_PORT="$(extract_dsn_port "$detected_dsn")" ||
      die "无法从 NewAPI 数据库 DSN 安全解析端口；原始 DSN 已隐藏"
  fi

  # ===== Host 模式：完全从 DSN 解析凭证，跳过数据库容器探测 =====
  if [[ "$USE_HOST_MODE" == "true" ]]; then
    [[ -n "$detected_dsn" ]] || die "host 模式下必须能从 NewAPI 容器读取 SQL_DSN，但未检测到"
    [[ -n "$DB_ENGINE" ]] || die "无法从 DSN 识别数据库引擎；原始 DSN 已隐藏"

    # 先解析出完整凭证（下面判断"怎么连"时要用到端口）
    DB_USER="$(extract_dsn_user "$detected_dsn")" || die "无法安全解析数据库用户名；原始 DSN 已隐藏"
    DB_PASSWORD="$(extract_dsn_password "$detected_dsn")" || die "无法安全解析数据库密码；原始 DSN 已隐藏"
    DB_PORT="$(extract_dsn_port "$detected_dsn")" || die "无法安全解析数据库端口；原始 DSN 已隐藏"
    DB_NAME="$(extract_dsn_dbname "$detected_dsn")" || die "无法安全解析数据库名；原始 DSN 已隐藏"

    if [[ "$DB_ENGINE" == "postgres" ]]; then
      DB_PORT="${DB_PORT:-5432}"
      DB_USER="${DB_USER:-postgres}"
      DB_NAME="${DB_NAME:-new-api}"
    elif [[ "$DB_ENGINE" == "mysql" ]]; then
      DB_PORT="${DB_PORT:-3306}"
      DB_USER="${DB_USER:-root}"
      DB_NAME="${DB_NAME:-new-api}"
    fi

    # ===== 决定 newapi-tools 怎么连到这个数据库 =====
    # 四种情形：
    #  (a1) DSN host 是 127.0.0.1/localhost，但数据库其实是个容器、端口只发布在宿主机
    #       回环上（127.0.0.1:PORT，1Panel/宝塔最常见）→ host.docker.internal(网关)连不通，
    #       把 newapi-tools 挂进该容器的网络、用容器名直连，端口改用容器内端口。
    #  (a2) DSN host 是 127.0.0.1/localhost，数据库是宿主机裸装进程(或发布到 0.0.0.0)
    #       → 用 host.docker.internal 走宿主机网关。
    #  (b)  DSN host 是某条 bridge 网络上某容器的 IP → 挂进那条网络，host 改成容器名。
    #  (c)  其它（外部 DB、真实 LAN IP、域名）→ 原样保留，靠 extra_hosts / 宿主路由可达。
    if [[ "$DB_DNS" == "127.0.0.1" || "$DB_DNS" == "localhost" || "$DB_DNS" == "::1" ]]; then
      local _hit _hit_net _hit_name _hit_port
      _hit="$(find_container_by_published_port "$DB_DNS" "$DB_PORT")"
      _hit_net="$(printf '%s' "$_hit" | cut -f1)"
      _hit_name="$(printf '%s' "$_hit" | cut -f2)"
      _hit_port="$(printf '%s' "$_hit" | cut -f3)"
      if [[ -n "$_hit_net" && -n "$_hit_name" ]]; then
        # 情形 (a1)：DB 是容器，端口仅发布在宿主机回环 → 挂进它的网络、用容器名连
        NEWAPI_NETWORK="$_hit_net"
        log_warn "数据库 127.0.0.1:${DB_PORT} 实为容器 '${_hit_name}'（端口仅发布在宿主机回环，网关不可达）"
        log_info "将把 newapi-tools 接入网络 '${_hit_net}'，用容器名 '${_hit_name}:${_hit_port}' 直连（绕开回环端口映射）"
        DB_DNS="$_hit_name"
        DB_PORT="$_hit_port"
        USE_HOST_MODE=false   # 不再脱离 external 网络：我们要挂进它，而非走 host overlay
        ORIGINAL_NETWORK="$_hit_net"
      else
        # 情形 (a2)：DB 在宿主机回环但非容器（或发布到 0.0.0.0）→ 走宿主机网关
        DB_DNS="host.docker.internal"
        log_info "数据库地址改写: 127.0.0.1 → host.docker.internal（数据库在宿主机回环上）"
      fi
    elif is_ipv4_literal "$DB_DNS"; then
      local _hit _hit_net _hit_name
      _hit="$(find_container_by_network_ip "$DB_DNS")"
      _hit_net="$(printf '%s' "$_hit" | cut -f1)"
      _hit_name="$(printf '%s' "$_hit" | cut -f2)"
      if [[ -n "$_hit_net" && -n "$_hit_name" ]]; then
        NEWAPI_NETWORK="$_hit_net"
        log_warn "数据库 ${DB_DNS} 是容器 '${_hit_name}' 在 bridge 网络 '${_hit_net}' 上的 IP"
        log_info "将把 newapi-tools 接入网络 '${_hit_net}'，并用容器名连接（IP 重启会变，容器名不会）"
        DB_DNS="$_hit_name"
        USE_HOST_MODE=false   # 不再走 host overlay：我们要挂进 external 网络，而非脱离它
        ORIGINAL_NETWORK="$_hit_net"
      else
        log_warn "数据库地址 ${DB_DNS} 是 IP 但不属于任何已知 docker 网络容器，按外部地址原样使用"
      fi
    else
      log_info "数据库地址为主机名/外部地址，原样使用: ${DB_DNS}"
    fi

    if [[ "$USE_HOST_MODE" == "true" ]]; then
      # 情形 (a)/(c)：数据库走宿主机（host.docker.internal）或外部地址，
      # newapi-tools 无需任何 external 网络 → 加载 host overlay 去掉 newapi-network 依赖。
      configure_host_compose_files
      log_success "检测到数据库 (host 模式): $DB_ENGINE @ $DB_DNS:$DB_PORT/$DB_NAME"
    else
      # 情形 (b)：数据库在另一条 bridge 网络上，newapi-tools 已改为挂进该网络（NEWAPI_NETWORK）。
      # 用主 compose（newapi-network=external 指向该网络），不加载 host overlay。
      log_success "检测到数据库 (跨网 bridge): $DB_ENGINE @ $DB_DNS:$DB_PORT/$DB_NAME (网络: $NEWAPI_NETWORK)"
    fi
    return 0
  fi

  # 检测数据库容器
  local db_container="" db_service=""
  if [[ -n "$compose_project" ]]; then
    local pg_cid mysql_cid
    pg_cid="$(detect_db_container_by_compose_service "$compose_project" 'postgres')"
    mysql_cid="$(detect_db_container_by_compose_service "$compose_project" 'mysql')"
    if [[ -n "$pg_cid" && -z "$mysql_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-postgres}"
      db_container="$pg_cid"
      db_service="postgres"
    elif [[ -n "$mysql_cid" && -z "$pg_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-mysql}"
      db_container="$mysql_cid"
      db_service="mysql"
    fi
  fi

  # 通过端口检测（使用原始网络，因为数据库可能还未连接到新网络）
  local detect_network="${ORIGINAL_NETWORK:-$NEWAPI_NETWORK}"
  if [[ -z "$db_container" ]]; then
    local pg_cid mysql_cid
    pg_cid="$(detect_db_container_by_exposed_port "$detect_network" '5432/tcp' || true)"
    mysql_cid="$(detect_db_container_by_exposed_port "$detect_network" '3306/tcp' || true)"
    if [[ -n "$pg_cid" && -z "$mysql_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-postgres}"
      db_container="$pg_cid"
    elif [[ -n "$mysql_cid" && -z "$pg_cid" ]]; then
      DB_ENGINE="${DB_ENGINE:-mysql}"
      db_container="$mysql_cid"
    fi
  fi

  DB_ENGINE="${DB_ENGINE:-postgres}"

  # 尝试常见容器名（使用原始网络）
  if [[ -z "$db_container" ]]; then
    if docker ps -q --filter "network=$detect_network" --filter "name=^/postgres$" | head -n 1 | grep -q .; then
      db_container="postgres"
      DB_ENGINE="postgres"
      db_service="postgres"
    elif docker ps -q --filter "network=$detect_network" --filter "name=^/mysql$" | head -n 1 | grep -q .; then
      db_container="mysql"
      DB_ENGINE="mysql"
      db_service="mysql"
    fi
  fi

  [[ -n "$db_container" ]] || die "在网络 '$detect_network' 上找不到数据库容器"
  DB_CONTAINER="$db_container"

  # 设置 DB_DNS
  # 优先级：1. 用户指定的 DB_DNS  2. IPv4 地址（bridge模式必须）  3. 服务名
  if [[ -n "$DB_DNS" ]]; then
    # 用户已指定（从 SQL_DSN 解析出来），保持不变
    log_info "使用指定的数据库主机: $DB_DNS"
  else
    # 尝试获取 IPv4 地址
    local db_ipv4=""
    db_ipv4="$(get_container_ipv4 "$db_container" "$detect_network" | trim)"

    if [[ "$USE_BRIDGE_MODE" == "true" ]]; then
      # Bridge 模式：必须使用 IPv4 地址，因为不支持服务发现
      if [[ -n "$db_ipv4" ]]; then
        DB_DNS="$db_ipv4"
        log_info "使用数据库 IPv4 地址: $db_ipv4 (bridge 模式)"
      else
        die "无法获取数据库容器的 IPv4 地址，请手动指定 DB_DNS 环境变量"
      fi
    else
      # 用户自定义网络：优先使用 IPv4，其次使用服务名
      if [[ -n "$db_ipv4" ]]; then
        DB_DNS="$db_ipv4"
        log_info "使用数据库 IPv4 地址: $db_ipv4"
      elif [[ -n "$db_service" ]]; then
        DB_DNS="$db_service"
        log_info "使用数据库服务名: $db_service"
      else
        db_service="$(docker_inspect_label "$db_container" 'com.docker.compose.service' | trim)"
        if [[ -n "$db_service" ]]; then
          DB_DNS="$db_service"
        else
          DB_DNS="$db_container"
        fi
        log_info "使用数据库主机: $DB_DNS"
      fi
    fi
  fi

  # 获取数据库凭证
  if [[ "$DB_ENGINE" == "postgres" ]]; then
    DB_PORT="${DB_PORT:-5432}"
    DB_USER="$(docker_inspect_env_value "$db_container" 'POSTGRES_USER' || true)"
    DB_NAME="$(docker_inspect_env_value "$db_container" 'POSTGRES_DB' || true)"
    DB_PASSWORD="$(docker_inspect_env_value "$db_container" 'POSTGRES_PASSWORD' || true)"
    DB_USER="${DB_USER:-postgres}"
    DB_NAME="${DB_NAME:-new-api}"
  elif [[ "$DB_ENGINE" == "mysql" ]]; then
    DB_PORT="${DB_PORT:-3306}"
    DB_USER="$(docker_inspect_env_value "$db_container" 'MYSQL_USER' || true)"
    DB_NAME="$(docker_inspect_env_value "$db_container" 'MYSQL_DATABASE' || true)"
    DB_PASSWORD="$(docker_inspect_env_value "$db_container" 'MYSQL_PASSWORD' || true)"
    [[ -z "$DB_PASSWORD" ]] && DB_PASSWORD="$(docker_inspect_env_value "$db_container" 'MYSQL_ROOT_PASSWORD' || true)"
    DB_USER="${DB_USER:-root}"
    DB_NAME="${DB_NAME:-new-api}"
  else
    die "不支持的数据库引擎: $DB_ENGINE"
  fi

  log_success "检测到数据库: $DB_ENGINE @ $DB_DNS:$DB_PORT/$DB_NAME"
}

#######################################
# 检测日志分库（LOG_SQL_DSN）
#
# 部分 NewAPI fork 用 LOG_SQL_DSN 把 logs 表整张分离到独立库。本工具需读取该库
# 才能看到实时日志/流量。本函数从 NewAPI 容器读取 LOG_SQL_DSN，解析并做与主库
# 同款的「容器名/网络」改写，产出：
#   LOG_SQL_DSN_FINAL  写入工具 .env 的最终 DSN（容器名直连/host.docker.internal）
#   LOG_NETWORK        日志库容器所在网络（与主库不同时需把工具接入它）
# NewAPI 未启用 LOG_SQL_DSN 时全部留空，工具自动回落主库（向后兼容）。
#######################################
detect_log_database() {
  LOG_SQL_DSN_FINAL=""
  LOG_NETWORK=""

  local raw=""
  raw="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'LOG_SQL_DSN' || true)"
  if [[ -z "$raw" ]]; then
    log_info "NewAPI 未启用日志分库（无 LOG_SQL_DSN），工具将从主库读取日志"
    return 0
  fi
  log_info "检测到 NewAPI 启用了日志分库（LOG_SQL_DSN），开始配置独立日志库连接..."

  local engine host port db
  engine="$(extract_dsn_engine "$raw")" || die "LOG_SQL_DSN 不受支持或格式不安全；原始 DSN 已隐藏"
  host="$(extract_dsn_host "$raw")" || die "无法从 LOG_SQL_DSN 安全解析 host；原始 DSN 已隐藏"
  port="$(extract_dsn_port "$raw")" || die "无法从 LOG_SQL_DSN 安全解析端口；原始 DSN 已隐藏"
  db="$(extract_dsn_dbname "$raw")" || die "无法从 LOG_SQL_DSN 安全解析数据库名；原始 DSN 已隐藏"
  [[ -n "$host" && -n "$db" ]] || die "LOG_SQL_DSN 缺少 host 或 dbname；原始 DSN 已隐藏"
  if [[ "$engine" == "mysql" ]]; then port="${port:-3306}"; else engine="postgres"; port="${port:-5432}"; fi

  # 与主库同款的连接方式改写
  if [[ "$host" == "127.0.0.1" || "$host" == "localhost" || "$host" == "::1" ]]; then
    local _hit _net _name _port
    _hit="$(find_container_by_published_port "$host" "$port")"
    _net="$(printf '%s' "$_hit" | cut -f1)"; _name="$(printf '%s' "$_hit" | cut -f2)"; _port="$(printf '%s' "$_hit" | cut -f3)"
    if [[ -n "$_net" && -n "$_name" ]]; then
      log_warn "日志库 ${host}:${port} 实为容器 '${_name}'（端口仅发布在宿主机回环）"
      log_info "将把 newapi-tools 接入网络 '${_net}'，用容器名 '${_name}:${_port}' 直连"
      host="$_name"; port="$_port"; LOG_NETWORK="$_net"
    else
      host="host.docker.internal"
      log_info "日志库在宿主机回环上，改写为 host.docker.internal"
    fi
  elif is_ipv4_literal "$host"; then
    local _hit _net _name
    _hit="$(find_container_by_network_ip "$host")"
    _net="$(printf '%s' "$_hit" | cut -f1)"; _name="$(printf '%s' "$_hit" | cut -f2)"
    if [[ -n "$_net" && -n "$_name" ]]; then
      log_warn "日志库 ${host} 是容器 '${_name}' 在网络 '${_net}' 上的 IP"
      log_info "将把 newapi-tools 接入网络 '${_net}'，用容器名直连"
      host="$_name"; LOG_NETWORK="$_net"
    else
      log_info "日志库地址 ${host} 是 IP 但不属于已知 docker 网络容器，按外部地址原样使用"
    fi
  else
    log_info "日志库地址为主机名/外部地址，原样使用: ${host}"
  fi

  LOG_SQL_DSN_FINAL="$(rewrite_dsn_host_port "$raw" "$host" "$port")" ||
    die "无法安全改写 LOG_SQL_DSN 的数据库地址；原始 DSN 已隐藏"

  # 日志库网络与主库不同时，追加日志叠加层并接入网络（相同则工具已在该网络上）
  if [[ -n "$LOG_NETWORK" && "$LOG_NETWORK" != "${NEWAPI_NETWORK:-}" ]]; then
    if [[ -f "$COMPOSE_LOGDB_FILE" ]]; then
      COMPOSE_FILES+=("-f" "$COMPOSE_LOGDB_FILE")
      log_success "已启用日志库网络叠加层（network=${LOG_NETWORK}）"
    else
      log_warn "未找到 $COMPOSE_LOGDB_FILE，日志库网络持久化可能失效（仍会用 docker network connect 兜底）"
    fi
  elif [[ -n "$LOG_NETWORK" ]]; then
    log_info "日志库与主库同在网络 '${LOG_NETWORK}'，无需额外网络配置"
    LOG_NETWORK=""  # 已通过主库网络可达，无需重复接入
  fi

  log_success "检测到日志分库: ${engine} @ ${host}:${port}/${db}${LOG_NETWORK:+ (网络: ${LOG_NETWORK})}"
}

#######################################
# 交互式配置
#######################################
interactive_config() {
  log_info "开始配置..."
  echo ""

  AUTO_GENERATED_PASSWORD=false

  # 前端访问密码
  if [[ -z "${ADMIN_PASSWORD:-}" ]]; then
    echo -e "${YELLOW}请设置前端访问密码${NC} ${BLUE}(直接回车自动生成 32 位强密码)${NC}:"
    read -srp "密码: " ADMIN_PASSWORD
    echo ""

    if [[ -z "$ADMIN_PASSWORD" ]]; then
      ADMIN_PASSWORD="$(generate_strong_password)"
      AUTO_GENERATED_PASSWORD=true
      log_success "已自动生成强密码（部署完成后会显示，请妥善保存）"
    else
      while true; do
        read -srp "确认密码: " ADMIN_PASSWORD_CONFIRM
        echo ""
        if [[ "$ADMIN_PASSWORD" == "$ADMIN_PASSWORD_CONFIRM" ]]; then
          break
        fi
        log_error "两次输入的密码不一致，请重新输入"
        echo ""
        read -srp "密码: " ADMIN_PASSWORD
        echo ""
        if [[ -z "$ADMIN_PASSWORD" ]]; then
          ADMIN_PASSWORD="$(generate_strong_password)"
          AUTO_GENERATED_PASSWORD=true
          log_success "已自动生成强密码"
          break
        fi
      done
    fi
  fi
  log_success "前端密码已设置"

  # API Key 自动生成
  API_KEY="${API_KEY:-$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | xxd -p | tr -d '\n' | head -c 64)}"

  # 前端端口默认 1145
  FRONTEND_PORT="${FRONTEND_PORT:-1145}"

  # 前端端口暴露范围
  if [[ -z "${FRONTEND_BIND:-}" ]]; then
    echo ""
    echo -e "${YELLOW}前端端口暴露范围${NC}"
    echo -e "  ${GREEN}1) 仅本机${NC}      仅监听 127.0.0.1，需自行配宿主机 nginx / Caddy 反代到 HTTPS 域名 ${GREEN}(默认，推荐)${NC}"
    echo -e "  ${YELLOW}2) 公网可达${NC}    显式监听 0.0.0.0；必须置于 HTTPS 反向代理、访问控制和防火墙之后"
    read -r -p "请选择 [1/2，回车默认 1]: " bind_choice
    case "$bind_choice" in
      2)
        FRONTEND_BIND="0.0.0.0"
        log_warn "已显式选择公网模式；请勿使用明文 HTTP，必须配置 HTTPS 反向代理和访问控制"
        ;;
      *)
        FRONTEND_BIND="127.0.0.1"
        log_info "已选仅本机模式，部署完成后请配置宿主机 HTTPS 反向代理"
        ;;
    esac
  fi

  # Go 仅在直接对端命中此精确列表时解析 X-Forwarded-For。
  # 合并镜像的内层 Nginx 通过 loopback 访问后端。
  TRUSTED_PROXY_CIDRS="${TRUSTED_PROXY_CIDRS:-127.0.0.1/32,::1/128}"

  echo ""
}

ensure_container_on_network() {
	local container="$1" network="$2" label="$3"
	[[ -n "$network" ]] || return 0
	if container_is_on_network "$container" "$network"; then
		return 0
	fi
	log_info "连接到${label}: $network"
	if ! docker network connect "$network" "$container" 2>/dev/null &&
		! container_is_on_network "$container" "$network"; then
		die "无法连接到${label} '$network'，请检查网络是否存在以及 Docker 权限"
	fi
	container_is_on_network "$container" "$network" || die "连接${label} '$network' 后验证失败"
	log_success "已连接到${label}: $network"
}

#######################################
# 生成 .env 文件
#######################################
generate_env_file() {
  log_info "生成配置文件: $ENV_FILE"

  # 构建 SQL_DSN
  local sql_dsn=""
  if [[ -n "${SOURCE_SQL_DSN:-}" ]]; then
    sql_dsn="$(rewrite_dsn_host_port "$SOURCE_SQL_DSN" "$DB_DNS" "$DB_PORT")" ||
      die "无法安全改写 NewAPI 数据库 DSN；原始 DSN 已隐藏"
  elif [[ "$DB_ENGINE" == "postgres" ]]; then
    sql_dsn="host=${DB_DNS} port=${DB_PORT} user=${DB_USER} password=${DB_PASSWORD} dbname=${DB_NAME} sslmode=disable"
  elif [[ "$DB_ENGINE" == "mysql" ]]; then
    sql_dsn="${DB_USER}:${DB_PASSWORD}@tcp(${DB_DNS}:${DB_PORT})/${DB_NAME}?charset=utf8mb4&parseTime=True"
  fi
	local jwt_secret
	jwt_secret="$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | xxd -p | tr -d '\n' | head -c 64)"
	local observability_token
	observability_token="${OBSERVABILITY_TOKEN:-$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | xxd -p | tr -d '\n' | head -c 64)}"

	# Derive a network-local NewAPI Admin API endpoint. Credentials remain an
	# explicit operator input; without them the console starts in read-only mode.
	local newapi_port newapi_baseurl
	newapi_port="$(docker_inspect_env_value "$NEWAPI_CONTAINER" 'PORT' || true)"
	newapi_port="${newapi_port:-3000}"
	is_valid_tcp_port "$newapi_port" || die "NewAPI PORT 必须是 1..65535 的十进制端口"
	if [[ -n "${NEWAPI_BASEURL:-}" ]]; then
		newapi_baseurl="$NEWAPI_BASEURL"
	elif [[ "$USE_HOST_MODE" == "true" ]]; then
		newapi_baseurl="http://host.docker.internal:${newapi_port}"
	else
		newapi_baseurl="http://${NEWAPI_CONTAINER}:${newapi_port}"
	fi

  # 给生成的 .env 标记网络模式，方便后续 status / 故障排查辨认
  local network_mode_tag="custom"
  [[ "$USE_BRIDGE_MODE" == "true" ]] && network_mode_tag="bridge"
  [[ "$USE_HOST_MODE" == "true" ]] && network_mode_tag="host"

  local env_content
  env_content="$(cat <<EOF
# NewAPI Middleware Tool 配置文件
# 由 deploy.sh 自动生成于 $(date '+%Y-%m-%d %H:%M:%S')
# 网络部署模式: ${network_mode_tag}

# 完整镜像引用（支持 tag 或 repo@sha256:digest）
NEWAPI_TOOLS_IMAGE=$(dotenv_quote "$NEWAPI_TOOLS_IMAGE")

# NewAPI 环境
NEWAPI_CONTAINER=${NEWAPI_CONTAINER}
NEWAPI_NETWORK=${NEWAPI_NETWORK}
# 供 install.sh 在容器重建后恢复手动附加的网络
NEWAPI_NETWORK_MODE=${network_mode_tag}
NEWAPI_ORIGINAL_NETWORK=${ORIGINAL_NETWORK:-}
NEWAPI_BASEURL=$(dotenv_quote "$newapi_baseurl")
NEWAPI_ADMIN_ACCESS_TOKEN=$(dotenv_quote "${NEWAPI_ADMIN_ACCESS_TOKEN:-}")
NEWAPI_ADMIN_USER_ID=${NEWAPI_ADMIN_USER_ID:-0}
# 仅当 NewAPI 容器明确配置 REDIS_CONN_STRING=（空）时为 true；未知或非空均为 false
NEWAPI_REDIS_DISABLED=${NEWAPI_REDIS_DISABLED:-false}
# 高风险兼容开关：默认禁止旁路批量判定/删除不活跃用户，请优先使用 NewAPI 管理 API
ALLOW_UNSAFE_BATCH_DELETE=false
# 高风险兼容开关：默认禁止不完整的直接数据库永久删除，请优先使用 NewAPI 管理 API
ALLOW_UNSAFE_HARD_DELETE=false
# 隐私敏感：默认不持续覆盖用户的 record_ip_log 设置

# 数据库配置 (Go 版本推荐 SQL_DSN)
SQL_DSN=$(dotenv_quote "$sql_dsn")
DB_ENGINE=${DB_ENGINE}
DB_DNS=${DB_DNS}
DB_PORT=${DB_PORT}
DB_NAME=$(dotenv_quote "$DB_NAME")
DB_USER=$(dotenv_quote "$DB_USER")
DB_PASSWORD=$(dotenv_quote "$DB_PASSWORD")

# 日志分库 (NewAPI 启用 LOG_SQL_DSN 时自动检测；为空则日志查询回落主库)
LOG_SQL_DSN=$(dotenv_quote "${LOG_SQL_DSN_FINAL:-}")
# 日志库容器所在网络 (与主库不同时由 docker-compose.logdb.yml 叠加层接入)
LOG_NETWORK=${LOG_NETWORK:-}

# 认证配置
ADMIN_PASSWORD=$(dotenv_quote "$ADMIN_PASSWORD")
API_KEY=$(dotenv_quote "$API_KEY")
API_KEY_ROLE=${API_KEY_ROLE:-viewer}

# 服务配置
FRONTEND_PORT=${FRONTEND_PORT}
FRONTEND_BIND=${FRONTEND_BIND}
# 外层反向代理存在时，请追加内层 Nginx 实际看到的代理 IP（精确 /32 或 /128）
TRUSTED_PROXY_CIDRS=${TRUSTED_PROXY_CIDRS}
TIMEZONE=Asia/Shanghai
LOG_LEVEL=info
OBSERVABILITY_TOKEN=$(dotenv_quote "$observability_token")
LOG_FRESHNESS_MAX_SECONDS=${LOG_FRESHNESS_MAX_SECONDS:-900}
TOOL_STORE_PATH=/app/data/control-plane.db

# JWT 配置
JWT_SECRET=$(dotenv_quote "$jwt_secret")
JWT_EXPIRE_HOURS=24

# Redis 配置
REDIS_PASSWORD=''
EOF
)"
  validate_deploy_credential_separation_content "$env_content" ||
    die "凭据预检失败；完整部署配置未发布"
  atomic_write_deploy_dotenv "$ENV_FILE" "$env_content" ||
    die "无法原子持久化完整部署配置；旧配置保持不变"
  DEPLOY_ENV_GENERATED_THIS_RUN=true
  log_success "配置文件已生成"
}

#######################################
# 检查 docker-compose.yml 是否存在
#######################################
check_compose_file() {
  if [[ ! -f "$COMPOSE_FILE" ]]; then
    die "找不到 docker-compose.yml 文件，请确保在项目根目录运行此脚本"
  fi
  log_success "找到 Docker Compose 配置文件"
}

#######################################
# 下载 GeoIP 数据库
#######################################
download_geoip_database() {
  local geoip_dir="${SCRIPT_DIR}/data/geoip"
  local city_db="${geoip_dir}/GeoLite2-City.mmdb"
  local expected_sha256="168b01d10d0742129be1bee92bba85affaaefcf2e86b4187bcf1924ea50068bf"
  local actual_sha256=""
  mkdir -p "$geoip_dir"

  if [[ ! -f "$city_db" ]]; then
    log_info "主机侧 GeoIP 自动下载已禁用；将使用镜像内的固定校验快照"
    return 0
  fi

  actual_sha256="$(sha256sum "$city_db" 2>/dev/null | awk '{print $1}' || true)"
  if [[ "$actual_sha256" == "$expected_sha256" ]]; then
    log_success "手工挂载的 GeoIP 数据库 checksum 已验证"
    return 0
  fi

  if rm -f -- "$city_db"; then
    log_warn "手工挂载的 GeoIP 数据库 checksum 无效；已移除并回退镜像内固定快照"
  else
    log_warn "无法移除无效 GeoIP 文件；运行时仍会拒绝其 checksum 并使用镜像内快照"
  fi
}

# Compose gives exported shell variables precedence over --env-file. Run every
# transactional Compose operation in a subshell with project interpolation
# variables removed, then opt in only to the image being activated. This keeps
# operator-supplied candidate secrets/bindings from contaminating rollback.
deploy_compose_env_keys() {
  local env_file="$1"
  [[ -r "$env_file" ]] || return 1
  {
    grep -hoE '\$\{[A-Za-z_][A-Za-z0-9_]*' \
      "$COMPOSE_FILE" "$COMPOSE_HOST_FILE" "$COMPOSE_LOGDB_FILE" 2>/dev/null |
      sed 's/^${//' || true
    printf '%s\n' \
      NEWAPI_TOOLS_IMAGE NEWAPI_TOOLS_VERSION \
      COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_PROFILES \
      COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE
  } | sort -u
}

run_deploy_compose() {
  local env_file="$1" image_override="${2:-}" project_name
  local -a project_args=()
  shift 2
  [[ -r "$env_file" ]] || {
    log_error "Compose 配置文件不可读：${env_file}"
    return 1
  }
  project_name="${DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE:-}"
  [[ -n "$project_name" ]] || project_name="$(env_file_value "$env_file" 'COMPOSE_PROJECT_NAME')"
  if [[ -n "$project_name" ]]; then
    [[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || {
      log_error "旧配置中的 COMPOSE_PROJECT_NAME 格式无效，拒绝猜测 Compose 项目"
      return 1
    }
    project_args=(-p "$project_name")
  fi
  (
    local key
    while IFS= read -r key; do
      [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
      unset "$key" 2>/dev/null || true
    done < <(deploy_compose_env_keys "$env_file")
    if [[ -n "$image_override" ]]; then
      NEWAPI_TOOLS_IMAGE="$image_override"
      export NEWAPI_TOOLS_IMAGE
    fi
    $DOCKER_COMPOSE "${project_args[@]}" "${COMPOSE_FILES[@]}" --env-file "$env_file" "$@"
  )
}

# Rebuild both Compose overlays and runtime network state from the restored old
# dotenv file. Candidate discovery globals are not authoritative during a
# rollback because the old deployment may have used host, bridge, custom, or a
# separate log database network.
configure_deploy_context_from_env() {
  local env_file="$1" mode configured_container
  COMPOSE_FILES=("-f" "$COMPOSE_FILE")
  configured_container="$(env_file_value "$env_file" 'NEWAPI_CONTAINER')"
  [[ -z "$configured_container" ]] || NEWAPI_CONTAINER="$configured_container"
  NEWAPI_NETWORK="$(env_file_value "$env_file" 'NEWAPI_NETWORK')"
  ORIGINAL_NETWORK="$(env_file_value "$env_file" 'NEWAPI_ORIGINAL_NETWORK')"
  LOG_NETWORK="$(env_file_value "$env_file" 'LOG_NETWORK')"
  mode="$(env_file_value "$env_file" 'NEWAPI_NETWORK_MODE')"
  if [[ -z "$mode" ]]; then
    mode="$(sed -n 's/^# 网络部署模式:[[:space:]]*//p' "$env_file" 2>/dev/null | head -n1 | tr -d '\r\n')"
  fi
  if [[ -z "$mode" ]]; then
    if ! grep -qE '^NEWAPI_NETWORK=' "$env_file" 2>/dev/null; then
      # Legacy/manual dotenv files without an explicit marker use the base
      # Compose fallback network, not the host overlay.
      mode="custom"
    elif [[ -z "$NEWAPI_NETWORK" ]]; then
      mode="host"
    elif [[ "$NEWAPI_NETWORK" == "newapi-tools-network" ]]; then
      mode="bridge"
    else
      mode="custom"
    fi
  fi

  USE_HOST_MODE=false
  USE_BRIDGE_MODE=false
  case "$mode" in
    host)
      USE_HOST_MODE=true
      [[ -f "$COMPOSE_HOST_FILE" ]] || {
        log_error "恢复 host 网络配置需要 ${COMPOSE_HOST_FILE}"
        return 1
      }
      COMPOSE_FILES+=("-f" "$COMPOSE_HOST_FILE")
      ;;
    bridge)
      USE_BRIDGE_MODE=true
      ORIGINAL_NETWORK="${ORIGINAL_NETWORK:-bridge}"
      ;;
  esac

  if [[ -n "$LOG_NETWORK" && "$LOG_NETWORK" != "$NEWAPI_NETWORK" && -f "$COMPOSE_LOGDB_FILE" ]]; then
    COMPOSE_FILES+=("-f" "$COMPOSE_LOGDB_FILE")
  elif [[ "$LOG_NETWORK" == "$NEWAPI_NETWORK" ]]; then
    LOG_NETWORK=""
  fi
}

is_v05_ready_response() {
  local body="${1:-}"
  printf '%s' "$body" | grep -Eq '^[[:space:]]*\{' &&
    printf '%s' "$body" | grep -Eq '"status"[[:space:]]*:[[:space:]]*"ready"' &&
    printf '%s' "$body" | grep -Eq '"main_database"[[:space:]]*:[[:space:]]*"ok"' &&
    printf '%s' "$body" | grep -Eq '"tool_store"[[:space:]]*:[[:space:]]*"ok"'
}

is_legacy_db_ready_response() {
  local body="${1:-}"
  printf '%s' "$body" | grep -Eq '^[[:space:]]*\{' &&
    printf '%s' "$body" | grep -Eq '"success"[[:space:]]*:[[:space:]]*true' &&
    printf '%s' "$body" | grep -Eq '"status"[[:space:]]*:[[:space:]]*"connected"'
}

verify_deploy_application_health() {
  local mode="$1" container="${2:-newapi-tools}" port="${3:-8080}" ready_body="" legacy_body=""
  ready_body="$(docker exec "$container" curl --silent --show-error \
    "http://localhost:${port}/readyz" 2>/dev/null || true)"
  if is_v05_ready_response "$ready_body"; then
    return 0
  fi
  [[ "$mode" == "rollback" ]] || return 1

  # A v0.5 JSON readiness response is authoritative even when not ready. Only
  # an absent/non-JSON legacy endpoint (v0.2 serves SPA HTML here) may use the
  # compatibility database probe.
  if printf '%s' "$ready_body" | grep -Eq '^[[:space:]]*\{'; then
    return 1
  fi
  legacy_body="$(docker exec "$container" curl --fail --silent --show-error \
    "http://localhost:${port}/api/health/db" 2>/dev/null || true)"
  is_legacy_db_ready_response "$legacy_body"
}

connect_deploy_runtime_networks() {
  local tools_container="${1:-newapi-tools}"
  # 将容器连接到 NewAPI 网络（用于访问数据库）。基础 Compose 已声明主要
  # 网络；这里恢复 bridge / 日志库场景下的运行时附加网络。
  if [[ "$USE_HOST_MODE" == "true" ]]; then
    log_info "host 模式：跳过 docker network connect"
  elif [[ "$USE_BRIDGE_MODE" == "true" ]]; then
    local bridge_network="${ORIGINAL_NETWORK:-bridge}"
    ensure_container_on_network "$NEWAPI_CONTAINER" "$NEWAPI_NETWORK" "NewAPI Tool API 网络"
    ensure_container_on_network "$tools_container" "$bridge_network" "NewAPI 原始 bridge 网络"
  elif [[ -n "$NEWAPI_NETWORK" ]]; then
    ensure_container_on_network "$tools_container" "$NEWAPI_NETWORK" "NewAPI 网络"
  fi

  if [[ -n "${LOG_NETWORK:-}" ]]; then
    ensure_container_on_network "$tools_container" "$LOG_NETWORK" "日志库网络"
  fi
}

start_deploy_services_and_wait() {
  local health_mode="${1:-candidate}" image_override="${2:-$NEWAPI_TOOLS_IMAGE}"
  if ! run_deploy_compose "$ENV_FILE" "$image_override" up -d; then
    return 1
  fi
  connect_deploy_runtime_networks || return 1
  run_deploy_compose "$ENV_FILE" "$image_override" \
    up -d --wait --wait-timeout 180 || return 1
  verify_deploy_application_health "$health_mode"
}

deploy_isolated_candidate_name() {
  local container request_id compose_project candidate_image project
  toolstore_txn_candidate_identity "$SCRIPT_DIR" container request_id compose_project candidate_image project || return 1
  printf '%s\n' "$container"
}

stop_deploy_isolated_candidate() {
  toolstore_txn_remove_candidate_container "$SCRIPT_DIR"
}

start_deploy_isolated_candidate_and_wait() {
  local image_override="$1" container request_id compose_project candidate_image project created_ref container_id deadline
  toolstore_txn_candidate_identity "$SCRIPT_DIR" container request_id compose_project candidate_image project || return 1
  [[ "$image_override" == "$candidate_image" ]] || return 1
  ! docker inspect "$container" >/dev/null 2>&1 || return 1
  created_ref="$(run_deploy_compose "$ENV_FILE" "$image_override" \
    run --no-deps -d --name "$container" \
    --label "io.newapi-tools.candidate.purpose=toolstore-migration-candidate" \
    --label "io.newapi-tools.candidate.request-id=${request_id}" \
    --label "io.newapi-tools.candidate.compose-project=${compose_project}" \
    --label "io.newapi-tools.candidate.project-dir=${project}" \
    --label "io.newapi-tools.candidate.image=${candidate_image}" \
    -e SERVER_HOST=127.0.0.1 -e SERVER_PORT=8000 -e MODEL_PROBE_ENABLED=false \
    --entrypoint /app/server newapi-tools)" || return 1
  created_ref="${created_ref##*$'\n'}"
  created_ref="${created_ref%$'\r'}"
  [[ "$created_ref" =~ ^[0-9a-f]{12,64}$ ]] || return 1
  container_id="$(docker inspect --format '{{.Id}}' "$created_ref" 2>/dev/null)" || return 1
  [[ "$container_id" =~ ^[0-9a-f]{64}$ ]] || return 1
  if ! toolstore_txn_record_candidate_container_id "$SCRIPT_DIR" "$container_id"; then
    docker rm -f "$container_id" >/dev/null 2>&1 || return 1
    ! docker inspect "$container_id" >/dev/null 2>&1 || return 1
    return 1
  fi
  if ! toolstore_txn_candidate_container_matches "$SCRIPT_DIR" "$container_id"; then
    toolstore_txn_remove_created_candidate_by_id "$SCRIPT_DIR" "$container_id" || return 1
    return 1
  fi
  connect_deploy_runtime_networks "$container_id" || return 1
  deadline=$((SECONDS + 180))
  while (( SECONDS < deadline )); do
    [[ "$(docker inspect -f '{{.State.Running}}' "$container_id" 2>/dev/null || true)" == "true" ]] || return 1
    if verify_deploy_application_health candidate "$container_id" 8000; then
      return 0
    fi
    sleep 2
  done
  return 1
}

restore_deploy_previous_configuration() {
  local rollback_content="$1" rollback_image="$2"
  if [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
    rollback_content="$(load_deploy_rollback_snapshot)" || {
      log_error "高危：持久化回滚快照不可读；拒绝使用仅存在于进程内存的旧配置"
      return 1
    }
  fi
  if ! (restore_deploy_env_content "$ENV_FILE" "$rollback_content" "$rollback_image"); then
    log_error "高危：无法把磁盘配置恢复为旧部署；请立即停止人工重启并检查 ${ENV_FILE}"
    return 1
  fi
  if [[ -n "$DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE" ]] &&
    ! (persist_deploy_compose_project_env "$ENV_FILE" "$DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE"); then
    log_error "高危：旧配置已恢复，但无法持久化权威 Compose project 标识"
    return 1
  fi
  NEWAPI_TOOLS_IMAGE="$rollback_image"
  export NEWAPI_TOOLS_IMAGE
  configure_deploy_context_from_env "$ENV_FILE" || {
    log_error "高危：旧配置已写回，但无法重建旧 Compose/网络上下文"
    return 1
  }
}

rollback_deploy_to_previous() {
  local rollback_content="$1" rollback_image="$2" cleanup_candidate="${3:-true}"
  local txn_content="" txn_state=""
  restore_deploy_previous_configuration "$rollback_content" "$rollback_image" || return 1

  if [[ "$cleanup_candidate" == "true" ]]; then
    if ! run_deploy_compose "$ENV_FILE" "$rollback_image" down; then
      log_error "清理失败或部分启动的候选服务时发生错误；仍将尝试重建旧服务"
    fi
  fi
  if toolstore_txn_has_active "$SCRIPT_DIR"; then
    stop_deploy_isolated_candidate || return 1
    txn_content="$(toolstore_txn_load_record "$SCRIPT_DIR")" || return 1
    txn_state="$(toolstore_txn_env_value "$txn_content" STATE)"
    toolstore_txn_unstage_data "$SCRIPT_DIR" || return 1
    if [[ "$txn_state" != "preparing" && "$txn_state" != "preflight" ]]; then
      toolstore_txn_restore_database "$SCRIPT_DIR" || {
        log_error "Tool Store 原子恢复失败；旧镜像不会在可能不兼容的 schema 上启动，回滚证据已保留"
        return 1
      }
    fi
  fi
  start_deploy_services_and_wait rollback "$rollback_image" || return 1
  if toolstore_txn_has_active "$SCRIPT_DIR"; then
    if [[ "$txn_state" == "preparing" || "$txn_state" == "preflight" ]]; then
      toolstore_txn_retire_preflight "$SCRIPT_DIR" || return 1
    else
      toolstore_txn_mark_rolled_back "$SCRIPT_DIR" || return 1
    fi
  fi
}

# Recover the database/config/image transaction before any new interactive
# configuration is generated. This is the crash-entry point for a host reboot
# after a candidate migrated the Tool Store or a safe reinstall staged data.
recover_active_deploy_toolstore_transaction() {
  local content state rollback_image candidate_image
  toolstore_txn_has_active "$SCRIPT_DIR" || return 0
  content="$(toolstore_txn_load_record "$SCRIPT_DIR")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  if [[ "$state" == "authoritative" &&
    "${TOOLSTORE_TXN_CONTINUE_REQUEST:-}" == "$(toolstore_txn_env_value "$content" REQUEST_ID)" ]]; then
    toolstore_txn_verify_backup "$SCRIPT_DIR" >/dev/null || return 1
    return 0
  fi
  stop_deploy_isolated_candidate || return 1
  if [[ "$state" == "committed" ]]; then
    candidate_image="$(toolstore_txn_env_value "$content" CANDIDATE_IMAGE)"
    persist_deploy_image_env "$ENV_FILE" "$candidate_image" || return 1
    NEWAPI_TOOLS_IMAGE="$candidate_image"
    export NEWAPI_TOOLS_IMAGE
    configure_deploy_context_from_env "$ENV_FILE" || return 1
    start_and_finalize_committed_deploy_candidate "$candidate_image"
    return $?
  fi
  if [[ "$state" == "rolled_back" ]]; then
    toolstore_txn_restore_files "$SCRIPT_DIR" || return 1
    rollback_image="$(toolstore_txn_env_value "$content" OLD_IMAGE)"
    configure_deploy_context_from_env "$ENV_FILE" || return 1
    start_deploy_services_and_wait rollback "$rollback_image" || return 1
    toolstore_txn_retire_rolled_back "$SCRIPT_DIR" || return 1
    log_success "已验证上次回滚后的旧服务，并保留回滚证据；下一候选将创建全新备份"
    return 0
  fi
  log_warn "检测到未完成的 Tool Store 事务 $(toolstore_txn_env_value "$content" REQUEST_ID)，先恢复旧数据库与旧镜像"
  toolstore_txn_restore_files "$SCRIPT_DIR" || return 1
  toolstore_txn_unstage_data "$SCRIPT_DIR" || return 1
  content="$(toolstore_txn_load_record "$SCRIPT_DIR")" || return 1
  rollback_image="$(toolstore_txn_env_value "$content" OLD_IMAGE)"
  configure_deploy_context_from_env "$ENV_FILE" || return 1
  if [[ "$state" == "preparing" || "$state" == "preflight" ]]; then
    start_deploy_services_and_wait rollback "$rollback_image" || return 1
    toolstore_txn_retire_preflight "$SCRIPT_DIR" || return 1
    return 0
  fi
  if ! run_deploy_compose "$ENV_FILE" "$rollback_image" down; then
    log_warn "清理中断事务容器返回失败；仍尝试在数据库恢复后重建旧服务"
  fi
  toolstore_txn_restore_database "$SCRIPT_DIR" || return 1
  start_deploy_services_and_wait rollback "$rollback_image" || return 1
  toolstore_txn_mark_rolled_back "$SCRIPT_DIR" || return 1
  toolstore_txn_retire_rolled_back "$SCRIPT_DIR" || return 1
  log_success "已恢复并验证中断事务前的 Tool Store、旧配置和旧镜像"
}

recover_preexisting_deploy_rollback_snapshot() {
  local rollback_content rollback_image rollback_project
  [[ "$DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING" == "true" ]] || return 0
  rollback_content="$(load_deploy_rollback_snapshot)" || return 1
  rollback_image="$(env_content_value "$rollback_content" 'NEWAPI_TOOLS_IMAGE')"
  rollback_project="$(env_content_value "$rollback_content" 'COMPOSE_PROJECT_NAME')"
  is_immutable_newapi_tools_image "$rollback_image" || return 1
  [[ "$rollback_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || return 1

  log_warn "正在从中断事务的权威快照恢复旧部署 ${rollback_image}"
  if ! (
    restore_deploy_env_snapshot "$ENV_FILE" "$rollback_content"
    DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE="$rollback_project"
    NEWAPI_TOOLS_IMAGE="$rollback_image"
    export NEWAPI_TOOLS_IMAGE
    configure_deploy_context_from_env "$ENV_FILE"
    if ! run_deploy_compose "$ENV_FILE" "$rollback_image" down; then
      log_error "清理中断事务当前容器时发生错误；仍将尝试按快照重建旧服务"
    fi
    start_deploy_services_and_wait rollback "$rollback_image"
  ); then
    log_error "无法按持久化快照恢复旧部署；快照已保留，拒绝生成或启动新的候选配置"
    return 1
  fi

  DEPLOY_ROLLBACK_ENV_CONTENT="$rollback_content"
  DEPLOY_ROLLBACK_ENV_AVAILABLE=true
  DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=false
  log_success "已从中断事务恢复并验证旧部署；回滚快照将保留到本次候选提交成功"
}

#######################################
# 启动服务
#######################################
start_services() {
  log_info "启动服务..."

  validate_deploy_credential_separation_file "$ENV_FILE" ||
    die "凭据预检失败；候选服务未拉取、停止或启动"

  [[ "$DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING" != "true" ]] ||
    die "检测到尚未恢复的中断部署快照；拒绝在当前可能是候选版本的容器上继续"
  if [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]] &&
    ! ensure_deploy_rollback_snapshot; then
    die "检测到旧服务，但无法建立或读取 mode-600 完整回滚快照；拒绝覆盖 .env"
  fi

  local candidate_request="$NEWAPI_TOOLS_IMAGE" candidate_env_content
  candidate_env_content="$(<"$ENV_FILE")"
  DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE=""

  # 下载 GeoIP 数据库
  if ! (download_geoip_database); then
    if [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      (restore_active_deploy_rollback_snapshot) ||
        log_error "高危：部署预检失败后无法恢复旧配置"
    fi
    die "部署预检失败；尚未拉取或切换候选服务"
  fi

  local had_existing_service=false had_rollback_deployment=false
  local rollback_env_content="" normalized_rollback_env_content=""
  local rollback_image="" rollback_reference="" rollback_project=""
  local existing_container_names

  # Capture the actual previous configuration before changing any image pin.
  # main() may have saved it before generate_env_file replaced .env; direct
  # callers (including upgrades) fall back to the current file.
  if ! existing_container_names="$(list_deploy_container_names)"; then
    if [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      (restore_active_deploy_rollback_snapshot) ||
        log_error "高危：Docker 容器枚举失败后无法恢复旧配置"
    fi
    die "无法可靠枚举现有 Docker 容器；拒绝在部署状态未知时继续"
  fi
  if printf '%s\n' "$existing_container_names" | grep -qE '^newapi-tools$'; then
    had_existing_service=true
    if [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      rollback_env_content="$DEPLOY_ROLLBACK_ENV_CONTENT"
    elif [[ "$DEPLOY_ENV_GENERATED_THIS_RUN" == "true" ]]; then
      die "检测到现有服务，但本次生成配置前没有可信旧 .env 快照；拒绝把候选配置伪装成回滚配置"
    else
      rollback_env_content="$(<"$ENV_FILE")"
    fi

    rollback_reference="$(env_content_value "$rollback_env_content" 'NEWAPI_TOOLS_IMAGE')"
    if [[ -z "$rollback_reference" ]]; then
      local legacy_version
      legacy_version="$(env_content_value "$rollback_env_content" 'NEWAPI_TOOLS_VERSION')"
      if [[ -n "$legacy_version" ]]; then
        if [[ ! "$legacy_version" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]]; then
          (restore_active_deploy_rollback_snapshot "$rollback_env_content") ||
            log_error "高危：无法恢复旧版本字段无效的完整配置"
          die "旧 NEWAPI_TOOLS_VERSION 格式无效；无法建立安全回滚锚点"
        fi
        rollback_reference="${NEWAPI_TOOLS_IMAGE_REPOSITORY}:${legacy_version}"
      fi
    fi
    if [[ -z "$rollback_reference" ]]; then
      (restore_active_deploy_rollback_snapshot "$rollback_env_content") ||
        log_error "高危：无法恢复缺少镜像引用的旧配置"
      die "检测到现有服务，但旧配置中没有可审计的镜像引用；拒绝在无回滚锚点时部署"
    fi
    if ! (validate_newapi_tools_image "$rollback_reference"); then
      (restore_active_deploy_rollback_snapshot "$rollback_env_content") ||
        log_error "高危：无法恢复镜像引用无效的旧配置"
      die "旧部署镜像引用无效；已拒绝部署"
    fi

    if ! rollback_project="$(resolve_deploy_running_compose_project "newapi-tools" "$rollback_env_content")"; then
      (restore_active_deploy_rollback_snapshot "$rollback_env_content") ||
        log_error "高危：无法恢复 Compose 项目标识校验失败前的旧配置"
      die "无法建立可信的旧 Compose project 身份；拒绝部署"
    fi
    DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE="$rollback_project"

    # The running container is authoritative. A mutable tag may already point
    # at a newly pulled image which has never served production traffic.
    if ! rollback_image="$(resolve_deploy_running_image_digest "newapi-tools" "$rollback_reference")"; then
      (restore_active_deploy_rollback_snapshot "$rollback_env_content") ||
        log_error "高危：无法恢复未建立回滚锚点前的旧配置"
      die "无法把现有容器实际运行镜像固定为唯一 OCI digest；拒绝在无回滚锚点时部署"
    fi

    # The rollback file is the authoritative recovery point across process or
    # host crashes. It preserves every previous setting while replacing only
    # the image/project identity with values verified from the running service.
    if ! normalized_rollback_env_content="$(normalize_deploy_rollback_env_content \
      "$rollback_env_content" "$rollback_image" "$rollback_project")" ||
      ! persist_deploy_rollback_snapshot "$normalized_rollback_env_content"; then
      (restore_active_deploy_rollback_snapshot "$rollback_env_content") ||
        log_error "高危：完整旧配置无法写回，且持久化回滚快照建立失败"
      die "无法在覆盖镜像锚点或停止服务前持久化完整 mode-600 回滚快照"
    fi
    rollback_env_content="$normalized_rollback_env_content"
    DEPLOY_ROLLBACK_ENV_AVAILABLE=true
    DEPLOY_ROLLBACK_ENV_CONTENT="$rollback_env_content"

    # Keep a crash-safe immutable old-image anchor on disk while the candidate
    # is pulled and health-checked. Shell interpolation still uses the exported
    # candidate request below.
    if ! (persist_deploy_image_env "$ENV_FILE" "$rollback_image"); then
      if ! (restore_deploy_env_content "$ENV_FILE" "$rollback_env_content" "$rollback_image"); then
        log_error "高危：旧回滚锚点无法持久化，且完整旧配置恢复失败"
      fi
      die "无法在候选拉取前持久化旧服务的不可变回滚锚点"
    fi
    if ! (persist_deploy_compose_project_env "$ENV_FILE" "$rollback_project"); then
      if ! (restore_deploy_env_content "$ENV_FILE" "$rollback_env_content" "$rollback_image"); then
        log_error "高危：Compose project 身份无法持久化，且完整旧配置恢复失败"
      fi
      die "无法在候选切换前持久化权威 Compose project 身份"
    fi
    NEWAPI_TOOLS_IMAGE="$candidate_request"
    export NEWAPI_TOOLS_IMAGE
    had_rollback_deployment=true
  elif [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
    # A deliberately stopped/removed installation still has an authoritative
    # pre-candidate snapshot. Treat it as a rollback-capable deployment rather
    # than as a first install, without consulting candidate container state.
    rollback_env_content="$(load_deploy_rollback_snapshot)" ||
      die "持久化旧部署快照不可读；拒绝把无容器安装当作首次部署"
    rollback_image="$(env_content_value "$rollback_env_content" 'NEWAPI_TOOLS_IMAGE')"
    rollback_project="$(env_content_value "$rollback_env_content" 'COMPOSE_PROJECT_NAME')"
    is_immutable_newapi_tools_image "$rollback_image" ||
      die "无容器旧部署快照未固定到不可变 OCI digest"
    [[ "$rollback_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "无容器旧部署快照缺少有效 Compose project"
    DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE="$rollback_project"
    if ! (persist_deploy_compose_project_env "$ENV_FILE" "$rollback_project"); then
      (restore_active_deploy_rollback_snapshot) ||
        log_error "高危：无容器旧部署的 Compose project 无法写入候选配置，且旧配置恢复失败"
      die "无法在候选切换前持久化无容器旧部署的权威 Compose project 身份"
    fi
    NEWAPI_TOOLS_IMAGE="$candidate_request"
    export NEWAPI_TOOLS_IMAGE
    had_rollback_deployment=true
  fi

  # 先拉取成功再停止旧容器，避免镜像不存在/网络失败造成不必要停机。
  log_info "拉取部署镜像 ${NEWAPI_TOOLS_IMAGE}..."
  if ! run_deploy_compose "$ENV_FILE" "$candidate_request" pull --include-deps newapi-tools; then
    if [[ "$had_rollback_deployment" == "true" ]] &&
      ! restore_deploy_previous_configuration "$rollback_env_content" "$rollback_image"; then
      die "候选镜像拉取失败，且完整旧配置恢复失败；当前容器可能仍运行，请勿重启"
    fi
    die "拉取 newapi-tools 及其策略门禁/Redis 依赖镜像失败；当前运行中的服务保持不变"
  fi
  # Compose resolves the requested tag during pull. Before stopping the old
  # container, convert that mutable tag to the immutable RepoDigest and verify
  # the OCI source revision. The new digest is committed only after health.
  NEWAPI_TOOLS_IMAGE="$candidate_request"
  export NEWAPI_TOOLS_IMAGE
  if ! pin_deploy_image_after_pull; then
    if [[ "$had_rollback_deployment" == "true" ]] &&
      ! restore_deploy_previous_configuration "$rollback_env_content" "$rollback_image"; then
      die "候选镜像验证失败，且完整旧配置恢复失败；当前容器可能仍运行，请勿重启"
    fi
    die "候选镜像 digest 或源码版本验证失败；现有服务保持不变"
  fi
  local candidate_image="$NEWAPI_TOOLS_IMAGE"

  if [[ "$had_rollback_deployment" == "true" ]]; then
    local toolstore_config_source="$ENV_FILE" toolstore_host_path
    if [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      toolstore_config_source="$(deploy_rollback_snapshot_path)"
    fi
    if ! toolstore_host_path="$(toolstore_txn_resolve_db_path "$toolstore_config_source" "$SCRIPT_DIR")"; then
      die "旧部署的 TOOL_STORE_PATH 为空、越界或穿越 symlink；拒绝停止旧服务"
    fi
    if [[ -e "$toolstore_host_path" || -L "$toolstore_host_path" ]]; then
      [[ -f "$toolstore_host_path" && ! -L "$toolstore_host_path" ]] ||
        die "旧部署的 Tool Store 不是安全的常规文件；拒绝停止旧服务"
      if ! toolstore_txn_prepare "$toolstore_config_source" "$SCRIPT_DIR" \
        "$rollback_image" "$candidate_image" "$candidate_image"; then
        restore_deploy_previous_configuration "$rollback_env_content" "$rollback_image" ||
          log_error "高危：Tool Store 备份失败后无法恢复完整旧配置"
        die "候选切换前无法创建并二次验证 Tool Store Online Backup；旧服务保持运行，未清理任何目录"
      fi
    else
      log_warn "旧部署没有 Tool Store 文件，按 pre-Tool-Store 兼容升级处理；候选仍必须通过内容健康检查"
    fi
  fi

  # 检查是否有旧容器
  if [[ "$had_existing_service" == "true" ]]; then
    log_warn "发现已存在的服务容器，正在停止..."
    if ! run_deploy_compose "$ENV_FILE" "$candidate_image" down; then
      log_error "停止现有服务返回失败；按可能已部分删除处理并立即重建旧服务"
      if rollback_deploy_to_previous "$rollback_env_content" "$rollback_image" false; then
        die "候选版本尚未启动；初次停止失败后已恢复旧镜像 ${rollback_image} 与旧配置"
      fi
      die "初次停止失败，且旧镜像 ${rollback_image} 无法恢复健康；请立即检查 docker compose ps/logs"
    fi
  fi

  local candidate_healthy=false
  if toolstore_txn_has_active "$SCRIPT_DIR"; then
    if ! toolstore_txn_authoritative_backup "$SCRIPT_DIR" ||
      ! toolstore_txn_mark_candidate "$SCRIPT_DIR"; then
      recover_active_deploy_toolstore_transaction ||
        die "旧服务已停止，但无法建立权威 Tool Store 回滚锚点且自动恢复失败"
      die "候选版本尚未启动；权威 Tool Store 备份失败后已恢复旧服务"
    fi
    if (start_deploy_isolated_candidate_and_wait "$candidate_image"); then
      candidate_healthy=true
    else
      stop_deploy_isolated_candidate ||
        die "隔离候选停止身份或容器 ID 无法验证；禁止恢复数据库或启动旧服务"
      log_error "隔离候选未在 180 秒内达到健康状态或运行时网络恢复失败"
    fi
  elif (start_deploy_services_and_wait candidate "$candidate_image"); then
    candidate_healthy=true
  else
    log_error "候选服务未在 180 秒内达到健康状态或运行时网络恢复失败"
  fi

  if [[ "$candidate_healthy" == "true" ]]; then
    if toolstore_txn_has_active "$SCRIPT_DIR"; then
      if ! stop_deploy_isolated_candidate || ! toolstore_txn_verify_candidate "$SCRIPT_DIR"; then
        candidate_healthy=false
        log_error "隔离候选停止或 Tool Store schema/关键表验证失败，将执行数据库与镜像回滚"
      fi
    fi
  fi

  if [[ "$candidate_healthy" == "true" ]]; then
    if (persist_deploy_image_env "$ENV_FILE" "$candidate_image"); then
      NEWAPI_TOOLS_IMAGE="$candidate_image"
      export NEWAPI_TOOLS_IMAGE
      if toolstore_txn_has_active "$SCRIPT_DIR" &&
        ! toolstore_txn_verify_candidate "$SCRIPT_DIR"; then
        candidate_healthy=false
        log_error "候选服务健康，但 Tool Store schema/核心表计数验证失败，将执行数据库与镜像回滚"
      elif toolstore_txn_has_active "$SCRIPT_DIR"; then
        if ! toolstore_txn_commit "$SCRIPT_DIR"; then
          candidate_healthy=false
          log_error "候选服务已健康，但 Tool Store 提交结果未确认；将读取持久事务状态后决定恢复方向"
        fi
      elif [[ "$DEPLOY_ROLLBACK_ENV_AVAILABLE" == "true" ]] &&
        ! (remove_deploy_rollback_snapshot); then
        candidate_healthy=false
        log_error "候选镜像已提交，但无法清除旧配置回滚快照"
      else
        DEPLOY_ROLLBACK_ENV_AVAILABLE=false
        DEPLOY_ROLLBACK_ENV_CONTENT=""
        log_success "候选镜像已健康，部署镜像已提交为 ${candidate_image}"
      fi
    else
      candidate_healthy=false
      log_error "候选服务已健康，但无法提交其镜像配置，将执行回滚"
    fi
  fi

  if toolstore_txn_has_active "$SCRIPT_DIR"; then
    local committed_content committed_state
    committed_content="$(toolstore_txn_load_record "$SCRIPT_DIR")" ||
      die "无法读取候选晋升事务状态；拒绝猜测是否可回滚"
    committed_state="$(toolstore_txn_env_value "$committed_content" STATE)"
    if [[ "$committed_state" == "committed" ]]; then
      if start_and_finalize_committed_deploy_candidate "$candidate_image"; then
        candidate_healthy=true
      else
        die "Tool Store 已提交且不能安全回滚；正式候选启动失败，事务证据已保留供下次继续晋升"
      fi
    fi
  fi

  if [[ "$candidate_healthy" != "true" ]]; then
    if toolstore_txn_has_active "$SCRIPT_DIR"; then
      stop_deploy_isolated_candidate ||
        die "无法证明隔离候选已停止；禁止恢复数据库或启动旧服务"
    fi
    if [[ "$had_rollback_deployment" != "true" ]]; then
      if ! run_deploy_compose "$ENV_FILE" "$candidate_image" down --remove-orphans; then
        log_error "清理首次安装候选 Compose 项目失败；将强制移除已知的可重启容器"
        run_deploy_compose "$ENV_FILE" "$candidate_image" \
          rm -s -f newapi-tools redis image-policy >/dev/null 2>&1 || true
      fi
      docker rm -f newapi-tools newapi-tools-redis >/dev/null 2>&1 || true
      if ! (restore_deploy_env_snapshot "$ENV_FILE" "$candidate_env_content"); then
        log_error "高危：首次安装失败后无法恢复 fail-closed 的候选配置"
      fi
      die "候选服务启动失败；没有可回滚的旧服务，镜像配置未提交"
    fi

    if rollback_deploy_to_previous "$rollback_env_content" "$rollback_image" true; then
      die "候选服务启动失败；已恢复旧镜像 ${rollback_image} 与旧配置，部署未提交"
    fi
    die "候选服务启动失败，且旧镜像 ${rollback_image} 的配置或健康回滚也失败；请立即检查 docker compose ps/logs"
  fi

  log_success "服务已启动!"

  # 获取服务器 IP
  local server_ip
  server_ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || server_ip="$(ip route get 1 2>/dev/null | awk '{print $7; exit}')" || server_ip="localhost"

  echo ""
  echo -e "${GREEN}========================================${NC}"
  echo -e "${GREEN}  NewAPI Middleware Tool 部署成功!${NC}"
  echo -e "${GREEN}========================================${NC}"
  echo ""
  if [[ "$FRONTEND_BIND" == "127.0.0.1" || "$FRONTEND_BIND" == "localhost" || "$FRONTEND_BIND" == "::1" ]]; then
    echo -e "${YELLOW}前端端口仅监听本机 127.0.0.1:${FRONTEND_PORT}，外部直连不可达${NC}"
    echo -e "请在宿主机配置 nginx 反代到 HTTPS 域名，参考配置："
    cat <<NGINX
   server {
     listen 443 ssl http2;
     server_name your-domain.com;
     ssl_certificate     /path/to/fullchain.pem;
     ssl_certificate_key /path/to/privkey.pem;
     location / {
       proxy_pass http://127.0.0.1:${FRONTEND_PORT};
       proxy_set_header Host \$host;
       proxy_set_header X-Real-IP \$remote_addr;
       # 覆盖客户端自带的 X-Forwarded-For，防止伪造限流身份。
       proxy_set_header X-Forwarded-For \$remote_addr;
       proxy_set_header X-Forwarded-Proto \$scheme;
     }
   }
NGINX
    echo -e "${YELLOW}提示：若内层 Nginx 看到的外层代理来源不是 loopback，请把该精确 IP（/32 或 /128）追加到 .env 的 TRUSTED_PROXY_CIDRS。${NC}"
  else
    echo -e "前端访问地址: ${BLUE}http://${server_ip}:${FRONTEND_PORT}${NC}"
    echo -e "API 地址: ${BLUE}http://${server_ip}:${FRONTEND_PORT}/api${NC}"
  fi
  echo ""

  if [[ "${AUTO_GENERATED_PASSWORD:-false}" == "true" ]]; then
    local sep
    sep="$(printf '═%.0s' {1..62})"
    echo -e "${YELLOW}╔${sep}╗${NC}"
    printf "${YELLOW}║${NC}  ${YELLOW}⚠  以下是自动生成的随机登录密码，请立即复制保存：${NC}        ${YELLOW}║${NC}\n"
    printf "${YELLOW}╠${sep}╣${NC}\n"
    printf "${YELLOW}║${NC}                                                                ${YELLOW}║${NC}\n"
    printf "${YELLOW}║${NC}    ${GREEN}%-56s${NC}    ${YELLOW}║${NC}\n" "$ADMIN_PASSWORD"
    printf "${YELLOW}║${NC}                                                                ${YELLOW}║${NC}\n"
    printf "${YELLOW}╠${sep}╣${NC}\n"
    printf "${YELLOW}║${NC}  忘记密码可重新运行 install.sh，管理面板内会显示该密码         ${YELLOW}║${NC}\n"
    printf "${YELLOW}║${NC}  也可执行: grep ADMIN_PASSWORD %-32s${YELLOW}║${NC}\n" "$ENV_FILE"
    echo -e "${YELLOW}╚${sep}╝${NC}"
  else
    echo -e "登录密码: ${YELLOW}${ADMIN_PASSWORD}${NC}"
  fi
  echo ""
  echo -e "配置文件: ${ENV_FILE}"
  echo -e "Compose 文件: ${COMPOSE_FILE}"
  echo ""
  echo -e "查看日志: ${YELLOW}cd ${SCRIPT_DIR} && docker compose logs -f${NC}"
  echo ""
}

#######################################
# 卸载服务
#######################################
uninstall() {
  log_warn "正在卸载 NewAPI Middleware Tool..."

  local snapshot_file snapshot_content
  snapshot_file="$(deploy_rollback_snapshot_path)"
  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    snapshot_content="$(load_deploy_rollback_snapshot)" ||
      die "卸载检测到不安全或不可读取的回滚快照；拒绝删除配置并误报成功"
    restore_deploy_env_snapshot "$ENV_FILE" "$snapshot_content" ||
      die "卸载前无法恢复权威旧部署配置；回滚快照已保留"
  fi

  if [[ -e "$ENV_FILE" || -L "$ENV_FILE" ]]; then
    [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] ||
      die "卸载配置不是安全的常规文件；拒绝继续"
    configure_deploy_context_from_env "$ENV_FILE" ||
      die "卸载前无法重建 Compose/网络上下文；配置与回滚快照已保留"
    DEPLOY_COMPOSE_PROJECT_NAME_OVERRIDE=""
    if ! run_deploy_compose "$ENV_FILE" "$(env_file_value "$ENV_FILE" 'NEWAPI_TOOLS_IMAGE')" down -v; then
      die "卸载服务清理失败；配置与回滚快照已保留，未报告卸载成功"
    fi
    log_success "容器已停止并移除"
  else
    local container_names
    container_names="$(list_deploy_container_names)" ||
      die "卸载时无法可靠枚举容器；拒绝误报成功"
    if printf '%s\n' "$container_names" | grep -qE '^newapi-tools(-redis)?$'; then
      die "检测到 newapi-tools 容器但缺少可审计配置；拒绝误报卸载成功"
    fi
  fi

  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    durable_remove_deploy_file "$snapshot_file" ||
      die "服务已清理，但无法持久删除回滚快照；卸载未完成"
  fi
  if [[ -e "$ENV_FILE" || -L "$ENV_FILE" ]]; then
    durable_remove_deploy_file "$ENV_FILE" ||
      die "服务已清理，但无法持久删除配置文件；卸载未完成"
    log_success "配置文件已删除"
  fi

  log_success "卸载完成"
}

#######################################
# 查看状态
#######################################
show_status() {
  log_info "服务状态:"
  echo ""

  if [[ -f "$COMPOSE_FILE" && -f "$ENV_FILE" ]]; then
    $DOCKER_COMPOSE -f "$COMPOSE_FILE" --env-file "$ENV_FILE" ps
  else
    log_warn "未找到配置文件，服务可能未部署"
  fi
}

#######################################
# 显示帮助
#######################################
show_help() {
  cat <<EOF
NewAPI Middleware Tool - 一键部署脚本

用法:
  ./deploy.sh              交互式部署（开发 checkout 可推导 commit 镜像；发行版需显式镜像身份）
  ./deploy.sh --uninstall  卸载服务
  ./deploy.sh --status     查看服务状态
  ./deploy.sh --help       显示帮助

环境变量:
  NEWAPI_CONTAINER   指定 NewAPI 容器名 (默认: 自动检测)
  NEWAPI_NETWORK     指定 Docker 网络名 (默认: 自动检测)
  ADMIN_PASSWORD     前端访问密码 (默认: 交互式输入)
  API_KEY            后端 API Key (默认: 交互式输入或自动生成)
  NEWAPI_TOOLS_IMAGE 完整 repo@sha256:digest（发行/无 Git 部署必填）
  NEWAPI_TOOLS_EXPECTED_REVISION 与镜像匹配的 40 位 Git commit（显式镜像必填）
  NEWAPI_TOOLS_RELEASE_TAG 与镜像签名/provenance 匹配的 vMAJOR.MINOR.PATCH（显式发行镜像必填）
  FRONTEND_PORT      前端端口 (默认: 1145)
  FRONTEND_BIND      前端端口绑定网卡 0.0.0.0/127.0.0.1 (默认: 交互式选择)
  TRUSTED_PROXY_CIDRS 允许解析其 XFF 的精确代理 IP/CIDR (默认: loopback)

示例:
  # 发行版基本部署（从 GitHub Release 复制三项值）
  NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<manifest-digest> \
  NEWAPI_TOOLS_EXPECTED_REVISION=<release-commit> \
  NEWAPI_TOOLS_RELEASE_TAG=v0.6.2 ./deploy.sh

  # 指定容器名部署
  NEWAPI_CONTAINER=my-newapi ./deploy.sh

  # 非交互式部署，用 nginx 反代模式
  ADMIN_PASSWORD=mypass API_KEY=mykey FRONTEND_BIND=127.0.0.1 ./deploy.sh
EOF
}

capture_deploy_rollback_env() {
  local container_names="$1" snapshot_file old_content rollback_reference
  local legacy_version rollback_project rollback_image normalized_content
  local has_existing_container=false
  DEPLOY_ROLLBACK_ENV_AVAILABLE=false
  DEPLOY_ROLLBACK_ENV_CONTENT=""
  DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=false
  snapshot_file="$(deploy_rollback_snapshot_path)"
  # A durable snapshot is authoritative even when a crash happened after
  # compose down removed the old container. Validate it before consulting the
  # current container list so that exact outage window remains recoverable.
  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    old_content="$(load_deploy_rollback_snapshot)" ||
      die "检测到不可读取、不安全或权限不是 0600 的旧回滚快照 ${snapshot_file}；拒绝覆盖"
    rollback_reference="$(env_content_value "$old_content" 'NEWAPI_TOOLS_IMAGE')"
    rollback_project="$(env_content_value "$old_content" 'COMPOSE_PROJECT_NAME')"
    is_immutable_newapi_tools_image "$rollback_reference" ||
      die "旧回滚快照未固定到不可变 OCI digest；拒绝用当前可能是候选版本的容器重写恢复锚点"
    [[ "$rollback_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "旧回滚快照缺少有效的 Compose project；拒绝用当前可能是候选版本的容器重写恢复锚点"
    DEPLOY_ROLLBACK_ENV_CONTENT="$old_content"
    DEPLOY_ROLLBACK_ENV_AVAILABLE=true
    DEPLOY_ROLLBACK_SNAPSHOT_PREEXISTING=true
    log_warn "检测到上次未提交完成的权威回滚快照；将先恢复其中固定的旧部署，绝不采用当前容器身份覆盖"
    return 0
  fi

  if printf '%s\n' "$container_names" | grep -qE '^newapi-tools$'; then
    has_existing_container=true
  elif [[ ! -e "$ENV_FILE" && ! -L "$ENV_FILE" ]]; then
    # A genuinely fresh install has neither a container nor a previous dotenv.
    return 0
  fi

  [[ -f "$ENV_FILE" && ! -L "$ENV_FILE" ]] ||
    die "检测到已有部署配置，但缺少安全的旧 .env；无法审计密码、DSN、网络与回滚配置，拒绝覆盖"
  old_content="$(<"$ENV_FILE")"

  rollback_reference="$(env_content_value "$old_content" 'NEWAPI_TOOLS_IMAGE')"
  if [[ -z "$rollback_reference" ]]; then
    legacy_version="$(env_content_value "$old_content" 'NEWAPI_TOOLS_VERSION')"
    if [[ -n "$legacy_version" ]]; then
      [[ "$legacy_version" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] ||
        die "旧回滚配置的 NEWAPI_TOOLS_VERSION 格式无效；拒绝覆盖"
      rollback_reference="${NEWAPI_TOOLS_IMAGE_REPOSITORY}:${legacy_version}"
    fi
  fi
  if [[ -z "$rollback_reference" && "$has_existing_container" != "true" ]]; then
    # A documented first install may start from a copied .env.example whose
    # persistent image keys are empty while the candidate is supplied through
    # the process environment. It has no committed deployment to roll back.
    return 0
  fi
  [[ -n "$rollback_reference" ]] ||
    die "旧回滚配置缺少 NEWAPI_TOOLS_IMAGE/NEWAPI_TOOLS_VERSION；拒绝覆盖"
  (validate_newapi_tools_image "$rollback_reference") ||
    die "旧回滚配置的镜像引用无效；拒绝覆盖"

  if [[ "$has_existing_container" == "true" ]]; then
    rollback_project="$(resolve_deploy_running_compose_project "newapi-tools" "$old_content")" ||
      die "无法在覆盖 .env 前固定运行中服务的 Compose project 身份"
    rollback_image="$(resolve_deploy_running_image_digest "newapi-tools" "$rollback_reference")" ||
      die "无法在覆盖 .env 前把运行中服务固定为唯一 OCI digest"
  else
    # With no container label to consult, only an explicit project identity is
    # authoritative. Guessing Compose's directory-derived default could target
    # a different project after the checkout or install directory has moved.
    rollback_project="$(env_content_value "$old_content" 'COMPOSE_PROJECT_NAME')"
    [[ "$rollback_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "旧 .env 对应的容器不存在，且缺少有效的显式 COMPOSE_PROJECT_NAME；拒绝猜测回滚项目"
    if is_immutable_newapi_tools_image "$rollback_reference"; then
      rollback_image="$rollback_reference"
    elif ! rollback_image="$(resolve_deploy_image_digest "$rollback_reference" "")"; then
      die "旧 .env 对应的容器不存在，且可变镜像无法从本地唯一固定为同仓库 OCI digest"
    fi
  fi
  normalized_content="$(normalize_deploy_rollback_env_content \
    "$old_content" "$rollback_image" "$rollback_project")" ||
    die "无法生成完整的旧部署恢复配置"
  persist_deploy_rollback_snapshot "$normalized_content" ||
    die "无法在覆盖 .env 前原子写入 mode-600 完整回滚快照 ${snapshot_file}"

  DEPLOY_ROLLBACK_ENV_CONTENT="$normalized_content"
  DEPLOY_ROLLBACK_ENV_AVAILABLE=true
}

#######################################
# 主函数
#######################################
main() {
  need_cmd docker
  need_cmd flock
  need_cmd id
  need_cmd sha256sum
  need_cmd realpath
  need_cmd stat
  need_cmd sync
  detect_docker_compose

  local mode="${1:-}"

  case "$mode" in
    --help|-h)
      show_help
      exit 0
      ;;
    --uninstall)
      acquire_deploy_state_lock "$SCRIPT_DIR"
      uninstall
      exit 0
      ;;
    --status)
      show_status
      exit 0
      ;;
    "")
      # 正常部署流程
      acquire_deploy_state_lock "$SCRIPT_DIR"
      local recovery_initial_content="" recovery_initial_state=""
      if toolstore_txn_has_active "$SCRIPT_DIR"; then
        recovery_initial_content="$(toolstore_txn_load_record "$SCRIPT_DIR")" ||
          die "无法在解析新候选前读取 durable Tool Store 活动事务"
        recovery_initial_state="$(toolstore_txn_env_value "$recovery_initial_content" STATE)"
        [[ -n "$recovery_initial_state" ]] ||
          die "durable Tool Store 活动事务缺少有效状态"
      fi
      recover_active_deploy_toolstore_transaction ||
        die "未完成的 Tool Store 事务无法安全恢复；拒绝生成或启动新候选配置"
      if [[ "$recovery_initial_state" == "committed" ]]; then
        local recovery_content recovery_state
        if toolstore_txn_has_active "$SCRIPT_DIR"; then
          recovery_content="$(toolstore_txn_load_record "$SCRIPT_DIR")" ||
            die "正式候选恢复后无法读取保留的 Tool Store 提交证据"
          recovery_state="$(toolstore_txn_env_value "$recovery_content" STATE)"
          [[ "$recovery_state" == "committed" ]] ||
            die "committed 恢复返回成功但残留事务状态已改变"
          log_warn "正式候选已恢复健康，但 committed 清理证据仍被保留；本次不生成新配置，也不读取旧回滚快照"
        else
          log_success "正式候选已恢复健康，committed 快照与提交证据已完成清理"
        fi
        log_success "当前候选服务可用；本次不解析或启动新的部署请求"
        return 0
      fi
      resolve_deploy_image
      echo ""
      echo -e "${BLUE}========================================${NC}"
      echo -e "${BLUE}  NewAPI Middleware Tool 部署脚本${NC}"
      echo -e "${BLUE}========================================${NC}"
      echo ""

      detect_environment
      detect_log_database
      interactive_config
      check_compose_file
      local existing_container_names
      existing_container_names="$(list_deploy_container_names)" ||
        die "无法可靠枚举现有 Docker 容器；拒绝覆盖可能正在使用的 .env"
      if [[ -e "$ENV_FILE" || -L "$ENV_FILE" ]]; then
        validate_deploy_credential_separation_file "$ENV_FILE" ||
          die "凭据预检失败；拒绝更新现有部署"
      fi
      capture_deploy_rollback_env "$existing_container_names"
      recover_preexisting_deploy_rollback_snapshot ||
        die "检测到中断部署，但无法恢复持久化的旧服务；拒绝继续"
      generate_env_file
      start_services
      ;;
    *)
      die "未知参数: $mode (使用 --help 查看帮助)"
      ;;
  esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
