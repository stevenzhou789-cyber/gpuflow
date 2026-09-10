#!/usr/bin/env bash
set -Eeuo pipefail
umask 077
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
package=$(cd -- "$script_dir/.." && pwd)
. "$script_dir/offline-lib.sh"
install_dir= public_key= public_url=http://127.0.0.1:18080
while [[ $# -gt 0 ]]; do
  case "$1" in
    --install-dir) install_dir=${2:?}; shift 2;;
    --public-key) public_key=${2:?}; shift 2;;
    --public-url) public_url=${2:?}; shift 2;;
    *) offline_die 'Usage: install-offline.sh --install-dir NEW_DIRECTORY --public-key TRUSTED_COSIGN_PUB [--public-url URL]';;
  esac
done
[[ "$install_dir" == /* && "$install_dir" != / && ! -e "$install_dir" && ! -L "$install_dir" ]] || offline_die 'A new absolute installation directory is required; existing .env/data are never overwritten.'
[[ "$public_url" =~ ^https?://[A-Za-z0-9._-]+(:[0-9]+)?/?$ ]] || offline_die 'Public URL must be a plain HTTP(S) origin.'
public_key=$(cd -- "$(dirname -- "$public_key")" && printf '%s/%s' "$PWD" "$(basename -- "$public_key")")
architecture=$(offline_architecture)
docker compose version >/dev/null
offline_verify_package "$package" "$public_key" "$architecture"
offline_load_images "$package" "$architecture"
offline_clear_compose_overrides
parent=$(cd -- "$(dirname -- "$install_dir")" && pwd -P)
install_dir="$parent/$(basename -- "$install_dir")"
[[ ! -e "$install_dir" && ! -L "$install_dir" ]] || offline_die 'Install directory already exists.'
project=$(basename -- "$install_dir" | tr '[:upper:]' '[:lower:]')
[[ "$project" =~ ^[a-z0-9][a-z0-9_-]*$ ]] || offline_die 'Installation directory name must also be a valid Compose project name.'
[[ -z $(docker ps -aq --filter "label=com.docker.compose.project=$project") ]] || offline_die 'A Compose project with this name already exists.'
# Original named-volume behavior is preserved, but old data with the same
# project name must never silently be adopted by a supposedly fresh install.
[[ -z $(docker volume ls -q --filter "label=com.docker.compose.project=$project") ]] || offline_die 'Existing Compose data volumes found; use the upgrade path instead.'
mkdir "$install_dir"
for file in compose.yaml compose.offline.yaml .env.example README.md PROJECT-README.md OFFLINE-README.md LICENSE VERSION ARCHITECTURE IMAGE-MANIFEST.tsv SOURCE-IMAGES.tsv images.env; do cp "$package/$file" "$install_dir/$file"; done
cp -R "$package/scripts" "$package/deploy" "$package/agents" "$install_dir/"
random_secret() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }
token=$(random_secret); mysql_password=$(random_secret); mysql_root_password=$(random_secret); minio_password=$(random_secret)
{
  printf 'GPUFLOW_TOKEN=%s\nGPUFLOW_PUBLIC_URL=%s\n' "$token" "$public_url"
  printf 'GPUFLOW_MYSQL_PASSWORD=%s\nGPUFLOW_MYSQL_ROOT_PASSWORD=%s\n' "$mysql_password" "$mysql_root_password"
  printf 'GPUFLOW_MINIO_ROOT_USER=gpuflow\nGPUFLOW_MINIO_ROOT_PASSWORD=%s\nGPUFLOW_S3_BUCKET=gpuflow-artifacts\n' "$minio_password"
  cat "$package/images.env"
} > "$install_dir/.env"
unset token mysql_password mysql_root_password minio_password
mkdir -p "$install_dir/.gpuflow"
cp "$package/IMAGE-MANIFEST.tsv" "$install_dir/.gpuflow/offline-images.tsv"
cp "$public_key" "$install_dir/.gpuflow/trusted-cosign.pub"
printf '%s\n' "$(cat "$package/VERSION")" > "$install_dir/.gpuflow/current-version"
# Keep the original compose.yaml exactly intact; CLI flags make first boot
# offline without requiring newer !reset merge tags or removing build support.
docker compose --project-directory "$install_dir" --file "$install_dir/compose.yaml" --file "$install_dir/compose.offline.yaml" \
  up -d --pull never --no-build --wait --wait-timeout 300
offline_health_checks "$install_dir"
printf 'Offline installation and MySQL/MinIO/authenticated API checks passed: %s\n' "$install_dir"
