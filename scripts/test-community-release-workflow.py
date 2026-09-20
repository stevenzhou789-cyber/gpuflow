#!/usr/bin/env python3
"""Check release job wiring without third-party YAML or network dependencies.

This deliberately checks the workflow's existing indentation-based job/step
layout, not arbitrary YAML. Publisher and packager behavior has separate tests.
"""

from pathlib import Path
import re
import unittest


ROOT = Path(__file__).resolve().parents[1]
VERSION_IF = "needs.validate.outputs.is_version == 'true'"
STABLE_IF = "github.ref == 'refs/tags/stable'"
PINNED_SHA = "${{ needs.validate.outputs.commit_sha }}"


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def section(text, name, indent):
    lines = text.splitlines()
    prefix = " " * indent
    starts = [i for i, line in enumerate(lines) if line == f"{prefix}{name}:"]
    require(len(starts) == 1, f"Expected one {name} section at indentation {indent}")
    start = starts[0] + 1
    end = next((i for i in range(start, len(lines))
                if lines[i].strip() and not lines[i].lstrip().startswith("#")
                and len(lines[i]) - len(lines[i].lstrip()) <= indent), len(lines))
    return "\n".join(lines[start:end])


def field(text, name, indent):
    matches = re.findall(rf"^{' ' * indent}{re.escape(name)}: (.+)$", text, re.M)
    require(len(matches) == 1, f"Expected one {name} field at indentation {indent}")
    return matches[0]


def steps(job):
    body = section(job, "steps", 4)
    return [match.group(0) for match in re.finditer(
        r"^      - .*(?:\n(?!      - ).*)*", body, re.M)]


def one_step(job, token):
    matches = [step for step in steps(job) if token in step]
    require(len(matches) == 1, f"Expected one step containing {token}")
    return matches[0]


