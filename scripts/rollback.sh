#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
DEFAULT_INSTALL_DIR=$(cd -- "$SCRIPT_DIR/.." && pwd)
# shellcheck source=upgrade-lib.sh
. "$SCRIPT_DIR/upgrade-lib.sh"

usage() {
  cat <<'EOF'
Usage: rollback.sh VERSION [options]

Rolls program images back without restoring MySQL or MinIO data.

Options:
  --install-dir DIR           Control-plane installation (default: repository root)
  --image-repository REPO    Image repository without a tag
  --inventory FILE           Roll agents back using FILE
  --skip-agents              Roll back only the control plane
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

while [ "$#" -gt 0 ]; do
  case "$1" in
    --install-dir) [ "$#" -ge 2 ] || die "--install-dir requires a directory"; INSTALL_DIR=$2; shift 2 ;;
    --image-repository) [ "$#" -ge 2 ] || die "--image-repository requires a value"; IMAGE_REPOSITORY=$2; shift 2 ;;
    --inventory) [ "$#" -ge 2 ] || die "--inventory requires a file"; INVENTORY=$2; shift 2 ;;
    --skip-agents) SKIP_AGENTS=true; shift ;;
    -h|--help) usage; exit 0 ;;
    *) die "unknown argument: $1" ;;
  esac
done

validate_version "$VERSION"
require_command docker
INSTALL_DIR=$(cd -- "$INSTALL_DIR" && pwd)
[ -n "$INVENTORY" ] || INVENTORY="$INSTALL_DIR/scripts/agents.conf"
[ -f "$INSTALL_DIR/.env" ] || die ".env not found in $INSTALL_DIR"
[ -f "$INSTALL_DIR/compose.yaml" ] || die "compose.yaml not found in $INSTALL_DIR"
TARGET_IMAGE="${IMAGE_REPOSITORY}:${VERSION}"
STATE_DIR="$INSTALL_DIR/.gpuflow"
TIMESTAMP=$(date -u +%Y%m%dT%H%M%SZ)
ROLLBACK_RECORD="$STATE_DIR/rollbacks/$TIMESTAMP"
mkdir -p "$ROLLBACK_RECORD"
cp "$INSTALL_DIR/.env" "$ROLLBACK_RECORD/.env.before-rollback"

log "checking rollback image $TARGET_IMAGE"
docker pull "$TARGET_IMAGE"
set_env_value "$INSTALL_DIR/.env" GPUFLOW_IMAGE "$TARGET_IMAGE"
set_env_value "$INSTALL_DIR/.env" GPUFLOW_AGENT_IMAGE "$TARGET_IMAGE"

log "rolling control plane back to $VERSION (database is unchanged)"
docker compose --project-directory "$INSTALL_DIR" up -d --no-deps --pull never control-plane
if ! wait_for_control_plane "$INSTALL_DIR" 30; then
  docker compose --project-directory "$INSTALL_DIR" logs --tail 100 control-plane >&2 || true
  cp "$ROLLBACK_RECORD/.env.before-rollback" "$INSTALL_DIR/.env"
  docker compose --project-directory "$INSTALL_DIR" up -d --no-deps --pull never control-plane || true
  die "rollback target failed its health check; pre-rollback image restored"
fi

printf '%s\n' "$VERSION" > "$STATE_DIR/current-version"
if [ "$SKIP_AGENTS" = false ]; then
  if [ -f "$INVENTORY" ]; then
    bash "$SCRIPT_DIR/upgrade-agents.sh" "$VERSION" --inventory "$INVENTORY" --image-repository "$IMAGE_REPOSITORY"
  else
    log "agent inventory not found; control plane rolled back, agents skipped ($INVENTORY)"
  fi
fi

log "program rollback completed: $VERSION"
log "MySQL and MinIO data were not restored"
