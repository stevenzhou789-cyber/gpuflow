#!/usr/bin/env python3
"""Scan a pinned dependency through an explicitly selected Docker archive.

Kept identical in Community scripts/ and Enterprise .github/scripts/.
Dependency allowlists and release policy belong to each caller, not this helper.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile


def require(condition, message):
    if not condition:
        raise ValueError(message)


def archive_identity(path, platform):
    with tarfile.open(path, "r:") as archive:
        members = archive.getmembers()
        require(len({member.name for member in members}) == len(members), "Duplicate archive member.")
        manifest = json.load(archive.extractfile("manifest.json"))
        require(isinstance(manifest, list) and len(manifest) == 1, "Expected exactly one dependency image.")
        image = manifest[0]
        config_member = archive.getmember(image["Config"])
        require(config_member.isfile() and 0 < config_member.size <= 1024 * 1024, "Invalid image config.")
        data = archive.extractfile(config_member).read()
        config = json.loads(data)
        require(f'{config.get("os")}/{config.get("architecture")}' == platform, "Wrong dependency archive platform.")
        require(isinstance(image.get("Layers"), list) and image["Layers"] and
                all(archive.getmember(layer).isfile() for layer in image["Layers"]), "Missing dependency layers.")
    with path.open("rb") as source:
        archive_digest = hashlib.file_digest(source, "sha256").hexdigest()
    return {"archive_sha256": archive_digest, "config_sha256": hashlib.sha256(data).hexdigest()}


def scan_image(reference, arch, output, run=subprocess.run):
    require(re.fullmatch(r"[^\s@]+@sha256:[0-9a-f]{64}", reference), "Dependency source must be immutable.")
    require(arch in ("amd64", "arm64"), "Unsupported dependency architecture.")
    output = Path(output)
    require(not output.exists(), "Refusing to overwrite a dependency scan.")
    platform = "linux/" + arch
    with tempfile.TemporaryDirectory(prefix="gpuflow-scan-image-") as directory:
        work = Path(directory)
        archive, raw, config = work / "image.tar", work / "raw.json", work / "trivy.yaml"
        config.write_text("{}\n", encoding="utf-8")
        # Docker resolves the immutable index/platform and checks downloaded
        # layers. Trivy reads this complete archive, never the remote layer stream
        # or an ambiguous cached architecture. Do not retag or remove shared images.
        run(["docker", "pull", "--platform", platform, reference], check=True, stdout=subprocess.DEVNULL)
        run(["docker", "image", "save", "--platform", platform, "--output", str(archive), reference], check=True)
        identity = archive_identity(archive, platform)
        result = run(["trivy", "image", "--config", str(config), "--ignorefile", os.devnull,
                      "--input", str(archive), "--platform", platform, "--scanners", "vuln,secret",
                      "--severity", "HIGH,CRITICAL", "--format", "json", "--output", str(raw),
                      "--exit-code", "42", "--timeout", "20m", "--no-progress"], check=False)
        require(result.returncode in (0, 42), "Dependency scanner operational failure.")
        report = json.loads(raw.read_text(encoding="utf-8"))
        require(report.get("ArtifactName") == str(archive) and report.get("ArtifactType") == "container_image" and
                report.get("Metadata", {}).get("ImageConfig", {}).get("os") == "linux" and
                report["Metadata"]["ImageConfig"].get("architecture") == arch,
                "Scanner report differs from the dependency archive/platform.")
        # Retain explicit provenance when translating the temporary input path
        # to the immutable source identity expected by both release policies.
        report["GPUFlowScanInput"] = dict(identity, kind="docker-archive", source=reference, platform=platform)
        report["ArtifactName"] = reference
        output.write_text(json.dumps(report) + "\n", encoding="utf-8")
        return result


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--arch", required=True, choices=("amd64", "arm64"))
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    raise SystemExit(scan_image(args.image, args.arch, args.output).returncode)
