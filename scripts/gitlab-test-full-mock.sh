#!/usr/bin/env bash
# Hermetic command double; used only by test-gitlab-full.sh, never by CI builds.
set -Eeuo pipefail
command_name=$(basename "$0")
printf '%s %s\n' "$command_name" "$*" >> "$MOCK_CALLS"
case "$command_name" in
  sleep) exit 0;;
  ssh)
    while [[ $# -gt 0 && "$1" != -- ]]; do shift; done
    [[ $# -gt 0 ]] || exit 1
    shift
    bash -s -- "$@"
    exit $?
    ;;
  git)
    if [[ "$1" == ls-files ]]; then
      find compose.yaml .env.example LICENSE README.md scripts deploy -type f ! -name .env ! -name agents.conf -print0
      if [[ ${MOCK_TRACKED_SECRET:-false} == true ]]; then printf 'deploy/.env\0'; fi
      exit 0
    fi
    ;;
  zip) printf 'ZIP-MOCK-ONLY\n' > "$2"; exit 0;;
  cosign)
    case "$1" in
      version) printf 'GitVersion: v3.1.3\n';;
      sign-blob)
        while [[ $# -gt 0 ]]; do
          if [[ "$1" == --bundle ]]; then printf '{}\n' > "$2"; break; fi
          shift
        done
        ;;
      verify-blob) [[ ${MOCK_BAD_SIGNATURE:-false} != true ]];;
      *) exit 0;;
    esac
    exit $?
    ;;
  docker)
    case "$1 $2" in
      'version --format') printf '%s.3.1 %s.3.1\n' "${MOCK_DOCKER_MAJOR:-29}" "${MOCK_DOCKER_MAJOR:-29}";;
      'info --format') printf 'linux/amd64\n';;
      'image inspect')
        last=${!#}
        [[ ${MOCK_IMAGES_MISSING:-false} != true ]] || exit 1
        architecture=amd64
        [[ "$*" != *arm64* ]] || architecture=arm64
        if [[ "$*" == *'{{.Os}}/{{.Architecture}}'* ]]; then printf 'linux/%s\n' "$architecture"
        else
          letter=a; [[ "$architecture" != arm64 ]] || letter=b
          printf 'sha256:'; printf "%064d\n" 0 | tr 0 "$letter"
        fi
        ;;
      'image save')
        shift 2
        architecture=amd64; output=; targets=()
        while [[ $# -gt 0 ]]; do
          case "$1" in
            --platform) architecture=${2#linux/}; shift 2;;
            --output) output=$2; shift 2;;
            *) targets+=("$1"); shift;;
          esac
        done
        [[ -n "$output" && ${#targets[@]} -gt 0 ]]
        fixture=$(mktemp -d -t gpuflow-save-fixture.XXXXXXXX)
        trap 'rm -rf -- "$fixture"' EXIT
        mkdir -p "$fixture/blobs/sha256"
        printf '[' > "$fixture/manifest.json"
        separator=
        for target in "${targets[@]}"; do
          role=${target%:*}; role=${role##*/}
          printf '{"architecture":"%s","os":"linux","config":{"Labels":{"role":"%s","changed":"%s"}}}' "$architecture" "$role" "${MOCK_WRONG_CONFIG:-false}" > "$fixture/config.json"
          config_digest=$(sha256sum "$fixture/config.json" | awk '{print $1}')
          config_path="blobs/sha256/$config_digest"
          if [[ ${MOCK_DOCKER_MAJOR:-29} == 24 ]]; then config_path="$config_digest.json"; fi
          mv "$fixture/config.json" "$fixture/$config_path"
          printf '%s{"Config":"%s","RepoTags":["%s"],"Layers":[]}' "$separator" "$config_path" "$target" >> "$fixture/manifest.json"
          separator=,
        done
        printf ']\n' >> "$fixture/manifest.json"
        tar -C "$fixture" -cf "$output" manifest.json blobs $(find "$fixture" -maxdepth 1 -name '*.json' ! -name manifest.json -printf '%f ')
        ;;
      'compose --project-directory')
        if [[ "$*" == *' ps -q '* ]]; then printf 'mock-container\n'
        elif [[ "$*" == *'SELECT 1'* ]]; then [[ ${MOCK_FAIL_MYSQL:-false} != true ]] || exit 1; printf '1\n'
        elif [[ "$*" == *mysqldump* ]]; then printf 'MOCK SQL BACKUP\n'; fi
        ;;
      'inspect -f') printf 'true\n';;
      'ps -q') if [[ ${MOCK_RUNNING_JOBS:-false} == true ]]; then printf 'mock-running-task\n'; fi;;
    esac
    exit 0
    ;;
esac
printf 'Unexpected mock command: %s %s\n' "$command_name" "$*" >&2
exit 91
