#!/usr/bin/env bash
set -Eeuo pipefail
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
package=$(cd -- "$script_dir/.." && pwd)
. "$script_dir/offline-lib.sh"
[[ $# -eq 2 && "$1" == --public-key ]] || offline_die 'Usage: load-offline-images.sh --public-key TRUSTED_COSIGN_PUB'
architecture=$(offline_architecture)
offline_verify_package "$package" "$2" "$architecture"
offline_load_images "$package" "$architecture"
printf 'Signed offline images loaded and verified for linux/%s.\n' "$architecture"
