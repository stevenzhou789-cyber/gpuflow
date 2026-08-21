#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
DEFAULT_INSTALL_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck source=upgrade-lib.sh
. "$SCRIPT_DIR/upgrade-lib.sh"

usage() {
  cat <<'EOF'
Usage: upgrade.sh VERSION [options]

Options:
  --install-dir DIR           Existing control-plane installation (default: repository root)
  --image-repository REPO    Image repository without a tag
  --inventory FILE           Upgrade agents listed in FILE after the control plane
  --skip-agents              Upgrade only the control plane
  --skip-backup              Do not create a MySQL/configuration backup
EOF
}

[ "${1:-}" != "-h" ] && [ "${1:-}" != "--help" ] || { usage; exit 0; }
[ "$#" -ge 1 ] || { usage; exit 2; }
VERSION=$1
shift
INSTALL_DIR=$DEFAULT_INSTALL_DIR
IMAGE_REPOSITORY=${GPUFLOW_IMAGE_REPOSITORY:-ghcr.io/stevenzhou789-cyber/gpuflow}
INVENTORY=
SKIP_AGENTS=false
SKIP_BACKUP=false

while [ "$#" -gt 0 ]; do
  case "$1" in
    --install-dir) [ "$#" -ge 2 ] || die "--install-dir requires a directory"; INSTALL_DIR=$2; shift 2 ;;
    --image-repository) [ "$#" -ge 2 ] || die "--image-repository requires a value"; IMAGE_REPOSITORY=$2; shift 2 ;;
    --inventory) [ "$#" -ge 2 ] || die "--inventory requires a file"; INVENTORY=$2; shift 2 ;;
    --skip-agents) SKIP_AGENTS=true; shift ;;
    --skip-backup) SKIP_BACKUP=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

validate_version "$VERSION"
require_command docker
docker compose version >/dev/null 2>&1 || die "Docker Compose v2 is required"
INSTALL_DIR=$(cd -- "$INSTALL_DIR" && pwd)
[ -n "$INVENTORY" ] || INVENTORY="$INSTALL_DIR/scripts/agents.conf"
[ -f "$INSTALL_DIR/compose.yaml" ] || die "compose.yaml not found in $INSTALL_DIR"
[ -f "$INSTALL_DIR/.env" ] || die ".env not found in $INSTALL_DIR"

TARGET_IMAGE="${IMAGE_REPOSITORY}:${VERSION}"
STATE_DIR="$INSTALL_DIR/.gpuflow"
TIMESTAMP=$(date -u +%Y%m%dT%H%M%SZ)
BACKUP_DIR="$STATE_DIR/backups/$TIMESTAMP"
mkdir -p "$STATE_DIR/backups"

log "checking image $TARGET_IMAGE"
docker pull "$TARGET_IMAGE"

if [ "$SKIP_BACKUP" = false ]; then
  backup_installation "$INSTALL_DIR" "$BACKUP_DIR"
else
  mkdir -p "$BACKUP_DIR"
  cp "$INSTALL_DIR/.env" "$BACKUP_DIR/.env"
  cp "$INSTALL_DIR/compose.yaml" "$BACKUP_DIR/compose.yaml"
  log "warning: MySQL backup skipped by request"
fi

PREVIOUS_IMAGE=$(read_env_value "$INSTALL_DIR/.env" GPUFLOW_IMAGE)
printf '%s\n' "${PREVIOUS_IMAGE:-gpuflow:local}" > "$BACKUP_DIR/previous-control-image"
printf '%s\n' "$TARGET_IMAGE" > "$BACKUP_DIR/target-image"

set_env_value "$INSTALL_DIR/.env" GPUFLOW_IMAGE "$TARGET_IMAGE"
set_env_value "$INSTALL_DIR/.env" GPUFLOW_AGENT_IMAGE "$TARGET_IMAGE"

log "upgrading control plane to $VERSION"
if ! docker compose --project-directory "$INSTALL_DIR" up -d --no-deps --pull never control-plane; then
  restore_control_image "$INSTALL_DIR" "$BACKUP_DIR/.env"
  die "control-plane replacement failed; previous image restored"
fi

if ! wait_for_control_plane "$INSTALL_DIR" 30; then
  docker compose --project-directory "$INSTALL_DIR" logs --tail 100 control-plane >&2 || true
  restore_control_image "$INSTALL_DIR" "$BACKUP_DIR/.env"
  die "health check failed; previous control-plane image restored (database was not restored)"
fi

printf '%s\n' "$VERSION" > "$STATE_DIR/current-version"
printf '%s\n' "$BACKUP_DIR" > "$STATE_DIR/last-backup"

if [ "$SKIP_AGENTS" = false ]; then
  if [ -f "$INVENTORY" ]; then
    bash "$SCRIPT_DIR/upgrade-agents.sh" "$VERSION" --inventory "$INVENTORY" --image-repository "$IMAGE_REPOSITORY"
  else
    log "agent inventory not found; control plane upgraded, agents skipped ($INVENTORY)"
  fi
fi

log "upgrade completed: $VERSION"
log "backup: $BACKUP_DIR"
