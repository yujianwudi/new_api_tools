#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  printf 'supply-chain pin check failed: %s\n' "$1" >&2
  exit 1
}

trim_yaml_value() {
  local value="$1"
  value="${value#*:}"
  value="${value%%[[:space:]]#*}"
  value="${value#"${value%%[![:space:]]*}"}"
  value="${value%"${value##*[![:space:]]}"}"
  if [[ ${#value} -ge 2 && ( "$value" == \"*\" || "$value" == \'*\' ) ]]; then
    value="${value:1:${#value}-2}"
  fi
  printf '%s\n' "$value"
}

is_pinned_sha256_image() {
  [[ "${1:-}" =~ ^[^@[:space:]]+@sha256:[0-9a-f]{64}$ ]]
}

is_pinned_syntax_frontend() {
  local image="${1:-}" reference final_component
  is_pinned_sha256_image "$image" || return 1
  reference="${image%@*}"
  final_component="${reference##*/}"
  [[ "$final_component" =~ ^[^:]+:[0-9]+\.[0-9]+\.[0-9]+$ ]]
}

syntax_test_digest='aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
is_pinned_syntax_frontend "docker/dockerfile:1.7.0@sha256:${syntax_test_digest}" ||
  fail 'syntax frontend guard rejected an exact version and digest'
if is_pinned_syntax_frontend 'docker/dockerfile:1'; then
  fail 'syntax frontend guard accepted a floating version'
fi
if is_pinned_syntax_frontend "docker/dockerfile:1@sha256:${syntax_test_digest}"; then
  fail 'syntax frontend guard accepted a floating major tag even with a digest'
fi

mapfile -t dockerfiles < <(
  git ls-files -co --exclude-standard '*Dockerfile*' | sort -u
)
(( ${#dockerfiles[@]} > 0 )) || fail 'repository contains no Dockerfiles to audit'

# A syntax directive downloads and executes a remote BuildKit frontend before
# any FROM instruction. Floating directives are therefore as sensitive as
# mutable base images. These Dockerfiles use the frontend bundled with the
# required modern BuildKit and intentionally need no directive.
for dockerfile in "${dockerfiles[@]}"; do
  while IFS= read -r line; do
    syntax_frontend="${line#*=}"
    syntax_frontend="${syntax_frontend#"${syntax_frontend%%[![:space:]]*}"}"
    syntax_frontend="${syntax_frontend%"${syntax_frontend##*[![:space:]]}"}"
    is_pinned_syntax_frontend "$syntax_frontend" ||
      fail "$dockerfile contains a mutable Dockerfile syntax frontend: $syntax_frontend"
  done < <(grep -Ei '^[[:space:]]*#[[:space:]]*syntax[[:space:]]*=' "$dockerfile" || true)
done

# Only real FROM instructions are parsed; comments and arbitrary occurrences
# of the word FROM do not satisfy the check.
for dockerfile in "${dockerfiles[@]}"; do
  from_count=0
  while IFS= read -r line; do
    instruction="${line%%#*}"
    read -r -a fields <<< "$instruction"
    index=1
    while [[ "${fields[index]:-}" == --* ]]; do
      index=$((index + 1))
    done
    image="${fields[index]:-}"
    [[ -n "$image" ]] || fail "$dockerfile contains an invalid FROM instruction: $line"
    is_pinned_sha256_image "$image" ||
      fail "$dockerfile contains a FROM image without an exact 64-character SHA-256 digest: $image"
    from_count=$((from_count + 1))
  done < <(grep -Ei '^[[:space:]]*FROM[[:space:]]+' "$dockerfile" || true)
  (( from_count > 0 )) || fail "$dockerfile contains no valid FROM instruction"
done

# Literal Compose images must be immutable. The application image is the one
# deliberate variable reference: install/deploy resolves and persists its
# digest after the targeted pull, before the old service is stopped.
while IFS= read -r compose_file; do
  compose_image_count=0
  while IFS= read -r line; do
    image="$(trim_yaml_value "$line")"
    if [[ "$image" == '${NEWAPI_TOOLS_IMAGE:?NEWAPI_TOOLS_IMAGE must be an immutable repo@sha256 digest; use install/deploy to resolve tags safely}' ]]; then
      compose_image_count=$((compose_image_count + 1))
      continue
    fi
    is_pinned_sha256_image "$image" ||
      fail "$compose_file contains an image without an exact 64-character SHA-256 digest: $image"
    compose_image_count=$((compose_image_count + 1))
  done < <(grep -E '^[[:space:]]*image[[:space:]]*:' "$compose_file" || true)
  if [[ "$compose_file" == './docker-compose.yml' ]]; then
    (( compose_image_count > 0 )) || fail 'docker-compose.yml contains no valid image declaration'
  fi
done < <(find . -maxdepth 1 -type f \( -name 'docker-compose*.yml' -o -name 'docker-compose*.yaml' \) -print | sort)

grep -Fxq 'NEWAPI_TOOLS_IMAGE=' .env.example ||
  fail '.env.example must not ship a mutable application image default'
if grep -Eq 'NEWAPI_TOOLS_IMAGE:-' docker-compose.yml; then
  fail 'docker-compose.yml must not provide a mutable application image fallback'
fi
grep -Fq 'image-policy:' docker-compose.yml ||
  fail 'docker-compose.yml must include the immutable application image policy gate'
grep -Fq 'condition: service_completed_successfully' docker-compose.yml ||
  fail 'newapi-tools must wait for the immutable application image policy gate'
grep -Fq 'ENFORCE_IP_RECORDING=${ENFORCE_IP_RECORDING:-false}' docker-compose.yml ||
  fail 'Compose must preserve the v0.2 ENFORCE_IP_RECORDING rollback contract'
grep -Fq '"tool_store"[[:space:]]*:[[:space:]]*"ok"' docker-compose.yml ||
  fail 'Compose health must validate v0.5 Tool Store readiness content'
grep -Fq 'http://localhost:8080/api/health/db' docker-compose.yml ||
  fail 'Compose health must retain the semantic v0.2 database rollback probe'
grep -Fq '"success"[[:space:]]*:[[:space:]]*true' docker-compose.yml ||
  fail 'legacy rollback health must validate JSON success rather than HTTP 200 alone'
grep -Fq '@sha256:[0-9a-f]{64}$$' docker-compose.yml ||
  fail 'the Compose image policy must require an exact lowercase SHA-256 digest'
newapi_service_block="$(sed -n '/^  newapi-tools:/,/^  image-policy:/p' docker-compose.yml)"
grep -Fq '      image-policy:' <<< "$newapi_service_block" ||
  fail 'newapi-tools must declare image-policy as a Compose dependency'
grep -Fq '      redis:' <<< "$newapi_service_block" ||
  fail 'newapi-tools must declare Redis as a Compose dependency'
policy_pull_count="$(grep -hF 'pull --include-deps newapi-tools' install.sh deploy.sh | wc -l | tr -d '[:space:]')"
[[ "$policy_pull_count" == "3" ]] ||
  fail 'install/deploy must pre-pull all Compose dependencies before stopping existing services'
if grep -Fq 'docker compose pull && docker compose down' docker-compose.yml; then
  fail 'docker-compose.yml must not recommend a destructive pull/down/up update sequence'
fi
grep -Fq '使用固定安装器或 ./deploy.sh 执行事务更新' docker-compose.yml ||
  fail 'docker-compose.yml must direct updates through the transactional installer/deployer'

frontend_docker_dependabot_count="$(awk '
  function unquote(value, first, last, single_quote) {
    first = substr(value, 1, 1)
    last = substr(value, length(value), 1)
    single_quote = sprintf("%c", 39)
    if ((first == "\"" && last == "\"") ||
        (first == single_quote && last == single_quote)) {
      return substr(value, 2, length(value) - 2)
    }
    return value
  }
  function flush_update() {
    if (ecosystem == "docker" && directory == "/frontend") count++
  }
  $1 == "-" && $2 == "package-ecosystem:" {
    flush_update()
    ecosystem = unquote($3)
    directory = ""
    next
  }
  $1 == "directory:" { directory = unquote($2) }
  END {
    flush_update()
    print count + 0
  }
' .github/dependabot.yml)"
[[ "$frontend_docker_dependabot_count" == "1" ]] ||
  fail 'Dependabot must cover frontend/Dockerfile with exactly one docker update entry'

mapfile -t versioned_release_docs < <(
  find . -maxdepth 1 -type f -name 'RELEASE_[0-9]*.[0-9]*.[0-9]*.md' -print |
    sed 's#^./##' | sort -V
)
(( ${#versioned_release_docs[@]} > 0 )) || fail 'repository contains no versioned release documents'
release_docs=(README.md "${versioned_release_docs[@]}")

grep -Fq \
  '生产部署必须使用发行页核验过并与发行 commit 绑定的 OCI manifest digest。' \
  RELEASE_0.5.0.md ||
  fail 'release notes must require a verified OCI manifest digest for production'
if grep -Fq '生产环境应使用 `0.5.0` 或 OCI digest。' RELEASE_0.5.0.md; then
  fail 'release notes must not recommend the mutable 0.5.0 OCI tag for production'
fi
if grep -Fq '稳定部署应使用 `0.2.0` 或 digest。' RELEASE_0.2.0.md; then
  fail 'historical release notes must not recommend a mutable OCI tag for stable deployment'
fi
grep -Fq \
  '`latest`、`0.2`、`0.2.0` 与短提交 SHA 都是可变 OCI tag，均可被重新指向其他镜像。' \
  RELEASE_0.2.0.md ||
  fail 'historical release notes must identify every published v0.2 image alias as mutable'

if grep -Eqs 'bash[[:space:]]*<\([[:space:]]*curl|curl[^#|]*\|[[:space:]]*(ba)?sh' \
  install.sh "${release_docs[@]}"; then
  fail 'installer and release docs must not execute unchecked remote scripts'
fi

# The image manifest digest and merge commit are only known after the protected
# tag build completes, so those values stay fail-closed placeholders in the
# repository copy. Historical release documents must bind their installer to
# the matching protected tag. A not-yet-tagged document for the current source
# version may temporarily bind to HEAD until its tag is created.
pending_release_version="$(sed -n 's/^[[:space:]]*Version[[:space:]]*=[[:space:]]*"\([0-9][0-9.]*\)"/\1/p' \
  backend/internal/buildinfo/buildinfo.go)"
[[ "$pending_release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail 'could not determine the pending source release version'

release_target_for_version() {
  local version="$1" tag
  tag="refs/tags/v${version}"
  if git show-ref --verify --quiet "$tag"; then
    printf '%s^{commit}\n' "$tag"
  elif [[ "$version" == "$pending_release_version" ]]; then
    printf 'HEAD\n'
  else
    return 1
  fi
}

release_version_at_least() {
  local current="$1" minimum="$2"
  [[ "$(printf '%s\n%s\n' "$minimum" "$current" | sort -V | head -n1)" == "$minimum" ]]
}

validate_release_installer_pin() {
  local release_doc="$1" version="$2" installer_commit installer_sha256
  local committed_installer_sha256 release_target
  installer_commit="$(sed -n 's/^INSTALLER_COMMIT_SHA=\([0-9a-f]\{40\}\)$/\1/p' "$release_doc")"
  installer_sha256="$(sed -n 's/^INSTALL_SCRIPT_SHA256=\([0-9a-f]\{64\}\)$/\1/p' "$release_doc")"
  if [[ -z "$installer_commit" && -z "$installer_sha256" ]]; then
    if release_version_at_least "$version" '0.5.0'; then
      fail "$release_doc must pin its installer commit and checksum"
    fi
    return 0
  fi
  [[ "$installer_commit" =~ ^[0-9a-f]{40}$ ]] ||
    fail "$release_doc release install template must pin exactly one installer commit"
  [[ "$installer_sha256" =~ ^[0-9a-f]{64}$ ]] ||
    fail "$release_doc release install template must pin exactly one installer checksum"
  git cat-file -e "${installer_commit}^{commit}" 2>/dev/null ||
    fail "$release_doc installer commit is not present in repository history"
  release_target="$(release_target_for_version "$version")" ||
    fail "$release_doc has no matching release tag and is not the pending source version"
  git merge-base --is-ancestor "$installer_commit" "$release_target" ||
    fail "$release_doc installer commit is not an ancestor of its release source ${release_target}"
  committed_installer_sha256="$(git cat-file blob "${installer_commit}:install.sh" | sha256sum | awk '{print $1}')"
  [[ "$committed_installer_sha256" == "$installer_sha256" ]] ||
    fail "$release_doc installer checksum does not match its pinned commit"
  grep -Fq \
    'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<MANIFEST_DIGEST>' \
    "$release_doc" ||
    fail "$release_doc release install template must require the exact manifest digest"
  grep -Fq 'NEWAPI_TOOLS_EXPECTED_REVISION=<RELEASE_COMMIT_SHA>' "$release_doc" ||
    fail "$release_doc release install template must bind the image to the expected release commit"
  grep -Fq \
    'https://raw.githubusercontent.com/yujianwudi/new_api_tools/${INSTALLER_COMMIT_SHA}/install.sh' \
    "$release_doc" ||
    fail "$release_doc release install template must download from the pinned installer commit"
  grep -Fq 'sha256sum -c -' "$release_doc" ||
    fail "$release_doc release install template must verify the downloaded installer"
  if release_version_at_least "$version" '0.5.1'; then
    grep -Fq \
      'printf '\''%s  %s\n'\'' "$INSTALL_SCRIPT_SHA256" "$install_script" | sha256sum -c - || exit 1' \
      "$release_doc" ||
      fail "$release_doc release install template must stop before execution when checksum verification fails"
  fi
}

for release_doc in "${versioned_release_docs[@]}"; do
  release_version="${release_doc#RELEASE_}"
  release_version="${release_version%.md}"
  [[ "$release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
    fail "invalid versioned release document name: $release_doc"
  validate_release_installer_pin "$release_doc" "$release_version"
done

readme_release_version="$(sed -n 's/^# NewAPI Tools v\([0-9][0-9.]*\)$/\1/p' README.md)"
[[ "$readme_release_version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
  fail 'README must declare exactly one semantic release version'
readme_release_doc="RELEASE_${readme_release_version}.md"
[[ -f "$readme_release_doc" ]] ||
  fail "README release document is missing: $readme_release_doc"
validate_release_installer_pin README.md "$readme_release_version"
readme_installer_commit="$(sed -n 's/^INSTALLER_COMMIT_SHA=\([0-9a-f]\{40\}\)$/\1/p' README.md)"
release_installer_commit="$(sed -n 's/^INSTALLER_COMMIT_SHA=\([0-9a-f]\{40\}\)$/\1/p' "$readme_release_doc")"
readme_installer_sha256="$(sed -n 's/^INSTALL_SCRIPT_SHA256=\([0-9a-f]\{64\}\)$/\1/p' README.md)"
release_installer_sha256="$(sed -n 's/^INSTALL_SCRIPT_SHA256=\([0-9a-f]\{64\}\)$/\1/p' "$readme_release_doc")"
[[ "$readme_installer_commit" == "$release_installer_commit" &&
   "$readme_installer_sha256" == "$release_installer_sha256" ]] ||
  fail 'README installer pin must match its current versioned release document'

workflow_dependabot_path_count="$(grep -cF "      - '.github/dependabot.yml'" \
  .github/workflows/build.yml || true)"
[[ "$workflow_dependabot_path_count" == "2" ]] ||
  fail 'build workflow push and pull_request paths must include .github/dependabot.yml'

# Every remote GitHub Action and reusable workflow must use a full commit SHA.
while IFS= read -r workflow; do
  while IFS= read -r line; do
    action="$(trim_yaml_value "$line")"
    [[ "$action" == ./* ]] && continue
    [[ "$action" =~ ^[^@[:space:]]+@[0-9a-f]{40}$ ]] ||
      fail "$workflow contains a remote action that is not pinned to a full 40-character commit SHA: $action"
  done < <(grep -E '^[[:space:]]*(-[[:space:]]*)?uses[[:space:]]*:' "$workflow" || true)
done < <(find .github -type f \( -path '*/workflows/*.yml' -o -path '*/workflows/*.yaml' -o -name 'action.yml' -o -name 'action.yaml' \) -print | sort)

if grep -RIEq 'IP_database/(main|master)/|IP_database@(main|master)' \
  Dockerfile backend/Dockerfile backend/internal/service/ip_geo.go install.sh deploy.sh; then
  fail 'GeoIP source still follows a mutable branch'
fi
if grep -Fq 'raw.gitmirror.com/adysec/IP_database' install.sh deploy.sh; then
  fail 'deployment scripts still use an unverified GeoIP mirror'
fi

grep -Eq '^ARG GEOIP_SOURCE_COMMIT=[0-9a-f]{40}$' Dockerfile || \
  fail 'GeoIP source commit is not immutable'
grep -Eq '^ARG GEOIP_SHA256=[0-9a-f]{64}$' Dockerfile || \
  fail 'GeoIP checksum is missing'
grep -Eq 'geoipDatabaseSHA256 = "[0-9a-f]{64}"' backend/internal/service/ip_geo.go || \
  fail 'runtime GeoIP checksum verification is missing'

root_commit="$(sed -n 's/^ARG GEOIP_SOURCE_COMMIT=//p' Dockerfile)"
backend_commit="$(sed -n 's/^ARG GEOIP_SOURCE_COMMIT=//p' backend/Dockerfile)"
runtime_commit="$(sed -n 's/^[[:space:]]*geoipDatabaseCommit = "\([0-9a-f]\{40\}\)"/\1/p' backend/internal/service/ip_geo.go)"
root_checksum="$(sed -n 's/^ARG GEOIP_SHA256=//p' Dockerfile)"
backend_checksum="$(sed -n 's/^ARG GEOIP_SHA256=//p' backend/Dockerfile)"
runtime_checksum="$(sed -n 's/^[[:space:]]*geoipDatabaseSHA256 = "\([0-9a-f]\{64\}\)"/\1/p' backend/internal/service/ip_geo.go)"
install_checksum="$(sed -n 's/^[[:space:]]*local expected_sha256="\([0-9a-f]\{64\}\)"/\1/p' install.sh)"
deploy_checksum="$(sed -n 's/^[[:space:]]*local expected_sha256="\([0-9a-f]\{64\}\)"/\1/p' deploy.sh)"
[[ -n "$root_commit" && "$root_commit" == "$backend_commit" && "$root_commit" == "$runtime_commit" ]] || \
  fail 'GeoIP source commit differs between Dockerfiles and runtime'
[[ -n "$root_checksum" && "$root_checksum" == "$backend_checksum" && "$root_checksum" == "$runtime_checksum" ]] || \
  fail 'GeoIP checksum differs between Dockerfiles and runtime'
[[ "$root_checksum" == "$install_checksum" && "$root_checksum" == "$deploy_checksum" ]] || \
  fail 'GeoIP checksum differs between images, runtime, and deployment scripts'

for dockerfile in Dockerfile backend/Dockerfile; do
  if grep -Fq 'Pinned build-time download unavailable' "$dockerfile"; then
    fail "$dockerfile permits a GeoIP download failure to produce an incomplete image"
  fi
done

printf 'supply-chain pin checks passed\n'
