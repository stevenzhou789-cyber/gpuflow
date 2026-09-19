#!/usr/bin/env bash
# Only attach a missing tag to the already scanned index; existing identities
# are immutable. The CI resource/concurrency lock serializes this repository.
set -euo pipefail
source_ref=${1:?immutable source}; target=${2:?destination tag}; builder=${3:-}
[[ "$source_ref" =~ @sha256:[a-f0-9]{64}$ && "$target" != *@* && "$target" != -* && "$target" != *[[:space:]]* ]] || exit 1
expected=${source_ref##*@}
command=(docker buildx); [[ -z "$builder" ]] || command+=(--builder "$builder")
command+=(imagetools)
work=$(mktemp -d); trap 'rm -rf -- "$work"' EXIT
if "${command[@]}" inspect "$target" --format '{{json .Manifest}}' > "$work/current.json" 2> "$work/error"; then
  [[ $(jq -er '.digest' "$work/current.json") == "$expected" ]] || { echo 'Refusing to replace an existing image tag with another digest.' >&2; exit 1; }
  exit 0
fi
{ grep -Eiq 'manifest unknown|404 Not Found' "$work/error" || grep -Fq -- "$target: not found" "$work/error"; } || { echo 'Cannot establish tag absence; promotion refused.' >&2; exit 1; }
"${command[@]}" create --tag "$target" --metadata-file "$work/promoted.json" "$source_ref"
[[ $(jq -er '."containerimage.descriptor".digest' "$work/promoted.json") == "$expected" ]] || { echo 'Promotion changed the scanned digest.' >&2; exit 1; }
"${command[@]}" inspect "$target" --format '{{json .Manifest}}' > "$work/verified.json"
[[ $(jq -er '.digest' "$work/verified.json") == "$expected" ]] || { echo 'Published tag differs from scanned digest.' >&2; exit 1; }
