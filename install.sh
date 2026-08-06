#!/usr/bin/env bash
set -euo pipefail

#######################################
# NewAPI Middleware Tool - 快速安装脚本
#
# 安装入口:
#   按 README.md / 当前发行说明中的固定 commit URL + SHA-256 校验步骤下载后执行。
#   不要从 tag/main URL 通过 curl process substitution 直接运行本脚本。
#
# 功能:
#   1. 自动检测 NewAPI 安装目录
#   2. 检测是否已安装，提供更新/重新安装选项
#   3. Clone 项目到 NewAPI 同级目录
#   4. 自动运行部署脚本
#######################################

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m'

log_info() { echo -e "${BLUE}[INFO]${NC} $*"; }
log_success() { echo -e "${GREEN}[SUCCESS]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*" >&2; }
die() { log_error "$*"; exit 1; }

REPO_URL="https://github.com/yujianwudi/new_api_tools.git"
PROJECT_NAME="new_api_tools"
NEWAPI_TOOLS_IMAGE_REPOSITORY="ghcr.io/yujianwudi/new_api_tools"
NEWAPI_TOOLS_SIGNING_REPOSITORY="yujianwudi/new_api_tools"
NEWAPI_TOOLS_COSIGN_IMAGE="ghcr.io/sigstore/cosign/cosign:v3.1.2@sha256:d91bc4e7e95e8d2f549c747a72dc174f90579e410a1695f57f686674f84ce849"
INSTALL_REF="${NEWAPI_TOOLS_REF:-v0.6.2}"
REQUESTED_NEWAPI_TOOLS_IMAGE="${NEWAPI_TOOLS_IMAGE:-}"
REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION="${NEWAPI_TOOLS_EXPECTED_REVISION:-}"
INSTALL_COMMIT=""
NEWAPI_TOOLS_IMAGE_DERIVED=false
NEWAPI_TOOLS_EXPECTED_REVISION=""
NEWAPI_TOOLS_RELEASE_TAG=""
REINSTALL=false
INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE=""
INSTALL_ROLLBACK_ENV_AVAILABLE=false
INSTALL_ROLLBACK_ENV_CONTENT=""
INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING=false
INSTALL_STATE_LOCK_FD=""
INSTALL_STATE_LOCK_PATH=""
INSTALL_SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
INSTALL_TOOLSTORE_TXN_LOADED=false

