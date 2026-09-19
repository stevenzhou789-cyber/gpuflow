#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
# shellcheck source=upgrade-lib.sh
. "$SCRIPT_DIR/upgrade-lib.sh"

usage() {
  cat <<'EOF'
Usage: upgrade-agents.sh VERSION [--inventory FILE] [--image-repository REPOSITORY]
       [--offline --offline-package DIR --public-key TRUSTED_KEY]

Inventory format (one node per line):
  ssh-target|agent-install-directory

Example:
  ops@gpu-01|/opt/gpuflow-agent
EOF
}

[ "${1:-}" != "-h" ] && [ "${1:-}" != "--help" ] || { usage; exit 0; }
[ "$#" -ge 1 ] || { usage; exit 2; }
VERSION=$1
shift
INVENTORY="$SCRIPT_DIR/agents.conf"
IMAGE_REPOSITORY=${GPUFLOW_IMAGE_REPOSITORY:-ghcr.io/stevenzhou789-cyber/gpuflow}
OFFLINE=false
OFFLINE_PACKAGE=
PUBLIC_KEY=

while [ "$#" -gt 0 ]; do
  case "$1" in
    --inventory) [ "$#" -ge 2 ] || die "--inventory requires a file"; INVENTORY=$2; shift 2 ;;
    --image-repository) [ "$#" -ge 2 ] || die "--image-repository requires a value"; IMAGE_REPOSITORY=$2; shift 2 ;;
    --offline) OFFLINE=true; shift ;;
    --offline-package) [ "$#" -ge 2 ] || die "--offline-package requires a directory"; OFFLINE_PACKAGE=$2; shift 2 ;;
    --public-key) [ "$#" -ge 2 ] || die "--public-key requires a file"; PUBLIC_KEY=$2; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

validate_version "$VERSION"
require_command ssh
[ -f "$INVENTORY" ] || die "agent inventory not found: $INVENTORY"
TARGET_IMAGE="${IMAGE_REPOSITORY}:${VERSION}"
EXPECTED_IMAGE_ID=
EXPECTED_ARCHITECTURE=
if [ "$OFFLINE" = true ]; then
  . "$SCRIPT_DIR/offline-lib.sh"
  [ -n "$OFFLINE_PACKAGE" ] && [ -n "$PUBLIC_KEY" ] || die "--offline requires --offline-package and --public-key"
  OFFLINE_PACKAGE=$(cd -- "$OFFLINE_PACKAGE" && pwd)
  EXPECTED_ARCHITECTURE=$(cat "$OFFLINE_PACKAGE/ARCHITECTURE")
  [[ "$EXPECTED_ARCHITECTURE" = amd64 || "$EXPECTED_ARCHITECTURE" = arm64 ]] || die "unknown offline architecture"
  offline_verify_package "$OFFLINE_PACKAGE" "$PUBLIC_KEY" "$EXPECTED_ARCHITECTURE"
  offline_validate_manifest "$OFFLINE_PACKAGE/IMAGE-MANIFEST.tsv" "$EXPECTED_ARCHITECTURE"
  [ "$(cat "$OFFLINE_PACKAGE/VERSION")" = "$VERSION" ] || die "offline version does not match"
  [ "$(awk -F '\t' '$1=="app" {print $2}' "$OFFLINE_PACKAGE/IMAGE-MANIFEST.tsv")" = "$TARGET_IMAGE" ] || die "--image-repository does not match signed offline manifest"
  EXPECTED_IMAGE_ID=$(awk -F '\t' '$1=="app" {print $3}' "$OFFLINE_PACKAGE/IMAGE-MANIFEST.tsv")
else
  [ -z "$OFFLINE_PACKAGE" ] && [ -z "$PUBLIC_KEY" ] || die "offline package options require --offline"
fi

