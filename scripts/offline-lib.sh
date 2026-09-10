#!/usr/bin/env bash
# Shared additive helpers; existing online upgrade/rollback remains available.
. "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/offline-image-lib.sh"
offline_die() { printf '[gpuflow-offline] ERROR: %s\n' "$*" >&2; exit 1; }
offline_clear_compose_overrides() {
  local variable
  while IFS= read -r variable; do
    case "$variable" in GPUFLOW_*|COMPOSE_FILE|COMPOSE_PROJECT_NAME|COMPOSE_PROFILES) unset "$variable";; esac
  done < <(compgen -e)
}
offline_architecture() {
  local result
  result=$(docker info --format '{{.OSType}}/{{.Architecture}}')
  case "$result" in linux/amd64|linux/x86_64) printf amd64;; linux/arm64|linux/aarch64) printf arm64;; *) offline_die "unsupported Docker server platform: $result";; esac
}
offline_verify_package() {
  local package=$1 public_key=$2 architecture=$3
  [[ -s "$public_key" ]] || offline_die 'Supply a separately trusted Cosign public key.'
  [[ $(cat "$package/ARCHITECTURE") == "$architecture" ]] || offline_die 'Package architecture does not match the Docker server.'
  command -v cosign >/dev/null || offline_die 'Cosign is required before loading an image.'
  # Do not disable the transparency log gate. The archived trusted root and
  # bundle permit verification without contacting GitLab, TUF, or Rekor.
  cosign verify-blob --offline --trusted-root "$package/trusted_root.json" \
    --key "$public_key" --bundle "$package/SHA256SUMS.sigstore.json" "$package/SHA256SUMS"
  (
    cd "$package"
    while read -r hash path; do
      path=${path#\*}
      [[ "$hash" =~ ^[0-9a-f]{64}$ && "$path" == ./* && "$path" != *'/../'* && "$path" != *'/./'* ]] || offline_die 'Invalid checksum entry.'
      [[ -f "$path" && ! -L "$path" ]] || offline_die 'Missing or symlinked package file.'
    done < SHA256SUMS
    sha256sum --check SHA256SUMS
  )
}
offline_validate_manifest() {
  local manifest=$1 architecture=$2 role image expected platform extra count=0 seen=' '
  while IFS=$'\t' read -r role image expected platform extra; do
    [[ "$role" == \#* || -z "$role" ]] && continue
    [[ -z "$extra" && "$role" =~ ^(app|probe|mysql|minio)$ && "$seen" != *" $role "* ]] || offline_die 'Invalid or duplicate image manifest role.'
    [[ "$image" == "gpuflow-offline/$role:"* && "$image" != *[[:space:]]* && "$expected" =~ ^sha256:[0-9a-f]{64}$ && "$platform" == "linux/$architecture" ]] || offline_die 'Invalid image manifest entry.'
    seen+="$role "; count=$((count + 1))
  done < "$manifest"
  [[ "$count" -eq 4 ]] || offline_die 'The offline package must contain all four required images.'
}
offline_verify_images() {
  local manifest=$1 architecture=$2 role image expected platform extra actual
  offline_validate_manifest "$manifest" "$architecture"
  while IFS=$'\t' read -r role image expected platform extra; do
    [[ "$role" == \#* || -z "$role" ]] && continue
    actual=$(offline_image_config_digest "$image" "$architecture") || offline_die "Cannot verify the loaded config digest/platform: $role"
    [[ "$actual" == "$expected" ]] || offline_die "Loaded image does not match signed config digest: $role"
  done < "$manifest"
}
offline_load_images() {
  local package=$1 architecture=$2 role image expected platform extra existing
  offline_validate_manifest "$package/IMAGE-MANIFEST.tsv" "$architecture"
  while IFS=$'\t' read -r role image expected platform extra; do
    [[ "$role" == \#* || -z "$role" ]] && continue
    if docker image inspect "$image" >/dev/null 2>&1; then
      existing=$(offline_image_config_digest "$image" "$architecture") || offline_die "Cannot verify an existing local image: $image"
      [[ "$existing" == "$expected" ]] || offline_die "Refusing to replace an existing local image tag: $image"
    fi
  done < "$package/IMAGE-MANIFEST.tsv"
  docker load --input "$package/images/docker-images.tar.gz"
  offline_verify_images "$package/IMAGE-MANIFEST.tsv" "$architecture"
}
offline_health_checks() {
  local install_dir=$1
  docker compose --project-directory "$install_dir" exec -T mysql sh -c \
    'MYSQL_PWD="$MYSQL_PASSWORD" mysql -h127.0.0.1 -ugpuflow gpuflow --batch --skip-column-names -e "SELECT 1"' | grep -qx 1 || return 1
  docker compose --project-directory "$install_dir" exec -T minio curl --fail --silent --output /dev/null http://127.0.0.1:9000/minio/health/ready || return 1
  docker compose --project-directory "$install_dir" exec -T control-plane sh -c \
    'wget -q -O /dev/null http://127.0.0.1:8080/healthz && wget -q -O /dev/null --header="Authorization: Bearer $GPUFLOW_TOKEN" http://127.0.0.1:8080/v1/nodes' || return 1
}
