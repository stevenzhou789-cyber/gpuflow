[CmdletBinding()]
param(
    [string]$RepoPath,
    [string]$GitLabRemote = 'gitlab',
    [string]$Tag
)

# GitLab-only. Never stages, commits, changes Git configuration, creates/moves
# tags, or contacts origin/GitHub. Requires an already authorized commit/push.
$ErrorActionPreference = 'Stop'

function Invoke-GitCommand {
    param([string[]]$Arguments)
    $gitArguments = @('-C', $RepoPath) + $Arguments
    $previousPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $commandOutput = @(& git @gitArguments 2>&1)
        $commandExitCode = $LASTEXITCODE
    }
    finally { $ErrorActionPreference = $previousPreference }
    [pscustomobject]@{
        ExitCode = $commandExitCode
        Lines = @($commandOutput | ForEach-Object { $_.ToString() })
    }
}

function Get-GitValue {
    param([string[]]$Arguments, [string]$FailureMessage)
    $result = Invoke-GitCommand -Arguments $Arguments
    if ($result.ExitCode -ne 0 -or $result.Lines.Count -ne 1) { throw $FailureMessage }
    $result.Lines[0].Trim()
}

function Assert-LocalGitLabUrl {
    param([string]$Value)
    [uri]$parsed = $null
    if (-not [uri]::TryCreate($Value, [UriKind]::Absolute, [ref]$parsed)) {
        throw 'GitLab push URL must use the configured local HTTP or SSH endpoint. No network request was attempted.'
    }
    $allowedHosts = @('gitlab.gpuflow.test', 'localhost', '127.0.0.1', '::1')
    $hostName = $parsed.DnsSafeHost.Trim('[', ']').ToLowerInvariant()
    $httpEndpoint = $parsed.Scheme -eq 'http' -and $parsed.Port -eq 8088 -and $parsed.UserInfo -eq ''
    $sshEndpoint = $parsed.Scheme -eq 'ssh' -and $parsed.Port -eq 2224 -and $parsed.UserInfo -eq 'git'
    if ($hostName -notin $allowedHosts -or (-not $httpEndpoint -and -not $sshEndpoint) -or
        $parsed.Query -ne '' -or $parsed.Fragment -ne '' -or $parsed.AbsolutePath -eq '/') {
        throw 'Refusing a destination outside the local GitLab HTTP:8088 / SSH:2224 endpoints. GitHub/origin and credential-bearing URLs are not allowed.'
    }
}