load_install_toolstore_transaction_library() {
  local project_dir="$1" library
  local project_abs root mode system external_active=false
  if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]]; then
    return 0
  fi
  project_abs="$(realpath -m -s -- "$project_dir" 2>/dev/null)" || return 1
  [[ "$project_abs" == "$(realpath -m -- "$project_dir" 2>/dev/null)" ]] || return 1
  root="$(dirname -- "$project_abs")/.$(basename -- "$project_abs").toolstore-transactions"
  if [[ -e "${root}/active.env" || -L "${root}/active.env" ]]; then
    [[ -f "${root}/active.env" && ! -L "${root}/active.env" ]] || return 1
    library="${root}/recovery-library.sh"
    external_active=true
  else
    library="${project_dir}/scripts/toolstore_transaction.sh"
  fi
  if [[ ! -f "$library" || -L "$library" ]]; then
    [[ "$external_active" != "true" ]] || return 1
    if [[ -f "${INSTALL_SCRIPT_DIR}/scripts/toolstore_transaction.sh" &&
          ! -L "${INSTALL_SCRIPT_DIR}/scripts/toolstore_transaction.sh" ]]; then
      library="${INSTALL_SCRIPT_DIR}/scripts/toolstore_transaction.sh"
    else
      library="${root}/recovery-library.sh"
    fi
  fi
  [[ -f "$library" && ! -L "$library" ]] || return 1
  mode="$(stat -Lc '%a' -- "$library" 2>/dev/null)" || return 1
  system="$(uname -s 2>/dev/null || true)"
  if [[ "$system" != MINGW* && "$system" != MSYS* && "$system" != CYGWIN* ]]; then
    if [[ "$library" == */recovery-library.sh ]]; then
      [[ "$mode" == "600" ]] || return 1
    else
      [[ "$mode" =~ ^[0-7]{3,4}$ ]] || return 1
      (( (8#$mode & 0022) == 0 )) || return 1
    fi
  fi
  TOOLSTORE_TXN_LIBRARY_SOURCE="$library"
  # shellcheck source=scripts/toolstore_transaction.sh
  source "$library"
  INSTALL_TOOLSTORE_TXN_LOADED=true
}

# The lock is project-scoped but stored beside the project directory. It must
# survive a purge and must be available before a first-install target exists.
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

acquire_install_state_lock() {
  local project_dir="$1" lock_path inherited_fd inherited_path old_umask
  lock_path="$(project_state_lock_path "$project_dir")" ||
    die "无法确定安装状态锁路径"

  inherited_fd="${NEWAPI_TOOLS_STATE_LOCK_FD:-}"
  inherited_path="${NEWAPI_TOOLS_STATE_LOCK_PATH:-}"
  if [[ "$inherited_path" == "$lock_path" ]] &&
    state_lock_fd_is_secure "$inherited_fd" "$lock_path"; then
    flock -n "$inherited_fd" || die "继承的安装状态锁已失效"
    INSTALL_STATE_LOCK_FD="$inherited_fd"
    INSTALL_STATE_LOCK_PATH="$lock_path"
    return 0
  fi
  unset NEWAPI_TOOLS_STATE_LOCK_FD NEWAPI_TOOLS_STATE_LOCK_PATH

  if [[ -e "$lock_path" || -L "$lock_path" ]]; then
    [[ -f "$lock_path" && ! -L "$lock_path" ]] ||
      die "安装状态锁不是安全的常规文件：${lock_path}"
    [[ "$(state_lock_path_owner "$lock_path")" == "$(state_lock_current_uid)" ]] ||
      die "安装状态锁不属于当前用户：${lock_path}"
  fi
  old_umask="$(umask)"
  umask 077
  if ! exec {INSTALL_STATE_LOCK_FD}>>"$lock_path"; then
    umask "$old_umask"
    die "无法打开安装状态锁：${lock_path}"
  fi
  umask "$old_umask"
  state_lock_fd_matches_path "$INSTALL_STATE_LOCK_FD" "$lock_path" ||
    die "安装状态锁在打开时发生替换：${lock_path}"
  [[ "$(stat -Lc '%u' -- "/proc/${BASHPID}/fd/${INSTALL_STATE_LOCK_FD}" 2>/dev/null)" == \
    "$(state_lock_current_uid)" ]] ||
    die "安装状态锁打开后不属于当前用户：${lock_path}"
  chmod 600 "/proc/${BASHPID}/fd/${INSTALL_STATE_LOCK_FD}" ||
    die "无法收紧安装状态锁权限：${lock_path}"
  state_lock_fd_is_secure "$INSTALL_STATE_LOCK_FD" "$lock_path" ||
    die "安装状态锁权限或身份无效：${lock_path}"
  flock -n "$INSTALL_STATE_LOCK_FD" ||
    die "另一个安装、部署或卸载进程正在操作该项目：${lock_path}"
  state_lock_fd_matches_path "$INSTALL_STATE_LOCK_FD" "$lock_path" ||
    die "安装状态锁在加锁时发生替换：${lock_path}"

  INSTALL_STATE_LOCK_PATH="$lock_path"
  export NEWAPI_TOOLS_STATE_LOCK_FD="$INSTALL_STATE_LOCK_FD"
  export NEWAPI_TOOLS_STATE_LOCK_PATH="$INSTALL_STATE_LOCK_PATH"
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

image_repository_without_tag() {
  local image="${1%@*}" final_component
  final_component="${image##*/}"
  if [[ "$final_component" == *:* ]]; then
    image="${image%:*}"
  fi
  printf '%s\n' "$image"
}

run_install_cosign() {
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

verify_install_release_signature() {
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
    if run_install_cosign verify \
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

resolve_install_release_platform_digests() {
  local image="$1" descriptors platform digest
  local amd64_digest='' arm64_digest=''

  is_immutable_newapi_tools_image "$image" || return 1
  command -v timeout >/dev/null 2>&1 || {
    log_error "发行镜像平台 digest 校验需要 timeout"
    return 1
  }
  docker buildx version >/dev/null 2>&1 || {
    log_error "发行镜像平台 digest 校验需要 Docker Buildx"
    return 1
  }
  descriptors="$(timeout -k 10s 120s docker buildx imagetools inspect "$image" --format \
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

verify_install_release_provenance() {
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
  platform_digests="$(resolve_install_release_platform_digests "$image")" || return 1
  read -r amd64_digest arm64_digest extra <<< "$platform_digests"
  [[ "$amd64_digest" =~ ^sha256:[0-9a-f]{64}$ &&
     "$arm64_digest" =~ ^sha256:[0-9a-f]{64}$ && -z "${extra:-}" ]] || return 1

  for index in "${!workflow_files[@]}"; do
    identity="https://github.com/${NEWAPI_TOOLS_SIGNING_REPOSITORY}/.github/workflows/${workflow_files[index]}@${workflow_refs[index]}"
    policy_file="$(mktemp --suffix=.cue)" || return 1
    cat >"$policy_file" <<EOF
package newapi_tools_release

"_type": "https://in-toto.io/Statement/v0.1"
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
    if NEWAPI_TOOLS_COSIGN_POLICY_FILE="$policy_file" run_install_cosign verify-attestation \
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

resolve_install_image_digest() {
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
    )
    (( ${#matching_digests[@]} == 1 )) ||
      die "目标仓库 ${repository} 必须且只能匹配一个 RepoDigest，实际为 ${#matching_digests[@]} 个"
  fi

  if [[ -n "$expected_revision" ]]; then
    actual_revision="$(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' "$image" 2>/dev/null)" ||
      die "无法读取镜像源码版本标签"
    [[ "${actual_revision,,}" == "${expected_revision,,}" ]] ||
      die "镜像源码版本与安装 Git commit 不匹配，已拒绝部署"
  fi

  printf '%s\n' "${matching_digests[0]}"
}

resolve_install_running_image_digest() {
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
  )
  (( ${#matching_digests[@]} == 1 )) ||
    die "现有容器镜像在目标仓库 ${repository} 中必须且只能匹配一个 RepoDigest，实际为 ${#matching_digests[@]} 个"
  if is_immutable_newapi_tools_image "$configured_image" &&
    [[ "${matching_digests[0]}" != "$configured_image" ]]; then
    die "旧配置的不可变镜像与现有容器实际运行镜像不一致；拒绝建立错误回滚锚点"
  fi
  printf '%s\n' "${matching_digests[0]}"
}

list_install_container_names() {
  docker ps -a --format '{{.Names}}'
}

resolve_install_running_compose_project() {
  local container="$1" env_file="$2" label_project configured_project
  label_project="$(docker inspect --format '{{ index .Config.Labels "com.docker.compose.project" }}' \
    "$container" 2>/dev/null)" || die "无法读取现有容器的 Compose project label"
  label_project="${label_project%$'\r'}"
  [[ "$label_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
    die "现有容器缺少有效的 com.docker.compose.project 标识；拒绝猜测部署身份"
  configured_project="$(env_file_value "$env_file" 'COMPOSE_PROJECT_NAME')"
  if [[ -n "$configured_project" ]]; then
    [[ "$configured_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "配置中的 COMPOSE_PROJECT_NAME 格式无效"
    [[ "$configured_project" == "$label_project" ]] ||
      die "配置的 COMPOSE_PROJECT_NAME 与运行容器 project label 不一致"
  fi
  printf '%s\n' "$label_project"
}

pin_install_image_after_pull() {
  local resolved
  if ! resolved="$(resolve_install_image_digest "$NEWAPI_TOOLS_IMAGE" "$NEWAPI_TOOLS_EXPECTED_REVISION")"; then
    log_error "候选镜像 digest 或源码版本验证失败；现有服务保持不变"
    return 1
  fi
  if [[ -n "$NEWAPI_TOOLS_RELEASE_TAG" ]] &&
    { ! verify_install_release_signature \
        "$resolved" "$NEWAPI_TOOLS_RELEASE_TAG" "$NEWAPI_TOOLS_EXPECTED_REVISION" ||
      ! verify_install_release_provenance \
        "$resolved" "$NEWAPI_TOOLS_RELEASE_TAG" "$NEWAPI_TOOLS_EXPECTED_REVISION"; }; then
    log_error "候选发行镜像的 Sigstore 身份、标签、commit、注解或 subject digest 验证失败；现有服务保持不变"
    return 1
  fi
  NEWAPI_TOOLS_IMAGE="$resolved"
  export NEWAPI_TOOLS_IMAGE
  log_success "候选部署镜像已验证并固定为 ${resolved}"
}

resolve_install_image() {
  local ref="$1" commit="$2" requested_image="${3:-}"
  [[ "$commit" =~ ^[0-9a-fA-F]{40}$ ]] || die "无法根据无效 Git commit 推导镜像"

  local image=""
  if [[ -n "$requested_image" ]]; then
    [[ "$ref" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
      die "显式 NEWAPI_TOOLS_IMAGE 仅用于严格发行标签；开发 ref 必须从 Git checkout 派生短 SHA 镜像"
    is_immutable_newapi_tools_image "$requested_image" ||
      die "显式 NEWAPI_TOOLS_IMAGE 必须使用发行页核验过的 repo@sha256:<digest>"
    [[ "$(image_repository_without_tag "$requested_image")" == "$NEWAPI_TOOLS_IMAGE_REPOSITORY" ]] ||
      die "显式 NEWAPI_TOOLS_IMAGE 必须属于受信任仓库 ${NEWAPI_TOOLS_IMAGE_REPOSITORY}"
    image="$requested_image"
  elif [[ "$ref" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    die "发行版本 ${ref} 必须同时提供发行页中的不可变 NEWAPI_TOOLS_IMAGE digest 和 NEWAPI_TOOLS_EXPECTED_REVISION"
  else
    image="${NEWAPI_TOOLS_IMAGE_REPOSITORY}:${commit:0:7}"
  fi

  validate_newapi_tools_image "$image"
  printf '%s\n' "$image"
}

validate_install_ref() {
  [[ "$INSTALL_REF" =~ ^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$ ]] ||
    die "NEWAPI_TOOLS_REF 格式无效"
  [[ "$INSTALL_REF" != *..* && "$INSTALL_REF" != *@\{* && "$INSTALL_REF" != */ && "$INSTALL_REF" != *. ]] ||
    die "NEWAPI_TOOLS_REF 格式无效"
  if [[ "$INSTALL_REF" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
    [[ ! "$INSTALL_REF" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    die "发行标签必须使用无前导零的严格 vMAJOR.MINOR.PATCH 格式"
  fi
}

install_release_tag_at_commit() {
  local commit="$1" tags tag release_tag=""
  tags="$(git tag --points-at "$commit")" ||
    die "无法枚举安装 commit ${commit} 上的 Git tag"
  while IFS= read -r tag; do
    [[ -n "$tag" ]] || continue
    if [[ "$tag" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] &&
      [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
      die "安装 commit 上的发行标签必须使用无前导零的严格 vMAJOR.MINOR.PATCH 格式: ${tag}"
    fi
    [[ "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || continue
    [[ "$(git cat-file -t "refs/tags/${tag}" 2>/dev/null)" == "tag" ]] ||
      die "发行标签必须是 annotated tag: ${tag}"
    [[ -z "$release_tag" ]] ||
      die "同一安装 commit 不能同时关联多个发行标签: ${release_tag}, ${tag}"
    release_tag="$tag"
  done <<< "$tags"
  printf '%s\n' "$release_tag"
}

verify_install_origin() {
  local origin
  origin="$(git remote get-url origin 2>/dev/null)" || die "现有安装目录缺少 origin 远端"
  case "$origin" in
    "https://github.com/yujianwudi/new_api_tools"|\
    "https://github.com/yujianwudi/new_api_tools.git"|\
    "git@github.com:yujianwudi/new_api_tools.git"|\
    "ssh://git@github.com/yujianwudi/new_api_tools.git")
      ;;
    *)
      die "现有安装目录 origin 不受信任：${origin}"
      ;;
  esac
}

checkout_install_ref() {
  validate_install_ref
  verify_install_origin
  git fetch --force --prune --tags origin

  local target=""
  if [[ "$INSTALL_REF" == "main" ]]; then
    git show-ref --verify --quiet "refs/remotes/origin/main" ||
      die "远端 main 分支不存在"
    target="refs/remotes/origin/main"
  elif [[ "$INSTALL_REF" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    git show-ref --verify --quiet "refs/tags/${INSTALL_REF}" ||
      die "发行版本 ${INSTALL_REF} 缺少签出的 Git tag"
    target="refs/tags/${INSTALL_REF}"
  elif git show-ref --verify --quiet "refs/remotes/origin/${INSTALL_REF}"; then
    target="refs/remotes/origin/${INSTALL_REF}"
  elif git rev-parse --verify --quiet "${INSTALL_REF}^{commit}" >/dev/null; then
    target="$INSTALL_REF"
  else
    git fetch --force origin "$INSTALL_REF"
    target="FETCH_HEAD"
  fi

  local commit
  commit="$(git rev-parse --verify "${target}^{commit}")" ||
    die "无法解析安装版本 ${INSTALL_REF}"
  local commit_release_tag
  if ! commit_release_tag="$(install_release_tag_at_commit "$commit")"; then
    die "无法安全解析安装 commit ${commit} 的发行标签身份"
  fi
  if [[ -n "$commit_release_tag" && "$INSTALL_REF" != "$commit_release_tag" ]]; then
    die "安装 ref ${INSTALL_REF} 指向发行 commit ${commit_release_tag}；必须改用该发行标签及其签名镜像"
  fi
  if [[ "$INSTALL_REF" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ &&
        "$commit_release_tag" != "$INSTALL_REF" ]]; then
    die "发行 ref ${INSTALL_REF} 未解析到同名 annotated tag"
  fi

  if [[ "$INSTALL_REF" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ &&
        -z "$REQUESTED_NEWAPI_TOOLS_IMAGE" ]]; then
    die "发行版本 ${INSTALL_REF} 必须使用发行页提供的不可变 NEWAPI_TOOLS_IMAGE digest 和 NEWAPI_TOOLS_EXPECTED_REVISION"
  fi
  if [[ -n "$REQUESTED_NEWAPI_TOOLS_IMAGE" ]]; then
    [[ "$INSTALL_REF" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] ||
      die "显式 NEWAPI_TOOLS_IMAGE 仅用于严格发行标签；开发 ref 必须从 Git checkout 派生短 SHA 镜像"
    is_immutable_newapi_tools_image "$REQUESTED_NEWAPI_TOOLS_IMAGE" ||
      die "显式 NEWAPI_TOOLS_IMAGE 必须使用 repo@sha256:<64 位小写十六进制>"
    [[ "$(image_repository_without_tag "$REQUESTED_NEWAPI_TOOLS_IMAGE")" == "$NEWAPI_TOOLS_IMAGE_REPOSITORY" ]] ||
      die "显式 NEWAPI_TOOLS_IMAGE 必须属于受信任仓库 ${NEWAPI_TOOLS_IMAGE_REPOSITORY}"
    [[ "$REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION" =~ ^[0-9a-fA-F]{40}$ ]] ||
      die "显式 NEWAPI_TOOLS_IMAGE 必须同时提供 40 位 NEWAPI_TOOLS_EXPECTED_REVISION"
    [[ "${commit,,}" == "${REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION,,}" ]] ||
      die "安装版本 ${INSTALL_REF} 当前解析到的 commit 与预期发行 commit 不一致"
  elif [[ -n "$REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION" ]]; then
    die "NEWAPI_TOOLS_EXPECTED_REVISION 只能与显式不可变 NEWAPI_TOOLS_IMAGE 同时使用"
  fi

  git reset --hard "$commit"
  INSTALL_COMMIT="$commit"
  if [[ "$INSTALL_REF" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]]; then
    NEWAPI_TOOLS_RELEASE_TAG="$INSTALL_REF"
  else
    NEWAPI_TOOLS_RELEASE_TAG=""
  fi
  if [[ -z "$REQUESTED_NEWAPI_TOOLS_IMAGE" ]]; then
    NEWAPI_TOOLS_IMAGE_DERIVED=true
    NEWAPI_TOOLS_EXPECTED_REVISION="$commit"
  else
    NEWAPI_TOOLS_IMAGE_DERIVED=false
    NEWAPI_TOOLS_EXPECTED_REVISION="${REQUESTED_NEWAPI_TOOLS_EXPECTED_REVISION,,}"
  fi
  export NEWAPI_TOOLS_IMAGE_DERIVED NEWAPI_TOOLS_EXPECTED_REVISION NEWAPI_TOOLS_RELEASE_TAG
  NEWAPI_TOOLS_IMAGE="$(resolve_install_image "$INSTALL_REF" "$commit" "$REQUESTED_NEWAPI_TOOLS_IMAGE")"
  export NEWAPI_TOOLS_IMAGE
  export NEWAPI_TOOLS_SOURCE_COMMIT="$commit"
  log_success "项目已固定到 ${INSTALL_REF} (${commit:0:12})"
  log_success "部署镜像已固定到 ${NEWAPI_TOOLS_IMAGE}"
}

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

env_file_value() {
  local env_file="$1" key="$2"
  local value
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

# Resolve the only Tool Store host mapping supported by the shipped Compose
# file without loading the transaction library. This lets genuine pre-Tool-
# Store installations upgrade through the legacy health contract, while any
# configured unsafe path still fails before the old service is stopped.
resolve_install_toolstore_host_path() {
  local env_file="$1" project_dir="$2" project raw relative host data_root
  [[ -f "$env_file" && ! -L "$env_file" ]] || return 1
  project="$(realpath -m -s -- "$project_dir" 2>/dev/null)" || return 1
  [[ "$project" == "$(realpath -m -- "$project_dir" 2>/dev/null)" && "$project" != "/" ]] || return 1
  if grep -qE '^TOOL_STORE_PATH=' "$env_file"; then
    raw="$(env_file_value "$env_file" TOOL_STORE_PATH)"
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
  [[ "$host" == "$data_root"/* && "$host" == "$(realpath -m -- "$host" 2>/dev/null)" ]] || return 1
  printf '%s\n' "$host"
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

validate_install_credential_separation_content() {
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

validate_install_credential_separation_file() {
  local env_file="$1"
  [[ -f "$env_file" && ! -L "$env_file" && -r "$env_file" ]] || {
    log_error "凭据预检要求安全且可读的常规 .env 文件"
    return 1
  }
  validate_install_credential_separation_content "$(<"$env_file")"
}

dotenv_quote() {
  local value="${1-}" escaped
  [[ "$value" != *$'\n'* && "$value" != *$'\r'* ]] || die "环境变量值不能包含换行"
  escaped="$(printf '%s' "$value" | sed "s/'/\\\\'/g")"
  printf "'%s'" "$escaped"
}

atomic_write_install_dotenv() {
  local target="$1" content="$2" tmp mode parent
  parent="$(dirname "$target")"
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

install_rollback_snapshot_path() {
  printf '%s.rollback\n' "$1"
}

install_rollback_compose_marker_path() {
  printf '%s.rollback.compose\n' "$1"
}

install_rollback_compose_snapshot_path() {
  local env_file="$1" name="$2"
  printf '%s.%s\n' "$(install_rollback_compose_marker_path "$env_file")" "$name"
}

install_rollback_compose_files() {
  printf '%s\n' \
    'docker-compose.yml' \
    'docker-compose.host.yml' \
    'docker-compose.logdb.yml'
}

install_rollback_compose_state_key() {
  case "$1" in
    docker-compose.yml) printf 'COMPOSE_BASE\n' ;;
    docker-compose.host.yml) printf 'COMPOSE_HOST\n' ;;
    docker-compose.logdb.yml) printf 'COMPOSE_LOGDB\n' ;;
    *) return 1 ;;
  esac
}

normalize_install_rollback_env_content() {
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

persist_install_rollback_snapshot() {
  local env_file="$1" content="$2" snapshot_file
  snapshot_file="$(install_rollback_snapshot_path "$env_file")"
  atomic_write_install_dotenv "$snapshot_file" "$content"
}

load_install_mode600_file() {
  local snapshot_file="$1" mode snapshot_fd fd_path
  local path_identity fd_identity status
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

load_install_rollback_snapshot() {
  load_install_mode600_file "$(install_rollback_snapshot_path "$1")"
}

validate_install_rollback_compose_bundle() {
  local env_file="$1" marker marker_content name key state snapshot_file
  marker="$(install_rollback_compose_marker_path "$env_file")"
  marker_content="$(load_install_mode600_file "$marker")" || return 1
  while IFS= read -r name; do
    key="$(install_rollback_compose_state_key "$name")" || return 1
    state="$(env_content_value "$marker_content" "$key")"
    snapshot_file="$(install_rollback_compose_snapshot_path "$env_file" "$name")"
    case "$state" in
      present)
        load_install_mode600_file "$snapshot_file" >/dev/null || return 1
        ;;
      absent)
        [[ ! -e "$snapshot_file" && ! -L "$snapshot_file" ]] || return 1
        ;;
      *) return 1 ;;
    esac
  done < <(install_rollback_compose_files)
  [[ "$(env_content_value "$marker_content" 'COMPOSE_BASE')" == "present" ]]
}

configure_install_toolstore_compose_sources() {
  local env_file="$1" marker_content name key state source_var
  validate_install_rollback_compose_bundle "$env_file" || return 1
  marker_content="$(load_install_mode600_file "$(install_rollback_compose_marker_path "$env_file")")" || return 1
  while IFS= read -r name; do
    key="$(install_rollback_compose_state_key "$name")" || return 1
    state="$(env_content_value "$marker_content" "$key")"
    case "$name" in
      docker-compose.yml) source_var=TOOLSTORE_TXN_COMPOSE_BASE_SOURCE ;;
      docker-compose.host.yml) source_var=TOOLSTORE_TXN_COMPOSE_HOST_SOURCE ;;
      docker-compose.logdb.yml) source_var=TOOLSTORE_TXN_COMPOSE_LOGDB_SOURCE ;;
    esac
    if [[ "$state" == "present" ]]; then
      printf -v "$source_var" '%s' "$(install_rollback_compose_snapshot_path "$env_file" "$name")"
    elif [[ "$state" == "absent" ]]; then
      printf -v "$source_var" '%s' '__ABSENT__'
    else
      return 1
    fi
  done < <(install_rollback_compose_files)
}

clear_install_toolstore_compose_sources() {
  unset TOOLSTORE_TXN_COMPOSE_BASE_SOURCE TOOLSTORE_TXN_COMPOSE_HOST_SOURCE TOOLSTORE_TXN_COMPOSE_LOGDB_SOURCE
}

discard_install_rollback_compose_bundle() {
  local env_file="$1" marker name snapshot_file cleanup_ok=true
  marker="$(install_rollback_compose_marker_path "$env_file")"
  while IFS= read -r name; do
    snapshot_file="$(install_rollback_compose_snapshot_path "$env_file" "$name")"
    durable_remove_install_file "$snapshot_file" || cleanup_ok=false
  done < <(install_rollback_compose_files)
  durable_remove_install_file "$marker" || cleanup_ok=false
  [[ "$cleanup_ok" == "true" ]]
}

persist_install_rollback_compose_bundle() {
  local env_file="$1" project_dir="$2" marker marker_content=""
  local name key source_file snapshot_file state
  marker="$(install_rollback_compose_marker_path "$env_file")"
  while IFS= read -r name; do
    key="$(install_rollback_compose_state_key "$name")" || return 1
    source_file="${project_dir}/${name}"
    snapshot_file="$(install_rollback_compose_snapshot_path "$env_file" "$name")"
    state=absent
    if [[ -e "$source_file" || -L "$source_file" ]]; then
      [[ -f "$source_file" && ! -L "$source_file" ]] || return 1
      atomic_write_install_dotenv "$snapshot_file" "$(<"$source_file")" || return 1
      state=present
    else
      durable_remove_install_file "$snapshot_file" || return 1
    fi
    marker_content+="${key}=${state}"$'\n'
  done < <(install_rollback_compose_files)
  [[ "$(env_content_value "$marker_content" 'COMPOSE_BASE')" == "present" ]] || return 1
  atomic_write_install_dotenv "$marker" "${marker_content%$'\n'}" || return 1
  validate_install_rollback_compose_bundle "$env_file"
}

restore_install_rollback_compose_bundle() {
  local env_file="$1" project_dir="$2" marker marker_content name key state snapshot_file target
  marker="$(install_rollback_compose_marker_path "$env_file")"
  [[ -e "$marker" || -L "$marker" ]] || return 0
  validate_install_rollback_compose_bundle "$env_file" || return 1
  marker_content="$(load_install_mode600_file "$marker")" || return 1
  while IFS= read -r name; do
    key="$(install_rollback_compose_state_key "$name")" || return 1
    state="$(env_content_value "$marker_content" "$key")"
    snapshot_file="$(install_rollback_compose_snapshot_path "$env_file" "$name")"
    target="${project_dir}/${name}"
    if [[ "$state" == "present" ]]; then
      atomic_write_install_dotenv "$target" "$(load_install_mode600_file "$snapshot_file")" || return 1
    else
      durable_remove_install_file "$target" || return 1
    fi
  done < <(install_rollback_compose_files)
}

build_install_rollback_compose_args() {
  local env_file="$1" project_dir="$2" output_name="$3" marker marker_content rollback_content
  local state network_value log_network
  local -n output_ref="$output_name"
  marker="$(install_rollback_compose_marker_path "$env_file")"
  if [[ ! -e "$marker" && ! -L "$marker" ]]; then
    build_install_compose_args "$env_file" "$project_dir" "$output_name"
    return
  fi
  validate_install_rollback_compose_bundle "$env_file" || return 1
  marker_content="$(load_install_mode600_file "$marker")" || return 1
  rollback_content="$(load_install_rollback_snapshot "$env_file")" || return 1
  output_ref=(--env-file "$(install_rollback_snapshot_path "$env_file")")
  output_ref+=(-f "$(install_rollback_compose_snapshot_path "$env_file" 'docker-compose.yml')")

  # File existence is not activation state: release checkouts normally contain
  # both optional overlays. Recreate the old file set from the authoritative
  # rollback dotenv exactly as setup_compose_files selected it at deployment.
  network_value="$(env_content_value "$rollback_content" 'NEWAPI_NETWORK')"
  if printf '%s\n' "$rollback_content" | grep -qE '^NEWAPI_NETWORK=' &&
    [[ -z "$network_value" ]]; then
    state="$(env_content_value "$marker_content" 'COMPOSE_HOST')"
    [[ "$state" == "present" ]] || return 1
    output_ref+=(-f "$(install_rollback_compose_snapshot_path "$env_file" 'docker-compose.host.yml')")
  fi

  log_network="$(env_content_value "$rollback_content" 'LOG_NETWORK')"
  if [[ -n "$log_network" && "$log_network" != "$network_value" ]]; then
    state="$(env_content_value "$marker_content" 'COMPOSE_LOGDB')"
    if [[ "$state" == "present" ]]; then
      output_ref+=(-f "$(install_rollback_compose_snapshot_path "$env_file" 'docker-compose.logdb.yml')")
    fi
  fi
}

restore_install_rollback_snapshot() {
  local env_file="$1" content
  content="$(load_install_rollback_snapshot "$env_file")" || return 1
  atomic_write_install_dotenv "$env_file" "$content"
}

remove_install_rollback_snapshot() {
  local env_file="$1" snapshot_file parent
  snapshot_file="$(install_rollback_snapshot_path "$env_file")"
  parent="$(dirname "$snapshot_file")"
  [[ -f "$snapshot_file" && ! -L "$snapshot_file" ]] || return 1
  rm -f -- "$snapshot_file" || return 1
  [[ ! -e "$snapshot_file" && ! -L "$snapshot_file" ]] || return 1
  if ! sync -f "$parent"; then
    persist_install_rollback_snapshot "$env_file" "$INSTALL_ROLLBACK_ENV_CONTENT" || true
    return 1
  fi
}

commit_install_rollback_transaction() {
  local env_file="$1" snapshot_file cleanup_ok=true
  snapshot_file="$(install_rollback_snapshot_path "$env_file")"
  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" != "true" ||
      -z "$INSTALL_ROLLBACK_ENV_CONTENT" ]]; then
      cleanup_ok=false
    elif remove_install_rollback_snapshot "$env_file"; then
      INSTALL_ROLLBACK_ENV_AVAILABLE=false
      INSTALL_ROLLBACK_ENV_CONTENT=""
      INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING=false
    else
      cleanup_ok=false
    fi
  else
    INSTALL_ROLLBACK_ENV_AVAILABLE=false
    INSTALL_ROLLBACK_ENV_CONTENT=""
    INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING=false
  fi
  if ! discard_install_rollback_compose_bundle "$env_file"; then
    cleanup_ok=false
  fi
  [[ "$cleanup_ok" == "true" ]]
}

start_and_finalize_committed_install_candidate() {
  local env_file="$1" project_dir="$2" candidate_image="$3"
  shift 3
  local snapshot_file rollback_content cleanup_ok=true
  snapshot_file="$(install_rollback_snapshot_path "$env_file")"

  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    if rollback_content="$(load_install_rollback_snapshot "$env_file")"; then
      INSTALL_ROLLBACK_ENV_CONTENT="$rollback_content"
      INSTALL_ROLLBACK_ENV_AVAILABLE=true
    else
      INSTALL_ROLLBACK_ENV_CONTENT=""
      INSTALL_ROLLBACK_ENV_AVAILABLE=false
      cleanup_ok=false
      log_warn "Tool Store 已提交；旧安装回滚快照不可读，已保留该文件和提交证据，仍将启动正式候选"
    fi
  else
    INSTALL_ROLLBACK_ENV_CONTENT=""
    INSTALL_ROLLBACK_ENV_AVAILABLE=false
  fi

  if ! commit_install_rollback_transaction "$env_file"; then
    cleanup_ok=false
    log_warn "Tool Store 已提交；旧安装或 Compose 回滚快照未完全清理，仍将启动正式候选，禁止恢复旧 schema"
  fi

  if ! start_install_services_and_wait "$env_file" "$project_dir" candidate "$candidate_image" "$@"; then
    log_error "Tool Store 已提交且不能安全回滚；正式候选启动失败，提交证据与未清理文件均已保留"
    return 1
  fi

  if [[ "$cleanup_ok" == "true" ]]; then
    if ! toolstore_txn_finish_committed "$project_dir"; then
      cleanup_ok=false
      log_warn "正式候选已健康，但 Tool Store 提交证据清理返回失败"
    elif toolstore_txn_has_active "$project_dir"; then
      cleanup_ok=false
      log_warn "正式候选已健康，但 Tool Store 提交证据仍处于活动状态"
    fi
  fi

  if [[ "$cleanup_ok" != "true" ]]; then
    log_warn "正式候选已健康并保持运行；已保留 active.env 与未删除快照，下次运行 install.sh 将幂等重试清理，请勿手工恢复旧数据库、配置或镜像"
  fi
  log_success "隔离候选验证和正式晋升均已完成，部署镜像为 ${candidate_image}"
}

durable_remove_install_tree() {
  local target="$1" parent base parent_abs resolved_target
  [[ -n "$target" && "$target" != "/" ]] || return 1
  parent="$(dirname -- "$target")"
  base="$(basename -- "$target")"
  [[ -n "$base" && "$base" != "." && "$base" != ".." && "$base" != "/" ]] || return 1
  parent_abs="$(cd -- "$parent" && pwd -P)" || return 1
  resolved_target="${parent_abs}/${base}"
  [[ "$resolved_target" != "/" ]] || return 1
  if [[ -e "$resolved_target" || -L "$resolved_target" ]]; then
    rm -rf -- "$resolved_target" || return 1
  fi
  [[ ! -e "$resolved_target" && ! -L "$resolved_target" ]] || return 1
  sync -f "$parent_abs" || return 1
}

durable_remove_install_file() {
  local target="$1" parent removed=false
  parent="$(dirname -- "$target")"
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

comment_and_replace_install_env_key() {
  local env_file="$1" key="$2" value="$3" comment_prefix="$4" content
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || return 1
  [[ -f "$env_file" && ! -L "$env_file" ]] || return 1
  content="$(
    awk -v k="$key" -v prefix="$comment_prefix" '
      index($0, k "=") == 1 { print prefix $0; next }
      { print }
    ' "$env_file"
    printf '%s=%s\n' "$key" "$value"
  )"
  atomic_write_install_dotenv "$env_file" "$content"
}

append_install_env_content_line() {
  local content="$1" line="$2"
  [[ -z "$content" ]] || printf '%s\n' "$content"
  printf '%s\n' "$line"
}

replace_install_env_content_optional() {
  local content="$1" key="$2" value="${3-}"
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || return 1
  printf '%s\n' "$content" | awk -v k="$key" 'index($0, k "=") == 1 { next } { print }'
  if [[ -n "$value" ]]; then
    printf '%s=%s\n' "$key" "$(dotenv_quote "$value")"
  fi
}

stop_install_project_for_removal() {
  local project_dir="$1" env_file="${1}/.env" image
  local -a compose_args=()
  [[ -f "$env_file" && ! -L "$env_file" && -f "${project_dir}/docker-compose.yml" ]] || return 1
  setup_compose_files "$project_dir"
  build_install_compose_args "$env_file" "$project_dir" compose_args
  image="$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')"
  run_install_compose "$env_file" "$project_dir" "$image" "${compose_args[@]}" down -v
}

# Replace an exact dotenv key atomically. An empty value removes the key so an
# unavailable auto-detection is not persisted as if it were configured.
replace_optional_env_value() {
  local env_file="$1" key="$2" value="${3-}" content
  [[ "$key" =~ ^[A-Z][A-Z0-9_]*$ ]] || die "无效的环境变量名称：${key}"
  [[ -f "$env_file" && ! -L "$env_file" ]] ||
    die "无法更新配置：${env_file} 不存在或不是安全的常规文件"

  content="$(
    awk -v k="$key" 'index($0, k "=") == 1 { next } { print }' "$env_file"
    if [[ -n "$value" ]]; then
      printf '%s=%s\n' "$key" "$(dotenv_quote "$value")"
    fi
  )"
  atomic_write_install_dotenv "$env_file" "$content"
}

legacy_image_version_to_reference() {
  local version="${1:-}"
  [[ "$version" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] ||
    die "旧 NEWAPI_TOOLS_VERSION 格式无效；请改用完整 NEWAPI_TOOLS_IMAGE"
  printf '%s:%s\n' "$NEWAPI_TOOLS_IMAGE_REPOSITORY" "$version"
}

# 将最终镜像引用以单一活动键幂等写入 .env，并停用旧版 tag-only 配置。
migrate_image_env_file() {
  local env_file="$1" selected_image="${2:-${NEWAPI_TOOLS_IMAGE:-}}"
  local allow_image_replacement="${3:-false}"
  [[ -f "$env_file" ]] || return 0

  local current_image legacy_version existing_image persisted_image rollback_anchor=""
  local existing_container_names="" has_existing_container=false
  current_image="$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')"
  legacy_version="$(env_file_value "$env_file" 'NEWAPI_TOOLS_VERSION')"
  existing_image="$current_image"
  if [[ -z "$existing_image" && -n "$legacy_version" ]]; then
    existing_image="$(legacy_image_version_to_reference "$legacy_version")"
  fi

  if [[ -z "$selected_image" ]]; then
    if [[ -n "$existing_image" ]]; then
      selected_image="$existing_image"
    else
      selected_image="${NEWAPI_TOOLS_IMAGE_REPOSITORY}:0.6.2"
    fi
  fi
  validate_newapi_tools_image "$selected_image"
  [[ -z "$existing_image" ]] || validate_newapi_tools_image "$existing_image"
  persisted_image="$selected_image"

  if [[ "$allow_image_replacement" != "true" ]]; then
    if ! existing_container_names="$(list_install_container_names)"; then
      die "无法可靠枚举现有 Docker 容器；拒绝在部署状态未知时迁移镜像配置"
    fi
    if printf '%s\n' "$existing_container_names" | grep -Fxq 'newapi-tools'; then
      has_existing_container=true
      [[ -n "$existing_image" ]] ||
        die "检测到现有 newapi-tools 容器，但配置中没有可审计的旧镜像引用"
    fi
  fi
  # Before pulling a candidate, every existing installation must have an
  # immutable rollback anchor that the current image-policy can restart. A
  # mutable current tag (including a legacy NEWAPI_TOOLS_VERSION or a same-tag
  # update) is resolved only from the already-local image. Zero or multiple
  # same-repository RepoDigests fail closed before any pull/down operation.
  # NEWAPI_TOOLS_IMAGE still exports the candidate for the later pull.
  if [[ -n "$existing_image" && "$allow_image_replacement" != "true" ]]; then
    if [[ "$has_existing_container" == "true" ]]; then
      if ! rollback_anchor="$(resolve_install_running_image_digest 'newapi-tools' "$existing_image")"; then
        die "无法把现有容器实际运行镜像固定为唯一 OCI digest；已拒绝在无回滚锚点时升级"
      fi
    elif is_immutable_newapi_tools_image "$existing_image"; then
      rollback_anchor="$existing_image"
    elif ! rollback_anchor="$(resolve_install_image_digest "$existing_image" "")"; then
      die "无法把现有镜像 ${existing_image} 固定为唯一 OCI digest；已拒绝在无回滚锚点时升级"
    fi
    persisted_image="$rollback_anchor"
  fi

  local content
  content="$(
    awk '
      index($0, "NEWAPI_TOOLS_IMAGE=") == 1 { next }
      index($0, "NEWAPI_TOOLS_VERSION=") == 1 {
        print "# Deprecated by install.sh: " $0
        next
      }
      { print }
    ' "$env_file"
    printf 'NEWAPI_TOOLS_IMAGE=%s\n' "$(dotenv_quote "$persisted_image")"
  )"
  atomic_write_install_dotenv "$env_file" "$content" ||
    die "无法原子持久化部署镜像配置"

  if [[ -n "$legacy_version" ]]; then
    log_info "已将旧 NEWAPI_TOOLS_VERSION 迁移为不可变回滚镜像 ${persisted_image}"
  elif [[ -n "$rollback_anchor" && "$rollback_anchor" != "$current_image" ]]; then
    log_info "已将现有镜像固定为升级回滚锚点 ${rollback_anchor}"
  elif [[ "$persisted_image" != "$current_image" ]]; then
    log_info "已写入部署镜像 NEWAPI_TOOLS_IMAGE=${persisted_image}"
  fi

  NEWAPI_TOOLS_IMAGE="$selected_image"
  export NEWAPI_TOOLS_IMAGE
}

install_compose_env_keys() {
  local env_file="$1" project_dir="$2"
  [[ -r "$env_file" ]] || return 1
  {
    grep -hoE '\$\{[A-Za-z_][A-Za-z0-9_]*' \
      "${project_dir}/docker-compose.yml" \
      "${project_dir}/docker-compose.host.yml" \
      "${project_dir}/docker-compose.logdb.yml" 2>/dev/null |
      sed 's/^${//' || true
    printf '%s\n' \
      NEWAPI_TOOLS_IMAGE NEWAPI_TOOLS_VERSION \
      COMPOSE_FILE COMPOSE_PROJECT_NAME COMPOSE_PROFILES \
      COMPOSE_ENV_FILES COMPOSE_DISABLE_ENV_FILE
  } | sort -u
}

run_install_compose() {
  local env_file="$1" project_dir="$2" image_override="${3:-}"
  local compose_file_override="${COMPOSE_FILE:-}" project_name
  local -a project_args=()
  shift 3
  [[ -r "$env_file" ]] || {
    log_error "Compose 配置文件不可读：${env_file}"
    return 1
  }
  project_name="${INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE:-}"
  [[ -n "$project_name" ]] || project_name="$(env_file_value "$env_file" 'COMPOSE_PROJECT_NAME')"
  if [[ -n "$project_name" ]]; then
    [[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || {
      log_error "配置中的 COMPOSE_PROJECT_NAME 格式无效，拒绝猜测 Compose 项目"
      return 1
    }
    project_args=(-p "$project_name")
  fi
  (
    local key
    while IFS= read -r key; do
      [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || continue
      unset "$key" 2>/dev/null || true
    done < <(install_compose_env_keys "$env_file" "$project_dir")
    if [[ -n "$compose_file_override" ]]; then
      COMPOSE_FILE="$compose_file_override"
      export COMPOSE_FILE
    fi
    if [[ -n "$image_override" ]]; then
      NEWAPI_TOOLS_IMAGE="$image_override"
      export NEWAPI_TOOLS_IMAGE
    fi
    $DOCKER_COMPOSE "${project_args[@]}" "$@"
  )
}

is_install_v05_ready_response() {
  local body="${1:-}"
  printf '%s' "$body" | grep -Eq '^[[:space:]]*\{' &&
    printf '%s' "$body" | grep -Eq '"status"[[:space:]]*:[[:space:]]*"ready"' &&
    printf '%s' "$body" | grep -Eq '"main_database"[[:space:]]*:[[:space:]]*"ok"' &&
    printf '%s' "$body" | grep -Eq '"tool_store"[[:space:]]*:[[:space:]]*"ok"'
}

is_install_legacy_db_ready_response() {
  local body="${1:-}"
  printf '%s' "$body" | grep -Eq '^[[:space:]]*\{' &&
    printf '%s' "$body" | grep -Eq '"success"[[:space:]]*:[[:space:]]*true' &&
    printf '%s' "$body" | grep -Eq '"status"[[:space:]]*:[[:space:]]*"connected"'
}

verify_install_application_health() {
  local mode="$1" container="${2:-newapi-tools}" port="${3:-8080}" ready_body="" legacy_body=""
  ready_body="$(docker exec "$container" curl --silent --show-error \
    "http://localhost:${port}/readyz" 2>/dev/null || true)"
  if is_install_v05_ready_response "$ready_body"; then
    return 0
  fi
  [[ "$mode" == "rollback" ]] || return 1
  if printf '%s' "$ready_body" | grep -Eq '^[[:space:]]*\{'; then
    return 1
  fi
  legacy_body="$(docker exec "$container" curl --fail --silent --show-error \
    "http://localhost:${port}/api/health/db" 2>/dev/null || true)"
  is_install_legacy_db_ready_response "$legacy_body"
}

start_install_services_and_wait() {
  local env_file="$1" project_dir="$2" health_mode="$3" image_override="$4"
  shift 4
  run_install_compose "$env_file" "$project_dir" "$image_override" "$@" up -d || return 1
  restore_runtime_network_connections "$project_dir" || return 1
  run_install_compose "$env_file" "$project_dir" "$image_override" "$@" \
    up -d --wait --wait-timeout 180 || return 1
  verify_install_application_health "$health_mode"
}

install_isolated_candidate_name() {
  local project_dir="$1" container request_id compose_project candidate_image project
  toolstore_txn_candidate_identity "$project_dir" container request_id compose_project candidate_image project || return 1
  printf '%s\n' "$container"
}

stop_install_isolated_candidate() {
  local project_dir="$1"
  toolstore_txn_remove_candidate_container "$project_dir"
}

start_install_isolated_candidate_and_wait() {
  local env_file="$1" project_dir="$2" image_override="$3"
  shift 3
  local container request_id compose_project candidate_image project created_ref container_id deadline
  toolstore_txn_candidate_identity "$project_dir" container request_id compose_project candidate_image project || return 1
  [[ "$image_override" == "$candidate_image" ]] || return 1
  ! docker inspect "$container" >/dev/null 2>&1 || return 1
  created_ref="$(run_install_compose "$env_file" "$project_dir" "$image_override" "$@" \
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
  if ! toolstore_txn_record_candidate_container_id "$project_dir" "$container_id"; then
    docker rm -f "$container_id" >/dev/null 2>&1 || return 1
    ! docker inspect "$container_id" >/dev/null 2>&1 || return 1
    return 1
  fi
  if ! toolstore_txn_candidate_container_matches "$project_dir" "$container_id"; then
    toolstore_txn_remove_created_candidate_by_id "$project_dir" "$container_id" || return 1
    return 1
  fi
  restore_runtime_network_connections "$project_dir" "$container_id" || return 1
  deadline=$((SECONDS + 180))
  while (( SECONDS < deadline )); do
    [[ "$(docker inspect -f '{{.State.Running}}' "$container_id" 2>/dev/null || true)" == "true" ]] || return 1
    if verify_install_application_health candidate "$container_id" 8000; then
      return 0
    fi
    sleep 2
  done
  return 1
}

recover_active_install_toolstore_transaction() {
  local project_dir="$1" content state rollback_image candidate_image
  local -a compose_args=()
  load_install_toolstore_transaction_library "$project_dir" || return 1
  toolstore_txn_has_active "$project_dir" || return 0
  content="$(toolstore_txn_load_record "$project_dir")" || return 1
  state="$(toolstore_txn_env_value "$content" STATE)"
  stop_install_isolated_candidate "$project_dir" || return 1
  if [[ "$state" == "committed" ]]; then
    candidate_image="$(toolstore_txn_env_value "$content" CANDIDATE_IMAGE)"
    (migrate_image_env_file "${project_dir}/.env" "$candidate_image" true) || return 1
    setup_compose_files "$project_dir"
    build_install_compose_args "${project_dir}/.env" "$project_dir" compose_args
    start_and_finalize_committed_install_candidate \
      "${project_dir}/.env" "$project_dir" "$candidate_image" "${compose_args[@]}"
    return $?
  fi
  rollback_image="$(toolstore_txn_env_value "$content" OLD_IMAGE)"
  toolstore_txn_restore_files "$project_dir" || return 1
  setup_compose_files "$project_dir"
  build_install_compose_args "${project_dir}/.env" "$project_dir" compose_args
  if [[ "$state" == "preparing" || "$state" == "preflight" ]]; then
    start_install_services_and_wait "${project_dir}/.env" "$project_dir" rollback \
      "$rollback_image" "${compose_args[@]}" || return 1
    toolstore_txn_retire_preflight "$project_dir" || return 1
    return 0
  fi
  if [[ "$state" == "rolled_back" ]]; then
    start_install_services_and_wait "${project_dir}/.env" "$project_dir" rollback \
      "$rollback_image" "${compose_args[@]}" || return 1
    toolstore_txn_retire_rolled_back "$project_dir" || return 1
    log_success "已验证上次回滚后的旧服务并保留证据；下一候选将创建全新 Tool Store 备份"
    return 0
  fi
  toolstore_txn_unstage_data "$project_dir" || return 1
  if ! run_install_compose "${project_dir}/.env" "$project_dir" "$rollback_image" \
    "${compose_args[@]}" down; then
    log_warn "清理中断事务容器返回失败；仍尝试恢复数据库并重建旧服务"
  fi
  toolstore_txn_restore_database "$project_dir" || return 1
  start_install_services_and_wait "${project_dir}/.env" "$project_dir" rollback \
    "$rollback_image" "${compose_args[@]}" || return 1
  toolstore_txn_mark_rolled_back "$project_dir" || return 1
  toolstore_txn_retire_rolled_back "$project_dir" || return 1
  log_success "已恢复并验证中断事务前的 Tool Store、旧配置和旧镜像"
}

build_install_compose_args() {
  local env_file="$1" project_dir="$2" output_name="$3"
  local -n output_ref="$output_name"
  output_ref=(--env-file "$env_file")
  if [[ -z "${COMPOSE_FILE:-}" ]]; then
    output_ref+=(-f "${project_dir}/docker-compose.yml")
  fi
}

capture_install_rollback_env() {
  local env_file="$1" snapshot_file old_content rollback_reference
  local legacy_version rollback_project rollback_image normalized_content
  local container_names has_existing_container=false
  INSTALL_ROLLBACK_ENV_AVAILABLE=false
  INSTALL_ROLLBACK_ENV_CONTENT=""
  INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING=false
  snapshot_file="$(install_rollback_snapshot_path "$env_file")"

  if [[ -e "$snapshot_file" || -L "$snapshot_file" ]]; then
    old_content="$(load_install_rollback_snapshot "$env_file")" ||
      die "检测到不可读取、不安全或权限不是 0600 的安装回滚快照 ${snapshot_file}"
    rollback_reference="$(env_content_value "$old_content" 'NEWAPI_TOOLS_IMAGE')"
    rollback_project="$(env_content_value "$old_content" 'COMPOSE_PROJECT_NAME')"
    is_immutable_newapi_tools_image "$rollback_reference" ||
      die "安装回滚快照未固定到不可变 OCI digest"
    [[ "$rollback_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "安装回滚快照缺少有效的 Compose project"
    INSTALL_ROLLBACK_ENV_CONTENT="$old_content"
    INSTALL_ROLLBACK_ENV_AVAILABLE=true
    INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING=true
    log_warn "检测到上次未提交完成的安装事务；将先恢复权威旧部署"
    return 0
  fi

  container_names="$(list_install_container_names)" ||
    die "无法可靠枚举现有 Docker 容器；拒绝建立安装回滚事务"
  if printf '%s\n' "$container_names" | grep -Fxq 'newapi-tools'; then
    has_existing_container=true
  elif [[ ! -e "$env_file" && ! -L "$env_file" ]]; then
    return 0
  fi
  [[ -f "$env_file" && ! -L "$env_file" ]] ||
    die "检测到已有安装配置，但缺少安全的旧 .env"
  old_content="$(<"$env_file")"

  rollback_reference="$(env_content_value "$old_content" 'NEWAPI_TOOLS_IMAGE')"
  if [[ -z "$rollback_reference" ]]; then
    legacy_version="$(env_content_value "$old_content" 'NEWAPI_TOOLS_VERSION')"
    if [[ -n "$legacy_version" ]]; then
      rollback_reference="$(legacy_image_version_to_reference "$legacy_version")"
    fi
  fi
  if [[ -z "$rollback_reference" && "$has_existing_container" != "true" ]]; then
    # A copied .env.example with empty persistent image keys is still fresh;
    # the first candidate may be supplied only through the process environment.
    return 0
  fi
  [[ -n "$rollback_reference" ]] ||
    die "现有安装缺少 NEWAPI_TOOLS_IMAGE/NEWAPI_TOOLS_VERSION"
  validate_newapi_tools_image "$rollback_reference"
  if [[ "$has_existing_container" == "true" ]]; then
    rollback_project="$(resolve_install_running_compose_project 'newapi-tools' "$env_file")" ||
      die "无法固定现有安装的 Compose project 身份"
    rollback_image="$(resolve_install_running_image_digest 'newapi-tools' "$rollback_reference")" ||
      die "无法把现有安装固定为唯一 OCI digest"
  else
    rollback_project="$(env_content_value "$old_content" 'COMPOSE_PROJECT_NAME')"
    [[ "$rollback_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "旧 .env 对应的容器不存在，且缺少有效的显式 COMPOSE_PROJECT_NAME；拒绝猜测回滚项目"
    if is_immutable_newapi_tools_image "$rollback_reference"; then
      rollback_image="$rollback_reference"
    elif ! rollback_image="$(resolve_install_image_digest "$rollback_reference" "")"; then
      die "旧 .env 对应的容器不存在，且可变镜像无法从本地唯一固定为同仓库 OCI digest"
    fi
  fi
  normalized_content="$(normalize_install_rollback_env_content \
    "$old_content" "$rollback_image" "$rollback_project")" ||
    die "无法生成完整安装回滚配置"
  persist_install_rollback_snapshot "$env_file" "$normalized_content" ||
    die "无法原子持久化安装回滚快照"
  atomic_write_install_dotenv "$env_file" "$normalized_content" ||
    die "无法在升级前持久化旧安装身份"

  INSTALL_ROLLBACK_ENV_CONTENT="$normalized_content"
  INSTALL_ROLLBACK_ENV_AVAILABLE=true
  INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE="$rollback_project"
}

restore_install_previous_configuration() {
  local env_file="$1" project_dir="$2" content rollback_image rollback_project
  [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]] || return 1
  content="$(load_install_rollback_snapshot "$env_file")" || return 1
  rollback_image="$(env_content_value "$content" 'NEWAPI_TOOLS_IMAGE')"
  rollback_project="$(env_content_value "$content" 'COMPOSE_PROJECT_NAME')"
  is_immutable_newapi_tools_image "$rollback_image" || return 1
  [[ "$rollback_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] || return 1
  atomic_write_install_dotenv "$env_file" "$content" || return 1
  INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE="$rollback_project"
  NEWAPI_TOOLS_IMAGE="$rollback_image"
  export NEWAPI_TOOLS_IMAGE
  restore_install_rollback_compose_bundle "$env_file" "$project_dir" || return 1
  setup_compose_files "$project_dir"
}

recover_preexisting_install_rollback_snapshot() {
  local env_file="$1" project_dir="$2" candidate_image="${NEWAPI_TOOLS_IMAGE:-}" rollback_image
  local candidate_was_set=false
  local -a compose_args=()
  [[ "$INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING" == "true" ]] || return 0
  [[ -n "${NEWAPI_TOOLS_IMAGE+x}" ]] && candidate_was_set=true
  restore_install_previous_configuration "$env_file" "$project_dir" || return 1
  rollback_image="$NEWAPI_TOOLS_IMAGE"
  build_install_compose_args "$env_file" "$project_dir" compose_args
  log_warn "正在从中断安装事务恢复旧部署 ${rollback_image}"
  if ! run_install_compose "$env_file" "$project_dir" "$rollback_image" \
    "${compose_args[@]}" down; then
    log_error "清理中断安装事务当前容器失败；仍将尝试重建旧服务"
  fi
  if ! start_install_services_and_wait "$env_file" "$project_dir" rollback \
    "$rollback_image" "${compose_args[@]}"; then
    if [[ "$candidate_was_set" == "true" ]]; then
      NEWAPI_TOOLS_IMAGE="$candidate_image"
      export NEWAPI_TOOLS_IMAGE
    else
      unset NEWAPI_TOOLS_IMAGE
    fi
    return 1
  fi
  if [[ "$candidate_was_set" == "true" ]]; then
    NEWAPI_TOOLS_IMAGE="$candidate_image"
    export NEWAPI_TOOLS_IMAGE
  else
    unset NEWAPI_TOOLS_IMAGE
  fi
  INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING=false
  log_success "已恢复并验证中断事务前的旧安装"
}

prepare_install_rollback_transaction() {
  local env_file="$1" project_dir="$2" snapshot_file compose_marker content
  local snapshot_was_present=false
  snapshot_file="$(install_rollback_snapshot_path "$env_file")"
  compose_marker="$(install_rollback_compose_marker_path "$env_file")"
  if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" &&
        "$INSTALL_ROLLBACK_SNAPSHOT_PREEXISTING" != "true" &&
        ( -e "$snapshot_file" || -L "$snapshot_file" ) ]]; then
    content="$(load_install_rollback_snapshot "$env_file")" ||
      die "安装回滚快照在更新前变得不可读"
    INSTALL_ROLLBACK_ENV_CONTENT="$content"
    if [[ -e "$compose_marker" || -L "$compose_marker" ]]; then
      validate_install_rollback_compose_bundle "$env_file" ||
        die "安装 Compose 回滚快照在更新前变得不可读"
    else
      persist_install_rollback_compose_bundle "$env_file" "$project_dir" ||
        die "无法持久化旧 Compose 清单回滚快照"
    fi
    return 0
  fi
  [[ -e "$snapshot_file" || -L "$snapshot_file" ]] && snapshot_was_present=true
  capture_install_rollback_env "$env_file"
  recover_preexisting_install_rollback_snapshot "$env_file" "$project_dir" ||
    die "无法恢复上次中断的安装事务；快照已保留"
  if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
    if [[ "$snapshot_was_present" != "true" ]]; then
      discard_install_rollback_compose_bundle "$env_file" ||
        die "无法清理非活动 Compose 回滚副本"
    fi
    if [[ -e "$compose_marker" || -L "$compose_marker" ]]; then
      validate_install_rollback_compose_bundle "$env_file" ||
        die "安装 Compose 回滚快照不可读"
    else
      persist_install_rollback_compose_bundle "$env_file" "$project_dir" ||
        die "无法持久化旧 Compose 清单回滚快照"
    fi
  fi
}

# Restart an existing installation as a transaction. The .env file keeps the
# previous immutable image until the candidate is healthy; only then is the
# candidate digest committed. Any failed candidate start (or failed commit)
# restores and health-checks the previous image before returning a failure.
restart_install_services_transactionally() {
  local env_file="$1" project_dir="$2"
  shift 2

  local candidate_image="$NEWAPI_TOOLS_IMAGE" rollback_image container_names project_name=""
  local rollback_env_file="$env_file"
  local rollback_content="" rollback_config_restored=true candidate_healthy=false rollback_healthy=false
  local toolstore_commit_succeeded=false
  local -a candidate_compose_args=() rollback_compose_args=()
  if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
    rollback_content="$(load_install_rollback_snapshot "$env_file")" ||
      die "持久化安装回滚快照不可读；拒绝切换服务"
    rollback_image="$(env_content_value "$rollback_content" 'NEWAPI_TOOLS_IMAGE')"
    project_name="$(env_content_value "$rollback_content" 'COMPOSE_PROJECT_NAME')"
    [[ "$project_name" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
      die "安装回滚快照缺少有效 Compose project"
    INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE="$project_name"
    rollback_env_file="$(install_rollback_snapshot_path "$env_file")"
  else
    rollback_image="$(env_file_value "$env_file" 'NEWAPI_TOOLS_IMAGE')"
    INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE=""
  fi
  is_immutable_newapi_tools_image "$candidate_image" ||
    die "候选镜像尚未固定为不可变 OCI digest；拒绝切换服务"
  is_immutable_newapi_tools_image "$rollback_image" ||
    die "现有服务缺少不可变回滚镜像；拒绝切换服务"

  if ! container_names="$(list_install_container_names)"; then
    die "无法可靠枚举现有 Docker 容器；拒绝在部署身份未知时切换服务"
  fi
  if [[ -z "$project_name" ]] && printf '%s\n' "$container_names" | grep -Fxq 'newapi-tools'; then
    project_name="$(resolve_install_running_compose_project 'newapi-tools' "$env_file")" ||
      die "无法建立可信的旧 Compose project 身份；拒绝切换服务"
    INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE="$project_name"
    replace_optional_env_value "$env_file" 'COMPOSE_PROJECT_NAME' "$project_name"
  fi

  setup_compose_files "$project_dir"
  build_install_compose_args "$env_file" "$project_dir" candidate_compose_args
  if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
    build_install_rollback_compose_args "$env_file" "$project_dir" rollback_compose_args ||
      die "无法构建旧 Compose 清单回滚参数"
  else
    rollback_compose_args=("${candidate_compose_args[@]}")
  fi

  local toolstore_host_path
  if ! toolstore_host_path="$(resolve_install_toolstore_host_path "$rollback_env_file" "$project_dir")"; then
    die "旧安装的 TOOL_STORE_PATH 为空、越界或穿越 symlink；拒绝停止旧服务"
  fi
  if [[ -e "$toolstore_host_path" || -L "$toolstore_host_path" ]]; then
    local toolstore_prepare_ok=false
    [[ -f "$toolstore_host_path" && ! -L "$toolstore_host_path" ]] ||
      die "旧安装的 Tool Store 不是安全的常规文件；拒绝停止旧服务"
    load_install_toolstore_transaction_library "$project_dir" ||
      die "当前 checkout 缺少安全的 Tool Store 事务库；拒绝停止旧服务"
    if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      configure_install_toolstore_compose_sources "$env_file" ||
        die "无法把旧 Compose 清单绑定到 Tool Store 回滚记录"
    fi
    if toolstore_txn_prepare "$rollback_env_file" "$project_dir" \
      "$rollback_image" "$candidate_image" "$candidate_image"; then
      toolstore_prepare_ok=true
    fi
    clear_install_toolstore_compose_sources
    if [[ "$toolstore_prepare_ok" != "true" ]]; then
      if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
        restore_install_previous_configuration "$env_file" "$project_dir" ||
          log_error "高危：Tool Store 备份失败后无法恢复完整旧安装配置"
      fi
      die "候选切换前无法创建并二次验证 Tool Store Online Backup；旧服务保持运行"
    fi
  else
    log_warn "旧安装没有 Tool Store 文件，按 pre-Tool-Store 兼容升级处理；候选仍必须通过内容健康检查"
  fi

  log_info "重启服务并等待候选版本健康..."
  if ! run_install_compose "$rollback_env_file" "$project_dir" "$rollback_image" \
    "${rollback_compose_args[@]}" down; then
    log_error "停止现有服务返回失败；按可能已部分删除处理并立即重建旧服务"
    if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      restore_install_previous_configuration "$env_file" "$project_dir" ||
        die "初次停止失败，且无法恢复完整旧安装配置"
    else
      NEWAPI_TOOLS_IMAGE="$rollback_image"
      export NEWAPI_TOOLS_IMAGE
      setup_compose_files "$project_dir"
    fi
    rollback_env_file="$env_file"
    build_install_compose_args "$env_file" "$project_dir" rollback_compose_args
    if start_install_services_and_wait "$env_file" "$project_dir" rollback "$rollback_image" \
      "${rollback_compose_args[@]}"; then
      if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
        toolstore_txn_has_active "$project_dir"; then
        toolstore_txn_retire_preflight "$project_dir" ||
          die "旧服务已恢复，但无法退役仅供预检的 Tool Store 备份"
      fi
      die "候选版本尚未启动；初次停止失败后已恢复旧镜像 ${rollback_image}"
    fi
    die "初次停止失败，且旧镜像 ${rollback_image} 无法恢复健康；请立即检查 docker compose ps/logs"
  fi

  if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
    toolstore_txn_has_active "$project_dir"; then
    if ! toolstore_txn_authoritative_backup "$project_dir" ||
      ! toolstore_txn_mark_candidate "$project_dir"; then
      recover_active_install_toolstore_transaction "$project_dir" ||
        die "旧服务已停止，但无法建立权威 Tool Store 回滚锚点且自动恢复失败"
      die "候选版本尚未启动；权威 Tool Store 备份失败后已恢复旧服务"
    fi
    if start_install_isolated_candidate_and_wait "$env_file" "$project_dir" "$candidate_image" \
      "${candidate_compose_args[@]}"; then
      candidate_healthy=true
    else
      stop_install_isolated_candidate "$project_dir" ||
        die "隔离候选停止身份或容器 ID 无法验证；禁止恢复数据库或启动旧服务"
      log_error "隔离候选无法启动、恢复运行时网络或通过内容健康检查，将执行回滚"
    fi
  elif start_install_services_and_wait "$env_file" "$project_dir" candidate "$candidate_image" \
    "${candidate_compose_args[@]}"; then
    candidate_healthy=true
  else
    log_error "候选服务无法启动、恢复运行时网络或通过内容健康检查，将执行回滚"
  fi

  if [[ "$candidate_healthy" == "true" ]]; then
    if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
      toolstore_txn_has_active "$project_dir"; then
      if ! stop_install_isolated_candidate "$project_dir" || ! toolstore_txn_verify_candidate "$project_dir"; then
        candidate_healthy=false
        log_error "隔离候选停止或 Tool Store schema/关键表验证失败，将执行数据库与镜像回滚"
      fi
    fi
  fi

  if [[ "$candidate_healthy" == "true" ]]; then
    # Run the atomic file update in a subshell so a persistence failure can be
    # caught and treated like any other failed candidate activation.
    if (migrate_image_env_file "$env_file" "$candidate_image" true); then
      NEWAPI_TOOLS_IMAGE="$candidate_image"
      export NEWAPI_TOOLS_IMAGE
      if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
        toolstore_txn_has_active "$project_dir" &&
        ! toolstore_txn_verify_candidate "$project_dir"; then
        candidate_healthy=false
        log_error "候选服务健康，但 Tool Store schema/核心表计数验证失败，将执行数据库与镜像回滚"
      elif [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
        toolstore_txn_has_active "$project_dir"; then
        if toolstore_txn_commit "$project_dir"; then
          toolstore_commit_succeeded=true
        else
          candidate_healthy=false
          log_error "候选服务已健康，但 Tool Store 提交结果未确认；将读取持久事务状态后决定恢复方向"
        fi
      elif [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]] &&
        ! commit_install_rollback_transaction "$env_file"; then
        candidate_healthy=false
        log_error "候选镜像已提交，但无法持久清理安装回滚快照"
      else
        if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" != "true" ]] ||
          ! toolstore_txn_has_active "$project_dir"; then
          log_success "候选镜像已健康，部署镜像已提交为 ${candidate_image}"
          return 0
        fi
      fi
    fi
    [[ "$candidate_healthy" == "false" || "$toolstore_commit_succeeded" == "true" ]] ||
      log_error "候选服务已健康，但无法提交其镜像配置，将执行回滚"
  fi

  if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
    toolstore_txn_has_active "$project_dir"; then
    local committed_content committed_state
    committed_content="$(toolstore_txn_load_record "$project_dir")" ||
      die "无法读取候选晋升事务状态；拒绝猜测是否可回滚"
    committed_state="$(toolstore_txn_env_value "$committed_content" STATE)"
    if [[ "$committed_state" == "committed" ]]; then
      if start_and_finalize_committed_install_candidate \
        "$env_file" "$project_dir" "$candidate_image" "${candidate_compose_args[@]}"; then
        return 0
      fi
      die "Tool Store 已提交且不能安全回滚；正式候选启动失败，事务证据已保留供下次继续晋升"
    fi
  fi

  if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
    toolstore_txn_has_active "$project_dir"; then
    stop_install_isolated_candidate "$project_dir" ||
      die "无法证明隔离候选已停止；禁止恢复数据库或启动旧服务"
  fi

  if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
    if ! restore_install_previous_configuration "$env_file" "$project_dir"; then
      rollback_config_restored=false
      log_error "无法恢复持久化的完整旧安装配置"
    fi
  else
    NEWAPI_TOOLS_IMAGE="$rollback_image"
    export NEWAPI_TOOLS_IMAGE
    if ! (migrate_image_env_file "$env_file" "$rollback_image" true); then
      rollback_config_restored=false
      log_error "无法持久化恢复回滚镜像配置"
    fi
    setup_compose_files "$project_dir"
  fi

  build_install_compose_args "$env_file" "$project_dir" rollback_compose_args

  if ! run_install_compose "$env_file" "$project_dir" "$rollback_image" \
    "${rollback_compose_args[@]}" down; then
    log_error "清理失败的候选服务时发生错误；仍将尝试重建旧服务"
  fi

  if [[ "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
    toolstore_txn_has_active "$project_dir"; then
    toolstore_txn_unstage_data "$project_dir" ||
      die "Tool Store 数据目录无法恢复到项目树；旧镜像未启动，回滚证据已保留"
    toolstore_txn_restore_database "$project_dir" ||
      die "Tool Store 原子恢复失败；旧镜像不会在可能不兼容的 schema 上启动，回滚证据已保留"
  fi

  if start_install_services_and_wait "$env_file" "$project_dir" rollback "$rollback_image" \
    "${rollback_compose_args[@]}"; then
    rollback_healthy=true
  fi

  if [[ "$rollback_healthy" == "true" && "$INSTALL_TOOLSTORE_TXN_LOADED" == "true" ]] &&
    toolstore_txn_has_active "$project_dir"; then
    toolstore_txn_mark_rolled_back "$project_dir" ||
      die "旧服务已恢复健康，但无法持久化 Tool Store 回滚完成状态；证据已保留"
  fi

  if [[ "$rollback_config_restored" == "true" && "$rollback_healthy" == "true" ]]; then
    die "候选服务启动失败；已恢复旧镜像 ${rollback_image}，更新未提交"
  fi
  die "候选服务启动失败，且旧镜像 ${rollback_image} 的配置或健康回滚也失败；请立即检查 docker compose ps/logs"
}

#######################################
# 根据 .env 中的网络配置叠加 host / 日志库 overlay，
# 设置 COMPOSE_FILE 让所有后续 docker compose 调用使用相同文件集合。
# 在任何 $DOCKER_COMPOSE 调用前先调用本函数（通常 cd 到 project_dir 之后）。
#######################################
setup_compose_files() {
  local project_dir="${1:-.}"
  local env_file="${project_dir}/.env"
  local base="${project_dir}/docker-compose.yml"
  local host_overlay="${project_dir}/docker-compose.host.yml"
  local log_overlay="${project_dir}/docker-compose.logdb.yml"

  unset COMPOSE_FILE

  [[ -f "$env_file" ]] || return 0

  local -a compose_files=("$base")
  local newapi_network=""

  # 必须显式存在 NEWAPI_NETWORK 行才判断；行缺失视为老版 .env，让 base compose 走默认 fallback
  # 注意：set -e + pipefail 下，grep 无匹配会让 pipe 退出码为 1 → 整个脚本死掉，必须 || true 兜底。
  if grep -qE '^NEWAPI_NETWORK=' "$env_file" 2>/dev/null; then
    newapi_network="$(env_file_value "$env_file" 'NEWAPI_NETWORK')"

    # NEWAPI_NETWORK= （空值）→ deploy.sh 在 host 模式下生成的标记
    if [[ -z "$newapi_network" ]]; then
      [[ -f "$host_overlay" ]] ||
        die "host 模式需要 ${host_overlay}，缺少该叠加层时无法安全移除基础 external network 配置"
      require_docker_compose_v224 "host 网络叠加层 ${host_overlay} 的 !reset 语法"
      compose_files+=("$host_overlay")
    fi
  fi

  local log_network
  log_network="$(env_file_value "$env_file" 'LOG_NETWORK')"
  if [[ -n "$log_network" && "$log_network" != "$newapi_network" ]]; then
    if [[ -f "$log_overlay" ]]; then
      compose_files+=("$log_overlay")
    else
      log_warn "LOG_NETWORK=${log_network}，但未找到 ${log_overlay}；更新后将仅尝试运行时接入该网络"
    fi
  fi

  if (( ${#compose_files[@]} > 1 )); then
    local compose_file_value="${compose_files[0]}" i
    for ((i = 1; i < ${#compose_files[@]}; i++)); do
      compose_file_value+=":${compose_files[$i]}"
    done
    export COMPOSE_FILE="$compose_file_value"
  fi
}

container_is_connected_to_network() {
  local container="$1" network="$2"
  docker network inspect "$network" -f '{{range .Containers}}{{println .Name}}{{end}}' 2>/dev/null |
    grep -Fxq "$container"
}

ensure_container_network() {
  local container="$1" network="$2" label="$3"
  [[ -n "$network" ]] || return 0

  if container_is_connected_to_network "$container" "$network"; then
    return 0
  fi

  log_info "连接到${label}: $network"
  if ! docker network connect "$network" "$container" 2>/dev/null &&
    ! container_is_connected_to_network "$container" "$network"; then
    log_error "无法连接到${label} '$network'，请检查网络是否存在以及 Docker 权限"
    return 1
  fi
  container_is_connected_to_network "$container" "$network" || {
    log_error "连接${label} '$network' 后验证失败"
    return 1
  }
  log_success "已连接到${label}: $network"
}

# Compose down/up 会移除 docker network connect 手动附加的网络；每次重建后恢复它们。
restore_runtime_network_connections() {
  local project_dir="$1" tools_container="${2:-newapi-tools}"
  local env_file="${project_dir}/.env"
  [[ -f "$env_file" ]] || return 0

  local network_mode original_network newapi_network log_network
  network_mode="$(env_file_value "$env_file" 'NEWAPI_NETWORK_MODE')"
  original_network="$(env_file_value "$env_file" 'NEWAPI_ORIGINAL_NETWORK')"
  newapi_network="$(env_file_value "$env_file" 'NEWAPI_NETWORK')"
  log_network="$(env_file_value "$env_file" 'LOG_NETWORK')"

  # 兼容旧版 deploy.sh：网络模式曾只写在注释里。
  if [[ -z "$network_mode" ]]; then
    network_mode="$(sed -n 's/^# 网络部署模式:[[:space:]]*//p' "$env_file" 2>/dev/null | head -n1 | tr -d '\r\n')"
  fi

  # 再兼容没有模式标记的默认 bridge 安装：以 NewAPI 容器实际网络模式为准。
  if [[ -z "$network_mode" && "$newapi_network" == "newapi-tools-network" ]]; then
    local newapi_container docker_network_mode
    newapi_container="$(env_file_value "$env_file" 'NEWAPI_CONTAINER')"
    [[ -n "$newapi_container" ]] || newapi_container="$(find_newapi_container || true)"
    docker_network_mode="$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$newapi_container" 2>/dev/null || true)"
    case "$docker_network_mode" in
      default|bridge)
        network_mode="bridge"
        original_network="${original_network:-bridge}"
        ;;
    esac
  fi

  case "$network_mode" in
    bridge)
      ensure_container_network "$tools_container" "${original_network:-bridge}" " NewAPI 原始 bridge 网络"
      ;;
    host)
      ;;
    *)
      ensure_container_network "$tools_container" "$newapi_network" " NewAPI 网络"
      ;;
  esac

  ensure_container_network "$tools_container" "$log_network" "日志库网络"
}

#######################################
# 只清理本项目命名的 Docker 残留资源。
# 注意：不要调用 docker system prune，它会全局删除其他项目的已停止容器、
# 未使用网络、悬空镜像和构建缓存。
#######################################
cleanup_project_docker_resources() {
  log_info "清理 newapi-tools 残留 Docker 资源..."

  docker ps -a --format '{{.Names}}' \
    | grep -E '^(newapi-tools|newapi-tools-redis|newapi-tools-backend|newapi-tools-frontend)$' \
    | xargs -r docker rm -f 2>/dev/null || true

  docker images --format '{{.Repository}}:{{.Tag}}' \
    | grep -E '^(ghcr\.io/(yujianwudi|james-6-23)/new_api_tools|new_api_tools|newapi-tools|newapi-tools-backend|newapi-tools-frontend)(:|$)' \
    | xargs -r docker rmi -f 2>/dev/null || true

  docker network rm newapi-tools-network new_api_tools_default 2>/dev/null || true
}

#######################################
# 检查必要命令
#######################################
check_requirements() {
  local missing=()

  command -v git >/dev/null 2>&1 || missing+=("git")
  command -v docker >/dev/null 2>&1 || missing+=("docker")
  command -v flock >/dev/null 2>&1 || missing+=("flock")
  command -v id >/dev/null 2>&1 || missing+=("id")
  command -v sha256sum >/dev/null 2>&1 || missing+=("sha256sum")
  command -v realpath >/dev/null 2>&1 || missing+=("realpath")
  command -v stat >/dev/null 2>&1 || missing+=("stat")
  command -v sync >/dev/null 2>&1 || missing+=("sync")
  command -v timeout >/dev/null 2>&1 || missing+=("timeout")

  # 当前基础 Compose 使用不可变镜像策略门禁，并与 host overlay 统一要求 v2.24+。
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    DOCKER_COMPOSE="docker compose"
    DOCKER_COMPOSE_V2_VERSION="$(get_docker_compose_v2_version)"
  else
    missing+=("Docker Compose v2.24+")
  fi

  if [[ ${#missing[@]} -gt 0 ]]; then
    die "缺少必要命令: ${missing[*]}"
  fi

  require_docker_compose_v224 "当前不可变镜像策略门禁"

  log_success "环境检查通过 (使用 $DOCKER_COMPOSE)"
}

#######################################
# 查找运行中的 NewAPI 容器，输出容器名（找不到则输出空并返回 1）。
# 兼容自定义命名 / fork 镜像：容器名或镜像名包含 new-api 词元即可
#   （例如 new-api-master、ghcr.io/xxx/new-api-my:latest）。
# 注意不会误伤本项目自身容器 newapi-tools（无连字符，不含 new-api 子串）。
# 可用环境变量 NEWAPI_CONTAINER=<容器名或ID> 强制指定，跳过自动检测。
#######################################
find_newapi_container() {
  # 1) 环境变量显式指定，优先级最高
  if [[ -n "${NEWAPI_CONTAINER:-}" ]]; then
    echo "$NEWAPI_CONTAINER"
    return 0
  fi

  local found=""

  # 2) 按容器名匹配：new-api / new-api-master / new-api-my ...
  found=$(docker ps --format '{{.Names}}' | awk 'tolower($0) ~ /(^|[-_])new-api([-_]|$)/ {print; exit}')
  [[ -n "$found" ]] && { echo "$found"; return 0; }

  # 3) 按 compose service 标签匹配
  found=$(docker ps --filter 'label=com.docker.compose.service=new-api' --format '{{.Names}}' | head -n 1)
  [[ -n "$found" ]] && { echo "$found"; return 0; }

  # 4) 按镜像名匹配：允许 fork 后缀（new-api-my:latest 也能命中）
  found=$(docker ps --format '{{.Names}}\t{{.Image}}' | awk -F'\t' 'tolower($2) ~ /(^|\/)new-api([-_:]|$)/ {print $1; exit}')
  [[ -n "$found" ]] && { echo "$found"; return 0; }

  return 1
}

#######################################
# 将 NewAPI 的 Redis 状态同步到工具 .env。
# 只有容器中明确存在空的 REDIS_CONN_STRING= 才写 true；变量缺失、
# 容器不可读或值非空都写 false，避免直接 DB 写入绕过 NewAPI 缓存。
#######################################
sync_newapi_mutation_safety_config() {
  local project_dir="$1"
  local env_file="${project_dir}/.env"
  [[ -f "$env_file" ]] || return 0

  local container="" env_lines="" redis_entry="" redis_value="" redis_disabled=false
  container=$(grep -E '^NEWAPI_CONTAINER=' "$env_file" 2>/dev/null | head -n1 | cut -d'=' -f2- || true)
  [[ -z "$container" ]] && container=$(find_newapi_container || true)

  if [[ -z "$container" ]]; then
    log_warn "无法定位 NewAPI 容器，Redis 状态未知；NEWAPI_REDIS_DISABLED=false"
    log_warn "用户、Token、分组和 IP 记录等直接写库操作将被阻止，请改用 NewAPI 管理 API"
  elif ! env_lines="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$container" 2>/dev/null)"; then
    log_warn "无法读取 NewAPI 容器环境变量，Redis 状态未知；NEWAPI_REDIS_DISABLED=false"
    log_warn "用户、Token、分组和 IP 记录等直接写库操作将被阻止，请改用 NewAPI 管理 API"
  else
    redis_entry="$(printf '%s\n' "$env_lines" | awk -F= '$1=="REDIS_CONN_STRING"{print; exit}')"
    if [[ -z "$redis_entry" ]]; then
      log_warn "NewAPI 未显式声明 REDIS_CONN_STRING，Redis 状态未知；NEWAPI_REDIS_DISABLED=false"
      log_warn "用户、Token、分组和 IP 记录等直接写库操作将被阻止，请改用 NewAPI 管理 API"
    else
      redis_value="${redis_entry#*=}"
      if [[ -z "$redis_value" ]]; then
        redis_disabled=true
        log_success "NewAPI 明确配置 REDIS_CONN_STRING=（空），允许受保护的直接数据库写操作"
      else
        log_warn "检测到 NewAPI 已配置 Redis；NEWAPI_REDIS_DISABLED=false"
        log_warn "为避免缓存中的权限延迟失效，相关直接写库操作将被阻止，请改用 NewAPI 管理 API"
      fi
    fi
  fi

  local content has_hard_delete=false has_batch_delete=false
  grep -q '^ALLOW_UNSAFE_HARD_DELETE=' "$env_file" 2>/dev/null && has_hard_delete=true
  grep -q '^ALLOW_UNSAFE_BATCH_DELETE=' "$env_file" 2>/dev/null && has_batch_delete=true
  content="$(
    awk 'index($0, "NEWAPI_REDIS_DISABLED=") == 1 { next } { print }' "$env_file"
    printf 'NEWAPI_REDIS_DISABLED=%s\n' "$redis_disabled"
    [[ "$has_hard_delete" == "true" ]] || printf 'ALLOW_UNSAFE_HARD_DELETE=false\n'
    [[ "$has_batch_delete" == "true" ]] || printf 'ALLOW_UNSAFE_BATCH_DELETE=false\n'
  )"
  atomic_write_install_dotenv "$env_file" "$content" ||
    die "无法原子持久化 NewAPI 写操作安全设置"

  if [[ "$has_hard_delete" != "true" ]]; then
    log_info "已加入安全默认值 ALLOW_UNSAFE_HARD_DELETE=false"
  fi

  if [[ "$has_batch_delete" != "true" ]]; then
    log_info "已加入安全默认值 ALLOW_UNSAFE_BATCH_DELETE=false"
  fi

}

#######################################
# 检测 NewAPI 容器和目录
#######################################
detect_newapi_location() {
  log_info "正在检测 NewAPI 安装位置..."

  # 查找 new-api 容器（兼容自定义命名 / fork 镜像，详见 find_newapi_container）
  local container_id
  container_id=$(find_newapi_container || true)

  if [[ -z "$container_id" ]]; then
    log_warn "未找到运行中的 NewAPI 容器"
    log_info "将安装到当前目录: $(pwd)"
    INSTALL_DIR="$(pwd)"
    return 0
  fi

  log_success "找到 NewAPI 容器: $container_id"

  # 尝试获取 compose 文件路径
  local compose_file
  compose_file=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project.config_files" }}' "$container_id" 2>/dev/null || true)

  if [[ -n "$compose_file" ]]; then
    # 提取第一个配置文件路径
    compose_file=$(echo "$compose_file" | sed 's/,.*$//')
    if [[ -f "$compose_file" ]]; then
      INSTALL_DIR=$(dirname "$compose_file")
      log_success "检测到 NewAPI 目录: $INSTALL_DIR"
      return 0
    fi
  fi

  # 尝试从 working_dir 获取
  local working_dir
  working_dir=$(docker inspect -f '{{ index .Config.Labels "com.docker.compose.project.working_dir" }}' "$container_id" 2>/dev/null || true)

  if [[ -n "$working_dir" && -d "$working_dir" ]]; then
    INSTALL_DIR="$working_dir"
    log_success "检测到 NewAPI 目录: $INSTALL_DIR"
    return 0
  fi

  # 默认使用当前目录
  log_warn "无法自动检测 NewAPI 目录位置"
  log_info "将安装到当前目录: $(pwd)"
  INSTALL_DIR="$(pwd)"
}

#######################################
# 显示初始安装环境检测
#######################################
show_initial_env_detection() {
  echo ""
  echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
  echo -e "${BLUE}                    环境检测结果${NC}"
  echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
  echo ""

  # 检测 NewAPI 容器信息（兼容自定义命名 / fork 镜像，详见 find_newapi_container）
  local newapi_container=""
  newapi_container=$(find_newapi_container || true)

  if [[ -n "$newapi_container" ]]; then
    echo -e "  ${GREEN}✓${NC} NewAPI 容器: ${GREEN}${newapi_container}${NC}"

    # 检测网络
    local networks network_mode
    networks=$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{println $k}}{{end}}' "$newapi_container" 2>/dev/null | head -n 1)
    network_mode=$(docker inspect -f '{{.HostConfig.NetworkMode}}' "$newapi_container" 2>/dev/null || true)

    if [[ "$network_mode" == "host" ]]; then
      echo -e "  ${YELLOW}!${NC} 网络模式: ${YELLOW}Host 模式${NC}"
      echo -e "    ${YELLOW}→ NewAPI 与宿主机共享网络栈${NC}"
      echo -e "    ${YELLOW}→ newapi-tools 将通过 host.docker.internal 访问数据库${NC}"
      echo -e "    ${YELLOW}→ 启动时会附加 docker-compose.host.yml overlay${NC}"
    elif [[ "$networks" == "bridge" ]]; then
      echo -e "  ${YELLOW}!${NC} 网络模式: ${YELLOW}Bridge 模式${NC}"
      echo -e "    ${YELLOW}→ NewAPI 使用默认 bridge 网络${NC}"
      echo -e "    ${YELLOW}→ 将使用 IPv4 地址连接数据库${NC}"
    else
      echo -e "  ${GREEN}✓${NC} 网络模式: ${GREEN}正常模式${NC}"
      echo -e "    → 网络名称: ${GREEN}${networks}${NC}"
    fi

    # 检测数据库类型
    local sql_dsn
    sql_dsn=$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$newapi_container" 2>/dev/null | awk -F= '$1=="SQL_DSN"{print $2; exit}')

    if [[ -n "$sql_dsn" ]]; then
      if [[ "$sql_dsn" =~ ^postgres ]]; then
        echo -e "  ${GREEN}✓${NC} 数据库类型: ${GREEN}PostgreSQL${NC}"
      elif [[ "$sql_dsn" =~ ^mysql ]]; then
        echo -e "  ${GREEN}✓${NC} 数据库类型: ${GREEN}MySQL${NC}"
      fi
    fi
  else
    echo -e "  ${RED}✗${NC} NewAPI 容器: ${RED}未找到${NC}"
    echo -e "    ${YELLOW}请确保 NewAPI 容器正在运行${NC}"
  fi

  echo ""
  echo -e "  安装目录: ${YELLOW}${INSTALL_DIR}/${PROJECT_NAME}${NC}"
  echo ""
  echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
  echo ""

  if [[ -z "$newapi_container" ]]; then
    echo -e "${YELLOW}警告: 未检测到 NewAPI 容器，部署可能会失败${NC}"
    echo ""
    read -r -p "是否继续安装? [y/N]: " confirm
    if [[ ! "$confirm" =~ ^[yY]$ ]]; then
      log_info "已取消安装"
      exit 0
    fi
  else
    read -r -p "按回车键开始安装，或输入 n 取消: " confirm
    if [[ "$confirm" =~ ^[nN]$ ]]; then
      log_info "已取消安装"
      exit 0
    fi
  fi
}

#######################################
# 检测是否已安装服务
#######################################
check_existing_installation() {
  local target_dir="${INSTALL_DIR}/${PROJECT_NAME}" target_abs transaction_root active_record
  local committed_cleanup_pending=false recovery_content recovery_state

  target_abs="$(realpath -m -s -- "$target_dir" 2>/dev/null)" ||
    die "无法解析安装目标路径"
  [[ "$target_abs" == "$(realpath -m -- "$target_dir" 2>/dev/null)" ]] ||
    die "安装目标或其父路径穿越 symlink"
  transaction_root="$(dirname -- "$target_abs")/.$(basename -- "$target_abs").toolstore-transactions"
  active_record="${transaction_root}/active.env"
  if [[ -e "$active_record" || -L "$active_record" ]]; then
    [[ -f "$active_record" && ! -L "$active_record" ]] ||
      die "外置 Tool Store 活动事务记录不安全"
    PROJECT_DIR="$target_dir"
    load_install_toolstore_transaction_library "$target_dir" ||
      die "无法加载 mode-600 外置 Tool Store 恢复库"
    recover_active_install_toolstore_transaction "$target_dir" ||
      die "项目树缺失或不完整时，外置 Tool Store 事务无法安全恢复"
    if toolstore_txn_has_active "$target_dir"; then
      recovery_content="$(toolstore_txn_load_record "$target_dir")" ||
        die "正式候选恢复后无法读取保留的 Tool Store 提交证据"
      recovery_state="$(toolstore_txn_env_value "$recovery_content" STATE)"
      [[ "$recovery_state" == "committed" ]] ||
        die "Tool Store 恢复返回成功但仍保留非 committed 活动事务"
      committed_cleanup_pending=true
    fi
  fi
  cleanup_install_clone_staging "$target_dir" ||
    die "无法安全清理上次中断的临时 clone 目录"

  # 检查项目目录是否存在
  if [[ ! -d "$target_dir" ]]; then
    # 显示初始安装环境检测
    show_initial_env_detection
    log_info "开始全新安装..."
    return 0
  fi

  if [[ ! -f "${target_dir}/.env" || -L "${target_dir}/.env" ||
    ! -f "${target_dir}/docker-compose.yml" || -L "${target_dir}/docker-compose.yml" ]]; then
    if install_interrupted_checkout_is_trusted "$target_dir"; then
      log_warn "检测到可信 origin 的中断 clone/checkout；将在同级隔离后重新原子发布"
      return 0
    fi
    die "安装目标是未知或不完整的目录，且没有可恢复的外置事务；拒绝当作现有安装或覆盖"
  fi

  # 设置 PROJECT_DIR 供后续函数使用
  PROJECT_DIR="$target_dir"

  if [[ -f "${target_dir}/scripts/toolstore_transaction.sh" ]]; then
    load_install_toolstore_transaction_library "$target_dir" ||
      die "Tool Store 事务库不安全或不可读取"
    if [[ "$committed_cleanup_pending" != "true" ]] &&
      toolstore_txn_has_active "$target_dir"; then
      recover_active_install_toolstore_transaction "$target_dir" ||
        die "未完成的 Tool Store 事务无法安全恢复；拒绝进入管理菜单"
      if toolstore_txn_has_active "$target_dir"; then
        recovery_content="$(toolstore_txn_load_record "$target_dir")" ||
          die "正式候选恢复后无法读取保留的 Tool Store 提交证据"
        recovery_state="$(toolstore_txn_env_value "$recovery_content" STATE)"
        [[ "$recovery_state" == "committed" ]] ||
          die "Tool Store 恢复返回成功但仍保留非 committed 活动事务"
        committed_cleanup_pending=true
      fi
    fi
  fi

  # Recover an interrupted update before exposing restart/start actions in the
  # management menu. The snapshot remains active until a later update commits.
  if [[ "$committed_cleanup_pending" == "true" ]]; then
    log_warn "Tool Store 已提交且正式候选健康；旧快照仅为待清理证据，本次不进入变更菜单，也不按其恢复旧配置、旧镜像或旧 schema"
    log_success "当前候选服务可用；下次运行 install.sh 将幂等重试快照与提交证据清理"
    exit 0
  fi

  local rollback_snapshot="${target_dir}/.env.rollback"
  if [[ -e "$rollback_snapshot" || -L "$rollback_snapshot" ]]; then
    capture_install_rollback_env "${target_dir}/.env"
    recover_preexisting_install_rollback_snapshot "${target_dir}/.env" "$target_dir" ||
      die "无法在进入管理菜单前恢复上次中断的安装事务"
    commit_install_rollback_transaction "${target_dir}/.env" ||
      die "旧部署已恢复，但无法在进入管理菜单前提交并清理回滚快照"
  fi

  log_info "检测到已安装的服务: $target_dir"

  # 检查服务状态
  local service_status="未知"
  local container_status
  container_status=$(docker ps --format '{{.Names}}' | grep -E '^newapi-tools$' 2>/dev/null || true)

  if [[ -n "$container_status" ]]; then
    service_status="${GREEN}运行中${NC}"
  else
    container_status=$(docker ps -a --format '{{.Names}}' | grep -E '^newapi-tools$' 2>/dev/null || true)
    if [[ -n "$container_status" ]]; then
      service_status="${YELLOW}已停止${NC}"
    else
      service_status="${RED}未运行${NC}"
    fi
  fi

  # 显示交互式菜单
  show_management_menu "$target_dir" "$service_status"
}

#######################################
# 检测环境详情
#######################################
detect_env_details() {
  local target_dir="$1"

  # 读取 .env 文件获取配置信息
  local env_file="${target_dir}/.env"

  if [[ -f "$env_file" ]]; then
    ENV_NEWAPI_NETWORK="$(env_file_value "$env_file" 'NEWAPI_NETWORK')"; ENV_NEWAPI_NETWORK="${ENV_NEWAPI_NETWORK:-未知}"
    ENV_DB_ENGINE="$(env_file_value "$env_file" 'DB_ENGINE')"; ENV_DB_ENGINE="${ENV_DB_ENGINE:-未知}"
    ENV_DB_DNS="$(env_file_value "$env_file" 'DB_DNS')"; ENV_DB_DNS="${ENV_DB_DNS:-未知}"
    ENV_DB_PORT="$(env_file_value "$env_file" 'DB_PORT')"; ENV_DB_PORT="${ENV_DB_PORT:-未知}"
    ENV_DB_NAME="$(env_file_value "$env_file" 'DB_NAME')"; ENV_DB_NAME="${ENV_DB_NAME:-未知}"
    ENV_FRONTEND_PORT="$(env_file_value "$env_file" 'FRONTEND_PORT')"; ENV_FRONTEND_PORT="${ENV_FRONTEND_PORT:-1145}"
    ENV_ADMIN_PASSWORD="$(env_file_value "$env_file" 'ADMIN_PASSWORD')"
    # SERVER_HOST 读取 .env 中显式声明的最后一行（处理用户多次写入的情况）；缺失视为默认 127.0.0.1
    local _sh_raw
    _sh_raw=$(grep -E '^SERVER_HOST=' "$env_file" 2>/dev/null | tail -n1 | cut -d'=' -f2- || true)
    _sh_raw="${_sh_raw//[\"\'\ $'\r'$'\n'$'\t']/}"
    ENV_SERVER_HOST="${_sh_raw:-127.0.0.1}"
    # FRONTEND_BIND 控制 1145 端口对外暴露（0.0.0.0 公开 / 127.0.0.1 仅本机）
    local _fb_raw
    _fb_raw=$(grep -E '^FRONTEND_BIND=' "$env_file" 2>/dev/null | tail -n1 | cut -d'=' -f2- || true)
    _fb_raw="${_fb_raw//[\"\'\ $'\r'$'\n'$'\t']/}"
    ENV_FRONTEND_BIND="${_fb_raw:-127.0.0.1}"
  else
    ENV_NEWAPI_NETWORK="未配置"
    ENV_DB_ENGINE="未配置"
    ENV_DB_DNS="未配置"
    ENV_DB_PORT="未配置"
    ENV_DB_NAME="未配置"
    ENV_SERVER_HOST="未配置"
    ENV_FRONTEND_BIND="127.0.0.1"
    ENV_FRONTEND_PORT="1145"
    ENV_ADMIN_PASSWORD=""
  fi

  # 判断网络模式
  if [[ "$ENV_NEWAPI_NETWORK" == "newapi-tools-network" ]]; then
    NETWORK_MODE="Bridge 模式"
    NETWORK_MODE_COLOR="${YELLOW}Bridge 模式${NC} (使用 IPv4 地址连接数据库)"
  elif [[ "$ENV_NEWAPI_NETWORK" == "未配置" || "$ENV_NEWAPI_NETWORK" == "未知" ]]; then
    NETWORK_MODE="未配置"
    NETWORK_MODE_COLOR="${RED}未配置${NC}"
  else
    NETWORK_MODE="正常模式"
    NETWORK_MODE_COLOR="${GREEN}正常模式${NC} (使用 Docker 网络服务发现)"
  fi

  # 判断后端绑定模式（影响 8000 端口的暴露范围）
  if [[ "$ENV_SERVER_HOST" == "0.0.0.0" || "$ENV_SERVER_HOST" == "::" ]]; then
    BIND_MODE="不安全"
    BIND_MODE_COLOR="${RED}${ENV_SERVER_HOST}${NC} (8000 端口对外暴露，不推荐)"
  elif [[ "$ENV_SERVER_HOST" == "127.0.0.1" || "$ENV_SERVER_HOST" == "localhost" || "$ENV_SERVER_HOST" == "::1" ]]; then
    BIND_MODE="安全"
    BIND_MODE_COLOR="${GREEN}${ENV_SERVER_HOST}${NC} (仅容器内 Nginx 反代访问)"
  else
    BIND_MODE="自定义"
    BIND_MODE_COLOR="${YELLOW}${ENV_SERVER_HOST}${NC}"
  fi

  # 判断前端端口暴露范围（FRONTEND_BIND 控制 1145 是否对外）
  if [[ "$ENV_FRONTEND_BIND" == "127.0.0.1" || "$ENV_FRONTEND_BIND" == "localhost" || "$ENV_FRONTEND_BIND" == "::1" ]]; then
    FRONTEND_BIND_MODE="仅本机"
    FRONTEND_BIND_COLOR="${GREEN}${ENV_FRONTEND_BIND}:${ENV_FRONTEND_PORT}${NC} (仅本机访问，需配 nginx 反代)"
  elif [[ "$ENV_FRONTEND_BIND" == "0.0.0.0" || "$ENV_FRONTEND_BIND" == "::" || "$ENV_FRONTEND_BIND" == "未配置" ]]; then
    FRONTEND_BIND_MODE="公网"
    FRONTEND_BIND_COLOR="${YELLOW}0.0.0.0:${ENV_FRONTEND_PORT}${NC} (任意 IP 可达)"
  else
    FRONTEND_BIND_MODE="自定义"
    FRONTEND_BIND_COLOR="${YELLOW}${ENV_FRONTEND_BIND}:${ENV_FRONTEND_PORT}${NC}"
  fi
}

show_frontend_access() {
  local bind="${1:-127.0.0.1}" port="${2:-1145}" server_ip="${3:-localhost}" label="${4:-访问地址}"
  if [[ "$bind" == "127.0.0.1" || "$bind" == "localhost" || "$bind" == "::1" ]]; then
    echo -e "${label}: ${BLUE}http://localhost:${port}${NC} ${YELLOW}(仅服务器本机)${NC}"
    echo -e "  远程访问: 请使用 nginx/Caddy 配置的 HTTPS 反向代理域名"
  else
    echo -e "${label}: ${BLUE}http://${server_ip}:${port}${NC}"
  fi
}

#######################################
# 显示管理菜单
#######################################
show_management_menu() {
  local target_dir="$1"
  local service_status="$2"

  # 检测环境详情
  detect_env_details "$target_dir"

  # 获取服务器 IP
  local server_ip
  server_ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || server_ip="localhost"

  while true; do
    echo ""
    echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}              NewAPI Middleware Tool 管理面板${NC}"
    echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
    echo ""
    echo -e "${GREEN}【环境检测】${NC}"
    echo -e "  项目目录: ${YELLOW}$target_dir${NC}"
    echo -e "  服务状态: $service_status"
    show_frontend_access "$ENV_FRONTEND_BIND" "$ENV_FRONTEND_PORT" "$server_ip" "  访问地址"
    echo ""
    echo -e "${GREEN}【登录凭证】${NC}"
    if [[ -n "$ENV_ADMIN_PASSWORD" ]]; then
      echo -e "  登录密码: ${YELLOW}${ENV_ADMIN_PASSWORD}${NC}"
    else
      echo -e "  登录密码: ${RED}未在 .env 中找到${NC}"
    fi
    echo ""
    echo -e "${GREEN}【网络模式】${NC}"
    echo -e "  运行模式: $NETWORK_MODE_COLOR"
    echo -e "  网络名称: ${YELLOW}${ENV_NEWAPI_NETWORK}${NC}"
    echo ""
    echo -e "${GREEN}【数据库配置】${NC}"
    echo -e "  数据库类型: ${YELLOW}${ENV_DB_ENGINE}${NC}"
    echo -e "  数据库地址: ${YELLOW}${ENV_DB_DNS}:${ENV_DB_PORT}${NC}"
    echo -e "  数据库名称: ${YELLOW}${ENV_DB_NAME}${NC}"
    echo ""
    echo -e "${GREEN}【后端绑定】${NC}"
    echo -e "  SERVER_HOST: $BIND_MODE_COLOR"
    echo -e "  对外端口:    $FRONTEND_BIND_COLOR"
    echo ""
    echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
    echo -e "${GREEN}【操作菜单】${NC}"
    echo ""
    echo "  1) 更新服务   (拉取最新代码和镜像，重启容器)"
    echo "  2) 查看状态   (显示容器运行状态和资源占用)"
    echo "  3) 查看日志   (实时查看容器日志，Ctrl+C 退出)"
    echo "  4) 重启服务   (重启所有容器，不更新镜像)"
    echo ""
    echo "  5) 停止服务   (停止所有容器，保留数据)"
    echo "  6) 启动服务   (启动已停止的容器)"
    echo ""
    echo "  7) 重新配置   (备份当前配置，重新运行部署向导)"
    echo "  8) 安全重装   (在线备份 Tool Store，保留回滚证据后重建项目)"
    echo "  9) 完全卸载   (删除所有内容，包括数据，需确认)"
    echo " 10) 完全重装   (完全卸载后重新安装，需确认)"
    echo ""
    if [[ "$BIND_MODE" == "不安全" || "$FRONTEND_BIND_MODE" == "公网" ]]; then
      echo -e " 11) ${GREEN}安全设置${NC}     (切换 SERVER_HOST / 切换前端端口暴露范围)"
    else
      echo " 11) 安全设置     (切换 SERVER_HOST / 切换前端端口暴露范围)"
    fi
    echo ""
    echo "  0) 退出"
    echo ""
    read -r -p "请选择操作 [0-11]: " choice

    case "$choice" in
      1)
        do_update_interactive "$target_dir"
        exit 0
        ;;
      2)
        do_status_interactive "$target_dir"
        echo ""
        read -r -p "按回车键继续..."
        ;;
      3)
        do_logs_interactive "$target_dir"
        ;;
      4)
        do_restart_interactive "$target_dir"
        echo ""
        read -r -p "按回车键继续..."
        service_status="${GREEN}运行中${NC}"
        ;;
      5)
        do_stop_interactive "$target_dir"
        echo ""
        read -r -p "按回车键继续..."
        service_status="${YELLOW}已停止${NC}"
        ;;
      6)
        do_start_interactive "$target_dir"
        echo ""
        read -r -p "按回车键继续..."
        service_status="${GREEN}运行中${NC}"
        ;;
      7)
        do_reconfigure_interactive "$target_dir"
        exit 0
        ;;
      8)
        echo ""
        echo -e "${YELLOW}安全重装将先创建并二次验证 Tool Store Online Backup，停止旧服务后才暂存 data 并重建项目。${NC}"
        echo "任何备份、路径、权限、恢复或健康检查失败都会停止操作并保留旧部署/回滚证据。"
        echo ""
        read -r -p "确认执行安全重装? [y/N]: " confirm
        if [[ "$confirm" =~ ^[yY]$ ]]; then
          REINSTALL=true
          perform_cleanup "$target_dir"
          return 0
        fi
        ;;
      9)
        do_purge_interactive "$target_dir"
        exit 0
        ;;
      10)
        do_full_reinstall_interactive "$target_dir"
        ;;
      11)
        do_security_settings_interactive "$target_dir"
        echo ""
        read -r -p "按回车键继续..."
        # 重新读取以刷新菜单上的状态
        detect_env_details "$target_dir"
        ;;
      0|"")
        log_info "退出"
        exit 0
        ;;
      *)
        log_warn "无效选择，请重新输入"
        ;;
    esac
  done
}

#######################################
# 安全设置子菜单
# 提供 SERVER_HOST / FRONTEND_BIND 两个开关
#######################################
do_security_settings_interactive() {
  local project_dir="$1"
  while true; do
    detect_env_details "$project_dir"
    echo ""
    echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
    echo -e "${BLUE}  安全设置${NC}"
    echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
    echo ""
    echo -e "  当前 SERVER_HOST     : $BIND_MODE_COLOR"
    echo -e "  当前对外端口绑定     : $FRONTEND_BIND_COLOR"
    echo ""
    echo "  1) 切换 SERVER_HOST（Go 后端 8000 端口绑定地址）"
    echo "  2) 切换前端端口绑定（${ENV_FRONTEND_PORT} 端口是否对公网开放）"
    echo ""
    echo "  0) 返回上级菜单"
    echo ""
    read -r -p "请选择 [0-2]: " choice
    case "$choice" in
      1) do_toggle_bind_mode_interactive "$project_dir" ;;
      2) do_toggle_frontend_bind_interactive "$project_dir" ;;
      0|"") return 0 ;;
      *) log_warn "无效选择" ;;
    esac
  done
}

#######################################
# 切换前端端口绑定（FRONTEND_BIND）
#######################################
do_toggle_frontend_bind_interactive() {
  local project_dir="$1"
  local env_file="${project_dir}/.env"
  [[ -f "$env_file" ]] || { log_error "未找到 .env"; return 1; }
  cd "$project_dir"

  local current
  current=$(grep -E '^FRONTEND_BIND=' "$env_file" 2>/dev/null | tail -n1 | cut -d'=' -f2- || true)
  current="${current//[\"\'\ $'\r'$'\n'$'\t']/}"
  current="${current:-127.0.0.1}"

  echo ""
  echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
  echo -e "${BLUE}  切换前端端口（${ENV_FRONTEND_PORT}）暴露范围${NC}"
  echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
  echo ""
  echo -e "  当前: ${YELLOW}${current}:${ENV_FRONTEND_PORT}${NC}"
  echo ""
  echo -e "  ${YELLOW}1) 公网可达${NC}    FRONTEND_BIND=0.0.0.0"
  echo -e "                  必须置于 HTTPS 反向代理、访问控制和防火墙之后"
  echo -e "                  不要在公网使用明文 http://server-ip:${ENV_FRONTEND_PORT}"
  echo ""
  echo -e "  ${GREEN}2) 仅本机${NC}      FRONTEND_BIND=127.0.0.1"
  echo -e "                  外部直连不通，需配宿主机 nginx 反代到 https://your-domain"
  echo -e "                  ${GREEN}推荐${NC}：HTTPS、域名、隔离公网攻击面"
  echo ""
  echo "  0) 取消"
  echo ""
  read -r -p "请选择 [0-2]: " choice
  local target=""
  case "$choice" in
    1)
      echo ""
      log_warn "该操作会把 ${ENV_FRONTEND_PORT} 端口绑定到所有网卡"
      log_warn "只有在 HTTPS 反向代理、访问控制和防火墙已经就绪时才应继续"
      read -r -p "确认切换到公网监听? [y/N]: " confirm
      [[ "$confirm" =~ ^[yY]$ ]] || { log_info "已取消"; return 0; }
      target="0.0.0.0"
      ;;
    2)
      target="127.0.0.1"
      log_info "将切换为仅本机监听；请使用宿主机 HTTPS 反向代理访问"
      log_info "示例 nginx 配置:"
      cat <<NGINX
   server {
     listen 443 ssl http2;
     server_name your-domain.com;
     ssl_certificate     /path/to/fullchain.pem;
     ssl_certificate_key /path/to/privkey.pem;
     location / {
       proxy_pass http://127.0.0.1:${ENV_FRONTEND_PORT};
       proxy_set_header Host \$host;
       proxy_set_header X-Real-IP \$remote_addr;
       # 覆盖客户端自带的 X-Forwarded-For，防止伪造限流身份。
       proxy_set_header X-Forwarded-For \$remote_addr;
       proxy_set_header X-Forwarded-Proto \$scheme;
     }
   }
NGINX
      log_warn "若内层 Nginx 看到的外层代理来源不是 loopback，请把该精确 IP（/32 或 /128）追加到 TRUSTED_PROXY_CIDRS"
      echo ""
      ;;
    0|"") log_info "已取消"; return 0 ;;
    *) log_warn "无效选择"; return 1 ;;
  esac

  if [[ "$current" == "$target" ]]; then
    log_info "当前已是 ${target}，无需切换"
    return 0
  fi

  comment_and_replace_install_env_key "$env_file" 'FRONTEND_BIND' "$target" \
    '# Disabled by install.sh: ' || {
    log_error "无法原子持久化 FRONTEND_BIND 设置"
    return 1
  }
  log_success "已写入 FRONTEND_BIND=${target}"

  setup_compose_files "$project_dir"
  log_info "重启服务以应用新绑定..."
  $DOCKER_COMPOSE down 2>&1 | tail -5
  $DOCKER_COMPOSE up -d 2>&1 | tail -5
  restore_runtime_network_connections "$project_dir"
  log_success "服务已重启"
}

#######################################
# 切换 Go 后端绑定地址（安全 ⇄ 暴露）
# 用法：菜单选项 11
#######################################
do_toggle_bind_mode_interactive() {
  local project_dir="$1"
  local env_file="${project_dir}/.env"

  if [[ ! -f "$env_file" ]]; then
    log_error "未找到 .env 文件: $env_file"
    return 1
  fi

  cd "$project_dir"

  # 读取当前值（与 detect_env_details 一致的解析规则）
  local current
  current=$(grep -E '^SERVER_HOST=' "$env_file" 2>/dev/null | tail -n1 | cut -d'=' -f2- || true)
  current="${current//[\"\'\ $'\r'$'\n'$'\t']/}"
  current="${current:-127.0.0.1}"

  echo ""
  echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
  echo -e "${BLUE}  Go 后端绑定模式切换${NC}"
  echo -e "${BLUE}════════════════════════════════════════════════════════════${NC}"
  echo ""
  echo -e "  当前: ${YELLOW}SERVER_HOST=${current}${NC}"
  echo ""
  echo -e "  ${GREEN}1) 安全模式${NC}    SERVER_HOST=127.0.0.1"
  echo -e "                  Go 后端只监听容器内 loopback，由 Nginx 反代到 ${ENV_FRONTEND_PORT} 端口对外。"
  echo -e "                  这是${GREEN}推荐${NC}配置。"
  echo ""
  echo -e "  ${RED}2) 暴露模式${NC}    SERVER_HOST=0.0.0.0"
  echo -e "                  Go 后端 8000 端口监听容器所有接口。"
  echo -e "                  ${RED}host 网络模式下会直接暴露到宿主机外网，有安全风险。${NC}"
  echo -e "                  仅在调试或自定义反代时使用。"
  echo ""
  echo "  0) 取消"
  echo ""
  read -r -p "请选择 [0-2]: " choice

  local target=""
  case "$choice" in
    1) target="127.0.0.1" ;;
    2)
      echo ""
      log_warn "你即将把 Go 后端 8000 端口暴露到容器虚拟网卡所有接口"
      log_warn "请确认你了解此操作的安全影响"
      read -r -p "继续? [y/N]: " confirm
      [[ "$confirm" =~ ^[yY]$ ]] || { log_info "已取消"; return 0; }
      target="0.0.0.0"
      ;;
    0|"") log_info "已取消"; return 0 ;;
    *) log_warn "无效选择"; return 1 ;;
  esac

  if [[ "$current" == "$target" ]]; then
    log_info "当前已是 ${target}，无需切换"
    return 0
  fi

  # 注释掉所有旧的 SERVER_HOST 行（保留追溯），追加新值到末尾
  comment_and_replace_install_env_key "$env_file" 'SERVER_HOST' "$target" \
    '# Disabled by install.sh: ' || {
    log_error "无法原子持久化 SERVER_HOST 设置"
    return 1
  }
  log_success "已写入 SERVER_HOST=${target}"

  # 重启容器使配置生效（环境变量只在容器启动时读取）
  setup_compose_files "$project_dir"
  log_info "重启服务以应用新绑定..."
  $DOCKER_COMPOSE down 2>&1 | tail -5
  $DOCKER_COMPOSE up -d 2>&1 | tail -5
  restore_runtime_network_connections "$project_dir"
  log_success "服务已用新绑定重启"
}

#######################################
# 交互式更新
#######################################
do_update_interactive() {
  local project_dir="$1"
  cd "$project_dir"

  # Capture the old dotenv and exact Compose bundle before checkout replaces
  # tracked manifests. A failed candidate must restart the old image with the
  # manifests it was actually deployed from.
  PROJECT_DIR="$project_dir"
  prepare_install_rollback_transaction "${project_dir}/.env" "$project_dir"

  # 更新代码
  if [[ -d ".git" ]]; then
    log_info "同步代码到固定版本 ${INSTALL_REF}..."
    checkout_install_ref
  fi

  # 下载 GeoIP 数据库
  download_geoip_database

  # 迁移旧版 .env（补充 Go 版本所需字段）
  migrate_env_file "$project_dir"

  # 按当前 NewAPI 容器 Redis 配置同步直接写库安全开关
  sync_newapi_mutation_safety_config "$project_dir"

  # 安全检查：SERVER_HOST 是否绑定到不安全的 0.0.0.0
  check_server_host_security "$project_dir"

  # 根据 .env 自动选择 compose 文件（host 模式叠加 overlay）
  setup_compose_files "$project_dir"

  # 拉取最新镜像并重启
  log_info "拉取最新镜像..."
  if ! run_install_compose "${project_dir}/.env" "$project_dir" "$NEWAPI_TOOLS_IMAGE" \
    pull --include-deps newapi-tools; then
    if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      restore_install_previous_configuration "${project_dir}/.env" "$project_dir" ||
        log_error "高危：候选拉取失败后无法恢复完整旧安装配置"
    fi
    die "拉取 newapi-tools 及其策略门禁/Redis 依赖镜像失败；当前运行中的服务保持不变"
  fi
  # Resolve the just-pulled tag to an immutable RepoDigest and validate the
  # derived image revision before stopping the currently running service.
  if ! pin_install_image_after_pull "${project_dir}/.env"; then
    if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      restore_install_previous_configuration "${project_dir}/.env" "$project_dir" ||
        log_error "高危：候选镜像验证失败后无法恢复完整旧安装配置"
    fi
    die "候选镜像 digest 或源码版本验证失败；现有服务保持不变"
  fi

  restart_install_services_transactionally "${project_dir}/.env" "$project_dir"

  log_success "更新完成!"
  echo ""
  $DOCKER_COMPOSE ps

  # 显示访问地址
  local frontend_port
  frontend_port=$(grep -E '^FRONTEND_PORT=' .env 2>/dev/null | cut -d'=' -f2 || echo "1145")
  local server_ip
  server_ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || server_ip="localhost"
  detect_env_details "$project_dir"
  echo ""
  show_frontend_access "$ENV_FRONTEND_BIND" "$frontend_port" "$server_ip" "访问地址"
}

#######################################
# 交互式查看状态
#######################################
do_status_interactive() {
  local project_dir="$1"
  cd "$project_dir"
  setup_compose_files "$project_dir"

  echo ""
  echo -e "${BLUE}--- 容器状态 ---${NC}"
  $DOCKER_COMPOSE ps

  echo ""
  echo -e "${BLUE}--- 资源使用 ---${NC}"
  docker stats --no-stream --format "table {{.Name}}\t{{.CPUPerc}}\t{{.MemUsage}}\t{{.NetIO}}" $($DOCKER_COMPOSE ps -q 2>/dev/null) 2>/dev/null || echo "无法获取资源使用情况"

  echo ""
  echo -e "${BLUE}--- 访问信息 ---${NC}"
  local frontend_port
  frontend_port=$(grep -E '^FRONTEND_PORT=' .env 2>/dev/null | cut -d'=' -f2 || echo "1145")
  local server_ip
  server_ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || server_ip="localhost"
  detect_env_details "$project_dir"
  show_frontend_access "$ENV_FRONTEND_BIND" "$frontend_port" "$server_ip" "访问地址"

  echo ""
  echo -e "${BLUE}--- 配置信息 ---${NC}"
  echo "数据库类型: $(grep -E '^DB_ENGINE=' .env 2>/dev/null | cut -d'=' -f2 || echo '未知')"
  echo "数据库地址: $(grep -E '^DB_DNS=' .env 2>/dev/null | cut -d'=' -f2 || echo '未知')"
  echo "网络: $(grep -E '^NEWAPI_NETWORK=' .env 2>/dev/null | cut -d'=' -f2 || echo '未知')"
}

#######################################
# 交互式查看日志
#######################################
do_logs_interactive() {
  local project_dir="$1"
  cd "$project_dir"
  setup_compose_files "$project_dir"
  log_info "显示实时日志 (Ctrl+C 返回菜单)..."
  echo ""
  $DOCKER_COMPOSE logs -f --tail=100 || true
}

#######################################
# 交互式重启
#######################################
do_restart_interactive() {
  local project_dir="$1"
  cd "$project_dir"
  validate_install_credential_separation_file "${project_dir}/.env" ||
    die "凭据预检失败；服务未重启"
  setup_compose_files "$project_dir"
  log_info "重启服务..."
  $DOCKER_COMPOSE restart
  log_success "服务已重启"
  echo ""
  $DOCKER_COMPOSE ps
}

#######################################
# 交互式停止
#######################################
do_stop_interactive() {
  local project_dir="$1"
  cd "$project_dir"
  setup_compose_files "$project_dir"
  log_info "停止服务..."
  $DOCKER_COMPOSE stop
  log_success "服务已停止"
}

#######################################
# 交互式启动
#######################################
do_start_interactive() {
  local project_dir="$1"
  cd "$project_dir"
  validate_install_credential_separation_file "${project_dir}/.env" ||
    die "凭据预检失败；服务未启动"
  setup_compose_files "$project_dir"
  log_info "启动服务..."
  $DOCKER_COMPOSE start
  log_success "服务已启动"
  echo ""
  $DOCKER_COMPOSE ps
}

#######################################
# 交互式重新配置
#######################################
do_reconfigure_interactive() {
  local project_dir="$1"
  cd "$project_dir"
  log_info "重新配置服务..."

  validate_install_credential_separation_file "${project_dir}/.env" ||
    die "凭据预检失败；拒绝更新现有部署"

  # 备份旧配置
  if [[ -e ".env" || -L ".env" ]]; then
    [[ -f ".env" && ! -L ".env" ]] ||
      die "重新配置拒绝读取不安全的 .env"
    local env_backup=".env.backup.$(date +%Y%m%d_%H%M%S)"
    atomic_write_install_dotenv "$env_backup" "$(<.env)" ||
      die "无法原子持久化重新配置备份"
    log_info "已备份旧配置文件"
  fi

  # deploy.sh intentionally rejects a live container without an authoritative
  # old dotenv. Establish its immutable image/project rollback identity before
  # removing .env to enter the configuration wizard.
  capture_install_rollback_env "${project_dir}/.env"
  [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]] ||
    die "无法在重新配置前建立权威 .env 回滚快照"

  # 删除旧配置以触发重新配置
  durable_remove_install_file "${project_dir}/.env" ||
    die "无法持久删除旧 .env；重新配置未启动"

  # 运行部署脚本
  if [[ "$NEWAPI_TOOLS_IMAGE_DERIVED" == "true" ]]; then
    unset NEWAPI_TOOLS_IMAGE NEWAPI_TOOLS_IMAGE_DERIVED NEWAPI_TOOLS_EXPECTED_REVISION \
      NEWAPI_TOOLS_RELEASE_TAG
  fi
  exec ./deploy.sh
}

