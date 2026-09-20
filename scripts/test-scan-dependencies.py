#!/usr/bin/env python3
"""Exercise both CI providers' report-only policy without Docker or network."""
import copy
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest

ROOT = Path(__file__).resolve().parents[1]
sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("scan", ROOT / "scripts/scan-dependencies.py")
scan = importlib.util.module_from_spec(spec)
spec.loader.exec_module(scan)


class DependencyTests(unittest.TestCase):
    def report(self, reference, arch="amd64"):
        return {"SchemaVersion": 2, "ArtifactName": reference, "ArtifactType": "container_image",
                "Metadata": {"ImageConfig": {"os": "linux", "architecture": arch}},
                "Results": [{"Vulnerabilities": [{"Severity": "HIGH"}, {"Severity": "CRITICAL"}], "Secrets": []}]}

    def test_counts_never_need_approval_and_both_platforms_are_retained(self):
        for env in ({"CI_COMMIT_SHA": "a" * 40, "CI_PIPELINE_ID": "42"},
                    {"GITHUB_SHA": "a" * 40, "GITHUB_RUN_ID": "42"}):
            with self.subTest(provider=env), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "reports"
                def run(args, check):
                    self.assertFalse(check)
                    self.assertEqual(args[args.index("--image-src") + 1], "remote")
                    self.assertNotIn("--ignore-unfixed", args)
                    report = self.report(args[-1], args[args.index("--platform") + 1].split("/")[1])
                    Path(args[args.index("--output") + 1]).write_text(json.dumps(report), encoding="utf-8")
                    return SimpleNamespace(returncode=42)
                scan.scan(output, run, env)
                summary = json.loads((output / "DEPENDENCY-FINDINGS.json").read_text())
                self.assertEqual(len(summary["dependencies"]), 6)
                self.assertEqual(summary["source_commit"], "a" * 40)
                self.assertEqual(summary["pipeline_id"], "42")
                self.assertIsNone(summary["approval"])
                self.assertEqual(len(list(output.glob("*-amd64.json"))), 3)
                self.assertEqual(len(list(output.glob("*-arm64.json"))), 3)

    def test_scan_errors_wrong_image_platform_and_secrets_still_fail(self):
        ref = scan.IMAGES["mysql"]
        original = self.report(ref)
        for status in (0, 1, 124):
            with self.subTest(status=status), self.assertRaises(ValueError):
                scan.inspect(original, ref, "amd64", status)
        for field, value in (("ArtifactName", "mysql:latest"), ("Results", None)):
            bad = dict(original, **{field: value})
            with self.subTest(field=field), self.assertRaises(ValueError):
                scan.inspect(bad, ref, "amd64", 42)
        with self.assertRaises(ValueError):
            scan.inspect(original, ref, "arm64", 42)
        bad = copy.deepcopy(original)
        bad["Results"][0]["Secrets"] = [{"Severity": "LOW", "Match": "fixture-sensitive-value"}]
        redacted = scan.redact(bad)
        self.assertNotIn("fixture-sensitive-value", json.dumps(redacted))
        with self.assertRaises(ValueError):
            scan.inspect(redacted, ref, "amd64", 42)

    def test_pins_match_delivered_dependencies_and_both_providers_use_same_script(self):
        for name in ("mysql", "minio"):
            for path in (".env.example", "compose.yaml", "scripts/gitlab-package-full.sh"):
                self.assertIn(scan.IMAGES[name], (ROOT / path).read_text(encoding="utf-8"))
        for path in (".gitlab-ci.yml", ".github/workflows/container-image.yml"):
            workflow = (ROOT / path).read_text(encoding="utf-8")
            self.assertIn("python3 scripts/scan-dependencies.py", workflow)
            self.assertIn("dependency-reports/", workflow)
            self.assertNotIn("RISK_APPROVAL", workflow)
            self.assertNotIn("allow_failure:", workflow)
            self.assertNotIn("continue-on-error:", workflow)


if __name__ == "__main__":
    unittest.main()
