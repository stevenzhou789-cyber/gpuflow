# Repository workflow requirements

- The latest user instruction supersedes the former dual-remote policy: use
  local GitLab only for code pushes and CI. Keep the existing origin remote,
  GitHub workflows, and existing tags as historical records; do not push to
  GitHub, trigger GitHub Actions, or fetch GitHub as an automatic fallback.
- After appropriate tests and an authorized commit/push, use
  `powershell -File scripts/push-gitlab.ps1` and verify every requested GitLab
  remote SHA. Read-only requests do not authorize commits or pushes.
- The old `scripts/push-all.ps1` command is a GitLab-only compatibility wrapper.
  Explicit legacy `-GitHubRemote` arguments are rejected, not silently ignored.
- Never force-push or create, move, delete, or recreate a release tag implicitly.
  Push an existing tag only when explicitly requested, using
  `scripts/push-gitlab.ps1 -Tag <existing-tag>`.
- Never commit local .env files, private signing/license keys, credentials,
  customer data, runtime data or generated caches. Untracked workspace files
  are not automatically included in commits or pushes.
- GitLab SHA/development builds, even with signed images or checksums, are not
  approved versioned formal releases. Preserve release signing, immutable-tag and approval
  gates. Missing security checks or evidence must block release.
- See docs/GITLAB-WORKFLOW.md for the current local-only workflow and endpoints.
