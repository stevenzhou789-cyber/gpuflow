#!/usr/bin/env bash
set -Eeuo pipefail
set +x
umask 077
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
. "$script_dir/gitlab-signing-lib.sh"
. "$script_dir/offline-image-lib.sh"
signing_key=$(gpuflow_signing_key)
version=${1:?VERSION BINARIES APP_DIGEST PROBE_DIGEST PUBLIC_KEY TRUSTED_ROOT}
binaries=${2:?}; app=${3:?}; probe=${4:?}; public_key=${5:?}; trusted_root=${6:?}
[[ "$version" == stable || "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][A-Za-z0-9._-]+)?$ ]]
[[ "$app" =~ @sha256:[0-9a-f]{64}$ && "$probe" =~ @sha256:[0-9a-f]{64}$ ]]
: "${CI_COMMIT_SHA:?}"
for path in "$binaries/linux-amd64/gpuflow" "$binaries/linux-arm64/gpuflow" "$binaries/windows-amd64/gpuflow.exe" "$public_key" "$trusted_root"; do test -s "$path"; done
[[ ! -e gitlab-artifacts/full ]] || { echo 'Refusing to mix this package with an existing full output directory.' >&2; exit 1; }
work=$(mktemp -d -t gpuflow-full-package.XXXXXXXX)
trap 'rm -rf -- "$work"' EXIT
dist="$work/dist"
mkdir -p "$dist"
package_name="gpuflow-deployment-$version"
base="$work/$package_name"
mkdir -p "$base"

# Exactly the original release tree, restricted to tracked source paths. This
# preserves ALL deployment/upgrade/rollback/agent scripts without local state.
git ls-files -z -- compose.yaml .env.example LICENSE README.md scripts deploy > "$work/tracked-files"
test -s "$work/tracked-files"
while IFS= read -r -d '' path; do
  case "$path" in *.key|*.pem|*/.env|.env|*/agents.conf|*/license.json|*/node_modules/*|*/backups/*|*/.gpuflow/*) echo "Refusing sensitive tracked package path: $path" >&2; exit 1;; esac
done < "$work/tracked-files"
tar --null -T "$work/tracked-files" -cf - | tar -xf - -C "$base"
cp "$base/README.md" "$base/PROJECT-README.md"
cp "$base/deploy/README.md" "$base/README.md"
printf '%s\n' "$version" > "$base/VERSION"
chmod +x "$base"/scripts/*.sh
cp "$public_key" "$dist/cosign.pub"
cp "$trusted_root" "$dist/trusted_root.json"
tar -C "$binaries/linux-amd64" -czf "$dist/gpuflow-linux-amd64.tar.gz" gpuflow
tar -C "$binaries/linux-arm64" -czf "$dist/gpuflow-linux-arm64.tar.gz" gpuflow
zip -j "$dist/gpuflow-windows-amd64.zip" "$binaries/windows-amd64/gpuflow.exe"
tar -C "$work" -czf "$dist/$package_name.tar.gz" "$package_name"

mysql_source='mysql:8.4.11@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb'
minio_source='minio/minio:RELEASE.2025-04-22T22-12-26Z@sha256:a1ea29fa28355559ef137d71fc570e508a214ec84ff8083e39bc5428980b015e'
for architecture in amd64 arm64; do
  offline_name="gpuflow-offline-$version-linux-$architecture"
  offline="$work/$offline_name"
  cp -R "$base" "$offline"
  mkdir -p "$offline/images" "$offline/agents"
  cp -R "$binaries/." "$offline/agents/"
  cp "$public_key" "$offline/cosign.pub"
  cp "$trusted_root" "$offline/trusted_root.json"
  printf '%s\n' "$architecture" > "$offline/ARCHITECTURE"
  printf '# role\timage_ref\tconfig_digest\tplatform\n' > "$offline/IMAGE-MANIFEST.tsv"
  printf '# role\tsource_digest\n' > "$offline/SOURCE-IMAGES.tsv"
  images=()
  for role in app probe mysql minio; do
    case "$role" in app) source=$app;; probe) source=$probe;; mysql) source=$mysql_source;; minio) source=$minio_source;; esac
    # Same local reference on every node, with the host-specific image loaded
    # from its own architecture package. The control plane can therefore hand
    # the SAME Agent/Probe reference to both amd64 and arm64 nodes offline.
    target="gpuflow-offline/$role:$version"
    docker pull --platform "linux/$architecture" "$source"
    platform=$(docker image inspect --platform "linux/$architecture" --format '{{.Os}}/{{.Architecture}}' "$source")
    [[ "$platform" == "linux/$architecture" ]]
    docker tag "$source" "$target"
    images+=("$target")
    printf '%s\t%s\n' "$role" "$source" >> "$offline/SOURCE-IMAGES.tsv"
  done
  image_archive="$work/images-$architecture.tar"
  docker image save --platform "linux/$architecture" --output "$image_archive" "${images[@]}"
  for role in app probe mysql minio; do
    target="gpuflow-offline/$role:$version"
    config_digest=$(offline_archive_config_digest "$image_archive" "$target")
    printf '%s\t%s\t%s\tlinux/%s\n' "$role" "$target" "$config_digest" "$architecture" >> "$offline/IMAGE-MANIFEST.tsv"
  done
  gzip -1 < "$image_archive" > "$offline/images/docker-images.tar.gz"
  rm -- "$image_archive"
  # The original Compose/templates remain available verbatim; the offline
  # installer uses a dedicated pull-never override rather than deleting them.
  cp deploy/offline/compose.offline.yaml "$offline/compose.offline.yaml"
  cp deploy/offline/README.md "$offline/OFFLINE-README.md"
  {
    printf 'GPUFLOW_IMAGE=gpuflow-offline/app:%s\nGPUFLOW_AGENT_IMAGE=gpuflow-offline/app:%s\n' "$version" "$version"
    printf 'GPUFLOW_PROBE_IMAGE=gpuflow-offline/probe:%s\n' "$version"
    printf 'GPUFLOW_MYSQL_IMAGE=gpuflow-offline/mysql:%s\nGPUFLOW_MINIO_IMAGE=gpuflow-offline/minio:%s\n' "$version" "$version"
  } > "$offline/images.env"
  (
    cd "$offline"
    find . -type f ! -name SHA256SUMS ! -name '*.sigstore.json' ! -name '*.sig' -print0 | LC_ALL=C sort -z | xargs -0 sha256sum > SHA256SUMS
    cosign sign-blob --yes --key "$signing_key" --bundle SHA256SUMS.sigstore.json SHA256SUMS
    cosign verify-blob --offline --trusted-root trusted_root.json --key cosign.pub --bundle SHA256SUMS.sigstore.json SHA256SUMS
    sha256sum --check SHA256SUMS
  )
  tar -C "$work" -czf "$dist/$offline_name.tar.gz" "$offline_name"
done
(
  cd "$dist"
  sha256sum ./*.tar.gz ./*.zip trusted_root.json > checksums.txt
  for artifact in ./*.tar.gz ./*.zip checksums.txt; do
    cosign sign-blob --yes --key "$signing_key" --bundle "$artifact.sigstore.json" "$artifact"
    cosign verify-blob --offline --trusted-root trusted_root.json --key cosign.pub --bundle "$artifact.sigstore.json" "$artifact"
  done
  sha256sum --check checksums.txt
)
mkdir -p gitlab-artifacts
mv "$dist" gitlab-artifacts/full
