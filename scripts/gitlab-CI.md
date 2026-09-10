# Local GitLab development CI

The single `linux-docker` job builds the web UI, runs the complete Go test suite
and `go vet` with two-way parallelism, creates Linux amd64/arm64 and Windows
amd64 executable archives and SHA-256 checksums, then pushes a Linux amd64 image
to `$CI_REGISTRY_IMAGE:development-unsigned-$CI_COMMIT_SHA`.

The runner supplies `DOCKER_HOST` for the isolated builder. The GitLab Container
Registry must be enabled. No signing key, host Docker socket, local `.env`, or
license material is needed. The separate Dockerfile allowlists source inputs to
avoid sending untracked workspace caches and secrets to the builder.

Artifacts and build/test logs are uploaded even on failure and expire after
three days. The server may retain the latest successful pipeline's artifacts
longer if its “keep latest successful artifacts” setting remains enabled.
Images have separate registry retention; configure that on the private project
according to the local disk budget.

Every archive and image is explicitly **development / unsigned**. Tag pipelines
fail closed before any build or push because GitLab signing and release approval
are not yet configured. Existing GitHub signed release workflows are unchanged;
GitLab development success is not evidence of formal release approval.

Smoke tests without a daemon: `sh scripts/gitlab-test-build.sh` (BusyBox ash or
Bash). For actual YAML validation, use the local GitLab project's CI Lint API.
