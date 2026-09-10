#!/usr/bin/env bash
# Portable image identity: hash the config bytes in Docker's save archive.
# Docker 29's inspect .Id may name an index/manifest, not the config used by
# classic Docker, so it must never be used as the signed offline identity.

offline_archive_config_digest() {
  local archive=$1 image=$2 manifest config_path path_digest actual_digest
  [[ "$image" =~ ^[a-zA-Z0-9][a-zA-Z0-9._:/-]*:[a-zA-Z0-9._-]+$ ]] || return 1
  manifest=$(tar -xOf "$archive" manifest.json) || return 1
  # Docker-generated manifest objects contain Config and RepoTags before any
  # nested LayerSources objects. Match an exact JSON string, never a substring
  # of another tag; multiple platform records for that tag fail closed.
  config_path=$(printf '%s\n' "$manifest" | awk -v reference="\"$image\"" '
    BEGIN { RS="\\{" }
    index($0, reference) {
      if (match($0, /"Config"[[:space:]]*:[[:space:]]*"[^"]+"/)) {
        value=substr($0, RSTART, RLENGTH)
        sub(/^"Config"[[:space:]]*:[[:space:]]*"/, "", value)
        sub(/"$/, "", value)
        print value
        count++
      }
    }
    END { if (count != 1) exit 1 }
  ') || return 1
  [[ "$config_path" =~ ^([0-9a-f]{64}\.json|blobs/sha256/[0-9a-f]{64})$ ]] || return 1
  path_digest=${config_path##*/}; path_digest=${path_digest%.json}
  actual_digest=$(tar -xOf "$archive" "$config_path" | sha256sum | awk '{print $1}') || return 1
  [[ "$actual_digest" == "$path_digest" ]] || return 1
  printf 'sha256:%s\n' "$actual_digest"
}

offline_image_config_digest() (
  set -Eeuo pipefail
  local image=$1 architecture=$2 versions client_version server_version platform work
  local platform_args=()
  [[ "$architecture" == amd64 || "$architecture" == arm64 ]] || return 1
  versions=$(docker version --format '{{.Client.Version}} {{.Server.Version}}') || return 1
  read -r client_version server_version <<< "$versions"
  [[ "$client_version" =~ ^[0-9]+\. && "$server_version" =~ ^[0-9]+\. ]] || return 1
  # The validated Docker 29 path selects a platform for BOTH inspect and save.
  # Older classic daemons (including Docker 24) only hold one platform per tag;
  # their portable archive is checked instead of sending unsupported options.
  if (( ${client_version%%.*} >= 29 && ${server_version%%.*} >= 29 )); then
    platform_args=(--platform "linux/$architecture")
  fi
  platform=$(docker image inspect "${platform_args[@]}" --format '{{.Os}}/{{.Architecture}}' "$image") || return 1
  [[ "$platform" == "linux/$architecture" ]] || return 1
  work=$(mktemp -d -t gpuflow-image-identity.XXXXXXXX) || return 1
  trap 'rm -rf -- "$work"' EXIT
  docker image save "${platform_args[@]}" --output "$work/image.tar" "$image" || return 1
  offline_archive_config_digest "$work/image.tar" "$image"
)
