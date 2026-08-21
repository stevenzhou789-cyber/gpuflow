#!/usr/bin/env bash

log() {
  printf '[gpuflow-upgrade] %s\n' "$*"
}

die() {
  printf '[gpuflow-upgrade] ERROR: %s\n' "$*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "missing required command: $1"
}

validate_version() {
  [[ "$1" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9._-]+)?$ ]] || \
    die "version must look like v1.0.1 (received: $1)"
}

read_env_value() {
  local file=$1 key=$2 line
  [ -f "$file" ] || return 0
  line=$(grep -E "^${key}=" "$file" | tail -n 1 || true)
  printf '%s' "${line#*=}"
}

set_env_value() {
  local file=$1 key=$2 value=$3 temporary
  temporary="${file}.tmp.$$"
  awk -v key="$key" -v value="$value" '
    BEGIN { updated = 0 }
    index($0, key "=") == 1 {
      if (!updated) print key "=" value
      updated = 1
      next
    }
    { print }
    END { if (!updated) print key "=" value }
  ' "$file" > "$temporary"
  mv "$temporary" "$file"
}

wait_for_control_plane() {
  local compose_dir=$1 attempts=${2:-30} container
  container=$(docker compose --project-directory "$compose_dir" ps -q control-plane)
  [ -n "$container" ] || return 1
  while [ "$attempts" -gt 0 ]; do
    if docker exec "$container" wget -q -O /dev/null http://127.0.0.1:8080/healthz >/dev/null 2>&1; then
      return 0
    fi
    attempts=$((attempts - 1))
    sleep 2
  done
  return 1
}

backup_installation() {
  local install_dir=$1 backup_dir=$2
  mkdir -p "$backup_dir"
  cp "$install_dir/.env" "$backup_dir/.env"
  cp "$install_dir/compose.yaml" "$backup_dir/compose.yaml"

  log "backing up MySQL to $backup_dir/mysql.sql"
  docker compose --project-directory "$install_dir" exec -T mysql \
    sh -c 'exec mysqldump -uroot -p"$MYSQL_ROOT_PASSWORD" --single-transaction --routines --events gpuflow' \
    > "$backup_dir/mysql.sql"
}

restore_control_image() {
  local install_dir=$1 saved_env=$2
  log "restoring the previous control-plane image"
  cp "$saved_env" "$install_dir/.env"
  docker compose --project-directory "$install_dir" up -d --no-deps --pull never control-plane
}
