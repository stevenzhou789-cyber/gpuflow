#!/usr/bin/env bash
# Test-only command double. Does not perform Docker operations or cryptography.
set -Eeuo pipefail
name=$(basename "$0")
printf '%s %s\n' "$name" "$*" >> "$MOCK_VERIFIER_LOG"
if [[ "$name" == cosign ]]; then [[ ${MOCK_BAD_SIGNATURE:-false} != true ]]; exit $?; fi
[[ "$name" == docker ]] || exit 90
if [[ ${1:-} == --host ]]; then [[ "$2" == tcp://builder:2375 ]]; shift 2; fi
case "${1:-} ${2:-}" in
  'info --format') printf 'linux/amd64\n';;
  'info ') exit 0;;
  'version --format') printf '29.3.1\n';;
  'image ls') if [[ ${MOCK_INNER_DIRTY:-false} == true ]]; then printf 'preexisting-image\n'; fi;;
  'ps -aq'|'volume ls') exit 0;;
  'image inspect') [[ ${MOCK_MISSING_HARNESS:-false} != true ]];;
  'volume inspect')
    resource=${!#}
    [[ -f "$MOCK_VERIFIER_STATE/$resource" ]] || exit 1
    if [[ "$*" == *--format* ]]; then cat "$MOCK_VERIFIER_STATE/$resource"; fi
    ;;
  'volume create')
    resource=${!#}
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == --label ]]; then printf '%s\n' "${2#*=}" > "$MOCK_VERIFIER_STATE/$resource"; break; fi
      shift
    done
    printf '%s\n' "$resource"
    ;;
  'volume rm') rm "$MOCK_VERIFIER_STATE/$3";;
  'create --name')
    resource_name=$3
    character=a; [[ "$resource_name" != *-client ]] || character=b
    resource=$(printf '%064d' 0 | tr 0 "$character")
    while [[ $# -gt 0 ]]; do
      if [[ "$1" == --label ]]; then printf '%s\n' "${2#*=}" > "$MOCK_VERIFIER_STATE/$resource"; break; fi
      shift
    done
    printf '%s\n' "$resource"
    ;;
  'inspect --format')
    case "$3" in
      *NetworkMode*) if [[ ${MOCK_UNSAFE_NETWORK:-false} == true ]]; then printf 'bridge\n'; else printf 'none\n'; fi;;
      *PortBindings*) printf '0\n';;
      *) cat "$MOCK_VERIFIER_STATE/${!#}";;
    esac
    ;;
  'rm --force') rm "$MOCK_VERIFIER_STATE/${!#}";;
  'start '*) [[ ${MOCK_START_FAIL:-false} != true ]];;
  'cp '*) exit 0;;
  'exec '*)
    if [[ "$*" == *'bash /input/verify.sh'* ]]; then [[ ${MOCK_INNER_FAIL:-false} != true ]]; fi
    ;;
  *) printf 'Unexpected test-double call: %s\n' "$*" >&2; exit 91;;
esac
