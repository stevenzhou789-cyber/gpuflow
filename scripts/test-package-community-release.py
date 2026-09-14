#!/usr/bin/env python3
"""Exercise Community packaging with synthetic source and non-executable binaries."""

from __future__ import annotations

import hashlib
import importlib.util
import os
from pathlib import Path
import shutil
import sys
import subprocess
import tarfile
import tempfile
import unittest
import zipfile


ROOT = Path(__file__).resolve().parents[1]
sys.dont_write_bytecode = True
SPEC = importlib.util.spec_from_file_location("community_package", ROOT / "scripts/package-community-release.py")
PACKAGE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(PACKAGE)

OPERATIONS = {
    "agents.conf.example", "upgrade.sh", "rollback.sh", "upgrade-agents.sh",
    "upgrade-lib.sh", "offline-lib.sh", "offline-image-lib.sh",
    "install-offline.sh", "load-offline-images.sh",
}
DEPLOY = {
    "README.md", "agent/.env.example", "agent/compose.yaml",
    "offline/README.md", "offline/compose.offline.yaml", "probe/Dockerfile", "probe/README.md",
}
REPOSITORY = "ghcr.io/example/gpuflow"
DIGEST = "sha256:" + "a" * 64


class CommunityPackageTests(unittest.TestCase):
    def setUp(self):
        parent = ROOT / ".tmp"
        parent.mkdir(exist_ok=True)
        self.temporary = tempfile.TemporaryDirectory(prefix="community-package-test-", dir=parent)
        self.root = Path(self.temporary.name).resolve()
        self.assertEqual(self.root.parent, parent.resolve())
        self.addCleanup(self.temporary.cleanup)
        self.source = self.root / "source"
        self.build = self.root / "build"
        self.output = self.root / "dist"
        self.source.mkdir()
        self.write("README.md", "Synthetic project README\n")
        self.write("LICENSE", "Synthetic MIT license\n")
        self.write("compose.yaml", "services:\n  control-plane:\n    image: ${GPUFLOW_IMAGE:-gpuflow:local}\n"
                   "    build: .\n    command: [server]\n  mysql:\n    image: mysql:8.4\n")
        self.write(".env.example", "GPUFLOW_IMAGE=gpuflow:local\nGPUFLOW_AGENT_IMAGE=gpuflow:local\n"
                   "GPUFLOW_TOKEN=replace-with-a-long-random-token\n")
        for name in OPERATIONS:
            self.write(f"scripts/{name}", f"# Synthetic operations file: {name}\n")
        for name in DEPLOY:
            self.write(f"deploy/{name}", f"# Synthetic deployment file: {name}\n")
        self.write("deploy/agent/.env.example", "GPUFLOW_AGENT_IMAGE=old:tag\nGPUFLOW_NODE_ID=example-node\n")
        shutil.copyfile(ROOT / "scripts/check-community-boundary.mjs",
                        self.source / "scripts/check-community-boundary.mjs")
        self.write("scripts/test-unused.py", "# Source guard should inspect, but packaging must not ship this.\n")
        self.write("scripts/publish-unused.py", "# Release tooling is not deployment tooling.\n")
        self.binaries = {
            "linux-amd64/gpuflow": b"MOCK linux amd64 program\n",
            "linux-arm64/gpuflow": b"MOCK linux arm64 program\n",
            "windows-amd64/gpuflow.exe": b"MOCK windows amd64 program\n",
        }
        for relative, contents in self.binaries.items():
            file = self.build / relative
            file.parent.mkdir(parents=True, exist_ok=True)
            file.write_bytes(contents)

    def write(self, relative, content):
        file = self.source / relative
        file.parent.mkdir(parents=True, exist_ok=True)
        file.write_text(content, encoding="utf-8", newline="")
        return file

    def package(self, **overrides):
        arguments = dict(source_root=self.source, version="v1.0.0", image_repository=REPOSITORY,
                         image_digest=DIGEST, build_dir=self.build, output_dir=self.output)
        arguments.update(overrides)
        return PACKAGE.package_release(**arguments)

    def deployment(self, version="v1.0.0"):
        archive = self.output / f"gpuflow-deployment-{version}.tar.gz"
        with tarfile.open(archive, "r:gz") as package:
            members = package.getmembers()
            self.assertTrue(all(member.isfile() for member in members))
            self.assertTrue(all(not member.name.startswith("/") and ".." not in Path(member.name).parts
                                for member in members))
            prefix = f"gpuflow-deployment-{version}/"
            self.assertTrue(all(member.name.startswith(prefix) for member in members))
            return {member.name.removeprefix(prefix): (package.extractfile(member).read(), member.mode)
                    for member in members}

    def test_complete_manifest_pinned_images_and_native_programs(self):
        self.package()
        self.assertEqual({path.name for path in self.output.iterdir()}, {
            "gpuflow-linux-amd64.tar.gz", "gpuflow-linux-arm64.tar.gz", "gpuflow-windows-amd64.zip",
            "gpuflow-deployment-v1.0.0.tar.gz", "checksums.txt",
        })
        files = self.deployment()
        expected = {"compose.yaml", ".env.example", "LICENSE", "README.md", "PROJECT-README.md", "VERSION"}
        expected |= {f"scripts/{name}" for name in OPERATIONS}
        expected |= {f"deploy/{name}" for name in DEPLOY}
        self.assertEqual(set(files), expected)
        self.assertEqual(files["VERSION"][0], b"v1.0.0\n")
        self.assertEqual(files["README.md"][0], files["deploy/README.md"][0])
        self.assertEqual(files["PROJECT-README.md"][0], b"Synthetic project README\n")
        expected_image = f"{REPOSITORY}@{DIGEST}".encode()
        for key in (b"GPUFLOW_IMAGE", b"GPUFLOW_AGENT_IMAGE"):
            self.assertIn(key + b"=" + expected_image + b"\n", files[".env.example"][0])
        self.assertIn(b"GPUFLOW_AGENT_IMAGE=" + expected_image + b"\n", files["deploy/agent/.env.example"][0])
        self.assertNotIn(b"build:", files["compose.yaml"][0])
        self.assertIn(b"image: mysql:8.4", files["compose.yaml"][0])
        for name in OPERATIONS:
            self.assertEqual(files[f"scripts/{name}"][0], (self.source / "scripts" / name).read_bytes())
            if name.endswith(".sh"):
                self.assertEqual(files[f"scripts/{name}"][1], 0o755)
        for architecture in ("linux-amd64", "linux-arm64"):
            with tarfile.open(self.output / f"gpuflow-{architecture}.tar.gz", "r:gz") as archive:
                self.assertEqual(archive.getnames(), ["gpuflow"])
                self.assertEqual(archive.extractfile("gpuflow").read(), self.binaries[f"{architecture}/gpuflow"])
                self.assertEqual(archive.getmember("gpuflow").mode, 0o755)
        with zipfile.ZipFile(self.output / "gpuflow-windows-amd64.zip") as archive:
            self.assertEqual(archive.namelist(), ["gpuflow.exe"])
            self.assertEqual(archive.read("gpuflow.exe"), self.binaries["windows-amd64/gpuflow.exe"])
        checksums = (self.output / "checksums.txt").read_text().splitlines()
        self.assertEqual(len(checksums), 4)
        for line in checksums:
            checksum, filename = line.split("  ")
            self.assertEqual(hashlib.sha256((self.output / filename).read_bytes()).hexdigest(), checksum)

    def test_stable_compatibility_and_reproducible_archives(self):
        self.package(version="stable")
        self.assertEqual(self.deployment("stable")["VERSION"][0], b"stable\n")
        second = self.root / "second"
        self.package(version="stable", output_dir=second)
        for file in self.output.iterdir():
            self.assertEqual(file.read_bytes(), (second / file.name).read_bytes())

    def test_windows_source_becomes_portable_lf_deployment(self):
        for relative in ("compose.yaml", ".env.example", "scripts/upgrade.sh", "deploy/agent/.env.example"):
            file = self.source / relative
            file.write_bytes(file.read_bytes().replace(b"\n", b"\r\n"))
        self.package()
        for content, _mode in self.deployment().values():
            self.assertNotIn(b"\r\n", content)

    def test_unselected_sensitive_source_is_rejected_before_output(self):
        bad_sources = {
            "scripts/unused-private.pem": "synthetic key path\n",
            "deploy/unused/.env": "DO_NOT_PRINT_CUSTOMER_VALUE=synthetic\n",
            "scripts/unused/internal/license/license.go": "package license\n",
            "deploy/unused/customer.db": "synthetic runtime data\n",
            "scripts/unused/archive.tar.gz": "synthetic archive\n",
            "scripts/unused/notes.txt": "-----BEGIN PRIVATE KEY-----\nDO_NOT_PRINT_KEY_BODY\n",
        }
        for relative, contents in bad_sources.items():
            with self.subTest(relative=relative):
                file = self.write(relative, contents)
                with self.assertRaisesRegex(PACKAGE.PackageError, "boundary check failed") as failure:
                    self.package()
                self.assertNotIn("DO_NOT_PRINT", str(failure.exception))
                self.assertFalse(self.output.exists())
                file.unlink()

    def test_existing_empty_or_populated_output_is_never_overwritten(self):
        self.output.mkdir()
        for populated in (False, True):
            with self.subTest(populated=populated):
                if populated:
                    (self.output / "sentinel.txt").write_bytes(b"keep original bytes")
                with self.assertRaisesRegex(PACKAGE.PackageError, "refusing to overwrite"):
                    self.package()
                expected = {"sentinel.txt"} if populated else set()
                self.assertEqual({path.name for path in self.output.iterdir()}, expected)
                if populated:
                    self.assertEqual((self.output / "sentinel.txt").read_bytes(), b"keep original bytes")

    def test_invalid_identity_fails_without_output(self):
        cases = [dict(version=value) for value in ("v01.0.0", "1.0.0", "v1.0.0-rc1", "../v1.0.0", "v1.0.0\n")]
        cases += [dict(image_repository=value) for value in ("ghcr.io/example/gpuflow:tag", "https://ghcr.io/example/gpuflow",
                                                            "ghcr.io/example/gpuflow@sha256:abc", "other.example/repo")]
        cases += [dict(image_digest=value) for value in ("sha256:short", "sha256:" + "A" * 64)]
        for overrides in cases:
            with self.subTest(overrides=overrides), self.assertRaises(PACKAGE.PackageError):
                self.package(**overrides)
            self.assertFalse(self.output.exists())

    def test_missing_or_empty_native_binary_fails_closed(self):
        binary = self.build / "linux-arm64/gpuflow"
        binary.unlink()
        with self.assertRaisesRegex(PACKAGE.PackageError, "Required regular file"):
            self.package()
        binary.write_bytes(b"")
        with self.assertRaisesRegex(PACKAGE.PackageError, "Native binary is empty"):
            self.package()
        self.assertFalse(self.output.exists())

    def test_unknown_compose_build_or_duplicate_image_setting_is_rejected(self):
        compose = self.source / "compose.yaml"
        original = compose.read_text()
        for change in (original.replace("    build: .", "    build: ./other"),
                       original + "  extra:\n    build: .\n", original.replace("    build: .\n", "")):
            with self.subTest(compose=change):
                compose.write_text(change)
                with self.assertRaisesRegex(PACKAGE.PackageError, "Unknown Compose build"):
                    self.package()
                self.assertFalse(self.output.exists())
        compose.write_text(original)
        with (self.source / ".env.example").open("a") as stream:
            stream.write("GPUFLOW_IMAGE=duplicate\n")
        with self.assertRaisesRegex(PACKAGE.PackageError, "exactly one GPUFLOW_IMAGE"):
            self.package()

    def test_source_symlink_cannot_bypass_boundary(self):
        outside = self.root / "outside"
        outside.mkdir()
        (outside / "value.txt").write_text("synthetic outside content")
        link = self.source / "deploy/linked-directory"
        try:
            link.symlink_to(outside, target_is_directory=True)
        except (OSError, NotImplementedError) as error:
            if os.name != "nt":
                raise
            # NTFS junctions do not need the privilege required for symlinks.
            result = subprocess.run(["cmd.exe", "/c", "mklink", "/J", str(link), str(outside)],
                                    capture_output=True)
            if result.returncode:
                self.skipTest(f"Host cannot create a fixture symlink or junction: {type(error).__name__}")
            self.addCleanup(link.rmdir)
        with self.assertRaisesRegex(PACKAGE.PackageError, "Symlink"):
            self.package()
        self.assertFalse(self.output.exists())
        self.assertEqual((outside / "value.txt").read_text(), "synthetic outside content")


if __name__ == "__main__":
    unittest.main(verbosity=2)
