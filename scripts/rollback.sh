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
  --offline                 Use a verified offline package without pulling
  --offline-package DIR     Target version's full offline package
  --public-key FILE         Separately trusted Cosign public key
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
OFFLINE=false
OFFLINE_PACKAGE=
PUBLIC_KEY=

while [ "$#" -gt 0 ]; do
  case "$1" in
    --install-dir) [ "$#" -ge 2 ] || die "--install-dir requires a directory"; INSTALL_DIR=$2; shift 2 ;;
    --image-repository) [ "$#" -ge 2 ] || die "--image-repository requires a value"; IMAGE_REPOSITORY=$2; shift 2 ;;
    --inventory) [ "$#" -ge 2 ] || die "--inventory requires a file"; INVENTORY=$2; shift 2 ;;
    --skip-agents) SKIP_AGENTS=true; shift ;;
    --offline) OFFLINE=true; shift ;;
    --offline-package) [ "$#" -ge 2 ] || die "--offline-package requires a directory"; OFFLINE_PACKAGE=$2; shift 2 ;;
    --public-key) [ "$#" -ge 2 ] || die "--public-key requires a file"; PUBLIC_KEY=$2; shift 2 ;;
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
if [ "$OFFLINE" = true ]; then
  . "$SCRIPT_DIR/offline-lib.sh"
  [ -n "$OFFLINE_PACKAGE" ] && [ -n "$PUBLIC_KEY" ] || die "--offline requires --offline-package and --public-key"
  OFFLINE_PACKAGE=$(cd -- "$OFFLINE_PACKAGE" && pwd)
  architecture=$(offline_architecture)
  offline_verify_package "$OFFLINE_PACKAGE" "$PUBLIC_KEY" "$architecture"
  [ "$(cat "$OFFLINE_PACKAGE/VERSION")" = "$VERSION" ] || die "offline package version does not match"
  [ "$(awk -F '\t' '$1=="app" {print $2}' "$OFFLINE_PACKAGE/IMAGE-MANIFEST.tsv")" = "$TARGET_IMAGE" ] || die "--image-repository does not match the signed offline package"
  offline_load_images "$OFFLINE_PACKAGE" "$architecture"
  offline_clear_compose_overrides
else
  [ -z "$OFFLINE_PACKAGE" ] && [ -z "$PUBLIC_KEY" ] || die "offline package options require --offline"
  docker pull "$TARGET_IMAGE"
fi
set_env_value "$INSTALL_DIR/.env" GPUFLOW_IMAGE "$TARGET_IMAGE"
set_env_value "$INSTALL_DIR/.env" GPUFLOW_AGENT_IMAGE "$TARGET_IMAGE"
if [ "$OFFLINE" = true ]; then
  set_env_value "$INSTALL_DIR/.env" GPUFLOW_PROBE_IMAGE "$(awk -F '\t' '$1=="probe" {print $2}' "$OFFLINE_PACKAGE/IMAGE-MANIFEST.tsv")"
fi

log "rolling control plane back to $VERSION (database is unchanged)"
docker compose --project-directory "$INSTALL_DIR" up -d --no-deps --pull never --no-build control-plane
if ! wait_for_control_plane "$INSTALL_DIR" 30 || { [ "$OFFLINE" = true ] && ! offline_health_checks "$INSTALL_DIR"; }; then
  docker compose --project-directory "$INSTALL_DIR" logs --tail 100 control-plane >&2 || true
  cp "$ROLLBACK_RECORD/.env.before-rollback" "$INSTALL_DIR/.env"
  docker compose --project-directory "$INSTALL_DIR" up -d --no-deps --pull never --no-build control-plane || true
  die "rollback target failed its health check; pre-rollback image restored"
fi

printf '%s\n' "$VERSION" > "$STATE_DIR/current-version"
if [ "$OFFLINE" = true ]; then
  cp "$OFFLINE_PACKAGE/IMAGE-MANIFEST.tsv" "$STATE_DIR/offline-images.tsv"
fi
if [ "$SKIP_AGENTS" = false ]; then
  if [ -f "$INVENTORY" ]; then
    offline_args=()
    if [ "$OFFLINE" = true ]; then offline_args=(--offline --offline-package "$OFFLINE_PACKAGE" --public-key "$PUBLIC_KEY"); fi
    bash "$SCRIPT_DIR/upgrade-agents.sh" "$VERSION" --inventory "$INVENTORY" --image-repository "$IMAGE_REPOSITORY" "${offline_args[@]}"
  else
    log "agent inventory not found; control plane rolled back, agents skipped ($INVENTORY)"
  fi
fi

log "program rollback completed: $VERSION"
log "MySQL and MinIO data were not restored"
