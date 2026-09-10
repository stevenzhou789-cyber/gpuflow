# Windows PowerShell 5.1 mock tests. No network, real pushes, commits, or config writes.
$ErrorActionPreference = 'Stop'
$pushScript = Join-Path $PSScriptRoot 'push-gitlab.ps1'
$testRepo = Split-Path -Parent $PSScriptRoot
$priorGit = Get-Item Function:\git -ErrorAction SilentlyContinue
$global:GitLabPushAllCalls = @()

function Reset-TestState {
    $global:GitLabPushMock = @{
        Dirty = $false; Untracked = $false; MultipleUrls = $false; TagExists = $true
        FailPush = $false; Mismatch = $false; Detached = $false; InvalidTag = $false
        Url = 'http://gitlab.gpuflow.test:8088/gpuflow/test.git'
        Head = ('a' * 40); Tag = ('b' * 40); Calls = @(); Refs = @{}
    }
}

function global:git {
    $arguments = @($args | ForEach-Object { [string]$_ })
    if ($arguments[0] -eq '-C') { $arguments = @($arguments | Select-Object -Skip 2) }
    $global:GitLabPushAllCalls += ,$arguments
    $global:GitLabPushMock.Calls += ,$arguments
    if ($arguments[0] -eq '-c') { $arguments = @($arguments | Select-Object -Skip 2) }
    $global:LASTEXITCODE = 0
    $command = $arguments -join ' '
    switch -Regex ($command) {
        '^rev-parse --show-toplevel$' { Write-Output 'C:/mock/repo'; return }
        '^status ' { if ($global:GitLabPushMock.Dirty) { Write-Output ' M source.go' }; return }
        '^ls-files ' { if ($global:GitLabPushMock.Untracked) { Write-Output 'private-cache.bin' }; return }
        '^symbolic-ref ' {
            if ($global:GitLabPushMock.Detached) { $global:LASTEXITCODE = 1 } else { Write-Output 'main' }
            return
        }
        '^rev-parse --verify HEAD$' { Write-Output $global:GitLabPushMock.Head; return }
        '^check-ref-format ' { if ($global:GitLabPushMock.InvalidTag) { $global:LASTEXITCODE = 1 }; return }
        '^rev-parse --verify --end-of-options refs/tags/' {
            if ($global:GitLabPushMock.TagExists) { Write-Output $global:GitLabPushMock.Tag } else { $global:LASTEXITCODE = 1 }
            return
        }
        '^remote get-url --push --all ' {
            if ($arguments[4] -ne 'gitlab') { throw 'Non-GitLab remote was inspected.' }
            Write-Output $global:GitLabPushMock.Url
            if ($global:GitLabPushMock.MultipleUrls) { Write-Output 'https://github.com/forbidden/extra.git' }
            return
        }
        '^push ' {
            if ($arguments[5] -ne $global:GitLabPushMock.Url) { throw 'Unexpected push target.' }
            if ($global:GitLabPushMock.FailPush) { $global:LASTEXITCODE = 1; Write-Output 'mock rejection'; return }
            foreach ($spec in @($arguments | Select-Object -Skip 6)) {
                $parts = $spec -split ':', 2
                $global:GitLabPushMock.Refs[$parts[1]] = if ($global:GitLabPushMock.Mismatch) { 'c' * 40 } else { $parts[0] }
            }
            return
        }
        '^ls-remote ' {
            if ($arguments[4] -ne $global:GitLabPushMock.Url) { throw 'Unexpected verification target.' }
            $ref = $arguments[5]
            $sha = $global:GitLabPushMock.Refs[$ref]
            if ($sha) { Write-Output ($sha + "`t" + $ref) } else { $global:LASTEXITCODE = 2 }
            return
        }
        default { throw "Unexpected mock Git command: $command" }
    }
}

function Assert-True([bool]$Condition, [string]$Message) {
    if (-not $Condition) { throw $Message }
}
function Get-PushCalls {
    @($global:GitLabPushMock.Calls | Where-Object { $_ -contains 'push' })
}

