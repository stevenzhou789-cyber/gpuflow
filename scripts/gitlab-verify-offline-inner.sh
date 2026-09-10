#!/usr/bin/env bash
# Invoked only inside the verifier client. Docker API is a shared PRIVATE socket.
set -Eeuo pipefail
set +x
umask 077
version=${1:?VERSION INPUT_DIRECTORY}
input=${2:?}
[[ ${DOCKER_HOST:-} == unix:///var/run/docker.sock ]] || { echo 'Only the fresh verifier Unix socket is accepted.' >&2; exit 1; }
[[ -z ${COSIGN_PRIVATE_KEY:-} && -z ${COSIGN_PRIVATE_KEY_FILE:-} && -z ${CI_REGISTRY_PASSWORD:-} ]] || { echo 'Signing/registry credentials must not enter the offline verifier.' >&2; exit 1; }
unset DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH
[[ "$input" == /* && -d "$input" && "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9._-]+)?$ ]]
for ((attempt=0; attempt<60; attempt++)); do
  if docker info >/dev/null 2>&1; then break; fi
  sleep 1
done
[[ $(docker version --format '{{.Server.Version}}') == 29.3.1 ]] || { echo 'Unexpected inner Docker server version.' >&2; exit 1; }
[[ $(docker info --format '{{.OSType}}/{{.Architecture}}') =~ ^linux/(amd64|x86_64)$ ]]
[[ -z $(docker image ls -aq) && -z $(docker ps -aq) && -z $(docker volume ls -q) ]] || { echo 'The verifier daemon is not fresh: images, containers or data volumes already exist.' >&2; exit 1; }
printf 'Verified fresh Docker 29.3.1 daemon: zero images, containers and application volumes.\n'
cosign verify-blob --offline --trusted-root "$input/trusted_root.json" --key "$input/trusted-cosign.pub" \
  --bundle "$input/package.tar.gz.sigstore.json" "$input/package.tar.gz"
package_name="gpuflow-offline-$version-linux-amd64"
# Signed input must still be a normal relative single-root archive. Reject path
# traversal before extraction, even though generated release archives are safe.
while IFS= read -r path; do
  [[ "$path" == "$package_name/"* && "$path" != *'/../'* && "$path" != *'/./'* ]] || { echo 'Unsafe archive entry.' >&2; exit 1; }
done < <(tar -tzf "$input/package.tar.gz")
mkdir "$input/extracted"
tar -xzf "$input/package.tar.gz" -C "$input/extracted" --no-same-owner
package="$input/extracted/$package_name"
[[ ! -e "$input/installed" ]]
bash "$package/scripts/install-offline.sh" --install-dir "$input/installed" --public-key "$input/trusted-cosign.pub"
. "$package/scripts/offline-lib.sh"
offline_verify_images "$package/IMAGE-MANIFEST.tsv" amd64
expected_count=$(awk -F '\t' '$1 !~ /^#/ && NF {print $3}' "$package/IMAGE-MANIFEST.tsv" | sort -u | wc -l | tr -d ' ')
actual_count=$(docker image ls -aq | sort -u | wc -l | tr -d ' ')
[[ "$expected_count" == 4 && "$actual_count" == "$expected_count" ]] || { echo 'Unexpected images exist after offline installation.' >&2; exit 1; }
offline_health_checks "$input/installed"
compose=(docker compose --project-directory "$input/installed")
[[ $("${compose[@]}" ps --status running --services | sort) == $'control-plane\nminio\nmysql' ]] || { echo 'All three core services must be running.' >&2; exit 1; }
# Validate auth rejects an unauthenticated request, not merely that a correctly
# authenticated HTTP request succeeds. Never print generated token or full env.
"${compose[@]}" exec -T control-plane sh -c \
  'response=$(wget -S -O /dev/null http://127.0.0.1:8080/v1/nodes 2>&1) && exit 1; printf "%s\n" "$response" | grep -Eq "HTTP/[0-9.]+ 401([[:space:]]|$)"'
printf 'PASS: 4 package-only amd64 images with portable config digests; control-plane/MySQL/MinIO running; SQL SELECT 1, MinIO ready, healthz, authenticated nodes and unauthenticated HTTP 401.\n'