#######################################
# 交互式完全卸载
#######################################
do_purge_interactive() {
  local project_dir="$1"
  cd "$project_dir"

  echo ""
  echo -e "${RED}════════════════════════════════════════════════════════════${NC}"
  echo -e "${RED}  警告: 完全卸载${NC}"
  echo -e "${RED}════════════════════════════════════════════════════════════${NC}"
  echo ""
  echo -e "${YELLOW}将永久删除以下 newapi-tools 自身的数据：${NC}"
  echo "  • 容器: newapi-tools / newapi-tools-redis"
  echo "  • 镜像: ghcr.io/yujianwudi/new_api_tools:*"
  echo "  • Redis 缓存卷 (仪表盘 / 模型状态 / 等缓存)"
  echo "  • Docker 网络: newapi-tools-network (若存在)"
  echo "  • 配置文件 .env (含登录密码)"
  echo "  • 项目目录: ${project_dir}"
  echo ""
  echo -e "${GREEN}NewAPI 本身完全不受影响：${NC}"
  echo "  ✓ NewAPI 容器、数据库、Redis、用户充值/Token/日志 → 全部保留"
  echo "  ✓ 本项目仅以只读方式访问 NewAPI 数据库，从不写入"
  echo ""
  echo -e "${YELLOW}卸载后想再用，重新跑 install.sh 一键部署即可${NC}"
  echo ""
  read -r -p "输入 'DELETE' 确认完全卸载: " confirm

  if [[ "$confirm" != "DELETE" ]]; then
    log_info "已取消"
    return 0
  fi

  log_warn "正在完全卸载..."

  # Stop services successfully before deleting the only durable configuration
  # and rollback snapshot. A failed cleanup must never be reported as success.
  stop_install_project_for_removal "$project_dir" ||
    die "卸载服务清理失败；项目配置与回滚快照已保留"

  # 删除相关镜像
  log_info "删除相关镜像..."
  docker images --format '{{.Repository}}:{{.Tag}}' |
    grep -E '^(ghcr\.io/(yujianwudi|james-6-23)/new_api_tools|new_api_tools|newapi-tools|newapi-tools-backend|newapi-tools-frontend)(:|$)' |
    xargs -r docker rmi -f 2>/dev/null || true

  # 删除网络
  docker network rm newapi-tools-network 2>/dev/null || true

  # 记录目录位置
  local dir_to_remove="$project_dir"

  # 切换到上级目录
  cd ..

  # 删除项目目录
  log_info "删除项目目录..."
  durable_remove_install_tree "$dir_to_remove" ||
    die "服务已清理，但无法持久删除项目目录；卸载未完成"

  log_success "完全卸载完成"
}

