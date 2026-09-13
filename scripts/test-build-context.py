#!/usr/bin/env python3
"""Exercise Docker's actual ignore handling using synthetic, scratch-only builds.

No repository source, credentials, base images, containers or registry writes are
used. Pass --enterprise-root to verify the private repository in the same run.
"""

from __future__ import annotations

import argparse
from pathlib import Path
import shutil
import subprocess
import tempfile


COMMUNITY_INPUTS = [
    "go.mod", "go.sum", "cmd/gpuflow/main.go", "internal/agent/agent.go",
    "internal/webui/dist/index.html", "pkg/platform/platform.go",
    "web/package.json", "web/package-lock.json", "web/src/App.tsx",
    "scripts/check-community-boundary.mjs", "internal/store/testdata/schema.sql",
]
ENTERPRISE_INPUTS = [
    "Dockerfile", "go.mod", "go.sum", "cmd/gpuflow-commercial/main.go",
    "internal/license/license.go", "internal/license/vendor-public.key",
    "pkg/fixture.go", ".github/community-ref",
    ".github/scripts/prepare-enterprise-ui.mjs",
    ".github/scripts/test-prepare-enterprise-ui.mjs",
    "patches/community-enterprise-ui.patch", "internal/store/testdata/schema.sql",
]
LOCAL_FILES = [
    ".env", ".env.production", ".local-gitlab/git-push.credential.xml",
    ".gitlab-migration-work/enterprise/private.go",
    ".enterprise-fix-example/private.go", ".review-enterprise-example/private.go",
    ".tmp-enterprise-ui-check.tar", ".tmp-enterprise-ui-overlay/private.tsx",
    ".go-cache-root/cache", ".issuer/signing.key", ".git/config",
    "data/customer.db", "deploy/.env", "deploy/license/license.json",
    "internal/local.key", "internal/local.pem", "internal/local.credential.xml",
    "internal/.env.production", "internal/license.json",
    "internal/local.exe", "internal/nested/license-customer.json",
    "internal/backups/customer.db", "internal/nested/.git/config",
    "web/node_modules/dependency/index.js", "web/nested/node_modules/index.js",
    "customer-export.csv", "unlisted-project/private.go",
    "internal/debug.tar", "internal/debug.tar.gz", "internal/debug.tgz",
    "internal/debug.zip", "internal/debug.7z", "internal/debug.rar",
    "internal/customer.db", "internal/customer.sqlite", "internal/customer.sqlite3",
    "internal/.go-cache-debug/object", "web/.build-cache/cache", "web/.cache/cache",
    "internal/.tmp/customer.txt", "internal/.tmp-debug/customer.txt",
    "internal/.local-gitlab/customer.txt", "internal/.gitlab-migration-work/customer.txt",
    "internal/.issuer/customer.txt",
]


def run(command: list[str], *, cwd: Path) -> str:
    result = subprocess.run(command, cwd=cwd, text=True, capture_output=True)
    if result.returncode:
        raise RuntimeError(f"Command failed ({result.returncode}): {' '.join(command)}\n"
                           f"{result.stdout}{result.stderr}")
    return result.stdout


def write_fixture(root: Path, paths: list[str]) -> None:
    for relative in paths:
        path = root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(f"synthetic fixture: {relative}\n", encoding="utf-8")


