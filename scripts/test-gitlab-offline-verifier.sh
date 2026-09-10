#!/usr/bin/env bash
set -Eeuo pipefail
source_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d -t gpuflow-verifier-contract.XXXXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/artifacts" "$test_root/state" "$test_root/input"
for executable in docker cosign; do cp "$source_root/scripts/gitlab-test-offline-verifier-mock.sh" "$test_root/bin/$executable"; chmod +x "$test_root/bin/$executable"; done
export PATH="$test_root/bin:$PATH" MOCK_VERIFIER_LOG="$test_root/calls" MOCK_VERIFIER_STATE="$test_root/state"
export DOCKER_HOST=tcp://builder:2375 CI_JOB_ID=777
export OFFLINE_DIND_IMAGE="docker:29.3.1-dind@sha256:$(printf '%064d' 0)"
export GPUFLOW_CI_IMAGE="gitlab.gpuflow.test:5055/gpuflow/gpuflow/ci-tools@sha256:$(printf '%064d' 0)"
unset COSIGN_PRIVATE_KEY COSIGN_PRIVATE_KEY_FILE CI_REGISTRY_PASSWORD
printf 'TEST-ONLY public key\n' > "$test_root/key.pub"
printf '{}\n' > "$test_root/root.json"
version=v0.0.0-git.111111111111
for architecture in amd64 arm64; do
  name="gpuflow-offline-$version-linux-$architecture"
  mkdir "$test_root/$name"
  printf '%s\n' "$architecture" > "$test_root/$name/ARCHITECTURE"
  printf '%s\n' "$version" > "$test_root/$name/VERSION"
  index=1
  for role in app probe mysql minio; do
    printf '%s\tgpuflow-offline/%s:%s\tsha256:%064d\tlinux/%s\n' "$role" "$role" "$version" "$index" "$architecture" >> "$test_root/$name/IMAGE-MANIFEST.tsv"
    index=$((index + 1))
  done
  tar -czf "$test_root/artifacts/$name.tar.gz" -C "$test_root" "$name"
  printf '{}\n' > "$test_root/artifacts/$name.tar.gz.sigstore.json"
done
verify=(bash "$source_root/scripts/gitlab-verify-offline.sh" "$version" "$test_root/artifacts" "$test_root/key.pub" "$test_root/root.json")
expect_failure() { if "$@" > "$test_root/rejected.log" 2>&1; then echo 'Unsafe verifier input was accepted.' >&2; exit 1; fi; }
assert_clean() { [[ -z $(find "$test_root/state" -type f -print -quit) ]]; }
expect_failure env DOCKER_HOST=unix:///var/run/docker.sock "${verify[@]}"
[[ ! -e "$MOCK_VERIFIER_LOG" ]]
expect_failure env OFFLINE_DIND_IMAGE=docker:latest "${verify[@]}"
[[ ! -e "$MOCK_VERIFIER_LOG" ]]
expect_failure env MOCK_BAD_SIGNATURE=true "${verify[@]}"
! grep -q 'docker .* create ' "$MOCK_VERIFIER_LOG"
assert_clean
expect_failure env MOCK_MISSING_HARNESS=true "${verify[@]}"
assert_clean
expect_failure env MOCK_START_FAIL=true "${verify[@]}"
assert_clean
expect_failure env MOCK_UNSAFE_NETWORK=true "${verify[@]}"
assert_clean
expect_failure env MOCK_INNER_FAIL=true "${verify[@]}"
assert_clean
"${verify[@]}" > "$test_root/success.log"
assert_clean
[[ $(grep -c '^docker --host tcp://builder:2375 create .*--network none' "$MOCK_VERIFIER_LOG") -eq 8 ]]
! grep -Eq '(docker .* (pull|prune) |type=bind|COSIGN_PRIVATE_KEY|CI_REGISTRY_PASSWORD|--publish|--network host)' "$MOCK_VERIFIER_LOG"
grep -q 'exec .* bash /input/verify.sh' "$MOCK_VERIFIER_LOG"
expect_failure env DOCKER_HOST=tcp://builder:2375 bash "$source_root/scripts/gitlab-verify-offline-inner.sh" "$version" "$test_root/input"
expect_failure env DOCKER_HOST=unix:///var/run/docker.sock COSIGN_PRIVATE_KEY=TEST-ONLY-FORBIDDEN bash "$source_root/scripts/gitlab-verify-offline-inner.sh" "$version" "$test_root/input"
expect_failure env DOCKER_HOST=unix:///var/run/docker.sock MOCK_INNER_DIRTY=true bash "$source_root/scripts/gitlab-verify-offline-inner.sh" "$version" "$test_root/input"
grep -q 'not fresh' "$test_root/rejected.log"
for file in gitlab-verify-offline.sh gitlab-verify-offline-inner.sh gitlab-test-offline-verifier-mock.sh test-gitlab-offline-verifier.sh; do bash -n "$source_root/scripts/$file"; done
printf 'PASS: verifier endpoint/pinning/signature/cache guards; dedicated no-network containers/socket/data; only-own cleanup on start/network/install failures; no host mounts/pulls/secrets; polluted daemon rejected (mock-only, not real offline installation).\n'
