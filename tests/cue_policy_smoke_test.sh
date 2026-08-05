#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

cue_bin_dir="$(mktemp -d)"
trap 'rm -rf -- "$cue_bin_dir"' EXIT
# Match the CUE engine embedded by Cosign v3.1.2 to avoid evaluator drift.
GOBIN="$cue_bin_dir" go install cuelang.org/go/cmd/cue@v0.16.1
cue_bin="${cue_bin_dir}/cue"

policy=tests/fixtures/cosign_slsa_policy.cue
valid=tests/fixtures/cosign_slsa_statement.json
forged=tests/fixtures/cosign_slsa_forged_platform.json
extra=tests/fixtures/cosign_slsa_extra_platform.json

"$cue_bin" vet -c "$policy" "$valid"
if "$cue_bin" vet -c "$policy" "$forged" >/dev/null 2>&1; then
  echo 'CUE policy accepted a different valid amd64 digest' >&2
  exit 1
fi
if "$cue_bin" vet -c "$policy" "$extra" >/dev/null 2>&1; then
  echo 'CUE policy accepted an unexpected third platform' >&2
  exit 1
fi

printf 'CUE provenance policy positive and negative fixtures passed\n'
