[CmdletBinding()]
param(
    [string]$RepoPath,
    [string]$GitHubRemote,
    [string]$GitLabRemote = 'gitlab',
    [string]$Tag
)
$ErrorActionPreference = 'Stop'
if ($PSBoundParameters.ContainsKey('GitHubRemote')) {
    Write-Warning 'GitHub pushing is disabled by the current user policy. Explicit -GitHubRemote is rejected; remove it only if you intend GitLab-only pushing.'
    exit 1
}
Write-Warning 'GitLab-only compatibility: push-all.ps1 now delegates exclusively to push-gitlab.ps1. GitHub/origin will NOT be contacted.'
$parameters = @{ GitLabRemote = $GitLabRemote }
if ($PSBoundParameters.ContainsKey('RepoPath')) { $parameters.RepoPath = $RepoPath }
if ($PSBoundParameters.ContainsKey('Tag')) { $parameters.Tag = $Tag }
& (Join-Path $PSScriptRoot 'push-gitlab.ps1') @parameters
exit $LASTEXITCODE
