#!/usr/bin/env bash
# Real integration gate: a fresh, network-less Docker server, never host Docker.
set -Eeuo pipefail
set +x
umask 077
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/offline-lib.sh"
version=${1:?VERSION ARTIFACT_DIRECTORY TRUSTED_PUBLIC_KEY TRUSTED_ROOT}
artifacts=${2:?}; public_key=${3:?}; trusted_root=${4:?}
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9._-]+)?$ ]] || offline_die 'Invalid version.'
[[ ${DOCKER_HOST:-} == tcp://builder:2375 ]] || offline_die 'This gate runs only against the dedicated local GitLab builder.'
[[ ${CI_JOB_ID:-} =~ ^[0-9]+$ ]] || offline_die 'A numeric CI job ID is required.'
[[ ${OFFLINE_DIND_IMAGE:-} =~ ^docker:29\.3\.1-dind@sha256:[0-9a-f]{64}$ ]] || offline_die 'A reviewed Docker 29.3.1 DinD image Digest is required.'
[[ ${GPUFLOW_CI_IMAGE:-} =~ ^gitlab\.gpuflow\.test:5055/gpuflow/gpuflow/ci-tools@sha256:[0-9a-f]{64}$ ]] || offline_die 'The approved private tools image Digest is required.'
for command in docker cosign tar sha256sum od; do command -v "$command" >/dev/null; done
test -s "$public_key" && test -s "$trusted_root"
artifacts=$(cd -- "$artifacts" && pwd)
unset DOCKER_CONTEXT DOCKER_TLS_VERIFY DOCKER_CERT_PATH
outer_docker() { docker --host tcp://builder:2375 "$@"; }
[[ $(outer_docker info --format '{{.OSType}}/{{.Architecture}}') =~ ^linux/(amd64|x86_64)$ ]] || offline_die 'The real integration runner must be Linux amd64.'

work=$(mktemp -d -t gpuflow-offline-integration.XXXXXXXX)
token="ce-offline-$CI_JOB_ID-$(od -An -N12 -tx1 /dev/urandom | tr -d ' \n')"
label=gpuflow.offline-verifier
daemon_id= client_id= socket_volume= data_volume=
cleanup() {
  local result=$? resource
  trap - EXIT
  # Remove only returned, ownership-labelled resource IDs created by this run.
  # No compose down, daemon-wide prune, host mounts or name-pattern deletion.
  for resource in "$client_id" "$daemon_id"; do
    if [[ -n "$resource" ]] && [[ $(outer_docker inspect --format "{{index .Config.Labels \"$label\"}}" "$resource" 2>/dev/null) == "$token" ]]; then
      outer_docker rm --force --volumes "$resource" >/dev/null || result=1
    fi
  done
  for resource in "$socket_volume" "$data_volume"; do
    if [[ -n "$resource" ]] && [[ $(outer_docker volume inspect --format "{{index .Labels \"$label\"}}" "$resource" 2>/dev/null) == "$token" ]]; then
      outer_docker volume rm "$resource" >/dev/null || result=1
    fi
  done
  rm -rf -- "$work"
  exit "$result"
}
trap cleanup EXIT

# Both tarballs must pass real offline Sigstore verification before containers
# are created. ARM's signed manifest is checked; ARM hardware is not simulated.
for architecture in amd64 arm64; do
  package_name="gpuflow-offline-$version-linux-$architecture"
  archive="$artifacts/$package_name.tar.gz"
  test -s "$archive" && test -s "$archive.sigstore.json"
  cosign verify-blob --offline --trusted-root "$trusted_root" --key "$public_key" \
    --bundle "$archive.sigstore.json" "$archive"
  [[ $(tar -xOf "$archive" "$package_name/ARCHITECTURE") == "$architecture" ]] || offline_die 'Signed package architecture does not match its filename.'
  [[ $(tar -xOf "$archive" "$package_name/VERSION") == "$version" ]] || offline_die 'Signed package version does not match.'
  tar -xOf "$archive" "$package_name/IMAGE-MANIFEST.tsv" > "$work/$architecture.tsv"
  offline_validate_manifest "$work/$architecture.tsv" "$architecture"
done

# Provisioning may cache the two TEST HARNESS images beforehand. The protected
# test itself never pulls, including its harness, and the fresh inner image
# store remains empty until the signed application package is loaded.
for image in "$OFFLINE_DIND_IMAGE" "$GPUFLOW_CI_IMAGE"; do
  outer_docker image inspect "$image" >/dev/null || offline_die 'Test harness image is not preloaded; provision it before this gate.'
done
for suffix in socket data; do
  name="$token-$suffix"
  if outer_docker volume inspect "$name" >/dev/null 2>&1; then offline_die 'Refusing to adopt an existing test volume.'; fi
  created=$(outer_docker volume create --label "$label=$token" "$name")
  [[ "$created" == "$name" ]] || offline_die 'Unexpected volume identity.'
  if [[ "$suffix" == socket ]]; then socket_volume=$created; else data_volume=$created; fi
done
daemon_id=$(outer_docker create --name "$token-daemon" --pull never --network none --privileged \
  --memory 1536m --cpus 2 --label "$label=$token" --env DOCKER_TLS_CERTDIR= \
  --mount "type=volume,source=$socket_volume,target=/var/run" \
  --mount "type=volume,source=$data_volume,target=/var/lib/docker" \
  --entrypoint dockerd "$OFFLINE_DIND_IMAGE" --host=unix:///var/run/docker.sock \
  --data-root=/var/lib/docker --storage-driver=vfs)
[[ "$daemon_id" =~ ^[0-9a-f]{64}$ ]] || offline_die 'Unexpected daemon container identity.'
client_id=$(outer_docker create --name "$token-client" --pull never --network none \
  --memory 384m --cpus 1 --label "$label=$token" \
  --env DOCKER_HOST=unix:///var/run/docker.sock --env DOCKER_CONTEXT= \
  --mount "type=volume,source=$socket_volume,target=/var/run" \
  --entrypoint bash "$GPUFLOW_CI_IMAGE" -c 'sleep infinity')
[[ "$client_id" =~ ^[0-9a-f]{64}$ ]] || offline_die 'Unexpected client container identity.'
outer_docker start "$daemon_id" "$client_id" >/dev/null
for resource in "$daemon_id" "$client_id"; do
  [[ $(outer_docker inspect --format '{{.HostConfig.NetworkMode}}' "$resource") == none ]] || offline_die 'The verifier must have no network attachment.'
  [[ $(outer_docker inspect --format '{{len .HostConfig.PortBindings}}' "$resource") == 0 ]] || offline_die 'Verifier ports must not be published.'
done
outer_docker exec "$client_id" mkdir -p /input
archive="$artifacts/gpuflow-offline-$version-linux-amd64.tar.gz"
outer_docker cp "$archive" "$client_id:/input/package.tar.gz"
outer_docker cp "$archive.sigstore.json" "$client_id:/input/package.tar.gz.sigstore.json"
outer_docker cp "$public_key" "$client_id:/input/trusted-cosign.pub"
outer_docker cp "$trusted_root" "$client_id:/input/trusted_root.json"
outer_docker cp "$script_dir/gitlab-verify-offline-inner.sh" "$client_id:/input/verify.sh"
# No shell environment inheritance or private signing key is forwarded.
outer_docker exec "$client_id" bash /input/verify.sh "$version" /input
printf 'PASS: amd64 fresh-daemon/no-egress install, SQL/MinIO/API/auth checks; signed arm64 package manifest verified (no ARM hardware or GPU-task claim).\n'