try {
    Reset-TestState
    $captured = @(& $pushScript -RepoPath $testRepo *>&1)
    Assert-True ($LASTEXITCODE -eq 0) 'GitLab-only push should succeed.'
    $pushes = @(Get-PushCalls)
    Assert-True ($pushes.Count -eq 1) 'Exactly one server should receive one push.'
    Assert-True ($pushes[0] -contains '--no-follow-tags') 'Implicit tags must be disabled even if configured by the user.'
    Assert-True (($pushes[0] -join ' ') -notmatch '(--force|--tags|--all|--mirror)') 'No force, all-tags, wildcard, or mirror pushes.'
    Assert-True ($pushes[0][-1] -eq (($global:GitLabPushMock.Head) + ':refs/heads/main')) 'Only the frozen explicit branch SHA is pushed by default.'

    Reset-TestState
    $captured = @(& $pushScript *>&1)
    Assert-True ($LASTEXITCODE -eq 0) 'Default RepoPath must work with PowerShell 5.1.'

    Reset-TestState
    $global:GitLabPushMock.Dirty = $true
    $captured = @(& $pushScript *>&1)
    Assert-True ($LASTEXITCODE -ne 0 -and @(Get-PushCalls).Count -eq 0) 'Tracked edits must block pushing.'

    Reset-TestState
    $global:GitLabPushMock.Untracked = $true
    $captured = @(& $pushScript *>&1)
    Assert-True ($LASTEXITCODE -eq 0 -and ($captured -join "`n") -match 'will NOT be included') 'Untracked files are warned about and never staged.'

    foreach ($mode in @('FailPush', 'Mismatch', 'Detached')) {
        Reset-TestState
        $global:GitLabPushMock[$mode] = $true
        $captured = @(& $pushScript *>&1)
        Assert-True ($LASTEXITCODE -ne 0) "$mode must report failure without falling back to GitHub."
    }

    Reset-TestState
    $captured = @(& $pushScript -Tag v-test *>&1)
    Assert-True ($LASTEXITCODE -eq 0) 'An explicit existing tag should be pushed with the branch.'
    Assert-True ($global:GitLabPushMock.Refs['refs/tags/v-test'] -eq $global:GitLabPushMock.Tag) 'Preserve and verify the annotated tag object SHA.'
    Assert-True (@($global:GitLabPushMock.Refs.Keys).Count -eq 2) 'Only the branch and explicitly requested tag are pushed.'

    foreach ($mode in @('TagExists', 'InvalidTag')) {
        Reset-TestState
        $global:GitLabPushMock[$mode] = $mode -ne 'TagExists'
        $captured = @(& $pushScript -Tag invalid-or-missing *>&1)
        Assert-True ($LASTEXITCODE -ne 0 -and @(Get-PushCalls).Count -eq 0) 'Invalid/missing tag must fail before pushing.'
    }

    Reset-TestState
    $global:GitLabPushMock.MultipleUrls = $true
    $captured = @(& $pushScript *>&1)
    Assert-True ($LASTEXITCODE -ne 0 -and @(Get-PushCalls).Count -eq 0) 'Multiple push URLs must be rejected before any network action.'

    foreach ($url in @('https://github.com/forbidden/project.git', 'http://gitlab.gpuflow.test:8088@github.com/repo', 'http://user:secret@gitlab.gpuflow.test:8088/repo')) {
        Reset-TestState
        $global:GitLabPushMock.Url = $url
        $captured = @(& $pushScript *>&1)
        Assert-True ($LASTEXITCODE -ne 0 -and @(Get-PushCalls).Count -eq 0) 'GitHub and credential-bearing destinations must be refused.'
    }

    Reset-TestState
    $captured = @(& $pushScript -GitLabRemote origin *>&1)
    Assert-True ($LASTEXITCODE -ne 0 -and $global:GitLabPushMock.Calls.Count -eq 0) 'Explicit origin must fail without any Git request.'

    $networkCalls = @($global:GitLabPushAllCalls | Where-Object { $_ -contains 'push' -or $_ -contains 'ls-remote' })
    Assert-True (@($networkCalls | Where-Object { ($_ -join ' ') -match 'github\.com|origin' }).Count -eq 0) 'ZERO GitHub requests are permitted across all cases.'
    Assert-True (@($global:GitLabPushAllCalls | Where-Object { $_ -contains 'add' -or $_ -contains 'commit' -or $_ -contains 'config' }).Count -eq 0) 'Script must not stage, commit, or change config.'
    Write-Host 'PASS: 15 explicit GitLab-only cases; ZERO GitHub network requests (fully mocked).'
    exit 0
}
finally {
    Remove-Item Function:\git -ErrorAction SilentlyContinue
    if ($priorGit) { Set-Item Function:\git -Value $priorGit.ScriptBlock }
    Remove-Variable GitLabPushMock,GitLabPushAllCalls -Scope Global -ErrorAction SilentlyContinue
}