#######################################
# 交互式完全重装 (卸载后重新安装)
#######################################
do_full_reinstall_interactive() {
  local project_dir="$1"

  echo ""
  echo -e "${RED}════════════════════════════════════════════════════════════${NC}"
  echo -e "${RED}  警告: 完全重新安装${NC}"
  echo -e "${RED}════════════════════════════════════════════════════════════${NC}"
  echo ""
  echo -e "${YELLOW}将执行：${NC}"
  echo "  1. 删除现有 newapi-tools 容器 / 镜像 / 缓存卷 / 项目目录"
  echo "  2. 重新克隆项目代码"
  echo "  3. 重新运行部署向导（会再次询问密码 / 端口绑定等）"
  echo ""
  echo -e "${YELLOW}影响范围：${NC}"
  echo "  • newapi-tools 自身数据丢失（密码、缓存、配置）"
  echo "  • 重新部署后需重新设置登录密码"
  echo ""
  echo -e "${GREEN}不影响：${NC}"
  echo "  ✓ NewAPI 容器、数据库、用户业务数据 → 全部保留"
  echo ""
  read -r -p "输入 'REINSTALL' 确认完全重装: " confirm

  if [[ "$confirm" != "REINSTALL" ]]; then
    log_info "已取消"
    return 0
  fi

  log_warn "正在执行完全重装..."

  cd "$project_dir"

  # 停止并删除容器和 volumes
  log_info "停止并删除容器..."
  stop_install_project_for_removal "$project_dir" ||
    die "重新安装前的服务清理失败；项目配置与回滚快照已保留"

  cleanup_project_docker_resources

  # 记录安装目录（项目目录的父目录）
  local install_dir
  install_dir=$(dirname "$project_dir")

  # 切换到上级目录
  cd "$install_dir"

  # 删除项目目录
  log_info "删除项目目录..."
  durable_remove_install_tree "$project_dir" ||
    die "服务已清理，但无法持久删除旧项目目录；重新安装已中止"

  log_success "卸载完成，开始重新安装..."
  echo ""

  # 重新设置安装目录并执行安装
  INSTALL_DIR="$install_dir"
  REINSTALL=true

  # 重新检测 NewAPI 环境并显示
  detect_newapi_location
  show_initial_env_detection

  # 克隆项目
  clone_or_update_project

  # 运行部署脚本
  run_deploy
}

