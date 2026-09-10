# GitLab-only development workflow

The latest user decision is to manage new pushes and builds through the local
GitLab only. This supersedes the old dual-push instructions. The GitHub
`origin` remote, existing GitHub workflows and historical release tags remain
untouched as records, but this workflow does not push, trigger CI, or retry
through GitHub.

After reviewing the exact files, passing the relevant tests and making an
authorized commit, run from this repository:

```powershell
powershell -File scripts/push-gitlab.ps1
```

The script pushes only the current committed branch to the configured
`gitlab` remote, using a frozen SHA and explicit refspec, then verifies the
remote SHA. Tracked uncommitted edits block pushing; untracked files are
reported but never staged or uploaded automatically. It never force-pushes.
A failed push or SHA mismatch is a failure, with no GitHub fallback.

Only when the user explicitly requests pushing an existing tag:

```powershell
powershell -File scripts/push-gitlab.ps1 -Tag <existing-tag>
```

Implicit `push.followTags` behavior is disabled. No tag is created, moved or
deleted. A Git tag push does not grant release approval or relax CI gates.

The legacy `push-all.ps1` now prints a migration warning and calls only the
GitLab script. Explicit `-GitHubRemote` is rejected because silently accepting
it would misrepresent the caller's intent. Scripts are self-contained and do
not depend on a sibling repository.

To prevent an accidentally misconfigured remote from contacting GitHub, the
script permits only this installation's endpoints: HTTP port 8088 or SSH port
2224 on `gitlab.gpuflow.test`, `localhost`, or a loopback address. HTTP URLs
must not embed credentials; use the configured credential manager. SSH URLs
must use the `git` user. An intentional move to another GitLab host requires
reviewing and updating this endpoint allowlist. The script never modifies Git
remotes or credentials itself.

Run the fully mocked safety checks without network access:

```powershell
powershell -File scripts/test-push-gitlab.ps1
```

The tests assert zero GitHub network requests, explicit-only tags, dirty-tree
refusal, SHA verification, no staging/commits/config changes, and safe behavior
of the old command. On a machine that requires an execution-policy exception,
use an approved per-process policy; do not weaken the machine-wide policy.
