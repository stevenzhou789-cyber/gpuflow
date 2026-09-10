# Dual-remote development workflow

The current user instruction requires committed changes to be synchronized to
GitHub (`origin`) and local GitLab (`gitlab`). Preserve both CI definitions and
report each server's build result separately. A quota or build failure does not
change the default synchronization policy or permit bypassing release gates.

After reviewing the exact files, passing the relevant tests and making an
authorized commit, run from this repository:

```powershell
powershell -File scripts/push-all.ps1
```

The script pushes the current committed branch to both configured remotes,
using a frozen SHA and explicit refspec, then verifies both remote SHAs.
Tracked uncommitted edits block pushing; untracked files are
reported but never staged or uploaded automatically. It never force-pushes.
A failed push or SHA mismatch must be reported for that server. The two pushes
are not a cross-server atomic transaction; do not imply success for both when
only one succeeded.

Only when the user explicitly requests pushing an existing tag:

```powershell
powershell -File scripts/push-all.ps1 -Tag <existing-tag>
```

No tag is created, moved or deleted by the script. A Git tag push does not grant
release approval or relax CI gates. Do not configure implicit tag following or
mirror pushes for these remotes.

`push-gitlab.ps1` remains available for explicitly authorized GitLab-only
operations. It is not the default and does not satisfy dual-remote
synchronization. Both scripts are self-contained and do not depend on a sibling
repository.

The explicit GitLab-only helper permits only this installation's endpoints:
HTTP port 8088 or SSH port
2224 on `gitlab.gpuflow.test`, `localhost`, or a loopback address. HTTP URLs
must not embed credentials; use the configured credential manager. SSH URLs
must use the `git` user. An intentional move to another GitLab host requires
reviewing and updating this endpoint allowlist. The script never modifies Git
remotes or credentials itself.

Run the fully mocked safety checks without network access:

```powershell
powershell -File scripts/test-push-all.ps1
powershell -File scripts/test-push-gitlab.ps1
```

The tests cover dual-remote success and partial failure, explicit tags,
dirty-tree refusal, SHA verification, no staging/commits/config changes, and
zero GitHub requests from the explicit GitLab-only helper. On a machine that requires an execution-policy exception,
use an approved per-process policy; do not weaken the machine-wide policy.
