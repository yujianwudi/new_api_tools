#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  printf 'supply-chain pin check failed: %s\n' "$1" >&2
  exit 1
}

install_cleanup_body="$({
  awk '
    /^perform_cleanup\(\)[[:space:]]*\{/ { capture = 1 }
    capture { print }
    capture && /^}/ { exit }
  ' install.sh
} 2>/dev/null)"
[[ -n "$install_cleanup_body" ]] || fail 'installer must retain an explicit fail-closed perform_cleanup guard'
backup_line="$(grep -nF 'toolstore_txn_prepare' <<<"$install_cleanup_body" | head -n1 | cut -d: -f1)"
stop_line="$(grep -nF 'run_install_compose' <<<"$install_cleanup_body" | head -n1 | cut -d: -f1)"
stage_line="$(grep -nF 'toolstore_txn_stage_data' <<<"$install_cleanup_body" | head -n1 | cut -d: -f1)"
delete_line="$(grep -nF 'durable_remove_install_tree' <<<"$install_cleanup_body" | head -n1 | cut -d: -f1)"
[[ "$backup_line" =~ ^[0-9]+$ && "$stop_line" =~ ^[0-9]+$ &&
   "$stage_line" =~ ^[0-9]+$ && "$delete_line" =~ ^[0-9]+$ ]] ||
  fail 'safe reinstall must expose backup, stop, data staging, and bounded deletion gates'
(( backup_line < stop_line && stop_line < stage_line && stage_line < delete_line )) ||
  fail 'safe reinstall must verify backup before stop, stage data before deleting the project tree'
if grep -Eq 'rm[[:space:]]+-rf|docker[[:space:]]+rm' <<<"$install_cleanup_body"; then
  fail 'safe reinstall must use bounded helpers instead of inline destructive commands'
fi
grep -Fq '8) 安全重装' install.sh ||
  fail 'installer menu must expose the verified Tool Store safe reinstall transaction'

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
    line="${line%$'\r'}"
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
    line="${line%$'\r'}"
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

# The release image and the backend-only image must compile and copy the same
# server entrypoint that their runtime command executes. Keep this static gate
# beside the image pin checks so a one-character cmd/server drift cannot reach
# the expensive multi-architecture build jobs.
for dockerfile in Dockerfile backend/Dockerfile; do
  grep -Eq '(^|[[:space:]])\./cmd/server([[:space:]]|$)' "$dockerfile" ||
    fail "$dockerfile must build the real ./cmd/server package"
  grep -Eq '^COPY[[:space:]].*/build/server[[:space:]]+/app/server[[:space:]]*$' "$dockerfile" ||
    fail "$dockerfile must copy the server binary to /app/server"
  grep -Fq '/app/server' "$dockerfile" ||
    fail "$dockerfile runtime must execute /app/server"
done
if grep -Eq '(^|[[:space:]])\./cmd/serve([[:space:]]|$)|/app/serve(["[:space:]]|$)' \
  Dockerfile backend/Dockerfile; then
  fail 'Dockerfiles must not reference the nonexistent cmd/serve or /app/serve entrypoint'
fi

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

grep -Fxq 'Dockerfile text eol=lf' .gitattributes ||
  fail 'all Dockerfiles must remain normalized to LF through .gitattributes'
grep -Fxq '.env.example text eol=lf' .gitattributes ||
  fail '.env.example must remain normalized to LF for local release checks'
grep -Fxq '.github/workflows/*.yml text eol=lf' .gitattributes ||
  fail 'workflow yml files must remain normalized to LF through .gitattributes'
grep -Fxq '.github/workflows/*.yaml text eol=lf' .gitattributes ||
  fail 'workflow yaml files must remain normalized to LF through .gitattributes'

