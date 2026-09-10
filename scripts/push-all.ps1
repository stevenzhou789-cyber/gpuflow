[CmdletBinding()]
param(
    [string]$RepoPath,
    [string]$GitHubRemote = 'origin',
    [string]$GitLabRemote = 'gitlab',
    [string]$Tag
)

# Compatible with Windows PowerShell 5.1. This script never stages, commits,
# creates/moves a tag, or force-pushes. The two servers are NOT one transaction.
$ErrorActionPreference = 'Stop'

function Invoke-GitCommand {
    param([string[]]$Arguments)
    $gitArguments = @('-C', $RepoPath) + $Arguments
    # Native Git uses stderr for normal progress as well as errors. Capture it
    # without PowerShell 5.1 turning native stderr into a terminating exception.
    $previousPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        $commandOutput = @(& git @gitArguments 2>&1)
        $commandExitCode = $LASTEXITCODE
    }
    finally {
        $ErrorActionPreference = $previousPreference
    }
    return [pscustomobject]@{
        ExitCode = $commandExitCode
        Lines = @($commandOutput | ForEach-Object { $_.ToString() })
    }
}

function Get-GitValue {
    param([string[]]$Arguments, [string]$FailureMessage)
    $result = Invoke-GitCommand -Arguments $Arguments
    if ($result.ExitCode -ne 0 -or $result.Lines.Count -ne 1) {
        throw $FailureMessage
    }
    return $result.Lines[0].Trim()
}

