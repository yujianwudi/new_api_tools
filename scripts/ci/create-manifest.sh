#!/usr/bin/env bash
set -euo pipefail

digest_dir="${1:-/tmp/digests}"
: "${DOCKER_METADATA_OUTPUT_JSON:?Docker metadata output is required}"
: "${REGISTRY:?Container registry is required}"
: "${IMAGE_NAME:?Container image name is required}"

command -v docker >/dev/null 2>&1 || {
  echo 'docker is required to create the manifest list' >&2
  exit 1
}
command -v jq >/dev/null 2>&1 || {
  echo 'jq is required to parse Docker metadata' >&2
  exit 1
}
[[ -d "$digest_dir" && ! -L "$digest_dir" ]] || {
  echo "digest directory is missing or unsafe: $digest_dir" >&2
  exit 1
}

mapfile -t manifest_tags < <(jq -er '.tags[]' <<< "$DOCKER_METADATA_OUTPUT_JSON")
(( ${#manifest_tags[@]} > 0 )) || {
  echo 'Docker metadata did not produce any manifest tags' >&2
  exit 1
}

tag_args=()
for tag in "${manifest_tags[@]}"; do
  [[ "$tag" == "${REGISTRY}/${IMAGE_NAME}:"* && "$tag" != *[[:space:]]* ]] || {
    echo "Docker metadata produced an unexpected manifest tag: $tag" >&2
    exit 1
  }
  tag_args+=('-t' "$tag")
done

shopt -s nullglob
digest_files=("$digest_dir"/*)
(( ${#digest_files[@]} == 2 )) || {
  echo "expected exactly two platform digests, got ${#digest_files[@]}" >&2
  exit 1
}

sources=()
for digest_file in "${digest_files[@]}"; do
  digest="${digest_file##*/}"
  [[ -f "$digest_file" && ! -L "$digest_file" && "$digest" =~ ^[0-9a-f]{64}$ ]] || {
    echo "invalid platform digest file: $digest_file" >&2
    exit 1
  }
  sources+=("${REGISTRY}/${IMAGE_NAME}@sha256:${digest}")
done

docker buildx imagetools create "${tag_args[@]}" "${sources[@]}"