tr -d '\r' < .env.example | grep -Fxq 'NEWAPI_TOOLS_IMAGE=' ||
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
  { sub(/\r$/, "") }
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
  if release_version_at_least "$version" '0.6.1'; then
    grep -Fq \
      'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:REPLACE_WITH_64_HEX_MANIFEST_DIGEST' \
      "$release_doc" ||
      fail "$release_doc release install template must use the fail-closed manifest placeholder"
    grep -Fq 'NEWAPI_TOOLS_EXPECTED_REVISION=REPLACE_WITH_40_HEX_RELEASE_COMMIT' "$release_doc" ||
      fail "$release_doc release install template must use the fail-closed revision placeholder"
  else
    grep -Fq \
      'NEWAPI_TOOLS_IMAGE=ghcr.io/yujianwudi/new_api_tools@sha256:<MANIFEST_DIGEST>' \
      "$release_doc" ||
      fail "$release_doc release install template must require the exact manifest digest"
    grep -Fq 'NEWAPI_TOOLS_EXPECTED_REVISION=<RELEASE_COMMIT_SHA>' "$release_doc" ||
      fail "$release_doc release install template must bind the image to the expected release commit"
  fi
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

if grep -Eq '^[[:space:]]+paths:' .github/workflows/build.yml; then
  fail 'required build checks must run for every main push and pull request without path filters'
fi

# Release publication is intentionally fail-closed around annotated tag
# identity. The normal tag workflow and the manual recovery path must both
# serialize mutable manifest aliases and must never rebuild from a tag ref
# after the validated tag object has been recorded.
grep -Fq \
  'git fetch --force --no-tags origin "+refs/tags/${tag}:refs/tags/${tag}"' \
  .github/workflows/build.yml ||
  fail 'tag workflow must exact-fetch the annotated tag before object validation'
build_tag_fetch_line="$(grep -nF \
  'git fetch --force --no-tags origin "+refs/tags/${tag}:refs/tags/${tag}"' \
  .github/workflows/build.yml | head -n 1 | cut -d: -f1)"
build_tag_type_line="$(grep -nF \
  'git cat-file -t "refs/tags/${tag}"' \
  .github/workflows/build.yml | head -n 1 | cut -d: -f1)"
[[ -n "$build_tag_fetch_line" && -n "$build_tag_type_line" &&
   "$build_tag_fetch_line" -lt "$build_tag_type_line" ]] ||
  fail 'tag workflow must restore the annotated tag ref before checking its type'

recovery_commit_checkout_count="$(grep -cF \
  'ref: ${{ needs.validate.outputs.tag_commit }}' \
  .github/workflows/release-recovery.yml || true)"
[[ "$recovery_commit_checkout_count" == "2" ]] ||
  fail 'release recovery quality and repair jobs must checkout the validated release commit'
if grep -Fq 'ref: refs/tags/${{ inputs.tag }}' .github/workflows/release-recovery.yml; then
  fail 'release recovery must not checkout an input tag after identity validation'
fi
if grep -Fq 'VCS_REF=${{ github.sha }}' .github/workflows/release-recovery.yml; then
  fail 'release recovery must not stamp the control-workflow commit into release images'
fi
grep -Fq \
  "      group: \${{ github.ref_type == 'tag' && format('ghcr-publish-minor-{0}', needs.quality.outputs.release_major_minor) || 'ghcr-publish-main' }}" \
  .github/workflows/build.yml ||
  fail 'normal image publication must use the shared GHCR publication lock'
grep -Fq \
  '      group: ghcr-publish-minor-${{ needs.validate.outputs.major_minor }}' \
  .github/workflows/release-recovery.yml ||
  fail 'release recovery must use the shared GHCR publication lock'
grep -Fq 'latest=false' .github/workflows/build.yml ||
  fail 'release tags must not implicitly move the latest image alias'
grep -Fq 'publish_minor=false' .github/workflows/build.yml ||
  fail 'normal tag publication must guard the mutable major.minor alias'
if grep -Fq 'docker buildx imagetools create' .github/workflows/release-recovery.yml; then
  fail 'release recovery must never create or move an exact or minor image tag'
fi
grep -Fq 'refusing to overwrite existing release image tag' .github/workflows/build.yml ||
  fail 'normal release publication must keep exact version image tags immutable'
grep -Fq 'release_image_exists=true' .github/workflows/release-recovery.yml ||
  fail 'release recovery must reuse an existing exact version image tag'
