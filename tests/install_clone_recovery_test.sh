#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=../install.sh
source "${REPO_ROOT}/install.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

TEST_TMP="$(mktemp -d)"
case "$TEST_TMP" in
  /tmp/*) ;;
  *) fail "unsafe clone-test directory: $TEST_TMP" ;;
esac
trap 'case "$TEST_TMP" in /tmp/*) rm -rf -- "$TEST_TMP" ;; esac' EXIT

source_repo="${TEST_TMP}/source"
mkdir -p "${source_repo}/scripts"
git -C "$source_repo" init -q -b main
git -C "$source_repo" config user.name 'clone-test'
git -C "$source_repo" config user.email 'clone-test@example.invalid'
printf '#!/usr/bin/env bash\n' >"${source_repo}/deploy.sh"
printf 'services: {}\n' >"${source_repo}/docker-compose.yml"
printf '#!/usr/bin/env bash\n' >"${source_repo}/scripts/toolstore_transaction.sh"
git -C "$source_repo" add deploy.sh docker-compose.yml scripts/toolstore_transaction.sh
git -C "$source_repo" commit -q -m fixture

log_info() { :; }
log_warn() { :; }
log_error() { :; }
log_success() { :; }
checkout_install_ref() {
  git reset --hard HEAD >/dev/null
  INSTALL_COMMIT="$(git rev-parse --verify HEAD)"
}

REPO_URL="$source_repo"
PROJECT_NAME='new_api_tools'
INSTALL_REF='main'

# A crash after clone leaves only a marked same-parent staging tree. The final
# path is absent, and a retry removes only that owned tree before publishing.
INSTALL_DIR="${TEST_TMP}/after-clone"
mkdir -p "$INSTALL_DIR"
INSTALL_CLONE_FAULT_POINT=after_clone
if clone_or_update_project; then
  fail 'after_clone fault unexpectedly succeeded'
fi
unset INSTALL_CLONE_FAULT_POINT
target="${INSTALL_DIR}/${PROJECT_NAME}"
staging="$(install_clone_staging_path "$target")"
[[ ! -e "$target" && -f "${staging}/.newapi-tools-clone-staging" ]] ||
  fail 'after_clone exposed a partial final tree or lost its staging marker'
clone_or_update_project
[[ -f "${target}/deploy.sh" && -f "${target}/scripts/toolstore_transaction.sh" && ! -e "$staging" ]] ||
  fail 'retry did not atomically publish and clean the checkout'

# A crash after checkout validation still cannot expose the candidate path.
cd "$TEST_TMP"
INSTALL_DIR="${TEST_TMP}/after-checkout"
mkdir -p "$INSTALL_DIR"
INSTALL_CLONE_FAULT_POINT=after_checkout
if clone_or_update_project; then
  fail 'after_checkout fault unexpectedly succeeded'
fi
unset INSTALL_CLONE_FAULT_POINT
target="${INSTALL_DIR}/${PROJECT_NAME}"
[[ ! -e "$target" && -d "$(install_clone_staging_path "$target")/tree/.git" ]] ||
  fail 'after_checkout exposed a partial final tree'
cd "$TEST_TMP"
clone_or_update_project
[[ -f "${target}/docker-compose.yml" && ! -e "$(install_clone_staging_path "$target")" ]] ||
  fail 'after_checkout retry did not publish atomically'

# A legacy direct clone interrupted in the final path is quarantined only when
# its origin is the trusted upstream. It is never mistaken for an installation.
cd "$TEST_TMP"
INSTALL_DIR="${TEST_TMP}/legacy-partial"
mkdir -p "$INSTALL_DIR"
target="${INSTALL_DIR}/${PROJECT_NAME}"
git clone -q "$source_repo" "$target"
git -C "$target" remote set-url origin 'https://github.com/yujianwudi/new_api_tools.git'
rm -f -- "${target}/deploy.sh"
clone_or_update_project
[[ -f "${target}/deploy.sh" ]] || fail 'trusted partial checkout was not replaced atomically'
find "$INSTALL_DIR" -maxdepth 1 -type d -name '.new_api_tools.interrupted-*' -print -quit | grep -q . ||
  fail 'trusted partial checkout was not retained as quarantine evidence'

# Publication itself is atomic: a crash after rename leaves a complete target;
# only the empty marked wrapper needs cleanup on the next invocation.
cd "$TEST_TMP"
INSTALL_DIR="${TEST_TMP}/after-publish"
mkdir -p "$INSTALL_DIR"
INSTALL_CLONE_FAULT_POINT=after_publish
if clone_or_update_project; then
  fail 'after_publish fault unexpectedly succeeded'
fi
unset INSTALL_CLONE_FAULT_POINT
target="${INSTALL_DIR}/${PROJECT_NAME}"
[[ -f "${target}/deploy.sh" && -f "${target}/docker-compose.yml" ]] ||
  fail 'after_publish did not leave a complete final checkout'
cleanup_install_clone_staging "$target" || fail 'could not clean owned post-publish staging wrapper'

# When the final project is absent, an adjacent active transaction forces the
# installer to select its private recovery library instead of current/partial
# checkout code.
cd "$TEST_TMP"
missing_target="${TEST_TMP}/missing/new_api_tools"
mkdir -p "$(dirname "$missing_target")"
recovery_root="$(dirname "$missing_target")/.new_api_tools.toolstore-transactions"
mkdir -m 700 -p "$recovery_root"
printf "FORMAT='1'\n" >"${recovery_root}/active.env"
chmod 600 "${recovery_root}/active.env"
cp "${REPO_ROOT}/scripts/toolstore_transaction.sh" "${recovery_root}/recovery-library.sh"
chmod 600 "${recovery_root}/recovery-library.sh"
INSTALL_TOOLSTORE_TXN_LOADED=false
load_install_toolstore_transaction_library "$missing_target" ||
  fail 'missing project could not load external recovery library'
[[ "$TOOLSTORE_TXN_LIBRARY_SOURCE" == "${recovery_root}/recovery-library.sh" ]] ||
  fail 'installer did not prefer the external recovery library'

printf 'PASS: atomic clone, interrupted checkout, and external recovery-library checks\n'
