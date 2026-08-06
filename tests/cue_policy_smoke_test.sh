#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

fail() {
  printf 'CUE policy smoke check failed: %s\n' "$1" >&2
  exit 1
}

command -v cosign >/dev/null 2>&1 || fail 'cosign is not installed'
command -v go >/dev/null 2>&1 || fail 'go is not installed'
command -v jq >/dev/null 2>&1 || fail 'jq is not installed'
command -v python3 >/dev/null 2>&1 || fail 'python3 is not installed'

cosign_version="$(cosign version 2>&1 | awk '$1 == "GitVersion:" { print $2 }')"
[[ "$cosign_version" == 'v3.1.2' ]] ||
  fail "expected Cosign v3.1.2, got ${cosign_version:-unknown}"

work_dir="$(mktemp -d)"
trap 'rm -rf -- "$work_dir"' EXIT

# Match the CUE engine embedded by Cosign v3.1.2 to avoid evaluator drift.
GOBIN="$work_dir/bin" go install cuelang.org/go/cmd/cue@v0.16.1
cue_bin="$work_dir/bin/cue"
cue_version="$($cue_bin version | awk '$1 == "cue" && $2 == "version" { print $3 }')"
[[ "$cue_version" == 'v0.16.1' ]] ||
  fail "expected CUE v0.16.1, got ${cue_version:-unknown}"

policy=tests/fixtures/cosign_slsa_policy.cue
valid=tests/fixtures/cosign_slsa_statement.json
forged=tests/fixtures/cosign_slsa_forged_platform.json
extra=tests/fixtures/cosign_slsa_extra_platform.json

"$cue_bin" vet -c "$policy" "$valid"
if "$cue_bin" vet -c "$policy" "$forged" >/dev/null 2>&1; then
  fail 'CUE policy accepted a different valid amd64 digest'
fi
if "$cue_bin" vet -c "$policy" "$extra" >/dev/null 2>&1; then
  fail 'CUE policy accepted an unexpected third platform'
fi

# Keep every artifact created by this drill inside the one bounded temporary
# directory.  The deliberately unreachable proxy proves the key-based path
# does not contact Fulcio, Rekor, a TSA, or any registry.
offline_cosign() {
  HTTP_PROXY=http://127.0.0.1:9 \
    HTTPS_PROXY=http://127.0.0.1:9 \
    ALL_PROXY=http://127.0.0.1:9 \
    NO_PROXY= \
    cosign "$@"
}

artifact="$work_dir/artifact.txt"
wrong_artifact="$work_dir/wrong-artifact.txt"
predicate="$work_dir/predicate.json"
signing_config="$work_dir/offline-signing-config.json"
trusted_root="$work_dir/offline-trusted-root.json"
key_prefix="$work_dir/smoke"
bundle="$work_dir/attestation.sigstore.json"
statement="$work_dir/statement.json"
tampered_bundle="$work_dir/tampered.sigstore.json"

printf '%s\n' 'new-api-tools offline Cosign policy smoke artifact' > "$artifact"
printf '%s\n' 'different artifact content' > "$wrong_artifact"
jq -e '.predicate' "$valid" > "$predicate"

offline_cosign signing-config create --out "$signing_config"
offline_cosign trusted-root create --out "$trusted_root"
COSIGN_PASSWORD='ci-policy-smoke-only' \
  offline_cosign generate-key-pair --output-key-prefix "$key_prefix"
COSIGN_PASSWORD='ci-policy-smoke-only' \
  offline_cosign attest-blob --yes \
    --signing-config "$signing_config" \
    --trusted-root "$trusted_root" \
    --key "${key_prefix}.key" \
    --type slsaprovenance1 \
    --predicate "$predicate" \
    --bundle "$bundle" \
    "$artifact"

offline_cosign verify-blob-attestation \
  --insecure-ignore-tlog \
  --trusted-root "$trusted_root" \
  --key "${key_prefix}.pub" \
  --bundle "$bundle" \
  --type slsaprovenance1 \
  "$artifact"

if offline_cosign verify-blob-attestation \
  --insecure-ignore-tlog \
  --trusted-root "$trusted_root" \
  --key "${key_prefix}.pub" \
  --bundle "$bundle" \
  --type slsaprovenance1 \
  "$wrong_artifact" >/dev/null 2>&1; then
  fail 'Cosign accepted an attestation whose in-toto subject digest did not match the blob'
fi

jq -er '.dsseEnvelope.payload' "$bundle" | base64 --decode > "$statement"
artifact_digest="$(sha256sum "$artifact" | awk '{ print $1 }')"
artifact_name="$(basename "$artifact")"
jq -e \
  --arg name "$artifact_name" \
  --arg digest "$artifact_digest" \
  '._type == "https://in-toto.io/Statement/v0.1" and
   .predicateType == "https://slsa.dev/provenance/v1" and
   (.subject | length == 1) and
   .subject[0].name == $name and
   .subject[0].digest.sha256 == $digest' \
  "$statement" >/dev/null
"$cue_bin" vet -c tests/fixtures/cosign_slsa_blob_policy.cue "$statement"

tampered_payload="$(
  jq -c '.predicate.buildDefinition.externalParameters.tag = "v9.9.9-forged"' "$statement" |
    base64 --wrap=0
)"
jq --arg payload "$tampered_payload" '.dsseEnvelope.payload = $payload' \
  "$bundle" > "$tampered_bundle"
if offline_cosign verify-blob-attestation \
  --insecure-ignore-tlog \
  --trusted-root "$trusted_root" \
  --key "${key_prefix}.pub" \
  --bundle "$tampered_bundle" \
  --type slsaprovenance1 \
  "$artifact" >/dev/null 2>&1; then
  fail 'Cosign accepted a bundle whose signed DSSE payload was modified'
fi

python3 tests/workflow_cue_policy_smoke.py \
  --cue "$cue_bin" \
  --work-dir "$work_dir/workflow-policies"

printf 'Cosign v3.1.2 offline attestation and CUE v0.16.1 policy smoke checks passed\n'
