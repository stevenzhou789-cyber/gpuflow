#!/bin/sh
set -eu
set -o pipefail

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
test_root="$(mktemp -d)"
# Test only disposable files inside the newly created temporary directory.
trap 'rm -f "$test_root/docker" "$test_root/calls" "$test_root/output.log" "$test_root/gitlab-logs/release-gate.log" "$test_root/gitlab-logs/verify-and-package.log" "$test_root/gitlab-logs/image-build-and-push.log" "$test_root/gitlab-artifacts/IMAGE.txt"; rmdir "$test_root/gitlab-logs" "$test_root/gitlab-artifacts" "$test_root" 2>/dev/null || true' EXIT

printf '%s\n' '#!/bin/sh' 'printf "%s\n" "$*" >> "$MOCK_CALLS"' 'if [ "$1" = login ]; then cat >/dev/null; exit 0; fi' 'case "$*" in *"--target artifacts"*) exit "${MOCK_VERIFY_STATUS:-0}" ;; esac' 'exit 0' > "$test_root/docker"
chmod +x "$test_root/docker"
export PATH="$test_root:$PATH" MOCK_CALLS="$test_root/calls"
export CI_COMMIT_SHA=1111111111111111111111111111111111111111
export CI_REGISTRY=registry.example.test CI_REGISTRY_IMAGE=registry.example.test/group/gpuflow
export CI_REGISTRY_USER=ci-test CI_REGISTRY_PASSWORD=test-only DOCKER_HOST=tcp://builder:2375
unset CI_COMMIT_TAG
cd "$test_root"

if CI_COMMIT_TAG=v1.0.0 sh "$script_dir/gitlab-build.sh" > output.log 2>&1; then
  printf '%s\n' 'FAIL: tag release did not fail closed.'; exit 1
fi
test ! -e calls
test -s gitlab-logs/release-gate.log

if MOCK_VERIFY_STATUS=19 sh "$script_dir/gitlab-build.sh" > output.log 2>&1; then
  printf '%s\n' 'FAIL: verification failure was swallowed by tee.'; exit 1
fi
if grep -q -- '--target runtime' calls; then
  printf '%s\n' 'FAIL: image was built after verification failed.'; exit 1
fi
test -s gitlab-logs/verify-and-package.log || test -f gitlab-logs/verify-and-package.log

sh "$script_dir/gitlab-build.sh" > output.log 2>&1
grep -q -- '--target runtime' calls
grep -q 'development-unsigned-1111111111111111111111111111111111111111' gitlab-artifacts/IMAGE.txt
grep -q 'signed=false' gitlab-artifacts/IMAGE.txt
if grep -q 'test-only' calls; then
  printf '%s\n' 'FAIL: password appeared in Docker arguments.'; exit 1
fi
printf '%s\n' 'PASS: tag fail-closed, verification failure, successful development build, and password stdin checks.'