try {
    # PSScriptRoot can be empty in a -File param default on PowerShell 5.1.
    if ([string]::IsNullOrWhiteSpace($RepoPath)) { $RepoPath = Split-Path -Parent $PSScriptRoot }
    $RepoPath = (Resolve-Path -LiteralPath $RepoPath).Path
    if ([string]::IsNullOrWhiteSpace($GitLabRemote) -or $GitLabRemote.StartsWith('-') -or $GitLabRemote -eq 'origin') {
        throw 'Use the local GitLab remote; origin is retained for history and must not be pushed.'
    }
    $null = Get-GitValue -Arguments @('rev-parse', '--show-toplevel') -FailureMessage 'RepoPath is not a Git working tree.'
    $tracked = Invoke-GitCommand -Arguments @('status', '--porcelain=v1', '--untracked-files=no')
    if ($tracked.ExitCode -ne 0) { throw 'Unable to check tracked working-tree changes.' }
    if ($tracked.Lines.Count -gt 0) {
        throw 'Tracked changes are not committed. Review, test and commit them before pushing. No push was attempted.'
    }
    $untracked = Invoke-GitCommand -Arguments @('ls-files', '--others', '--exclude-standard')
    if ($untracked.ExitCode -ne 0) { throw 'Unable to check untracked files.' }
    if ($untracked.Lines.Count -gt 0) {
        Write-Warning ("{0} untracked file(s) will NOT be included. Only the existing commit is pushed." -f $untracked.Lines.Count)
    }
    $branch = Get-GitValue -Arguments @('symbolic-ref', '--quiet', '--short', 'HEAD') -FailureMessage 'Detached HEAD is not supported. Check out the intended branch.'
    $headSha = Get-GitValue -Arguments @('rev-parse', '--verify', 'HEAD') -FailureMessage 'Unable to resolve the current commit.'
    $references = @([pscustomobject]@{ Ref = 'refs/heads/' + $branch; Sha = $headSha })
    if ($PSBoundParameters.ContainsKey('Tag')) {
        if ([string]::IsNullOrWhiteSpace($Tag)) { throw 'Tag must name an existing, nonempty local tag.' }
        $tagRef = 'refs/tags/' + $Tag
        $format = Invoke-GitCommand -Arguments @('check-ref-format', $tagRef)
        if ($format.ExitCode -ne 0) { throw 'Invalid tag name. No push was attempted.' }
        $tagSha = Get-GitValue -Arguments @('rev-parse', '--verify', '--end-of-options', $tagRef) -FailureMessage 'The requested local tag does not exist. No tag will be created.'
        $references += [pscustomobject]@{ Ref = $tagRef; Sha = $tagSha }
    }
    $urls = Invoke-GitCommand -Arguments @('remote', 'get-url', '--push', '--all', $GitLabRemote)
    if ($urls.ExitCode -ne 0 -or $urls.Lines.Count -ne 1) {
        throw 'The GitLab remote must have exactly one push URL. Configure it locally first; no push was attempted.'
    }
    $pushUrl = $urls.Lines[0].Trim()
    Assert-LocalGitLabUrl $pushUrl
    # Freeze both SHA and the resolved URL. No implicit tags (including when
    # push.followTags=true), wildcard refs, remote mirror config, or force push.
    # The URL is never printed; native output remains captured and sanitized.
    $refSpecs = @($references | ForEach-Object { $_.Sha + ':' + $_.Ref })
    Write-Host ("Pushing GitLab only: {0} ({1}). GitHub/origin is not contacted." -f $GitLabRemote, $branch)
    $push = Invoke-GitCommand -Arguments (@('-c', 'http.followRedirects=false', 'push', '--porcelain', '--atomic', '--no-follow-tags', '--', $pushUrl) + $refSpecs)
    $verified = $true
    foreach ($reference in $references) {
        $lookup = Invoke-GitCommand -Arguments @('-c', 'http.followRedirects=false', 'ls-remote', '--exit-code', '--refs', '--', $pushUrl, $reference.Ref)
        $matches = @($lookup.Lines | Where-Object {
            $fields = $_ -split '\s+', 2
            $fields.Count -eq 2 -and $fields[1] -eq $reference.Ref -and $fields[0] -eq $reference.Sha
        })
        if ($lookup.ExitCode -ne 0 -or $matches.Count -ne 1) {
            $verified = $false
            Write-Warning ("GitLab SHA verification failed: {0}" -f $reference.Ref)
        }
        else { Write-Host ("VERIFIED GitLab: {0} = {1}" -f $reference.Ref, $reference.Sha) }
    }
    if ($push.ExitCode -ne 0 -or -not $verified) {
        Write-Warning ("GitLab push is not verified successful (push exit {0}). No GitHub fallback was attempted. Inspect permissions and branch/tag protection; never force-push or move a release tag." -f $push.ExitCode)
        exit 1
    }
    Write-Host ("SUCCESS: GitLab verified at commit {0}. GitHub remains unchanged." -f $headSha)
    if ($PSBoundParameters.ContainsKey('Tag')) {
        Write-Host ("Existing tag '{0}' was verified on GitLab; no formal release approval is implied." -f $Tag)
    }
    exit 0
}
catch {
    Write-Warning ("GitLab-only push stopped: {0}" -f $_.Exception.Message)
    exit 1
}
