#!/bin/sh
# Docker's CLI image provides BusyBox ash with pipefail support.
set -eu
set -o pipefail

mkdir -p gitlab-logs gitlab-artifacts

# Tags MUST NOT silently produce an unsigned substitute for a signed release.
# The existing GitHub release/signing workflow remains the release authority.
if [ -n "${CI_COMMIT_TAG:-}" ]; then
  printf '%s\n' 'REFUSED: GitLab tag release signing and approval gates are not configured. No release image or package was published.' | tee gitlab-logs/release-gate.log
  exit 1
fi

: "${CI_COMMIT_SHA:?CI_COMMIT_SHA is required}"
: "${CI_REGISTRY:?GitLab Container Registry must be enabled}"
: "${CI_REGISTRY_IMAGE:?CI_REGISTRY_IMAGE is required}"
: "${CI_REGISTRY_USER:?CI_REGISTRY_USER is required}"
: "${CI_REGISTRY_PASSWORD:?CI_REGISTRY_PASSWORD is required}"
: "${DOCKER_HOST:?An isolated Docker builder must be configured by the runner}"
case "$CI_COMMIT_SHA" in
  *[!0-9a-f]*|'') printf '%s\n' 'Invalid commit SHA.' >&2; exit 1 ;;
esac
if [ "${#CI_COMMIT_SHA}" -ne 40 ]; then
  printf '%s\n' 'Expected a full 40-character commit SHA.' >&2
  exit 1
fi

image_ref="${CI_REGISTRY_IMAGE}:development-unsigned-${CI_COMMIT_SHA}"
# Authentication stays in an ephemeral directory, excluded from artifacts.
docker_auth_dir="$(mktemp -d)"
export DOCKER_CONFIG="$docker_auth_dir"
trap 'rm -f "$docker_auth_dir/config.json"; rmdir "$docker_auth_dir" 2>/dev/null || true' EXIT
printf '%s' "$CI_REGISTRY_PASSWORD" | docker login "$CI_REGISTRY" --username "$CI_REGISTRY_USER" --password-stdin

# pipefail prevents tee from masking failed tests, compilation or registry push.
docker buildx build --platform linux/amd64 --progress plain \
  --file scripts/gitlab-build.Dockerfile --target artifacts \
  --build-arg "VCS_REF=$CI_COMMIT_SHA" \
  --output type=local,dest=gitlab-artifacts . 2>&1 | tee gitlab-logs/verify-and-package.log

docker buildx build --platform linux/amd64 --progress plain \
  --file scripts/gitlab-build.Dockerfile --target runtime \
  --build-arg "VCS_REF=$CI_COMMIT_SHA" \
  --tag "$image_ref" --push . 2>&1 | tee gitlab-logs/image-build-and-push.log

printf 'image=%s\nchannel=development\nsigned=false\nrelease_approved=false\n' "$image_ref" > gitlab-artifacts/IMAGE.txt
printf 'Unsigned development archives and image built for %s. Do not use these as a signed release.\n' "$CI_COMMIT_SHA"