#######################################
# 执行清理操作 (重新安装时)
#######################################
perform_cleanup() {
  local target_dir="$1" env_file="${1}/.env" rollback_content old_image
  local rollback_env_file old_project toolstore_prepare_ok=false
  local -a rollback_compose_args=()
  [[ -n "$target_dir" && "$target_dir" != "/" && -d "$target_dir" && ! -L "$target_dir" ]] ||
    die "安全重装拒绝空路径、根目录、symlink 或不存在的项目树"
  [[ -f "$env_file" && ! -L "$env_file" ]] ||
    die "安全重装缺少安全的旧 .env；未停止服务、未删除目录"
  load_install_toolstore_transaction_library "$target_dir" ||
    die "当前安装尚无 Tool Store 事务库；请先使用菜单 1 更新，再执行安全重装"

  prepare_install_rollback_transaction "$env_file" "$target_dir"
  [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]] ||
    die "无法建立旧镜像/配置/Compose 回滚锚点；未停止服务、未删除目录"
  rollback_content="$(load_install_rollback_snapshot "$env_file")" ||
    die "旧配置回滚快照不可读；未停止服务、未删除目录"
  old_image="$(env_content_value "$rollback_content" NEWAPI_TOOLS_IMAGE)"
  rollback_env_file="$(install_rollback_snapshot_path "$env_file")"
  old_project="$(env_content_value "$rollback_content" COMPOSE_PROJECT_NAME)"
  [[ "$old_project" =~ ^[a-z0-9][a-z0-9_-]{0,62}$ ]] ||
    die "旧 Compose project 身份无效；未停止服务、未删除目录"
  INSTALL_COMPOSE_PROJECT_NAME_OVERRIDE="$old_project"
  build_install_rollback_compose_args "$env_file" "$target_dir" rollback_compose_args ||
    die "旧 Compose 清单回滚快照不完整；未停止服务、未删除目录"

  configure_install_toolstore_compose_sources "$env_file" ||
    die "无法把旧 Compose 清单绑定到 Tool Store 安全重装记录"
  if toolstore_txn_prepare "$rollback_env_file" "$target_dir" \
    "$old_image" "$old_image" "$old_image"; then
    toolstore_prepare_ok=true
  fi
  clear_install_toolstore_compose_sources
  if [[ "$toolstore_prepare_ok" != "true" ]]; then
    restore_install_previous_configuration "$env_file" "$target_dir" || true
    die "Tool Store Online Backup/二次校验失败；旧服务保持运行，项目目录未删除"
  fi
  if ! run_install_compose "$rollback_env_file" "$target_dir" "$old_image" \
    "${rollback_compose_args[@]}" down; then
    die "旧服务停止失败；Tool Store 备份和旧部署证据已保留，项目目录未删除"
  fi
  toolstore_txn_authoritative_backup "$target_dir" ||
    die "旧服务已停止，但无法创建停写后的权威 Tool Store 备份；事务证据和旧项目均已保留"
  toolstore_txn_stage_data "$target_dir" ||
    die "Tool Store/data 无法安全迁出待删树；旧项目和备份证据已保留"
  durable_remove_install_tree "$target_dir" ||
    die "旧项目树删除失败；暂存 data 和事务证据已保留，可重复运行恢复"
  log_success "旧项目已在 Tool Store 备份验证和 data 暂存完成后移除；准备重建"
}

