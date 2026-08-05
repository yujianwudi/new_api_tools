#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  printf 'Cosign v3 compatibility check failed: %s\n' "$1" >&2
  exit 1
}

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
    fail "$workflow does not execute the real pinned Cosign CLI surface gate"
  grep -Fq 'bash tests/cue_policy_smoke_test.sh' "$workflow" ||
    fail "$workflow does not evaluate CUE policy positive and negative fixtures"
  grep -Fq 'policy_file="$(mktemp --suffix=.cue)"' "$workflow" ||
    fail "$workflow does not give the CUE policy an explicit suffix"
  grep -Fq 'platform_digests: close({' "$workflow" ||
    fail "$workflow does not close the platform digest policy"

  sign_block="$(extract_command_block "$workflow" 'cosign sign --yes' '"$subject"')"
  verify_block="$(extract_command_block "$workflow" 'cosign verify ' '"$subject"')"
  attest_block="$(extract_command_block "$workflow" 'cosign attest --yes' '"$subject"')"
  verify_attestation_block="$(extract_command_block "$workflow" 'cosign verify-attestation' '"$subject"')"
  for signature_block in "$sign_block" "$verify_block"; do
    grep -Fq -- '-a "git_sha=' <<< "$signature_block" ||
      fail "$workflow signature command lost the target commit annotation"
    grep -Fq -- '-a "tag=' <<< "$signature_block" ||
      fail "$workflow signature command lost the target tag annotation"
  done
  for attestation_block in "$attest_block" "$verify_attestation_block"; do
    [[ -n "$attestation_block" ]] || fail "$workflow attestation block could not be isolated"
    if grep -Eq '^[[:space:]]+-a[[:space:]]' <<< "$attestation_block"; then
      fail "$workflow passes unsupported annotations to a Cosign v3 attestation command"
    fi
  done
done

for script in install.sh deploy.sh; do
  runner_prefix="run_${script%.sh}_cosign"
  signature_block="$(extract_command_block "$script" "${runner_prefix} verify " '"$image"')"
  provenance_block="$(extract_command_block "$script" "${runner_prefix} verify-attestation" '"$image"')"
  grep -Fq -- '-a "git_sha=${git_sha}"' <<< "$signature_block" ||
    fail "$script signature verification lost the commit annotation"
  grep -Fq -- '-a "tag=${tag}"' <<< "$signature_block" ||
    fail "$script signature verification lost the tag annotation"
  if grep -Eq '^[[:space:]]+-a[[:space:]]' <<< "$provenance_block"; then
    fail "$script passes unsupported annotations to Cosign v3 verify-attestation"
  fi
  grep -Fq 'policy_file="$(mktemp --suffix=.cue)"' "$script" ||
    fail "$script does not give local Cosign a CUE policy suffix"
  grep -Fq 'platform_digests: close({' "$script" ||
    fail "$script does not bind the exact two child manifest digests"
  grep -Fq 'refs/heads/main' "$script" ||
    fail "$script does not recognize the protected-main recovery identity"
done

recovery=.github/workflows/release-recovery.yml
grep -Fq 'expected_manifest_digest:' "$recovery" ||
  fail 'recovery does not require the recorded immutable manifest digest'
grep -Fq 'protected-main recovery is supported only for v0.6.2 and newer releases' "$recovery" ||
  fail 'recovery does not reject legacy tags whose consumers cannot trust the main signer profile'
grep -Fq '[[ "${GITHUB_REF}" == "refs/heads/main" ]]' "$recovery" ||
  fail 'recovery is not restricted to the protected main branch'
grep -Fq 'RECOVERY_WORKFLOW_SHA: ${{ github.workflow_sha }}' "$recovery" ||
  fail 'recovery does not bind its real workflow revision'
grep -Fq 'refs/recovery/tags/${tag}' "$recovery" ||
  fail 'recovery does not isolate the fetched target tag'
grep -Fq '"$version_digest" == "$EXPECTED_MANIFEST_DIGEST"' "$recovery" ||
  fail 'recovery can sign a manifest other than the recorded digest'
grep -Fq '"$final_version_digest" == "$EXPECTED_MANIFEST_DIGEST"' "$recovery" ||
  fail 'recovery does not recheck the exact manifest after Sigstore writes'
if grep -Fq 'docker buildx imagetools create' "$recovery" ||
   grep -Fq 'docker/build-push-action@' "$recovery"; then
  fail 'recovery can create or replace release images'
fi

printf 'Cosign v3 compatibility checks passed\n'