def check_context(work: Path, name: str, ignore: Path, required: list[str],
                  excluded: list[str], *, named: bool = False) -> None:
    context = work / name / "context"
    context.mkdir(parents=True)
    write_fixture(context, required + excluded)
    shutil.copyfile(ignore, context / ".dockerignore")
    dockerfile = work / name / "Probe.Dockerfile"
    output = work / name / "output"
    copy = "COPY --from=gpuflow . /payload/" if named else "COPY . /payload/"
    dockerfile.write_text(f"FROM scratch\n{copy}\n", encoding="utf-8")
    command = ["docker", "buildx", "build", "--network=none", "--no-cache",
               "--progress=plain", "--file", str(dockerfile),
               "--output", f"type=local,dest={output}"]
    if named:
        primary = work / name / "primary"
        primary.mkdir()
        command += ["--build-context", f"gpuflow={context}", str(primary)]
    else:
        command.append(str(context))
    run(command, cwd=work)
    actual = {path.relative_to(output / "payload").as_posix()
              for path in (output / "payload").rglob("*") if path.is_file()}
    missing = set(required) - actual
    leaked = set(excluded) & actual
    if missing or leaked:
        raise AssertionError(f"{name}: missing required inputs={sorted(missing)}; "
                             f"included local files={sorted(leaked)}")
    print(f"PASS {name}: {len(required)} build inputs retained; "
          f"{len(excluded)} local/private fixture paths excluded", flush=True)


def check_gitignore(root: Path, excluded: list[str], required: list[str]) -> None:
    command = ["git", "-c", f"safe.directory={root.as_posix()}", "check-ignore",
               "--no-index", "-z", "--stdin"]
    result = subprocess.run(command, cwd=root,
                            input=("\0".join(excluded + required) + "\0").encode(),
                            capture_output=True)
    if result.returncode not in (0, 1):
        raise RuntimeError(result.stderr.decode())
    ignored = set(result.stdout.decode().split("\0"))
    missing = set(excluded) - ignored
    hidden = set(required) & ignored
    if missing or hidden:
        raise AssertionError(f"{root.name} gitignore: unprotected={sorted(missing)}; "
                             f"hidden source={sorted(hidden)}")
    print(f"PASS {root.name} gitignore: local paths ignored and source visible", flush=True)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--enterprise-root", type=Path)
    args = parser.parse_args()
    root = Path(__file__).resolve().parents[1]
    git_excluded = [".enterprise-fix-example/private.go",
                    ".review-enterprise-example/private.go",
                    ".tmp-enterprise-ui-check.tar", ".go-cache-root/cache",
                    ".local-gitlab/git-push.credential.xml",
                    ".gitlab-migration-work/enterprise/private.go", ".env.production",
                    "deploy/.env.production"]
    check_gitignore(root, git_excluded, [".env.example", "internal/store/store.go",
                                        "scripts/check-community-boundary.mjs"])
    # Every recursive cleanup is confined to a generated child of this workspace.
    temp_parent = root / ".tmp"
    temp_parent.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="build-context-", dir=temp_parent) as temporary:
        work = Path(temporary).resolve()
        if work.parent != temp_parent.resolve():
            raise RuntimeError("Fixture cleanup target escaped the workspace temporary directory")
        inputs = COMMUNITY_INPUTS + ["Dockerfile", "deploy/probe/Dockerfile"]
        excluded = LOCAL_FILES + [".github/workflows/private.yml", "scripts/agents.conf"]
        check_context(work, "community-default", root / ".dockerignore", inputs, excluded)
        check_context(work, "community-named", root / ".dockerignore", inputs, excluded, named=True)
        for filename in ("gitlab-build.Dockerfile.dockerignore",
                         "gitlab-full-build.Dockerfile.dockerignore"):
            check_context(work, filename, root / "scripts" / filename, COMMUNITY_INPUTS,
                          excluded + ["Dockerfile", "deploy/probe/Dockerfile"])
        if args.enterprise_root:
            enterprise = args.enterprise_root.resolve()
            if not (enterprise / ".dockerignore").is_file():
                raise RuntimeError("Enterprise root must contain .dockerignore")
            check_gitignore(enterprise, git_excluded + [".ui-check/private.tsx", "review.tar"],
                            ["deploy/.env.example", "internal/license/vendor-public.key",
                             "patches/community-enterprise-ui.patch"])
            check_context(work, "enterprise-default", enterprise / ".dockerignore",
                          ENTERPRISE_INPUTS,
                          LOCAL_FILES + [".ui-check/private.tsx", "review.tar",
                                         ".github/workflows/private.yml", "gpuflow/private.txt",
                                         ".github/scripts/local.credential.xml"])


if __name__ == "__main__":
    main()
