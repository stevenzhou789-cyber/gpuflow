# Repository workflow requirements

- The user requires future committed code changes to be synchronized to both
  GitHub (`origin`) and local GitLab (`gitlab`). After appropriate tests and an
  authorized commit, use `powershell -File scripts/push-all.ps1` when pushing and
  verify both remote SHAs. Report partial failures; never imply a two-server
  atomic transaction. Read-only requests do not authorize commits or pushes.
- Do not force-push or move an existing release tag. Push a tag only when
  explicitly requested, using `scripts/push-all.ps1 -Tag <existing-tag>`.
- Never commit local `.env` files, signing/license keys, credentials, data or
  generated caches. Untracked workspace files are not automatically included.
- GitLab development/unsigned artifacts are not signed, approved formal
  releases. Preserve the existing release signing and approval gates.
