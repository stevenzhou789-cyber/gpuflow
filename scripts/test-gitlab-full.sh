#!/usr/bin/env bash
set -Eeuo pipefail
source_root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
test_root=$(mktemp -d -t gpuflow-full-contract.XXXXXXXX)
trap 'rm -rf -- "$test_root"' EXIT
mkdir -p "$test_root/bin" "$test_root/source" "$test_root/binaries" "$test_root/extract"
for executable in docker git cosign zip ssh sleep; do cp "$source_root/scripts/gitlab-test-full-mock.sh" "$test_root/bin/$executable"; chmod +x "$test_root/bin/$executable"; done
export PATH="$test_root/bin:$PATH" MOCK_CALLS="$test_root/calls"
unset COSIGN_PRIVATE_KEY_FILE
export COSIGN_PRIVATE_KEY=test-placeholder-not-a-real-key COSIGN_PASSWORD='' CI_COMMIT_SHA=1111111111111111111111111111111111111111
for path in compose.yaml .env.example LICENSE README.md; do cp "$source_root/$path" "$test_root/source/"; done
cp -R "$source_root/scripts" "$source_root/deploy" "$test_root/source/"
printf 'DO-NOT-PACKAGE\n' > "$test_root/source/.env"
printf 'DO-NOT-PACKAGE\n' > "$test_root/source/deploy/.env"
printf 'DO-NOT-PACKAGE\n' > "$test_root/source/scripts/agents.conf"
printf 'mock public key\n' > "$test_root/cosign.pub"
printf '{}\n' > "$test_root/trusted_root.json"
for binary in linux-amd64/gpuflow linux-arm64/gpuflow windows-amd64/gpuflow.exe; do
  mkdir -p "$test_root/binaries/$(dirname "$binary")"
  printf 'mock native binary %s\n' "$binary" > "$test_root/binaries/$binary"
