#!/usr/bin/env python3
"""Build Community release archives from verified binaries and public deployment files.

This command packages only. It does not sign, upload, run Docker, or authorize a
release. The release workflow must verify and sign the resulting checksums.
"""

from __future__ import annotations

import argparse
import gzip
import hashlib
import io
import os
from pathlib import Path
import re
import shutil
import stat
import subprocess
import tarfile
import tempfile
import zipfile


OPERATIONS = (
    "agents.conf.example", "upgrade.sh", "rollback.sh", "upgrade-agents.sh",
    "upgrade-lib.sh", "offline-lib.sh", "offline-image-lib.sh",
    "install-offline.sh", "load-offline-images.sh",
)
DEPLOY_FILES = (
    "README.md", "agent/.env.example", "agent/compose.yaml",
    "offline/README.md", "offline/compose.offline.yaml",
    "probe/Dockerfile", "probe/README.md",
)
NATIVE_FILES = (
    ("linux-amd64/gpuflow", "gpuflow-linux-amd64.tar.gz"),
    ("linux-arm64/gpuflow", "gpuflow-linux-arm64.tar.gz"),
    ("windows-amd64/gpuflow.exe", "gpuflow-windows-amd64.zip"),
)


class PackageError(RuntimeError):
    pass


def validate_identity(version: str, repository: str, digest: str) -> None:
    if not re.fullmatch(r"stable|v(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)", version):
        raise PackageError("Version must be stable or vMAJOR.MINOR.PATCH without leading zeroes.")
    component = r"[a-z0-9]+(?:[._-][a-z0-9]+)*"
    if not re.fullmatch(rf"ghcr\.io/{component}/{component}", repository):
        raise PackageError("Image repository must be a lowercase ghcr.io/owner/repository without a tag or credentials.")
    if not re.fullmatch(r"sha256:[0-9a-f]{64}", digest):
        raise PackageError("Image digest must be sha256 followed by 64 lowercase hexadecimal characters.")


def linked_path(path: Path) -> bool:
    if path.is_symlink():
        return True
    try:
        attributes = getattr(path.lstat(), "st_file_attributes", 0)
    except FileNotFoundError:
        return False
    return bool(attributes & getattr(stat, "FILE_ATTRIBUTE_REPARSE_POINT", 0))


def regular_file(path: Path) -> None:
    if not path.is_file() or linked_path(path):
        raise PackageError(f"Required regular file is missing or unsafe: {path.name}")


def copy_snapshot(source: Path, destination: Path) -> None:
    if linked_path(source):
        raise PackageError(f"Symlink or reparse point is not an allowed package input: {source.name}")
    if source.is_dir():
        destination.mkdir()
        for child in sorted(source.iterdir()):
            copy_snapshot(child, destination / child.name)
    else:
        regular_file(source)
        if source.stat().st_size > 16 * 1024 * 1024:
            raise PackageError(f"Deployment source file is too large: {source.name}")
        shutil.copyfile(source, destination)


def check_boundary(checker: Path, root: Path) -> None:
    result = subprocess.run(["node", str(checker), "--delivery-tree", str(root)],
                            capture_output=True, text=True, encoding="utf-8")
    if result.returncode:
        # The boundary checker prints paths and reason codes, never file content.
        raise PackageError("Community deployment boundary check failed:\n" + result.stderr.strip())


def copy_text(source: Path, destination: Path) -> None:
    regular_file(source)
    try:
        content = source.read_text(encoding="utf-8-sig").replace("\r\n", "\n")
    except UnicodeError as error:
        raise PackageError(f"Deployment input is not UTF-8 text: {source.name}") from error
    destination.parent.mkdir(parents=True, exist_ok=True)
    destination.write_text(content, encoding="utf-8", newline="\n")


def pin_setting(template: Path, key: str, value: str) -> None:
    content = template.read_text(encoding="utf-8")
    content, count = re.subn(rf"(?m)^{re.escape(key)}=[^\r\n]*$", f"{key}={value}", content)
    if count != 1:
        raise PackageError(f"Expected exactly one {key} in {template.name}.")
    template.write_text(content, encoding="utf-8", newline="\n")


def remove_source_build(compose: Path) -> None:
    content = compose.read_text(encoding="utf-8")
    declarations = re.findall(r"(?m)^\s*build\s*:[^\r\n]*$", content)
    if len(declarations) != 1 or content.splitlines().count("    build: .") != 1:
        raise PackageError("Unknown Compose build configuration; expected only the control-plane '    build: .'.")
    content = "".join(line for line in content.splitlines(keepends=True)
                      if line.rstrip("\r\n") != "    build: .")
    compose.write_text(content, encoding="utf-8", newline="\n")


def write_tar(path: Path, entries: list[tuple[str, bytes, int]]) -> None:
    # Stable gzip/tar metadata makes the same verified inputs reproducible.
    with path.open("xb") as raw:
        with gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as archive:
                for name, content, mode in sorted(entries):
                    info = tarfile.TarInfo(name)
                    info.size = len(content)
                    info.mode = mode
                    info.mtime = 0
                    archive.addfile(info, io.BytesIO(content))


