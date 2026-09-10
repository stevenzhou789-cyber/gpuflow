#!/usr/bin/env bash
# Resolve only a key reference; never print or copy private key contents.
gpuflow_signing_key() {
  [[ ${COSIGN_PASSWORD+x} ]] || { echo 'COSIGN_PASSWORD must be explicitly configured.' >&2; return 1; }
  if [[ -n ${COSIGN_PRIVATE_KEY_FILE:-} && -n ${COSIGN_PRIVATE_KEY:-} ]]; then
    echo 'Configure either COSIGN_PRIVATE_KEY_FILE or COSIGN_PRIVATE_KEY, not both.' >&2; return 1
  elif [[ -n ${COSIGN_PRIVATE_KEY_FILE:-} ]]; then
    [[ -s "$COSIGN_PRIVATE_KEY_FILE" && ! -L "$COSIGN_PRIVATE_KEY_FILE" ]] || { echo 'Signing key file is missing or unsafe.' >&2; return 1; }
    printf '%s/%s' "$(cd -- "$(dirname -- "$COSIGN_PRIVATE_KEY_FILE")" && pwd)" "$(basename -- "$COSIGN_PRIVATE_KEY_FILE")"
  elif [[ -n ${COSIGN_PRIVATE_KEY:-} ]]; then
    printf 'env://COSIGN_PRIVATE_KEY'
  else
    echo 'The existing protected signing key is required; unsigned output is forbidden.' >&2; return 1
  fi
}
