# Local GitLab full signed CI

The current pipeline is `full-signed-build`, not the superseded unsigned
development-only script. GitHub workflows remain historical contract references
and are not pushed or invoked. Only protected main uses `release-signing`;
all tag runs fail closed before publication.

`gitlab-full-build.sh` builds Node 22 UI assets and runs complete Go test/vet,
exports Linux amd64/arm64 and Windows amd64 native programs, builds both Linux
image architectures, and signs app/probe image indices using the existing
Cosign v3.1.3 key. Community's original transparency log policy is not disabled.

The original `gpuflow-deployment-VERSION.tar.gz` is retained with ALL scripts and
deploy files, READMEs, Compose/config templates, LICENSE and VERSION. Additional
`gpuflow-offline-VERSION-linux-{amd64,arm64}.tar.gz` packages contain all four
architecture-matched app/probe/MySQL/MinIO images and all three native programs.
Every program/deployment/offline tar/zip and checksums.txt has a Sigstore bundle;
offline packages also contain signed internal checksums and image-config-digest
manifests. Config bytes are hashed from `docker save` archives, not `.Id` (which
can mean an index/platform manifest in Docker 29). Load verification re-exports
local image content and validates the same config digest, using explicit platform
selection on Docker 29 and portable single-platform archives on classic Docker.
Original online upgrade/rollback/SSH-agent interfaces remain, with additive
signature-verified `--offline` modes. See `deploy/offline/README.md`.

Configure `GPUFLOW_CI_IMAGE` to the approved tools image Digest. It must contain
Docker CLI/Compose/Buildx, Bash, Git, jq, GNU coreutils/tar, gzip, zip and Cosign
v3.1.3. Default source version `v0.0.0-git.<SHA12>` is a signed source snapshot,
not an automatically approved stable release. Required protected variables:

- Exactly one of `COSIGN_PRIVATE_KEY_FILE` (GitLab file variable) or
  `COSIGN_PRIVATE_KEY`, plus explicitly configured `COSIGN_PASSWORD`.
- `SIGSTORE_TRUSTED_ROOT_FILE`: official Sigstore trusted-root JSON supplied by
  the approved tools image or file variable. Strict offline bundle verification
  runs before building and after packaging; no ignore-tlog fallback exists.
- Standard `CI_REGISTRY*` variables. Docker API must be `tcp://builder:2375`;
  HTTP is permitted only for `gitlab.gpuflow.test:5055` and the private CE project.

Each job creates/removes its own pinned BuildKit container in isolated DinD,
limited to 1.5 GiB / 2 CPU and max parallelism 1. Root provisions QEMU or native
arm64 support. Source contexts and credentials stay separate.

Logs are retained on failure for 90 days. Full artifact output appears only
after every signature, checksum and both architecture packages verify. The
`gitlab-verify-offline.sh` fresh-daemon/no-egress integration gate is mandatory
after packaging. BuildKit is removed first to release memory. The pinned Docker
29.3.1 DinD harness image is cached before the gate; the gate itself never pulls,
including its harness images. A unique DinD container and tools client have
`--network none`, no published ports, no host bind mounts, and share only a
new job-owned Unix socket volume. The server uses a separate new data volume;
zero images, containers and application volumes are checked before loading.
No signing private key or registry credentials enter these test containers.

Both architecture archives must pass offline signature, version and image
manifest checks. Linux amd64 is then actually installed in that empty daemon;
all four package images must match their signed config digests, the three core services
must run, and MySQL SELECT 1, MinIO ready, healthz, authenticated nodes and
unauthenticated HTTP 401 must pass. The ARM package check is not an ARM hardware
test, and no GPU task validation is claimed. Cleanup only removes this run's
ownership-labelled containers/volumes; it never prunes the shared builder.

`OFFLINE_DIND_IMAGE` and `GPUFLOW_CI_IMAGE` must be reviewed immutable Digests.
For a deliberately provisioned local-builder experiment, run:

```bash
bash scripts/gitlab-verify-offline.sh VERSION gitlab-artifacts/full /trusted/cosign.pub "$SIGSTORE_TRUSTED_ROOT_FILE"
```

The command requires the dedicated `DOCKER_HOST=tcp://builder:2375`, a numeric
`CI_JOB_ID`, both image variables and cached harness images. It fails closed on
any other endpoint, missing inputs, network attachment or non-empty daemon.

Contract tests `bash scripts/test-gitlab-full.sh`
use command doubles, not real signing/Docker/GPU integration, and cover full
contents, secrets exclusion, tamper, install/upgrade/rollback/SSH-agent upgrade,
running-task protection, readiness restoration and no offline pulling.
`bash scripts/test-gitlab-offline-verifier.sh` additionally checks the verifier's
endpoint/pinning/signature/cache guards, no-network resource creation, polluted
daemon refusal and only-own cleanup under simulated failures. Neither mock suite
substitutes for the real integration gate.

`test-offline-image-digest.sh PRELOADED_ALPINE_AT_DIGEST` is a small real test
against the dedicated builder only. It creates a unique temporary tag, exports
its amd64 image, independently parses the config with jq, removes only that tag,
loads the archive and verifies the same config digest after another export.
It never pulls or deletes the source tag. Classic Docker 24 archive/API behavior
is covered by command-double tests, not claimed as a real Docker 24 daemon run.
