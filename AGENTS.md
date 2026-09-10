# Repository workflow requirements

- The latest user instruction requires future committed code changes to be
  synchronized to both GitHub (`origin`) and local GitLab (`gitlab`). After
  appropriate tests and an authorized commit/push, use
  `powershell -File scripts/push-all.ps1` and verify both remote SHAs. Report
  partial failures; the two servers are not one atomic transaction. Read-only
  requests do not authorize commits or pushes.
- Preserve the existing GitHub workflows and GitLab CI gates. Report each
  server's CI outcome separately; a successful push is not a successful build.
- `scripts/push-gitlab.ps1` remains an auxiliary tool for explicitly authorized
  GitLab-only pushes; it does not satisfy the default dual-remote requirement.
- Never force-push or create, move, delete, or recreate a release tag implicitly.
  Push an existing tag only when explicitly requested, using
  `scripts/push-all.ps1 -Tag <existing-tag>`.
- Never commit local .env files, private signing/license keys, credentials,
  customer data, runtime data or generated caches. Untracked workspace files
  are not automatically included in commits or pushes.
- GitLab SHA/development builds, even with signed images or checksums, are not
  approved versioned formal releases. Preserve release signing, immutable-tag and approval
  gates. Missing security checks or evidence must block release.
- See docs/GITLAB-WORKFLOW.md for the current dual-remote workflow and endpoints.