try {
    # Windows PowerShell 5.1 may leave PSScriptRoot empty while evaluating a
    # param default under -File. Resolve it only after the script body starts.
    if ([string]::IsNullOrWhiteSpace($RepoPath)) {
        $RepoPath = Split-Path -Parent $PSScriptRoot
    }
    $RepoPath = (Resolve-Path -LiteralPath $RepoPath).Path
    $null = Get-GitValue -Arguments @('rev-parse', '--show-toplevel') -FailureMessage 'RepoPath is not a Git working tree.'
    if ($GitHubRemote -eq $GitLabRemote) {
        throw 'GitHubRemote and GitLabRemote must be two different remotes.'
    }
    foreach ($remoteName in @($GitHubRemote, $GitLabRemote)) {
        if ([string]::IsNullOrWhiteSpace($remoteName) -or $remoteName.StartsWith('-')) {
            throw 'Remote names must be nonempty and must not begin with a dash.'
        }
    }

    $trackedStatus = Invoke-GitCommand -Arguments @('status', '--porcelain=v1', '--untracked-files=no')
    if ($trackedStatus.ExitCode -ne 0) { throw 'Unable to check tracked working-tree changes.' }
    if ($trackedStatus.Lines.Count -gt 0) {
        throw 'Tracked changes are not committed. Review, test and commit them before synchronizing. No push was attempted.'
    }
    $untracked = Invoke-GitCommand -Arguments @('ls-files', '--others', '--exclude-standard')
    if ($untracked.ExitCode -ne 0) { throw 'Unable to check untracked files.' }
    if ($untracked.Lines.Count -gt 0) {
        Write-Warning ("{0} untracked file(s) will NOT be included. Only the existing commit is synchronized." -f $untracked.Lines.Count)
    }

    $branchName = Get-GitValue -Arguments @('symbolic-ref', '--quiet', '--short', 'HEAD') -FailureMessage 'Detached HEAD is not supported. Check out the branch to synchronize.'
    $headSha = Get-GitValue -Arguments @('rev-parse', '--verify', 'HEAD') -FailureMessage 'Unable to resolve the current commit.'
    $branchRef = 'refs/heads/' + $branchName
    $references = @([pscustomobject]@{ Ref = $branchRef; Sha = $headSha })
    if ($PSBoundParameters.ContainsKey('Tag')) {
        if ([string]::IsNullOrWhiteSpace($Tag)) { throw 'Tag must be an existing, nonempty tag name.' }
        $tagRef = 'refs/tags/' + $Tag
        $tagFormat = Invoke-GitCommand -Arguments @('check-ref-format', $tagRef)
        if ($tagFormat.ExitCode -ne 0) { throw 'Invalid tag name. No push was attempted.' }
        $tagSha = Get-GitValue -Arguments @('rev-parse', '--verify', '--end-of-options', $tagRef) -FailureMessage 'The requested local tag does not exist. No tag will be created.'
        $references += [pscustomobject]@{ Ref = $tagRef; Sha = $tagSha }
    }

    # Preflight BOTH destinations before changing either. A remote with multiple
    # push URLs would make one push touch multiple servers and repeat work here.
    $destinations = @()
    foreach ($remoteName in @($GitHubRemote, $GitLabRemote)) {
        $urls = Invoke-GitCommand -Arguments @('remote', 'get-url', '--push', '--all', $remoteName)
        if ($urls.ExitCode -ne 0 -or $urls.Lines.Count -eq 0) {
            throw ("Remote '{0}' has no usable push URL. Configure it first." -f $remoteName)
        }
        if ($urls.Lines.Count -ne 1) {
            throw ("Remote '{0}' has multiple push URLs. Use one GitHub URL for origin and one GitLab URL for gitlab; no push was attempted." -f $remoteName)
        }
        $destinations += [pscustomobject]@{ Name = $remoteName; Url = $urls.Lines[0] }
    }
    if ($destinations[0].Url -eq $destinations[1].Url) {
        throw 'Both remotes use the same push URL. Configure two distinct destinations.'
    }

    # SHA-based explicit refspecs freeze the intended snapshot even if HEAD is
    # changed elsewhere while this script is running. No other branches or tags
    # are included. --atomic applies only WITHIN each individual remote.
    $refSpecs = @($references | ForEach-Object { $_.Sha + ':' + $_.Ref })
    $results = @()
    foreach ($destination in $destinations) {
        Write-Host ("Synchronizing {0} ({1})..." -f $destination.Name, $branchName)
        $push = Invoke-GitCommand -Arguments (@('push', '--porcelain', '--atomic', $destination.Name) + $refSpecs)
        $verified = $true
        foreach ($reference in $references) {
            # Verify the actual PUSH URL, not a potentially different fetch URL.
            # Never print it: configured URLs may contain credentials.
            $lookup = Invoke-GitCommand -Arguments @('ls-remote', '--exit-code', '--refs', $destination.Url, $reference.Ref)
            $matchingLines = @($lookup.Lines | Where-Object {
                $fields = $_ -split '\s+', 2
                $fields.Count -eq 2 -and $fields[1] -eq $reference.Ref -and $fields[0] -eq $reference.Sha
            })
            if ($lookup.ExitCode -ne 0 -or $matchingLines.Count -ne 1) {
                $verified = $false
                Write-Warning ("{0}: remote SHA verification failed for {1}." -f $destination.Name, $reference.Ref)
            }
            else {
                Write-Host ("VERIFIED {0}: {1} = {2}" -f $destination.Name, $reference.Ref, $reference.Sha)
            }
        }
        $success = $push.ExitCode -eq 0 -and $verified
        $results += [pscustomobject]@{ Name = $destination.Name; Success = $success; Verified = $verified; PushExitCode = $push.ExitCode }
        if ($success) {
            Write-Host ("SUCCESS {0}: push completed and all requested remote SHAs match." -f $destination.Name)
        }
        elseif ($verified) {
            Write-Warning ("FAILED {0}: push exited {1}, although the requested remote SHAs currently match. Treating this as failure; inspect authentication/server status." -f $destination.Name, $push.ExitCode)
        }
        else {
            Write-Warning ("FAILED {0}: push exit code {1}; synchronization is not verified. Check credentials, permissions and branch/tag protection for this remote." -f $destination.Name, $push.ExitCode)
        }
        if ($push.ExitCode -ne 0) {
            # Summarize common causes without echoing server output that could
            # contain an authenticated URL or other credential-bearing details.
            $pushText = $push.Lines -join "`n"
            if ($pushText -match 'non-fast-forward|fetch first') {
                Write-Warning 'The remote contains changes missing locally. Fetch and review them before retrying; do not force-push.'
            }
            elseif ($pushText -match 'already exists|would clobber') {
                Write-Warning 'A remote tag/ref already exists with a different value. Do not move an existing release tag.'
            }
        }
    }

    if (@($results | Where-Object { -not $_.Success }).Count -gt 0) {
        $successfulRemotes = @($results | Where-Object { $_.Success } | ForEach-Object { $_.Name })
        $successText = if ($successfulRemotes.Count -gt 0) { $successfulRemotes -join ', ' } else { '(none)' }
        Write-Warning ("Dual synchronization FAILED. Successfully updated and verified: {0}. The two servers are not atomic; completed pushes were NOT rolled back. Fix the failed destination and run this script again." -f $successText)
        exit 1
    }

    Write-Host ("SUCCESS: both remotes verified at commit {0}." -f $headSha)
    if ($PSBoundParameters.ContainsKey('Tag')) {
        Write-Host ("Existing tag '{0}' was also synchronized and verified; no release approval is implied." -f $Tag)
    }
    exit 0
}
catch {
    Write-Warning ("Synchronization stopped: {0}" -f $_.Exception.Message)
    exit 1
}