#######################################
# Clone 或更新项目
#######################################
install_clone_fault_point() {
  local point="$1"
  if [[ "${INSTALL_CLONE_FAULT_POINT:-}" == "$point" ]]; then
    log_error "clone 故障注入点: ${point}"
    return 97
  fi
}

install_interrupted_checkout_is_trusted() {
  local target="$1" origin
  [[ -d "$target" && ! -L "$target" && -d "${target}/.git" && ! -L "${target}/.git" ]] || return 1
  origin="$(git -C "$target" remote get-url origin 2>/dev/null)" || return 1
  case "$origin" in
    "https://github.com/yujianwudi/new_api_tools"|\
    "https://github.com/yujianwudi/new_api_tools.git"|\
    "git@github.com:yujianwudi/new_api_tools.git"|\
    "ssh://git@github.com/yujianwudi/new_api_tools.git") return 0 ;;
    *) return 1 ;;
  esac
}

install_clone_staging_path() {
  local target="$1" parent base
  parent="$(dirname -- "$target")"
  base="$(basename -- "$target")"
  printf '%s/.%s.clone-staging\n' "$parent" "$base"
}

cleanup_install_clone_staging() {
  local target="$1" staging marker
  staging="$(install_clone_staging_path "$target")" || return 1
  marker="${staging}/.newapi-tools-clone-staging"
  [[ -e "$staging" || -L "$staging" ]] || return 0
  [[ -d "$staging" && ! -L "$staging" && -f "$marker" && ! -L "$marker" ]] || return 1
  [[ "$(<"$marker")" == "new-api-tools-clone-staging-v1" ]] || return 1
  durable_remove_install_tree "$staging"
}

