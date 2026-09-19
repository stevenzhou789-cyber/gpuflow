#!/usr/bin/env bash
set -euo pipefail
set +x
umask 077
: "${CI_JOB_ID:?}" "${CI_REGISTRY:?}" "${CI_REGISTRY_USER:?}" "${CI_REGISTRY_PASSWORD:?}" "${BUILDKIT_IMAGE:?}"
[[ "$CI_REGISTRY" == gitlab.gpuflow.test:5055 && "${DOCKER_HOST:-}" == tcp://builder:2375 && "$BUILDKIT_IMAGE" =~ @sha256:[a-f0-9]{64}$ ]] || exit 1
python3 scripts/gitlab-release.py check
work=$(mktemp -d); export DOCKER_CONFIG="$work/docker"
mkdir "$DOCKER_CONFIG"
builder="gpuflow-ce-publish-$CI_JOB_ID-$RANDOM"
created=false
cleanup() { if [[ "$created" == true ]]; then docker buildx rm "$builder" >/dev/null 2>&1 || true; fi; rm -rf -- "$work"; }
trap cleanup EXIT
printf '%s' "$CI_REGISTRY_PASSWORD" | docker login "$CI_REGISTRY" --username "$CI_REGISTRY_USER" --password-stdin
printf '[registry."%s"]\n  http = true\n' "$CI_REGISTRY" > "$work/buildkitd.toml"
docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" --buildkitd-config "$work/buildkitd.toml"
created=true
export BUILDX_BUILDER="$builder"
# No build or bootstrap: this resolver promotes the already verified index.
python3 scripts/gitlab-release.py publish