def check_contract(workflow, gitlab_ci, gitlab_build):
    validate = section(workflow, "validate", 2)
    build = section(workflow, "build", 2)
    release = section(workflow, "release", 2)
    require('      - "v*"' in section(section(workflow, "on", 0), "push", 2),
            "Numbered tag pushes must reach the release guard")
    require(field(build, "needs", 4) == "[validate, dependency-scan]", "Image build must depend on validation")
    require(set(field(release, "needs", 4).strip("[]").replace(" ", "").split(","))
            == {"validate", "build"}, "Release must depend on both validation and signed build")
    require(field(release, "if", 4) == f"{STABLE_IF} || {VERSION_IF}",
            "Release condition must retain success gating and explicit version selection")
    require(not re.search(r"^\s*(?:continue-on-error|allow-failure):", workflow, re.M),
            "Publication gates must not ignore failures")

    checkout = one_step(validate, "uses: actions/checkout@")
    require(field(checkout, "ref", 10) == "${{ github.sha }}", "Guard checkout must use event SHA")
    for name, job in (("build", build), ("release", release)):
        require(field(one_step(job, "uses: actions/checkout@"), "ref", 10) == PINNED_SHA,
                f"{name} checkout must use validated commit SHA")
    outputs = section(validate, "outputs", 4)
    for output in ("is_version", "tag", "tag_object_sha", "commit_sha"):
        require(field(outputs, output, 6) == "${{ steps.release-ref.outputs." + output + " }}",
                f"Guard output {output} must be forwarded unchanged")
    guard = one_step(validate, "node scripts/community-release.mjs guard")
    require(field(guard, "id", 8) == "release-ref", "Guard output step ID changed")
    require(not re.search(r"^        if:", guard, re.M), "Every build must pass the ref guard")
    for command in ("node --test scripts/community-release.test.mjs",
                    "python3 scripts/test-package-community-release.py"):
        step = one_step(validate, command)
        require(not re.search(r"^        if:", step, re.M), f"Required test is conditional: {command}")

    build_outputs = section(build, "outputs", 4)
    require(field(build_outputs, "image_name", 6) == "${{ steps.image.outputs.name }}",
            "Release image name must come from this build")
    require(field(build_outputs, "image_digest", 6) == "${{ steps.push.outputs.digest }}",
            "Release image digest must come from this build")
    require(field(one_step(build, "uses: docker/build-push-action@"), "id", 8) == "push",
            "The signed image output must refer to the actual build step")
    metadata = one_step(build, "uses: docker/metadata-action@")
    for tag_type in ("sha", "ref"):
        lines = [line for line in metadata.splitlines() if f"type={tag_type}," in line]
        require(lines and all("enable=${{ needs.validate.outputs.is_version != 'true' }}" in line
                              for line in lines), "Numbered builds must not overwrite existing image tags")
    require("type=raw,value=run-${{ github.run_id }}-${{ github.run_attempt }}" in metadata,
            "Numbered builds need a candidate image tag before immutable promotion")
    require("flavor: latest=false" in metadata, "Candidate images must not implicitly publish latest")

    package = one_step(release, "python3 scripts/package-community-release.py")
    publish = one_step(release, "node scripts/community-release.mjs publish --assets-dir dist")
    require(field(publish, "if", 8) == VERSION_IF, "Immutable publisher must be limited to validated versions")
    for step in (package, publish):
        for variable, output in (("IMAGE_NAME", "image_name"), ("IMAGE_DIGEST", "image_digest")):
            require(field(step, variable, 10) == "${{ needs.build.outputs." + output + " }}",
                    f"{variable} must use the signed build output")
    require('--image-digest "$IMAGE_DIGEST"' in package, "Packager must receive the immutable digest")
    for variable, output in (("EXPECTED_TAG_OBJECT_SHA", "tag_object_sha"),
                             ("EXPECTED_COMMIT_SHA", "commit_sha")):
        require(field(publish, variable, 10) == "${{ needs.validate.outputs." + output + " }}",
                f"Publisher must recheck the original {output}")
    for step in steps(release):
        if "--clobber" in step or re.search(r"gh release (?:create|upload|edit|delete)\b", step):
            require(field(step, "if", 8) == STABLE_IF,
                    "Direct or overwriting release commands must be confined to legacy stable")

    image_sign = one_step(build, "cosign sign --yes")
    require(field(image_sign, "if", 8) == "github.event_name != 'pull_request'",
            "Every published image must be signed")
    require(field(image_sign, "IMAGE_DIGEST", 10) == "${{ steps.push.outputs.digest }}",
            "Container signature must cover this build's digest")
    require('"${IMAGE_NAME}@${IMAGE_DIGEST}"' in image_sign, "Container signing must use a digest")
    blob_sign = one_step(release, "cosign sign-blob --yes")
    require(not re.search(r"^        if:", blob_sign, re.M), "Artifact signatures must not be conditional")
    require("dist/*.tar.gz dist/*.zip dist/checksums.txt" in blob_sign
            and '--bundle "${artifact}.sigstore.json"' in blob_sign,
            "Every archive and checksum list needs a signature bundle")
    require(release.index(blob_sign) < release.index(publish), "Artifacts must be signed before publication")
    require(not re.search(r"--(?:tlog-upload=false|insecure-ignore-tlog|insecure-ignore-sct)", workflow),
            "Community transparency verification must not be disabled")
    for job in (build, release):
        require(field(one_step(job, "uses: sigstore/cosign-installer@"), "cosign-release", 10)
                == "v3.1.3", "Keep the tested Cosign version for signing and verification")

    require("- if: '$CI_COMMIT_TAG'" in gitlab_ci and "bash scripts/gitlab-full-build.sh" in gitlab_ci,
            "GitLab tag pipelines must reach the verified build")
    require('python3 scripts/gitlab-release.py guard' in gitlab_ci and
            'python3 scripts/gitlab-release.py check' in gitlab_build and
            'bash scripts/gitlab-publish-version.sh' in gitlab_ci and
            '- job: full-signed-build' in gitlab_ci, "Formal GitLab publication requires tag provenance and the successful build")
    require("default branch or a guarded protected tag may build.' >&2; exit 1; }" in gitlab_build,
            "Unguarded branches must fail closed")
    require(gitlab_build.index('bash scripts/gitlab-verify-offline.sh') < gitlab_build.index('python3 scripts/gitlab-release.py record'),
            "Release evidence must follow real offline installation")


class ReleaseWorkflowTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.workflow = (ROOT / ".github/workflows/container-image.yml").read_text(encoding="utf-8")
        cls.gitlab_ci = (ROOT / ".gitlab-ci.yml").read_text(encoding="utf-8")
        cls.gitlab_build = (ROOT / "scripts/gitlab-full-build.sh").read_text(encoding="utf-8")

    def test_publication_contract(self):
        check_contract(self.workflow, self.gitlab_ci, self.gitlab_build)

    def test_broken_safety_connections_are_rejected(self):
        changes = (
            ("    needs: [validate, build]", "    needs: [build]"),
            ("          ref: " + PINNED_SHA, "          ref: main"),
            ("          IMAGE_DIGEST: ${{ needs.build.outputs.image_digest }}", "          IMAGE_DIGEST: latest"),
            ("        if: " + STABLE_IF, "        if: " + VERSION_IF),
            ('--image-digest "$IMAGE_DIGEST"', '--image-digest "sha256:other"'),
            ("        if: github.event_name != 'pull_request'", "        if: false"),
        )
        for old, new in changes:
            with self.subTest(connection=old):
                self.assertIn(old, self.workflow)
                with self.assertRaises(AssertionError):
                    check_contract(self.workflow.replace(old, new), self.gitlab_ci, self.gitlab_build)
        with self.assertRaises(AssertionError):
            check_contract(self.workflow, self.gitlab_ci, self.gitlab_build.replace("exit 1; }", "exit 0; }"))


if __name__ == "__main__":
    unittest.main()