done
app='registry.example.test/gpuflow@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'
probe='registry.example.test/probe@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'
version=v0.0.0-git.111111111111
cd "$test_root/source"
expect_failure() { if "$@" > "$test_root/rejected.log" 2>&1; then echo 'Expected failure was accepted.' >&2; exit 1; fi; }
expect_failure env -u COSIGN_PRIVATE_KEY bash scripts/gitlab-full-build.sh
[[ ! -e "$MOCK_CALLS" ]]
expect_failure env MOCK_TRACKED_SECRET=true bash scripts/gitlab-package-full.sh "$version" "$test_root/binaries" "$app" "$probe" "$test_root/cosign.pub" "$test_root/trusted_root.json"
[[ ! -e gitlab-artifacts/full ]]
expect_failure env MOCK_BAD_SIGNATURE=true bash scripts/gitlab-package-full.sh "$version" "$test_root/binaries" "$app" "$probe" "$test_root/cosign.pub" "$test_root/trusted_root.json"
[[ ! -e gitlab-artifacts/full ]]
bash scripts/gitlab-package-full.sh "$version" "$test_root/binaries" "$app" "$probe" "$test_root/cosign.pub" "$test_root/trusted_root.json" > "$test_root/package.log"
dist="$test_root/source/gitlab-artifacts/full"
for artifact in "$dist"/*.tar.gz "$dist"/*.zip "$dist/checksums.txt"; do test -s "$artifact.sigstore.json"; done
(cd "$dist" && sha256sum --check checksums.txt >/dev/null)
for architecture in amd64 arm64; do
  tar -xzf "$dist/gpuflow-offline-$version-linux-$architecture.tar.gz" -C "$test_root/extract"
  package="$test_root/extract/gpuflow-offline-$version-linux-$architecture"
  for path in compose.yaml .env.example README.md PROJECT-README.md LICENSE VERSION scripts/upgrade.sh scripts/rollback.sh scripts/upgrade-agents.sh deploy/README.md deploy/agent/compose.yaml agents/linux-amd64/gpuflow agents/linux-arm64/gpuflow agents/windows-amd64/gpuflow.exe images/docker-images.tar.gz SHA256SUMS.sigstore.json; do test -s "$package/$path"; done
  [[ $(awk -F '\t' '$1 !~ /^#/ && NF {n++} END {print n}' "$package/IMAGE-MANIFEST.tsv") -eq 4 ]]
  [[ ! -e "$package/.env" && ! -e "$package/deploy/.env" && ! -e "$package/scripts/agents.conf" ]]
  (cd "$package" && sha256sum --check SHA256SUMS >/dev/null)
done
package="$test_root/extract/gpuflow-offline-$version-linux-amd64"
expect_failure env MOCK_BAD_SIGNATURE=true bash "$package/scripts/load-offline-images.sh" --public-key "$test_root/cosign.pub"
[[ $(grep -c '^docker load ' "$MOCK_CALLS" || true) -eq 0 ]]
bash "$package/scripts/load-offline-images.sh" --public-key "$test_root/cosign.pub" > "$test_root/load.log"
[[ $(grep -c '^docker load ' "$MOCK_CALLS") -eq 1 ]]
# Docker 24 config IDs and Docker 29 index/manifest IDs differ, but the bytes
# exported in legacy <sha>.json and OCI blobs/sha256/<sha> have the same digest.
env MOCK_DOCKER_MAJOR=24 bash "$package/scripts/load-offline-images.sh" --public-key "$test_root/cosign.pub" > "$test_root/load-legacy-docker.log"
loads_before_wrong_config=$(grep -c '^docker load ' "$MOCK_CALLS")
expect_failure env MOCK_WRONG_CONFIG=true bash "$package/scripts/load-offline-images.sh" --public-key "$test_root/cosign.pub"
[[ $(grep -c '^docker load ' "$MOCK_CALLS") -eq "$loads_before_wrong_config" ]]
mkdir "$test_root/existing"
printf 'existing-secret\n' > "$test_root/existing/.env"
expect_failure bash "$package/scripts/install-offline.sh" --install-dir "$test_root/existing" --public-key "$test_root/cosign.pub"
grep -qx existing-secret "$test_root/existing/.env"
bash "$package/scripts/install-offline.sh" --install-dir "$test_root/new-install" --public-key "$test_root/cosign.pub" > "$test_root/install.log"
grep -q '^GPUFLOW_IMAGE=gpuflow-offline/app:' "$test_root/new-install/.env"
upgrader=(bash "$package/scripts/upgrade.sh" "$version" --install-dir "$test_root/new-install" --image-repository gpuflow-offline/app --skip-agents --offline --offline-package "$package" --public-key "$test_root/cosign.pub")
"${upgrader[@]}" > "$test_root/upgrade.log"
before=$(sha256sum "$test_root/new-install/.env")
expect_failure env MOCK_FAIL_MYSQL=true "${upgrader[@]}"
[[ $(sha256sum "$test_root/new-install/.env") == "$before" ]]
bash "$package/scripts/rollback.sh" "$version" --install-dir "$test_root/new-install" --image-repository gpuflow-offline/app --skip-agents --offline --offline-package "$package" --public-key "$test_root/cosign.pub" > "$test_root/rollback.log"
mkdir "$test_root/agent"
cp "$source_root/deploy/agent/compose.yaml" "$test_root/agent/compose.yaml"
cp "$source_root/deploy/agent/.env.example" "$test_root/agent/.env"
printf 'mock-node|%s\n' "$test_root/agent" > "$test_root/agents.conf"
agent_upgrader=(bash "$package/scripts/upgrade-agents.sh" "$version" --inventory "$test_root/agents.conf" --image-repository gpuflow-offline/app --offline --offline-package "$package" --public-key "$test_root/cosign.pub")
"${agent_upgrader[@]}" > "$test_root/agents.log"
before=$(sha256sum "$test_root/agent/.env")
expect_failure env MOCK_RUNNING_JOBS=true "${agent_upgrader[@]}"
[[ $(sha256sum "$test_root/agent/.env") == "$before" ]]
# The offline upgrade paths must not pull. Earlier pulls belong to packaging.
last_pull_line=$(grep -n '^docker pull ' "$MOCK_CALLS" | tail -1 | cut -d: -f1)
first_load_line=$(grep -n '^docker load ' "$MOCK_CALLS" | head -1 | cut -d: -f1)
[[ "$last_pull_line" -lt "$first_load_line" ]]
loads_before_tamper=$(grep -c '^docker load ' "$MOCK_CALLS")
printf 'tampered\n' >> "$package/agents/linux-amd64/gpuflow"
expect_failure bash "$package/scripts/load-offline-images.sh" --public-key "$test_root/cosign.pub"
[[ $(grep -c '^docker load ' "$MOCK_CALLS") -eq "$loads_before_tamper" ]]
for script in "$source_root"/scripts/*.sh; do bash -n "$script"; done
for script in upgrade.sh rollback.sh upgrade-agents.sh; do
  grep -q -- '--offline)' "$source_root/scripts/$script"
  grep -q 'docker pull' "$source_root/scripts/$script"
done
printf 'PASS: full dual-arch package tree, native programs/signature sidecars, fail-closed signing/tamper, secret exclusions, install/upgrade/rollback/SSH agent upgrade, readiness-failure restoration, running-task protection, no offline pulls (mock-only; not cryptographic or Docker integration validation).\n'