quarantine_interrupted_install_checkout() {
  local target="$1" parent base parent_abs target_abs quarantine
  parent="$(dirname -- "$target")"
  base="$(basename -- "$target")"
  parent_abs="$(cd -- "$parent" && pwd -P)" || return 1
  target_abs="$(realpath -m -s -- "$target")" || return 1
  [[ "$target_abs" == "${parent_abs}/${base}" && "$target_abs" != "/" ]] || return 1
  install_interrupted_checkout_is_trusted "$target_abs" || return 1
  quarantine="${parent_abs}/.${base}.interrupted-$(date -u +%Y%m%dT%H%M%SZ)-$$"
  [[ ! -e "$quarantine" && ! -L "$quarantine" ]] || return 1
  mv -T -- "$target_abs" "$quarantine" || return 1
  sync -f "$parent_abs" || return 1
  log_warn "已把可信但不完整的旧 checkout 隔离保留到 ${quarantine}"
}

clone_or_update_project() {
  local target_dir="${INSTALL_DIR}/${PROJECT_NAME}" parent staging checkout marker

  if [[ -d "$target_dir" ]]; then
    if [[ -f "${target_dir}/.env" && ! -L "${target_dir}/.env" &&
      -f "${target_dir}/docker-compose.yml" && ! -L "${target_dir}/docker-compose.yml" ]]; then
      log_info "项目已存在，正在同步到 ${INSTALL_REF}..."
      cd "$target_dir"
      checkout_install_ref
      PROJECT_DIR="$target_dir"
      return 0
    fi
    quarantine_interrupted_install_checkout "$target_dir" ||
      die "现有不完整项目树无法安全隔离；拒绝覆盖"
  fi

  parent="$(dirname -- "$target_dir")"
  [[ -d "$parent" && ! -L "$parent" ]] || die "clone 目标父目录不安全"
  cleanup_install_clone_staging "$target_dir" ||
    die "上次中断的 clone staging 目录不属于本安装器；拒绝删除"
  staging="$(install_clone_staging_path "$target_dir")"
  checkout="${staging}/tree"
  marker="${staging}/.newapi-tools-clone-staging"
  mkdir -m 700 "$staging" || die "无法创建同级 clone staging 目录"
  (umask 077; printf 'new-api-tools-clone-staging-v1\n' >"$marker") ||
    die "无法写入 clone staging 身份标记"
  chmod 600 "$marker" || die "无法保护 clone staging 身份标记"
  sync -f "$marker" && sync -f "$staging" || die "无法持久化 clone staging 身份"

  log_info "正在隔离克隆项目，验证完成后才原子发布到: $target_dir"
  git clone --no-checkout "$REPO_URL" "$checkout"
  install_clone_fault_point after_clone || return $?
  cd "$checkout"
  checkout_install_ref
  install_clone_fault_point after_checkout || return $?
  [[ -f "${checkout}/deploy.sh" && ! -L "${checkout}/deploy.sh" &&
    -f "${checkout}/docker-compose.yml" && ! -L "${checkout}/docker-compose.yml" &&
    -f "${checkout}/scripts/toolstore_transaction.sh" && ! -L "${checkout}/scripts/toolstore_transaction.sh" ]] ||
    die "候选 checkout 缺少安全的部署/事务文件"
  [[ "$(git -C "$checkout" rev-parse --verify HEAD)" == "$INSTALL_COMMIT" ]] ||
    die "候选 checkout HEAD 与已验证安装 commit 不一致"
  [[ ! -e "$target_dir" && ! -L "$target_dir" ]] ||
    die "clone 验证期间最终目标被并发创建"
  install_clone_fault_point before_publish || return $?
  mv -T -- "$checkout" "$target_dir" || die "无法原子发布已验证 checkout"
  sync -f "$parent" || die "无法持久化已验证 checkout 发布"
  install_clone_fault_point after_publish || return $?
  cleanup_install_clone_staging "$target_dir" ||
    log_warn "checkout 已发布，但无法清理空的 clone staging 目录"
  log_success "项目克隆、版本验证和原子发布完成"
  cd "$target_dir"
  PROJECT_DIR="$target_dir"
}

