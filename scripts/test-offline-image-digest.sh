#!/usr/bin/env bash
# Small REAL save/load test against the dedicated builder; never host Docker.
# Requires a preloaded pinned Alpine image. No pulls, signing keys or app build.
set -Eeuo pipefail
set +x
umask 077
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/offline-image-lib.sh"
source=${1:?PRELOADED_ALPINE_IMAGE_AT_DIGEST}
[[ ${DOCKER_HOST:-} == tcp://builder:2375 ]] || { echo 'Only the dedicated builder is allowed.' >&2; exit 1; }
[[ "$source" =~ ^alpine(@sha256:|:[a-zA-Z0-9._-]+@sha256:)[0-9a-f]{64}$ ]] || exit 1
unset DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH
for command in docker tar sha256sum jq; do command -v "$command" >/dev/null; done
docker image inspect --platform linux/amd64 "$source" >/dev/null
work=$(mktemp -d -t gpuflow-real-image-test.XXXXXXXX)
reference="gpuflow-offline/config-compat-test:test-$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
created=false
cleanup() {
  local result=$?
  trap - EXIT
  if [[ "$created" == true ]] && docker image inspect "$reference" >/dev/null 2>&1; then
    docker image rm "$reference" >/dev/null || result=1
  fi
  rm -rf -- "$work"
  exit "$result"
}
trap cleanup EXIT
if docker image inspect "$reference" >/dev/null 2>&1; then echo 'Refusing to adopt an existing test tag.' >&2; exit 1; fi
docker image tag "$source" "$reference"
created=true
reported_id=$(docker image inspect --platform linux/amd64 --format '{{.Id}}' "$reference")
docker image save --platform linux/amd64 --output "$work/first.tar" "$reference"
# Independent JSON parsing validates the Bash-only production extractor.
config_path=$(tar -xOf "$work/first.tar" manifest.json | jq -er --arg ref "$reference" '[.[] | select(.RepoTags | index($ref))] | select(length == 1) | .[0].Config')
expected="sha256:$(tar -xOf "$work/first.tar" "$config_path" | sha256sum | awk '{print $1}')"
[[ $(offline_archive_config_digest "$work/first.tar" "$reference") == "$expected" ]]
[[ $(offline_image_config_digest "$reference" amd64) == "$expected" ]]
docker image rm "$reference" >/dev/null
if docker image inspect "$reference" >/dev/null 2>&1; then echo 'The temporary tag was not removed.' >&2; exit 1; fi
docker load --input "$work/first.tar" >/dev/null
[[ $(offline_image_config_digest "$reference" amd64) == "$expected" ]]
printf 'PASS: real Docker save/delete-test-tag/load/save config identity; inspect ID=%s; config digest=%s. Docker 24 archive/API compatibility is separately mock-tested, not a Docker 24 daemon claim.\n' "$reported_id" "$expected"
