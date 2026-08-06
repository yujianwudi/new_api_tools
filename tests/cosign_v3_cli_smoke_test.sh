#!/usr/bin/env bash
set -euo pipefail

fail() {
  printf 'Cosign v3 CLI smoke check failed: %s\n' "$1" >&2
  exit 1
}

command -v cosign >/dev/null 2>&1 || fail 'cosign is not installed'
cosign_version="$(cosign version 2>&1 | awk '$1 == "GitVersion:" { print $2 }')"
[[ "$cosign_version" == 'v3.1.2' ]] ||
  fail "workflow installed Cosign ${cosign_version:-unknown}, expected exactly v3.1.2"

sign_help="$(cosign sign --help)"
verify_help="$(cosign verify --help)"
attest_help="$(cosign attest --help)"
verify_attestation_help="$(cosign verify-attestation --help)"

grep -Fq -- '--annotations' <<< "$sign_help" || fail 'cosign sign no longer exposes signature annotations'
grep -Fq -- '--annotations' <<< "$verify_help" || fail 'cosign verify no longer exposes signature annotations'
if grep -Fq -- '--annotations' <<< "$attest_help"; then
  fail 'cosign attest unexpectedly exposes annotations; review the command contract before changing workflows'
fi
if grep -Fq -- '--annotations' <<< "$verify_attestation_help"; then
  fail 'cosign verify-attestation unexpectedly exposes annotations; review the command contract before changing workflows'
fi
grep -Fq -- '--policy' <<< "$verify_attestation_help" ||
  fail 'cosign verify-attestation no longer exposes policy verification'

printf 'Cosign v3.1.2 CLI surface checks passed\n'
