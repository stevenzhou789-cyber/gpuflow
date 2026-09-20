# Community third-party vulnerability records

GitHub and GitLab run `scripts/scan-dependencies.py` using Trivy 0.74.0.
The policy is `dependency-high-critical-report-only-dual-platform-v3`:
third-party HIGH/CRITICAL findings, including unfixed findings, are recorded
without an approval requirement or a vulnerability-count build/release gate.

The Community scope is MySQL, MinIO and the Probe base image, pinned by digest
and scanned on linux/amd64 and linux/arm64. Online defaults and offline MySQL
and MinIO delivery use these same references. Community does not gain an
Enterprise Registry, NGINX service, License requirement or Enterprise lifecycle
acceptance. Enterprise maintains its own dependency set and pins the Community
commit explicitly.

`dependency-reports/` contains redacted reports, hashes, scan timestamps and
`DEPENDENCY-FINDINGS.json` / `SUMMARY.md`. Both providers retain CI artifacts
for 90 days. Findings remain visible; recording them does not claim remediation.
Scanner failures, absent or malformed reports, wrong image/platform and Secret
findings still fail. Existing image/package signing and immutable publication
checks remain in place. No approval keys, records or per-tag Runner changes
are needed for this policy.