grep -Fq -- '--arg tag "$RELEASE_TAG"' .github/workflows/release-recovery.yml ||
  fail 'release recovery must validate the normal tag workflow OCI version label'
grep -Fq 'org.opencontainers.image.version"] == $tag' \
  .github/workflows/release-recovery.yml ||
  fail 'release recovery must accept only the exact release tag as the version-label alternative'
grep -Fq 'id-token: write' .github/workflows/build.yml ||
  fail 'release manifest signing job must request a GitHub OIDC token'
grep -Fq 'sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6' \
  .github/workflows/build.yml ||
  fail 'release workflow must install Cosign from the reviewed immutable action commit'
grep -Fq 'cosign sign --yes' .github/workflows/build.yml ||
  fail 'release workflow must keylessly sign the immutable manifest digest'
grep -Fq -- '--certificate-identity "$certificate_identity"' .github/workflows/build.yml ||
  fail 'release workflow must verify the exact GitHub workflow certificate identity'
for claim in repository ref sha name trigger; do
  grep -Fq -- "--certificate-github-workflow-${claim}" .github/workflows/build.yml ||
    fail "release signature verification must bind the GitHub workflow ${claim} claim"
done
grep -Fq -- '-a "git_sha=${GITHUB_SHA}"' .github/workflows/build.yml ||
  fail 'release signature must bind the audited commit annotation'
grep -Fq -- '-a "tag=${GITHUB_REF_NAME}"' .github/workflows/build.yml ||
  fail 'release signature must bind the protected release tag annotation'
grep -Fq 'candidate_source_version=' .github/workflows/build.yml ||
  fail 'normal release publication must ignore invalid higher-version tags'
grep -Fq 'NEWAPI_TOOLS_REF=${candidate_tag} \\' .github/workflows/build.yml ||
  fail 'normal release publication must require the candidate installer ref marker'
grep -Fq 'Run Go race detector' .github/workflows/release-recovery.yml ||
  fail 'release recovery must repeat the Go race quality gate'
grep -Fq 'npm audit --audit-level=high' .github/workflows/release-recovery.yml ||
  fail 'release recovery must audit the complete dependency graph for high severity vulnerabilities'
grep -Fq 'npm audit --omit=dev --audit-level=moderate' .github/workflows/release-recovery.yml ||
  fail 'release recovery must repeat the stricter production dependency audit'
grep -Fq 'in-toto.io/predicate-type' .github/workflows/release-recovery.yml ||
  fail 'release recovery must verify SBOM and provenance attestations'

extract_command_block() {
  local file="$1" marker="$2" terminal="$3"
  awk -v marker="$marker" -v terminal="$terminal" '
    index($0, marker) { capture = 1 }
    capture { print }
    capture && index($0, terminal) { exit }
  ' "$file"
}