upgrade_remote_agent() {
  local ssh_target=$1 install_dir=$2
  log "upgrading agent on $ssh_target"
  {
    if [ "$OFFLINE" = true ]; then
      declare -f offline_archive_config_digest offline_image_config_digest
    fi
    cat <<'REMOTE_SCRIPT'
set -Eeuo pipefail
install_dir=$1
umask 077
target_image=$2
offline=$3
expected_image_id=$4
expected_architecture=$5
env_file="$install_dir/.env"

[ -d "$install_dir" ] || { echo "agent directory not found: $install_dir" >&2; exit 1; }
[ -f "$install_dir/compose.yaml" ] || { echo "compose.yaml not found in $install_dir" >&2; exit 1; }
[ -f "$env_file" ] || { echo ".env not found in $install_dir" >&2; exit 1; }
command -v docker >/dev/null 2>&1 || { echo "docker is not installed" >&2; exit 1; }
command -v flock >/dev/null 2>&1 || { echo 'flock is required for an exclusive Agent upgrade.' >&2; exit 1; }
exec 9>"$install_dir/.gpuflow-upgrade.lock"
flock -n 9 || { echo 'Another Agent upgrade owns this node.' >&2; exit 1; }

old_container=$(docker compose --project-directory "$install_dir" ps -q agent)
[ -n "$old_container" ] || { echo 'No running Agent to perform a verified handoff.' >&2; exit 1; }
old_id=$(docker inspect -f '{{.Image}}' "$old_container")
[[ "$old_id" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo 'Cannot identify the previous Agent image.' >&2; exit 1; }
# Old versions cannot prove that a claimed attempt has finished. Do not replace
# them automatically; the first migration requires a scheduled offline window.
docker exec "$old_container" gpuflow agent-handoff --timeout 10s ready || {
  echo 'Agent does not support safe handoff or is not ready; use a planned offline migration.' >&2; exit 1;
}

if [ -n "$(docker ps -q --filter label=gpuflow.job)" ]; then
  echo "running GPUFlow job containers found; refusing to interrupt them" >&2
  exit 42
fi

if [ "$offline" = true ]; then
  # Shell exports must not override the verified image in the node's .env.
  while IFS= read -r variable; do
    case "$variable" in GPUFLOW_*|COMPOSE_FILE|COMPOSE_PROJECT_NAME|COMPOSE_PROFILES) unset "$variable";; esac
  done < <(compgen -e)
  actual=$(offline_image_config_digest "$target_image" "$expected_architecture")
  [ "$actual" = "$expected_image_id" ] || { echo 'Offline target config digest/platform differs from signed manifest; load the matching package on this node first.' >&2; exit 1; }
else
  docker pull "$target_image"
fi
target_id=$(docker image inspect "$target_image" --format '{{.Id}}')
[[ "$target_id" =~ ^sha256:[a-f0-9]{64}$ ]] || { echo 'Cannot identify the prepared Agent image.' >&2; exit 1; }
quiesced=true
resume_old() {
  if [ "$quiesced" = true ]; then
    docker exec "$old_container" gpuflow agent-handoff --timeout 10s resume >&2 || true
  fi
}
trap resume_old EXIT
docker exec "$old_container" gpuflow agent-handoff --timeout 30m quiesce || {
  echo 'Agent did not finish its claimed attempts; replacement refused.' >&2; exit 1;
}
# The acknowledged process cannot claim again. Include a physical check for
# unmanaged leftovers, while preserving existing session/attempt fencing.
[ -z "$(docker ps -q --filter label=gpuflow.job)" ] || { echo 'Job containers remain after handoff; replacement refused.' >&2; exit 42; }
backup_env=$(mktemp)
cp "$env_file" "$backup_env"
temporary="${env_file}.tmp.$$"
awk -v value="$target_id" '
  BEGIN { updated = 0 }
  index($0, "GPUFLOW_AGENT_IMAGE=") == 1 {
    if (!updated) print "GPUFLOW_AGENT_IMAGE=" value
    updated = 1
    next
  }
  { print }
  END { if (!updated) print "GPUFLOW_AGENT_IMAGE=" value }
' "$env_file" > "$temporary"
mv "$temporary" "$env_file"

if ! docker compose --project-directory "$install_dir" stop agent; then
  cp "$backup_env" "$env_file"
  echo 'Old Agent could not be stopped; replacement refused and environment restored.' >&2
  exit 1
fi
# The control plane retains the old session lease. Let it expire before the
# replacement registers; a successful process start alone is not registration.
sleep 35

wait_ready() {
  local expected=$1 current attempt
  for attempt in $(seq 1 30); do
    current=$(docker compose --project-directory "$install_dir" ps -q agent)
    if [ -n "$current" ] && [ "$(docker inspect -f '{{.Image}}' "$current")" = "$expected" ] && docker exec "$current" gpuflow agent-handoff --timeout 5s ready; then return 0; fi
    sleep 2
  done
  return 1
}
restore_previous() {
  local current
  current=$(docker compose --project-directory "$install_dir" ps -q agent)
  if [ -n "$current" ] && [ "$(docker inspect -f '{{.State.Running}}' "$current")" = true ]; then
    docker exec "$current" gpuflow agent-handoff --timeout 30m quiesce || { echo "Cannot safely stop the replacement; previous environment retained at $backup_env" >&2; return 1; }
    docker compose --project-directory "$install_dir" stop agent || return 1
    sleep 35
  fi
  # The old tag may have moved too. Restore the captured immutable image.
  awk -v value="$old_id" 'index($0,"GPUFLOW_AGENT_IMAGE=")==1 {print "GPUFLOW_AGENT_IMAGE=" value;next} {print}' "$backup_env" > "$temporary"
  mv "$temporary" "$env_file"
  docker compose --project-directory "$install_dir" up -d --no-deps --pull never --no-build agent && wait_ready "$old_id" || {
    echo "Previous Agent could not be verified ready; environment retained at $backup_env" >&2; return 1;
  }
  quiesced=false
  rm -f "$backup_env"
  echo 'Previous Agent image, registration, cleanup and heartbeat restored.' >&2
}

if ! docker compose --project-directory "$install_dir" up -d --no-deps --pull never --no-build agent; then
  restore_previous || exit 1
  echo 'Agent replacement failed.' >&2
  exit 1
fi
container=$(docker compose --project-directory "$install_dir" ps -q agent)
if ! wait_ready "$target_id"; then
  [ -z "$container" ] || docker logs --tail 100 "$container" >&2 || true
  restore_previous || exit 1
  echo 'New Agent did not become ready.' >&2
  exit 1
fi
quiesced=false
rm -f "$backup_env"
REMOTE_SCRIPT
  } | ssh -o BatchMode=yes "$ssh_target" bash -s -- "$install_dir" "$TARGET_IMAGE" "$OFFLINE" "$EXPECTED_IMAGE_ID" "$EXPECTED_ARCHITECTURE"
}

count=0
while IFS='|' read -r ssh_target install_dir extra || [ -n "${ssh_target:-}" ]; do
  ssh_target=${ssh_target%%#*}
  ssh_target=$(printf '%s' "$ssh_target" | tr -d '[:space:]')
  [ -n "$ssh_target" ] || continue
  [ -z "${extra:-}" ] || die "invalid inventory row for $ssh_target"
  install_dir=$(printf '%s' "${install_dir:-}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  [ -n "$install_dir" ] || die "missing install directory for $ssh_target"
  case "$install_dir" in /*) ;; *) die "agent directory must be absolute for $ssh_target" ;; esac
  upgrade_remote_agent "$ssh_target" "$install_dir"
  count=$((count + 1))
done < "$INVENTORY"

log "agent handoff, registration, cleanup and heartbeat verified on $count node(s): $VERSION"