#######################################
# 下载 GeoIP 数据库
#######################################
download_geoip_database() {
  local geoip_dir="${PROJECT_DIR}/data/geoip"
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

#######################################
# 检查并更新配置文件
#######################################
check_and_update_configs() {
  local compose_file="${PROJECT_DIR}/docker-compose.yml"
  local env_file="${PROJECT_DIR}/.env"
  local updated=false

  # 检查 docker-compose.yml 是否包含 geoip 挂载
  if ! grep -q "data/geoip" "$compose_file" 2>/dev/null; then
    log_info "检测到旧版配置，更新 docker-compose.yml..."
    # 使用 git 更新后的文件已包含 geoip 配置，无需手动修改
    updated=true
  fi

  # 检查 geoip 目录是否存在
  if [[ ! -d "${PROJECT_DIR}/data/geoip" ]]; then
    log_info "创建 GeoIP 数据目录..."
    mkdir -p "${PROJECT_DIR}/data/geoip"
    updated=true
  fi

  if [[ "$updated" == "true" ]]; then
    log_success "配置已更新；GeoIP 默认使用镜像内固定校验快照"
  fi
}

#######################################
# 迁移旧版 .env 文件 (从 Python 版升级到 Go 版)
# 为旧用户自动补充 Go 版本所需的新字段
#######################################
migrate_env_file() {
  local project_dir="$1"
  local env_file="${project_dir}/.env"

  [[ -f "$env_file" ]] || return 0

  local migrated=false content

  # 镜像与当前安装 ref/显式覆盖保持一致；旧 tag-only 键只用于一次性迁移。
  migrate_image_env_file "$env_file" "${NEWAPI_TOOLS_IMAGE:-}"
  content="$(<"$env_file")"

  # 补充 SQL_DSN（从分离字段构建）
  if ! printf '%s\n' "$content" | grep -q '^SQL_DSN='; then
    local db_engine db_dns db_port db_user db_password db_name sql_dsn=""
    db_engine="$(env_file_value "$env_file" 'DB_ENGINE')"
    db_dns="$(env_file_value "$env_file" 'DB_DNS')"
    db_port="$(env_file_value "$env_file" 'DB_PORT')"
    db_user="$(env_file_value "$env_file" 'DB_USER')"
    db_password="$(env_file_value "$env_file" 'DB_PASSWORD')"
    db_name="$(env_file_value "$env_file" 'DB_NAME')"

    if [[ -n "$db_dns" ]]; then
      if [[ "$db_engine" == "postgres" || "$db_engine" == "postgresql" ]]; then
        sql_dsn="host=${db_dns} port=${db_port:-5432} user=${db_user} password=${db_password} dbname=${db_name} sslmode=disable"
      else
        sql_dsn="${db_user}:${db_password}@tcp(${db_dns}:${db_port:-3306})/${db_name}?charset=utf8mb4&parseTime=True"
      fi
    fi

    content="$(append_install_env_content_line "$content" \
      "SQL_DSN=$(dotenv_quote "$sql_dsn")")"
    migrated=true
    log_info "已补充 SQL_DSN 配置"
  fi

  # 补充 TIMEZONE
  if ! printf '%s\n' "$content" | grep -q '^TIMEZONE='; then
    content="$(append_install_env_content_line "$content" 'TIMEZONE=Asia/Shanghai')"
    migrated=true
  fi

  # 补充 LOG_LEVEL
  if ! printf '%s\n' "$content" | grep -q '^LOG_LEVEL='; then
    content="$(append_install_env_content_line "$content" 'LOG_LEVEL=info')"
    migrated=true
  fi

  # 补充 REDIS_PASSWORD（避免 compose WARN）
  if ! printf '%s\n' "$content" | grep -q '^REDIS_PASSWORD='; then
    content="$(append_install_env_content_line "$content" "REDIS_PASSWORD=''")"
    migrated=true
  fi

  # 合并镜像的内层 Nginx 通过 loopback 访问 Go；只有精确信任的
  # 直接对端才允许后端解析 X-Forwarded-For。
  if ! printf '%s\n' "$content" | grep -q '^TRUSTED_PROXY_CIDRS='; then
    content="$(append_install_env_content_line "$content" \
      'TRUSTED_PROXY_CIDRS=127.0.0.1/32,::1/128')"
    migrated=true
  fi

  if ! printf '%s\n' "$content" | grep -q '^API_KEY_ROLE='; then
    content="$(append_install_env_content_line "$content" 'API_KEY_ROLE=viewer')"
    migrated=true
  fi

  if ! printf '%s\n' "$content" | grep -q '^NEWAPI_ADMIN_ACCESS_TOKEN='; then
    content="$(append_install_env_content_line "$content" 'NEWAPI_ADMIN_ACCESS_TOKEN=')"
    migrated=true
  fi
  if ! printf '%s\n' "$content" | grep -q '^NEWAPI_ADMIN_USER_ID='; then
    content="$(append_install_env_content_line "$content" 'NEWAPI_ADMIN_USER_ID=0')"
    migrated=true
  fi

  local current_newapi_baseurl
  current_newapi_baseurl="$(env_content_value "$content" 'NEWAPI_BASEURL')"
  current_newapi_baseurl="${current_newapi_baseurl#"${current_newapi_baseurl%%[![:space:]]*}"}"
  current_newapi_baseurl="${current_newapi_baseurl%"${current_newapi_baseurl##*[![:space:]]}"}"
  if [[ -z "$current_newapi_baseurl" ]]; then
    local newapi_container newapi_mode newapi_port newapi_host newapi_env detected_newapi_baseurl=""
    newapi_container="$(env_content_value "$content" 'NEWAPI_CONTAINER')"
    newapi_mode="$(env_content_value "$content" 'NEWAPI_NETWORK_MODE')"
    newapi_port=""
    if [[ -n "$newapi_container" ]] &&
      newapi_env="$(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "$newapi_container" 2>/dev/null)"; then
      newapi_port="$(printf '%s\n' "$newapi_env" | sed -n 's/^PORT=//p' | tail -n1)"
      newapi_port="${newapi_port:-3000}"
    fi
    newapi_host="$newapi_container"
    [[ "$newapi_mode" == "host" ]] && newapi_host="host.docker.internal"
    if [[ -n "$newapi_host" && "$newapi_port" =~ ^[0-9]{1,5}$ ]] &&
      (( 10#$newapi_port >= 1 && 10#$newapi_port <= 65535 )); then
      detected_newapi_baseurl="http://${newapi_host}:${newapi_port}"
    fi

    if [[ -n "$detected_newapi_baseurl" ]]; then
      content="$(replace_install_env_content_optional \
        "$content" 'NEWAPI_BASEURL' "$detected_newapi_baseurl")"
      migrated=true
      log_info "已自动检测并补充 NEWAPI_BASEURL"
    elif printf '%s\n' "$content" | grep -q '^NEWAPI_BASEURL='; then
      content="$(replace_install_env_content_optional "$content" 'NEWAPI_BASEURL')"
      migrated=true
      log_warn "NEWAPI_BASEURL 仍未检测到；已移除空配置，写操作保持禁用"
    fi
  fi

  if ! printf '%s\n' "$content" | grep -q '^OBSERVABILITY_TOKEN='; then
    local observability_token
    observability_token="$(openssl rand -hex 32 2>/dev/null || head -c 64 /dev/urandom | sha256sum | awk '{print $1}')"
    content="$(append_install_env_content_line "$content" \
      "OBSERVABILITY_TOKEN=$(dotenv_quote "$observability_token")")"
    migrated=true
  fi
  if ! printf '%s\n' "$content" | grep -q '^LOG_FRESHNESS_MAX_SECONDS='; then
    content="$(append_install_env_content_line "$content" \
      'LOG_FRESHNESS_MAX_SECONDS=900')"
    migrated=true
  fi
  if ! printf '%s\n' "$content" | grep -q '^TOOL_STORE_PATH='; then
    content="$(append_install_env_content_line "$content" \
      'TOOL_STORE_PATH=/app/data/control-plane.db')"
    migrated=true
  fi

  # Commit every migration addition/removal as one complete dotenv image. This
  # also repairs old permissive modes without exposing a truncate/append window.
  validate_install_credential_separation_content "$content" ||
    die "凭据预检失败；完整 .env 迁移结果未发布"
  atomic_write_install_dotenv "$env_file" "$content" ||
    die "无法原子持久化完整 .env 迁移结果"

  if [[ "$migrated" == "true" ]]; then
    log_success "已自动补充 Go 版本所需的配置字段"
  fi
}

#######################################
# 检查 SERVER_HOST 安全性
# 默认 Go 后端绑定 127.0.0.1，仅 Nginx 反代访问
# 若用户显式配了 0.0.0.0，给出告警并询问是否改回（保留兼容旧配置的用户）
#######################################
check_server_host_security() {
  local env_file="${1}/.env"
  [[ -f "$env_file" ]] || return 0

  local host_line
  # set -e + pipefail 下，grep 无匹配会让 pipe 退出码为 1 → 必须 || true 兜底，否则脚本静默退出。
  host_line=$(grep -E '^SERVER_HOST=' "$env_file" 2>/dev/null | tail -n1 || true)
  [[ -z "$host_line" ]] && return 0

  local host_value
  host_value=$(echo "$host_line" | cut -d'=' -f2-)
  # 去掉所有引号、空白、回车
  host_value="${host_value//[\"\'\ $'\r'$'\n'$'\t']/}"

  if [[ "$host_value" == "0.0.0.0" || "$host_value" == "::" ]]; then
    echo ""
    log_warn "⚠ 检测到 .env 中 SERVER_HOST=${host_value}"
    log_warn "   Go 后端 (8000 端口) 会暴露到容器虚拟网卡所有接口"
    log_warn "   若是 host 网络模式部署，会直接暴露到宿主机外部，有安全风险"
    echo ""
    read -r -p "是否改为安全默认值 SERVER_HOST=127.0.0.1（推荐）? [Y/n]: " confirm
    if [[ ! "$confirm" =~ ^[nN]$ ]]; then
      comment_and_replace_install_env_key "$env_file" 'SERVER_HOST' '127.0.0.1' \
        '# Disabled by install.sh (insecure): ' ||
        die "无法原子持久化安全 SERVER_HOST 设置"
      log_success "已改为 SERVER_HOST=127.0.0.1"
    else
      log_info "保留 SERVER_HOST=${host_value}（确认你了解风险）"
    fi
  fi
}

#######################################
# 快速更新服务 (保留配置)
#######################################
quick_update() {
  log_info "执行快速更新..."

  local env_file="${PROJECT_DIR}/.env"
  local compose_file="${PROJECT_DIR}/docker-compose.yml"

  if [[ ! -f "$env_file" ]]; then
    log_warn "未找到 .env 配置文件，将执行完整部署流程"
    return 1
  fi

  if [[ ! -f "$compose_file" ]]; then
    die "找不到 docker-compose.yml 文件"
  fi

  validate_install_credential_separation_file "$env_file" ||
    die "凭据预检失败；拒绝更新现有部署"

  cd "$PROJECT_DIR"

  prepare_install_rollback_transaction "$env_file" "$PROJECT_DIR"

  # 检查并更新配置（为老用户添加 GeoIP 支持）
  check_and_update_configs

  # 迁移旧版 .env（补充 Go 版本所需字段）
  migrate_env_file "$PROJECT_DIR"

  # 按当前 NewAPI 容器 Redis 配置同步直接写库安全开关
  sync_newapi_mutation_safety_config "$PROJECT_DIR"

  # 安全检查：SERVER_HOST 是否绑定到不安全的 0.0.0.0
  check_server_host_security "$PROJECT_DIR"

  # 下载 GeoIP 数据库
  download_geoip_database

  # 根据 .env 自动选择 compose 文件（host 模式叠加 overlay）
  setup_compose_files "$PROJECT_DIR"
  local -a compose_args=(--env-file "$env_file")
  if [[ -z "${COMPOSE_FILE:-}" ]]; then
    compose_args+=(-f "$compose_file")
  fi

  # 拉取最新镜像
  log_info "拉取最新镜像..."
  if ! run_install_compose "$env_file" "$PROJECT_DIR" "$NEWAPI_TOOLS_IMAGE" \
    "${compose_args[@]}" pull --include-deps newapi-tools; then
    if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      restore_install_previous_configuration "$env_file" "$PROJECT_DIR" ||
        log_error "高危：候选拉取失败后无法恢复完整旧安装配置"
    fi
    die "拉取 newapi-tools 及其策略门禁/Redis 依赖镜像失败；当前运行中的服务保持不变"
  fi
  # Resolve mutable release/SHA tags to the exact pulled RepoDigest before
  # stopping the old service. Auto-derived images also have their OCI source
  # revision checked against the checked-out commit.
  if ! pin_install_image_after_pull "$env_file"; then
    if [[ "$INSTALL_ROLLBACK_ENV_AVAILABLE" == "true" ]]; then
      restore_install_previous_configuration "$env_file" "$PROJECT_DIR" ||
        log_error "高危：候选镜像验证失败后无法恢复完整旧安装配置"
    fi
    die "候选镜像 digest 或源码版本验证失败；现有服务保持不变"
  fi

  restart_install_services_transactionally "$env_file" "$PROJECT_DIR" "${compose_args[@]}"

  # 获取前端端口
  local frontend_port
  frontend_port=$(grep -E '^FRONTEND_PORT=' "$env_file" | cut -d'=' -f2 || echo "1145")

  # 获取服务器 IP
  local server_ip
  server_ip="$(hostname -I 2>/dev/null | awk '{print $1}')" || server_ip="$(ip route get 1 2>/dev/null | awk '{print $7; exit}')" || server_ip="localhost"
  detect_env_details "$PROJECT_DIR"

  echo ""
  echo -e "${GREEN}========================================${NC}"
  echo -e "${GREEN}  更新完成!${NC}"
  echo -e "${GREEN}========================================${NC}"
  echo ""
  show_frontend_access "$ENV_FRONTEND_BIND" "$frontend_port" "$server_ip" "前端访问地址"
  echo ""
  echo -e "查看日志: ${YELLOW}cd ${PROJECT_DIR} && docker compose logs -f${NC}"
  echo ""

  return 0
}

#######################################
# 运行部署脚本
#######################################
run_deploy() {
  if load_install_toolstore_transaction_library "$PROJECT_DIR" &&
    toolstore_txn_has_active "$PROJECT_DIR"; then
    local continuation_content continuation_state
    toolstore_txn_restore_config_only "$PROJECT_DIR" ||
      die "安全重装无法恢复旧配置；事务证据已保留"
    toolstore_txn_unstage_data "$PROJECT_DIR" ||
      die "安全重装无法把暂存 data 恢复到新项目树；事务证据已保留"
    continuation_content="$(toolstore_txn_load_record "$PROJECT_DIR")" ||
      die "安全重装无法读取外置事务记录"
    continuation_state="$(toolstore_txn_env_value "$continuation_content" STATE)"
    [[ "$continuation_state" == "authoritative" ]] ||
      die "安全重装 data 恢复后事务未回到权威备份状态"
    TOOLSTORE_TXN_CONTINUE_REQUEST="$(toolstore_txn_env_value "$continuation_content" REQUEST_ID)"
    export TOOLSTORE_TXN_CONTINUE_REQUEST
  fi

  # 如果不是重新安装且已有配置，执行快速更新
  if [[ "$REINSTALL" == "false" && -f "${PROJECT_DIR}/.env" ]]; then
    if quick_update; then
      exit 0
    fi
  fi
  
  log_info "正在启动部署脚本..."

  if [[ ! -f "${PROJECT_DIR}/deploy.sh" ]]; then
    die "找不到部署脚本: ${PROJECT_DIR}/deploy.sh"
  fi

  chmod +x "${PROJECT_DIR}/deploy.sh"

  # 运行部署脚本
  if [[ "$NEWAPI_TOOLS_IMAGE_DERIVED" == "true" ]]; then
    unset NEWAPI_TOOLS_IMAGE NEWAPI_TOOLS_IMAGE_DERIVED NEWAPI_TOOLS_EXPECTED_REVISION \
      NEWAPI_TOOLS_RELEASE_TAG
  fi
  exec "${PROJECT_DIR}/deploy.sh"
}

#######################################
# 查找已安装的项目目录
#######################################
find_installed_dir() {
  # 优先检查环境变量
  if [[ -n "${PROJECT_DIR:-}" && -d "$PROJECT_DIR" ]]; then
    echo "$PROJECT_DIR"
    return 0
  fi

  # 检查当前目录
  if [[ -f "./docker-compose.yml" && -f "./.env" ]]; then
    echo "$(pwd)"
    return 0
  fi

  # 检查常见安装位置
  local possible_dirs=(
    "/opt/new_api_tools"
    "/root/new_api_tools"
    "$HOME/new_api_tools"
    "$(pwd)/new_api_tools"
  )

  for dir in "${possible_dirs[@]}"; do
    if [[ -f "$dir/docker-compose.yml" && -f "$dir/.env" ]]; then
      echo "$dir"
      return 0
    fi
  done

  # 尝试通过容器查找
  local container_dir
  container_dir=$(docker inspect newapi-tools 2>/dev/null | grep -oP '"Source": "\K[^"]+(?=/data")' | head -1 || true)
  if [[ -n "$container_dir" ]]; then
    local parent_dir=$(dirname "$container_dir")
    if [[ -f "$parent_dir/docker-compose.yml" ]]; then
      echo "$parent_dir"
      return 0
    fi
  fi

  return 1
}

#######################################
# 显示帮助信息
#######################################
show_help() {
  cat <<EOF
NewAPI Middleware Tool - 安装管理脚本

用法:
  install.sh [选项]

选项:
  (无参数)        交互式安装和管理
  --help          显示此帮助信息

环境变量:
  PROJECT_DIR        指定项目目录（默认: 自动检测）
  NEWAPI_CONTAINER   指定 NewAPI 容器名（默认: 自动检测）
  NEWAPI_TOOLS_REF              Git 安装版本（默认: v0.6.2；main 会锁定本次 commit 的短 SHA 镜像）
  NEWAPI_TOOLS_IMAGE            发行页核验的完整 repo@sha256:digest；严格发行标签必填，开发 ref 禁止显式设置
  NEWAPI_TOOLS_EXPECTED_REVISION 发行页核验的 40 位 Git commit；显式镜像时必填

更多信息: https://github.com/yujianwudi/new_api_tools
EOF
}

#######################################
# 主函数
#######################################
main() {
  local action="${1:-}"

  # 只处理 --help 选项
  if [[ "$action" == "--help" || "$action" == "-h" ]]; then
    show_help
    exit 0
  fi

  # 如果有其他参数，显示错误
  if [[ -n "$action" ]]; then
    log_error "未知选项: $action"
    echo "使用 --help 查看帮助"
    exit 1
  fi

  # 交互式安装/管理
  echo ""
  echo -e "${BLUE}========================================${NC}"
  echo -e "${BLUE}  NewAPI Middleware Tool 安装管理${NC}"
  echo -e "${BLUE}========================================${NC}"
  echo ""

  check_requirements
  detect_newapi_location
  acquire_install_state_lock "${INSTALL_DIR}/${PROJECT_NAME}"
  check_existing_installation
  clone_or_update_project
  run_deploy
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  main "$@"
fi