for workflow in .github/workflows/build.yml .github/workflows/release-recovery.yml; do
  grep -Fq 'bash tests/cosign_v3_cli_smoke_test.sh' "$workflow" ||
    fail "$workflow must execute the real pinned Cosign CLI surface gate"
  grep -Fq 'cosign attest --yes' "$workflow" ||
    fail "$workflow must publish signed SLSA provenance for the immutable manifest"
  grep -Fq -- '--type slsaprovenance1' "$workflow" ||
    fail "$workflow must use the SLSA provenance v1 predicate type"
  grep -Fq 'cosign verify-attestation' "$workflow" ||
    fail "$workflow must consume and verify the signed provenance it publishes"
  grep -Fq 'platform_digests' "$workflow" ||
    fail "$workflow provenance must bind both platform digests"
  grep -Fq 'manifest_digest' "$workflow" ||
    fail "$workflow provenance must bind the immutable manifest digest"
  grep -Fq 'resolvedDependencies' "$workflow" ||
    fail "$workflow provenance must bind its Git source dependency"
  grep -Fq 'policy_file="$(mktemp --suffix=.cue)"' "$workflow" ||
    fail "$workflow must give Cosign v3 an explicit CUE policy suffix"
  grep -Fq 'platform_digests: close({' "$workflow" ||
    fail "$workflow provenance policy must reject extra platform keys"

  sign_block="$(extract_command_block "$workflow" 'cosign sign --yes' '"$subject"')"
  verify_block="$(extract_command_block "$workflow" 'cosign verify ' '"$subject"')"
  attest_block="$(extract_command_block "$workflow" 'cosign attest --yes' '"$subject"')"
  verify_attestation_block="$(extract_command_block "$workflow" 'cosign verify-attestation' '"$subject"')"
  for signature_block in "$sign_block" "$verify_block"; do
    grep -Fq -- '-a "git_sha=' <<< "$signature_block" ||
      fail "$workflow signature command must bind the target commit annotation"
    grep -Fq -- '-a "tag=' <<< "$signature_block" ||
      fail "$workflow signature command must bind the target tag annotation"
  done
  for attestation_block in "$attest_block" "$verify_attestation_block"; do
    [[ -n "$attestation_block" ]] || fail "$workflow attestation command block could not be isolated"
    if grep -Eq '^[[:space:]]+-a[[:space:]]' <<< "$attestation_block"; then
      fail "$workflow must not pass unsupported annotation flags to Cosign v3 attestation commands"
    fi
  done
done
for script in install.sh deploy.sh; do
  grep -Fq 'verify-attestation' "$script" ||
    fail "$script must verify signed release provenance before activation"
  grep -Fq -- '--type slsaprovenance1' "$script" ||
    fail "$script must require SLSA provenance v1"
  grep -Fq 'platform_digests' "$script" ||
    fail "$script provenance policy must require amd64 and arm64 digests"
  grep -Fq 'manifest_digest:' "$script" ||
    fail "$script provenance policy must bind the requested manifest digest"
  grep -Fq 'platform_digests: close({' "$script" ||
    fail "$script provenance policy must bind the exact manifest platform digests"
  grep -Fq 'policy_file="$(mktemp --suffix=.cue)"' "$script" ||
    fail "$script must give a local Cosign v3 runner an explicit CUE policy suffix"
  grep -Fq 'refs/heads/main' "$script" ||
    fail "$script must recognize the protected-main recovery signer profile"

  runner_prefix="run_${script%.sh}_cosign"
  signature_block="$(extract_command_block "$script" "${runner_prefix} verify " '"$image"')"
  provenance_block="$(extract_command_block "$script" "${runner_prefix} verify-attestation" '"$image"')"
  grep -Fq -- '-a "git_sha=${git_sha}"' <<< "$signature_block" ||
    fail "$script signature verification must retain the target commit annotation"
  grep -Fq -- '-a "tag=${tag}"' <<< "$signature_block" ||
    fail "$script signature verification must retain the target tag annotation"
  if grep -Eq '^[[:space:]]+-a[[:space:]]' <<< "$provenance_block"; then
    fail "$script must not pass unsupported annotation flags to Cosign v3 verify-attestation"
  fi
done
grep -Fq 'expected_manifest_digest:' .github/workflows/release-recovery.yml ||
  fail 'release recovery must require an operator-supplied immutable manifest digest'
grep -Fq '[[ "${GITHUB_REF}" == "refs/heads/main" ]]' .github/workflows/release-recovery.yml ||
  fail 'release recovery must dispatch only from the protected main branch'
grep -Fq 'RECOVERY_WORKFLOW_SHA: ${{ github.workflow_sha }}' .github/workflows/release-recovery.yml ||
  fail 'release recovery must bind the actual protected workflow revision'
grep -Fq 'ref: ${{ github.workflow_sha }}' .github/workflows/release-recovery.yml ||
  fail 'release recovery validation must checkout the workflow revision, not the target tag'
grep -Fq 'refs/recovery/tags/${tag}' .github/workflows/release-recovery.yml ||
  fail 'release recovery must fetch the target tag into a private validation namespace'
grep -Fq '"$version_digest" == "$EXPECTED_MANIFEST_DIGEST"' .github/workflows/release-recovery.yml ||
  fail 'release recovery must reject an existing manifest that differs from the recorded digest'