def write_zip(path: Path, name: str, content: bytes) -> None:
    with zipfile.ZipFile(path, mode="x", compression=zipfile.ZIP_DEFLATED) as archive:
        info = zipfile.ZipInfo(name, date_time=(1980, 1, 1, 0, 0, 0))
        info.create_system = 3
        info.external_attr = (stat.S_IFREG | 0o755) << 16
        info.compress_type = zipfile.ZIP_DEFLATED
        archive.writestr(info, content)


def package_release(*, source_root: Path, version: str, image_repository: str,
                    image_digest: str, build_dir: Path, output_dir: Path) -> list[Path]:
    validate_identity(version, image_repository, image_digest)
    source_root, build_dir = source_root.resolve(), build_dir.resolve()
    output_dir = Path(os.path.abspath(output_dir))
    if output_dir.exists() or output_dir.is_symlink():
        raise PackageError("Output directory already exists; refusing to overwrite release material.")
    if not output_dir.parent.is_dir():
        raise PackageError("Output parent directory must already exist.")
    for directory in (source_root / "scripts", source_root / "deploy"):
        if output_dir.resolve().is_relative_to(directory.resolve()):
            raise PackageError("Output must not be inside deployment source directories.")
    checker = source_root / "scripts/check-community-boundary.mjs"
    regular_file(checker)
    binaries = {}
    for relative, archive_name in NATIVE_FILES:
        binary = build_dir / relative
        if linked_path(binary.parent):
            raise PackageError("Binary architecture directory must not be a symlink.")
        regular_file(binary)
        content = binary.read_bytes()
        if not content:
            raise PackageError(f"Native binary is empty: {relative}")
        binaries[archive_name] = (binary.name, content)

    with tempfile.TemporaryDirectory(prefix=".tmp-community-package-", dir=output_dir.parent) as temporary:
        work = Path(temporary).resolve()
        # Cleanup is confined to this generated child, never an input/output tree.
        if work.parent != output_dir.parent.resolve():
            raise PackageError("Temporary package directory escaped its expected parent.")
        snapshot = work / "source"
        snapshot.mkdir()
        for relative in ("compose.yaml", ".env.example", "LICENSE", "README.md", "scripts", "deploy"):
            copy_snapshot(source_root / relative, snapshot / relative)
        check_boundary(checker, snapshot)

        name = f"gpuflow-deployment-{version}"
        deployment = work / name
        deployment.mkdir()
        for relative in ("compose.yaml", ".env.example", "LICENSE"):
            copy_text(snapshot / relative, deployment / relative)
        copy_text(snapshot / "README.md", deployment / "PROJECT-README.md")
        copy_text(snapshot / "deploy/README.md", deployment / "README.md")
        for filename in OPERATIONS:
            copy_text(snapshot / "scripts" / filename, deployment / "scripts" / filename)
        for filename in DEPLOY_FILES:
            copy_text(snapshot / "deploy" / filename, deployment / "deploy" / filename)
        image = f"{image_repository}@{image_digest}"
        pin_setting(deployment / ".env.example", "GPUFLOW_IMAGE", image)
        pin_setting(deployment / ".env.example", "GPUFLOW_AGENT_IMAGE", image)
        pin_setting(deployment / "deploy/agent/.env.example", "GPUFLOW_AGENT_IMAGE", image)
        remove_source_build(deployment / "compose.yaml")
        (deployment / "VERSION").write_text(version + "\n", encoding="utf-8", newline="\n")
        check_boundary(checker, deployment)

        staged = work / "artifacts"
        staged.mkdir()
        for archive_name, (binary_name, content) in binaries.items():
            if archive_name.endswith(".zip"):
                write_zip(staged / archive_name, binary_name, content)
            else:
                write_tar(staged / archive_name, [(binary_name, content, 0o755)])
        entries = []
        for file in sorted(deployment.rglob("*")):
            if file.is_file():
                relative = file.relative_to(deployment).as_posix()
                mode = 0o755 if relative.endswith(".sh") else 0o644
                entries.append((f"{name}/{relative}", file.read_bytes(), mode))
        write_tar(staged / f"{name}.tar.gz", entries)
        hashes = [f"{hashlib.sha256(file.read_bytes()).hexdigest()}  {file.name}\n"
                  for file in sorted(staged.iterdir())]
        (staged / "checksums.txt").write_text("".join(hashes), encoding="utf-8", newline="\n")

        # Exclusive directory/file creation also refuses a competing publisher.
        output_dir.mkdir()
        outputs = []
        for file in sorted(staged.iterdir()):
            destination = output_dir / file.name
            with file.open("rb") as source, destination.open("xb") as target:
                shutil.copyfileobj(source, target)
            outputs.append(destination)
        return outputs


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--image-repository", required=True)
    parser.add_argument("--image-digest", required=True)
    parser.add_argument("--build-dir", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    args = parser.parse_args()
    try:
        outputs = package_release(source_root=Path(__file__).resolve().parents[1], **vars(args))
    except (PackageError, OSError) as error:
        parser.exit(1, f"Community packaging failed: {error}\n")
    print(f"Packaged {len(outputs)} Community release files in {args.output_dir}.")


if __name__ == "__main__":
    main()
