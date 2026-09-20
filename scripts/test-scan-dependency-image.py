#!/usr/bin/env python3
"""Archive input identity and scanner failure regressions; no Docker/network."""
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
from types import SimpleNamespace
import unittest

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("scanner", Path(__file__).with_name("scan-dependency-image.py"))
scanner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scanner)
IMAGE = "fixture/dependency@sha256:" + "a" * 64


class ImageScanTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.output = Path(self.temp.name) / "report.json"
        self.mode, self.status, self.calls = "valid", 42, []

    def command(self, args, check, **kwargs):
        self.calls.append(args)
        arch = args[args.index("--platform") + 1].split("/")[1]
        if args[:2] == ["docker", "pull"]:
            self.assertTrue(check)
            self.assertEqual(args[-1], IMAGE)
            if self.mode == "pull-failure":
                raise subprocess.CalledProcessError(1, args)
        elif args[:3] == ["docker", "image", "save"]:
            self.assertTrue(check)
            self.assertEqual(args[-1], IMAGE)
            config = json.dumps({"os": "linux", "architecture": "wrong" if self.mode == "archive-platform" else arch}).encode()
            self.config_hash = hashlib.sha256(config).hexdigest()
            manifest = [{"Config": "config.json", "Layers": ["layer.tar"]}]
            if self.mode == "extra-image":
                manifest *= 2
            files = {"config.json": config, "layer.tar": b"fixture", "manifest.json": json.dumps(manifest).encode()}
            if self.mode == "missing-layer":
                del files["layer.tar"]
            with tarfile.open(args[args.index("--output") + 1], "w") as target:
                for name, data in files.items():
                    member = tarfile.TarInfo(name)
                    member.size = len(data)
                    target.addfile(member, io.BytesIO(data))
        else:
            self.assertEqual(args[:2], ["trivy", "image"])
            self.assertFalse(check)
            self.assertNotIn("--image-src", args)
            self.assertNotIn("--ignore-unfixed", args)
            self.assertEqual(args[args.index("--scanners") + 1], "vuln,secret")
            self.assertEqual(args[args.index("--exit-code") + 1], "42")
            self.assertEqual(args[args.index("--severity") + 1], "HIGH,CRITICAL")
            self.input = Path(args[args.index("--input") + 1])
            self.archive_hash = hashlib.sha256(self.input.read_bytes()).hexdigest()
            raw = Path(args[args.index("--output") + 1])
            report = {"SchemaVersion": 2, "ArtifactName": str(self.input), "ArtifactType": "container_image",
                      "Metadata": {"ImageConfig": {"os": "linux", "architecture": arch}},
                      "Results": [{"Vulnerabilities": [{"Severity": "HIGH"}], "Secrets": []}]}
            if self.mode == "report-platform":
                report["Metadata"]["ImageConfig"]["architecture"] = "wrong"
            if self.mode == "wrong-input":
                report["ArtifactName"] = "unrelated.tar"
            if self.mode != "missing-report":
                raw.write_text("invalid-json" if self.mode == "malformed" else json.dumps(report), encoding="utf-8")
            return SimpleNamespace(returncode=self.status)
        return SimpleNamespace(returncode=0)

    def test_both_architectures_keep_findings_and_archive_provenance(self):
        for arch in ("amd64", "arm64"):
            result = scanner.scan_image(IMAGE, arch, self.output, self.command)
            self.assertEqual(result.returncode, 42)
            report = json.loads(self.output.read_text(encoding="utf-8"))
            self.assertEqual(report["ArtifactName"], IMAGE)
            self.assertEqual(report["GPUFlowScanInput"], {"kind": "docker-archive", "source": IMAGE,
                             "platform": "linux/" + arch, "archive_sha256": self.archive_hash, "config_sha256": self.config_hash})
            self.assertEqual(report["Results"][0]["Vulnerabilities"], [{"Severity": "HIGH"}])
            self.assertFalse(self.input.exists())
            self.output.unlink()

    def test_archive_and_pull_failures_never_scan(self):
        for self.mode in ("pull-failure", "archive-platform", "missing-layer", "extra-image"):
            self.calls = []
            with self.subTest(mode=self.mode), self.assertRaises((ValueError, KeyError, subprocess.CalledProcessError)):
                scanner.scan_image(IMAGE, "amd64", self.output, self.command)
            self.assertFalse(any(command[0] == "trivy" for command in self.calls))
            self.assertFalse(self.output.exists())

    def test_scanner_failures_and_wrong_reports_cannot_publish(self):
        for self.mode in ("missing-report", "malformed", "report-platform", "wrong-input"):
            with self.subTest(mode=self.mode), self.assertRaises((ValueError, OSError)):
                scanner.scan_image(IMAGE, "amd64", self.output, self.command)
            self.assertFalse(self.output.exists())
        self.mode = "valid"
        for self.status in (1, 124):
            with self.subTest(status=self.status), self.assertRaises(ValueError):
                scanner.scan_image(IMAGE, "amd64", self.output, self.command)
            self.assertFalse(self.output.exists())

    def test_mutable_reference_platform_and_existing_output_rejected(self):
        for ref, arch in (("fixture:latest", "amd64"), (IMAGE, "s390x")):
            with self.assertRaises(ValueError):
                scanner.scan_image(ref, arch, self.output, self.command)
        self.output.write_text("existing", encoding="utf-8")
        with self.assertRaises(ValueError):
            scanner.scan_image(IMAGE, "amd64", self.output, self.command)
        self.assertEqual(self.calls, [])
        self.assertEqual(self.output.read_text(), "existing")


if __name__ == "__main__":
    unittest.main()