grep -Fq '"$final_version_digest" == "$EXPECTED_MANIFEST_DIGEST"' .github/workflows/release-recovery.yml ||
  fail 'release recovery must recheck the exact manifest after Sigstore writes'
if grep -Fq 'docker/build-push-action@' .github/workflows/release-recovery.yml; then
  fail 'release recovery must not rebuild or push replacement platform images'
fi
grep -Fq 'registry_manifest()' .github/workflows/release-recovery.yml ||
  fail 'release recovery must read attestation manifests through the registry API'
grep -Fq 'registry_reference_digest()' .github/workflows/release-recovery.yml ||
  fail 'release recovery must inspect the exact tag through the registry API'
if grep -Eiq 'manifest unknown|could not determine whether the (release image tag|minor alias) exists' \
  .github/workflows/release-recovery.yml; then
  fail 'release recovery must not infer registry status from Buildx error text'
fi
grep -Fq 'invalid attestation manifest digest' .github/workflows/release-recovery.yml ||
  fail 'release recovery must validate attestation digest syntax before registry access'
grep -Fiq 'docker-content-digest:' .github/workflows/release-recovery.yml ||
  fail 'release recovery must verify the registry content digest header'
grep -Fq 'sha256sum "$destination"' .github/workflows/release-recovery.yml ||
  fail 'release recovery must verify the raw attestation manifest body digest'
grep -Fq 'attestation manifest was not found' .github/workflows/release-recovery.yml ||
  fail 'release recovery must distinguish missing attestations from other registry errors'
grep -Fq '"${image}@${version_digest}"' .github/workflows/release-recovery.yml ||
  fail 'release recovery must sign and attest only the exact immutable manifest digest'
[[ "$(grep -Fc '[[ "$current_version_digest" == "$version_digest" ]]' \
  .github/workflows/release-recovery.yml)" -ge 1 ]] ||
  fail 'release recovery must recheck the pinned exact digest before Sigstore writes'
grep -Fq '.schemaVersion == 2' .github/workflows/release-recovery.yml ||
  fail 'release recovery must validate OCI schema version 2 manifests'
grep -Fq 'umask 077' .github/workflows/release-recovery.yml ||
  fail 'release recovery must protect temporary credential and manifest files'
if grep -Fq \
  'docker buildx imagetools inspect --raw "${image}@${attestation_digest}"' \
  .github/workflows/release-recovery.yml; then
  fail 'release recovery must not use Buildx to decode attestation manifests'
fi

release_alias_line="$(grep -nF 'id: release_alias' .github/workflows/build.yml |
  tail -n 1 | cut -d: -f1)"
release_metadata_line="$(grep -nF 'id: meta' .github/workflows/build.yml |
  tail -n 1 | cut -d: -f1)"
[[ -n "$release_alias_line" && -n "$release_metadata_line" &&
   "$release_alias_line" -lt "$release_metadata_line" ]] ||
  fail 'minor alias eligibility must be decided before release metadata tags are generated'

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

root_commit="$(tr -d '\r' < Dockerfile | sed -n 's/^ARG GEOIP_SOURCE_COMMIT=//p')"
backend_commit="$(tr -d '\r' < backend/Dockerfile | sed -n 's/^ARG GEOIP_SOURCE_COMMIT=//p')"
runtime_commit="$(tr -d '\r' < backend/internal/service/ip_geo.go | sed -n 's/^[[:space:]]*geoipDatabaseCommit = "\([0-9a-f]\{40\}\)"/\1/p')"
root_checksum="$(tr -d '\r' < Dockerfile | sed -n 's/^ARG GEOIP_SHA256=//p')"
backend_checksum="$(tr -d '\r' < backend/Dockerfile | sed -n 's/^ARG GEOIP_SHA256=//p')"
runtime_checksum="$(tr -d '\r' < backend/internal/service/ip_geo.go | sed -n 's/^[[:space:]]*geoipDatabaseSHA256 = "\([0-9a-f]\{64\}\)"/\1/p')"
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
