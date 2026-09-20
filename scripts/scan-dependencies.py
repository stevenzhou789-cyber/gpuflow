#!/usr/bin/env python3
"""Retain Community dependency findings; vulnerability counts never block release."""
import argparse
from datetime import datetime, timezone
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile

sys.dont_write_bytecode = True
spec = importlib.util.spec_from_file_location("image_scanner", Path(__file__).with_name("scan-dependency-image.py"))
image_scanner = importlib.util.module_from_spec(spec)
spec.loader.exec_module(image_scanner)

IMAGES = {
    "mysql": "mysql:8.4.11@sha256:b3b90af2a6552ae30c266fdb7d5dd55f3afb72404bb78d37fe8a23eb857fd3fb",
    "minio": "quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z@sha256:a1ea29fa28355559ef137d71fc570e508a214ec84ff8083e39bc5428980b015e",
    "probe-base": "debian:bookworm-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171",
}
POLICY = "dependency-high-critical-report-only-dual-platform-v3"


def redact(value):
    if isinstance(value, dict):
        return {key: ("[REDACTED]" if key in ("Code", "Env") else redact(child))
                for key, child in value.items() if key != "Match"}
    if isinstance(value, list):
        return [redact(child) for child in value]
    return value


def inspect(report, reference, arch, status):
    if status not in (0, 42):
        raise ValueError("Dependency scanner failed; no valid vulnerability record was produced.")
    if (type(report.get("SchemaVersion")) is not int or report["SchemaVersion"] < 1 or
            report.get("ArtifactName") != reference or report.get("ArtifactType") != "container_image" or
            report.get("Metadata", {}).get("ImageConfig", {}).get("os") != "linux" or
            report.get("Metadata", {}).get("ImageConfig", {}).get("architecture") != arch or
            not isinstance(report.get("Results"), list)):
        raise ValueError("Dependency report differs from the delivered image/platform.")
    counts = {"HIGH": 0, "CRITICAL": 0}
    for result in report["Results"]:
        if not isinstance(result, dict) or result.get("Secrets"):
            raise ValueError("Malformed result or Secret finding; third-party policy covers vulnerabilities only.")
        vulnerabilities = result.get("Vulnerabilities", [])
        if not isinstance(vulnerabilities, list):
            raise ValueError("Malformed vulnerability list.")
        for finding in vulnerabilities:
            if not isinstance(finding, dict) or finding.get("Severity") not in ("UNKNOWN", "LOW", "MEDIUM", "HIGH", "CRITICAL"):
                raise ValueError("Malformed vulnerability finding.")
            if finding["Severity"] in counts:
                counts[finding["Severity"]] += 1
    if (status == 42) != (sum(counts.values()) > 0):
        raise ValueError("Scanner status and vulnerability counts disagree.")
    return counts


def scan(output, image_scan=image_scanner.scan_image, environment=None):
    env = os.environ if environment is None else environment
    output.mkdir(parents=True, exist_ok=False)
    rows = []
    with tempfile.TemporaryDirectory(prefix="gpuflow-dependency-scan-") as temporary:
        raw = Path(temporary) / "raw.json"
        for name, reference in IMAGES.items():
            for arch in ("amd64", "arm64"):
                raw.unlink(missing_ok=True)
                result = image_scan(reference, arch, raw)
                report = redact(json.loads(raw.read_text(encoding="utf-8")))
                data = (json.dumps(report, sort_keys=True, separators=(",", ":")) + "\n").encode()
                (output / f"{name}-{arch}.json").write_bytes(data)
                counts = inspect(report, reference, arch, result.returncode)
                rows.append({"name": name, "image": reference, "platform": "linux/" + arch,
                             "report_sha256": hashlib.sha256(data).hexdigest(), "counts": counts,
                             "scanned_at_utc": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")})
    summary = {"schema": 1, "policy": POLICY, "approval": None,
               "source_commit": env.get("CI_COMMIT_SHA") or env.get("GITHUB_SHA"),
               "pipeline_id": env.get("CI_PIPELINE_ID") or env.get("GITHUB_RUN_ID"), "dependencies": rows}
    (output / "DEPENDENCY-FINDINGS.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    text = "# Third-party dependency findings\n\nVulnerability counts are informational; no approval is required.\n\n"
    for row in rows:
        text += f'- {row["name"]} {row["platform"]}: HIGH={row["counts"]["HIGH"]}, CRITICAL={row["counts"]["CRITICAL"]}\n'
    (output / "SUMMARY.md").write_text(text, encoding="utf-8")
    print(text)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", default="dependency-reports")
    args = parser.parse_args()
    scan(Path(args.output))
