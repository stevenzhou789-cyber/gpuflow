#!/usr/bin/env bash
set -Eeuo pipefail
set +x
umask 077
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/gitlab-signing-lib.sh"

# Signatures are mandatory before the expensive build. Never substitute an
# unsigned/development-only artifact when migrating the GitHub release graph.
signing_key=$(gpuflow_signing_key)
: "${CI_COMMIT_SHA:?}" "${CI_REGISTRY:?}" "${CI_REGISTRY_IMAGE:?}"
: "${CI_REGISTRY_USER:?}" "${CI_REGISTRY_PASSWORD:?}" "${DOCKER_HOST:?}"
: "${SIGSTORE_TRUSTED_ROOT_FILE:?Provide the Sigstore trusted root used for offline bundle verification}"
: "${CI_JOB_ID:?}" "${BUILDKIT_IMAGE:?Provide an approved immutable BuildKit image Digest}"
: "${OFFLINE_DIND_IMAGE:?Provide the approved immutable fresh-daemon test image}" "${GPUFLOW_CI_IMAGE:?}"
[[ "$CI_COMMIT_SHA" =~ ^[0-9a-f]{40}$ ]]
[[ "$CI_JOB_ID" =~ ^[0-9]+$ && "$BUILDKIT_IMAGE" =~ @sha256:[0-9a-f]{64}$ ]]
[[ -z ${CI_COMMIT_TAG:-} ]] || { echo 'Tag publication is fail-closed; existing stable tags/assets are not replaced.' >&2; exit 1; }
[[ "${CI_COMMIT_BRANCH:-}" == "${CI_DEFAULT_BRANCH:?}" ]] || { echo 'Only the protected default branch may access release signing.' >&2; exit 1; }
[[ "$CI_REGISTRY" == gitlab.gpuflow.test:5055 && "$CI_REGISTRY_IMAGE" == "$CI_REGISTRY/gpuflow/gpuflow" && "$DOCKER_HOST" == tcp://builder:2375 ]] || { echo 'Publishing is restricted to the isolated local GitLab project and builder.' >&2; exit 1; }
for command in docker cosign git jq bash tar gzip zip sha256sum; do command -v "$command" >/dev/null; done
[[ "$(git rev-parse HEAD)" == "$CI_COMMIT_SHA" ]] || { echo 'The checked-out commit does not match CI_COMMIT_SHA.' >&2; exit 1; }
git diff --quiet && git diff --cached --quiet || { echo 'Tracked build source was modified after checkout.' >&2; exit 1; }
cosign version 2>&1 | grep -Eq 'v3\.1\.3([^0-9]|$)' || { echo 'Cosign v3.1.3 is required, matching the GitHub workflow.' >&2; exit 1; }
test -s "$SIGSTORE_TRUSTED_ROOT_FILE"
docker compose version >/dev/null
docker buildx version >/dev/null

mkdir -p gitlab-logs
bash scripts/test-gitlab-full.sh 2>&1 | tee gitlab-logs/offline-contract-tests.log
bash scripts/test-gitlab-offline-verifier.sh 2>&1 | tee gitlab-logs/offline-verifier-contract-tests.log
work=$(mktemp -d -t gpuflow-full-ci.XXXXXXXX)
builder="gpuflow-ce-$CI_JOB_ID-$RANDOM"
builder_created=false
cleanup() { if [[ "$builder_created" == true ]]; then docker buildx rm "$builder" >/dev/null 2>&1 || true; fi; docker logout "$CI_REGISTRY" >/dev/null 2>&1 || true; rm -rf -- "$work"; }
trap cleanup EXIT
version="v0.0.0-git.${CI_COMMIT_SHA:0:12}"
image="$CI_REGISTRY_IMAGE:$version"
probe="$CI_REGISTRY_IMAGE/probe:$version"
builder_args=(--builder "$builder")
registry_args=(--allow-http-registry)
export DOCKER_CONFIG="$work/docker-config"
mkdir "$DOCKER_CONFIG"
printf '%s' "$CI_REGISTRY_PASSWORD" | docker login "$CI_REGISTRY" --username "$CI_REGISTRY_USER" --password-stdin
printf '[worker.oci]\n  max-parallelism = 1\n[registry."%s"]\n  http = true\n' "$CI_REGISTRY" > "$work/buildkitd.toml"
if docker buildx inspect "$builder" >/dev/null 2>&1; then echo 'Refusing to take ownership of an existing builder.' >&2; exit 1; fi
docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$BUILDKIT_IMAGE" \
  --driver-opt network=host --driver-opt memory=1536m --driver-opt cpu-period=100000 --driver-opt cpu-quota=200000 \
  --buildkitd-config "$work/buildkitd.toml"
builder_created=true
docker buildx inspect "$builder" --bootstrap > gitlab-logs/buildkit.txt
grep -Eq 'Platforms:.*linux/arm64([,/[:space:]]|$)' gitlab-logs/buildkit.txt || { echo 'arm64 builder support is required; dropping the architecture is forbidden.' >&2; exit 1; }
cosign public-key --key "$signing_key" > "$work/cosign.pub"
printf '%s\n' "$CI_COMMIT_SHA" > "$work/signing-preflight.txt"
cosign sign-blob --yes --key "$signing_key" --bundle "$work/signing-preflight.sigstore.json" "$work/signing-preflight.txt"
cosign verify-blob --offline --trusted-root "$SIGSTORE_TRUSTED_ROOT_FILE" --key "$work/cosign.pub" \
  --bundle "$work/signing-preflight.sigstore.json" "$work/signing-preflight.txt"

docker buildx build "${builder_args[@]}" --platform linux/amd64 --progress plain \
  --file scripts/gitlab-full-build.Dockerfile --target binaries \
  --output "type=local,dest=$work/binaries" . 2>&1 | tee gitlab-logs/test-and-binaries.log
docker buildx build "${builder_args[@]}" --platform linux/amd64,linux/arm64 --progress plain \
  --file scripts/gitlab-full-build.Dockerfile --target runtime --build-arg "VCS_REF=$CI_COMMIT_SHA" \
  --provenance=false --sbom=false --tag "$image" --push --metadata-file "$work/app.json" . \
  2>&1 | tee gitlab-logs/multiarch-image.log
docker buildx build "${builder_args[@]}" --platform linux/amd64,linux/arm64 --progress plain \
  --file deploy/probe/Dockerfile --provenance=false --sbom=false --tag "$probe" \
  --push --metadata-file "$work/probe.json" . 2>&1 | tee gitlab-logs/multiarch-probe.log
app_digest=$(jq -er '."containerimage.digest"' "$work/app.json")
probe_digest=$(jq -er '."containerimage.digest"' "$work/probe.json")
[[ "$app_digest" =~ ^sha256:[0-9a-f]{64}$ && "$probe_digest" =~ ^sha256:[0-9a-f]{64}$ ]]

# Preserve Community's existing default transparency-log/signing policy.
cosign sign --yes "${registry_args[@]}" --key "$signing_key" "$CI_REGISTRY_IMAGE@$app_digest"
cosign sign --yes "${registry_args[@]}" --key "$signing_key" "$CI_REGISTRY_IMAGE/probe@$probe_digest"
cosign verify "${registry_args[@]}" --key "$work/cosign.pub" "$CI_REGISTRY_IMAGE@$app_digest" > gitlab-logs/app-signature-verification.json
cosign verify "${registry_args[@]}" --key "$work/cosign.pub" "$CI_REGISTRY_IMAGE/probe@$probe_digest" > gitlab-logs/probe-signature-verification.json

bash scripts/gitlab-package-full.sh "$version" "$work/binaries" \
  "$CI_REGISTRY_IMAGE@$app_digest" "$CI_REGISTRY_IMAGE/probe@$probe_digest" \
  "$work/cosign.pub" "$SIGSTORE_TRUSTED_ROOT_FILE" 2>&1 | tee gitlab-logs/package-and-sign.log
# Keep signed candidates private until the real installation gate also passes.
# A failed job may retain diagnostics, never an apparently finished delivery.
mv gitlab-artifacts/full "$work/signed-artifacts"
# Release BuildKit memory before the real offline gate. Only the test harness
# image is fetched here; the fresh verifier daemon never accesses this cache.
docker buildx rm "$builder"
builder_created=false
docker pull --platform linux/amd64 "$OFFLINE_DIND_IMAGE" 2>&1 | tee gitlab-logs/offline-harness-prefetch.log
bash scripts/gitlab-verify-offline.sh "$version" "$work/signed-artifacts" \
  "$work/cosign.pub" "$SIGSTORE_TRUSTED_ROOT_FILE" 2>&1 | tee gitlab-logs/offline-real-install.log
mv "$work/signed-artifacts" gitlab-artifacts/full
printf 'Full signed build and real amd64 offline installation passed for %s (ARM package verified, no ARM hardware/GPU claim).\n' "$CI_COMMIT_SHA"
