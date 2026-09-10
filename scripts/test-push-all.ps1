# Windows PowerShell 5.1 mock tests. No real Git command or network is used.
$ErrorActionPreference = 'Stop'
$pushScript = Join-Path $PSScriptRoot 'push-all.ps1'
$testRepo = Split-Path -Parent $PSScriptRoot
$priorGitFunction = Get-Item Function:\git -ErrorAction SilentlyContinue

function Reset-TestState {
    $global:PushAllMock = @{
        Dirty = $false; Untracked = $false; MultiUrl = $false; TagExists = $true
        FailRemote = ''; MismatchRemote = ''; Calls = @(); RemoteRefs = @{}
        Head = ('a' * 40); Tag = ('b' * 40)
    }
}

function global:git {
    $gitArgs = @($args | ForEach-Object { [string]$_ })
    if ($gitArgs[0] -eq '-C') { $gitArgs = @($gitArgs | Select-Object -Skip 2) }
    $global:PushAllMock.Calls += ,$gitArgs
    $global:LASTEXITCODE = 0
    $command = $gitArgs -join ' '
    switch -Regex ($command) {
        '^rev-parse --show-toplevel$' { Write-Output 'C:/mock/repo'; return }
        '^status ' { if ($global:PushAllMock.Dirty) { Write-Output ' M source.go' }; return }
        '^ls-files ' { if ($global:PushAllMock.Untracked) { Write-Output 'untracked-cache.bin' }; return }
        '^symbolic-ref ' { Write-Output 'main'; return }
        '^rev-parse --verify HEAD$' { Write-Output $global:PushAllMock.Head; return }
        '^check-ref-format ' { return }
        '^rev-parse --verify --end-of-options refs/tags/' {
            if ($global:PushAllMock.TagExists) { Write-Output $global:PushAllMock.Tag } else { $global:LASTEXITCODE = 1 }
            return
        }
        '^remote get-url --push --all ' {
            $remoteName = $gitArgs[4]
            Write-Output ("https://{0}.example.test/project.git" -f $remoteName)
            if ($global:PushAllMock.MultiUrl -and $remoteName -eq 'origin') { Write-Output 'https://second.example.test/project.git' }
            return
        }
        '^push ' {
            $remoteName = $gitArgs[3]
            if ($global:PushAllMock.FailRemote -eq $remoteName) { $global:LASTEXITCODE = 1; Write-Output 'mock push rejected'; return }
            foreach ($spec in @($gitArgs | Select-Object -Skip 4)) {
                $pieces = $spec -split ':', 2
                $sha = if ($global:PushAllMock.MismatchRemote -eq $remoteName) { 'c' * 40 } else { $pieces[0] }
                $global:PushAllMock.RemoteRefs[$remoteName + '|' + $pieces[1]] = $sha
            }
            return
        }
        '^ls-remote ' {
            $remoteName = ([uri]$gitArgs[3]).Host.Split('.')[0]
            $refName = $gitArgs[4]
            $sha = $global:PushAllMock.RemoteRefs[$remoteName + '|' + $refName]
            if ($sha) { Write-Output ($sha + "`t" + $refName) } else { $global:LASTEXITCODE = 2 }
            return
        }
        default { throw ("Unexpected mock Git command: {0}" -f $command) }
    }
}

function Assert-True {
    param([bool]$Condition, [string]$Message)
    if (-not $Condition) { throw $Message }
}

try {
    Reset-TestState
    $captured = @(& $pushScript -RepoPath $testRepo *>&1)
    Assert-True ($LASTEXITCODE -eq 0) 'Basic dual push should succeed.'
    $pushes = @($global:PushAllMock.Calls | Where-Object { $_[0] -eq 'push' })
    Assert-True ($pushes.Count -eq 2) 'Exactly two independent pushes are required.'
    Assert-True (($pushes[0] -join ' ') -notmatch '(--force|--tags|--all)') 'Must not force or push all tags/branches.'
    Assert-True ($pushes[0].Count -eq 5) 'Default push must contain only one explicit refspec.'

    Reset-TestState
    $captured = @(& $pushScript *>&1)
    Assert-True ($LASTEXITCODE -eq 0) 'Default RepoPath must resolve inside the script body.'

    Reset-TestState
    $global:PushAllMock.Dirty = $true
    $captured = @(& $pushScript -RepoPath $testRepo *>&1)
    Assert-True ($LASTEXITCODE -ne 0) 'Tracked dirty changes must block pushing.'
    Assert-True (@($global:PushAllMock.Calls | Where-Object { $_[0] -eq 'push' }).Count -eq 0) 'Dirty preflight must not push.'

    Reset-TestState
    $global:PushAllMock.Untracked = $true
    $captured = @(& $pushScript -RepoPath $testRepo *>&1)
    Assert-True ($LASTEXITCODE -eq 0) 'Untracked caches must not block existing commits.'
    Assert-True (($captured -join "`n") -match 'will NOT be included') 'Untracked files require a warning.'

    foreach ($failureRemote in @('origin', 'gitlab')) {
        Reset-TestState
        $global:PushAllMock.FailRemote = $failureRemote
        $captured = @(& $pushScript -RepoPath $testRepo *>&1)
        Assert-True ($LASTEXITCODE -ne 0) 'A failed destination must produce a nonzero exit.'
        Assert-True (@($global:PushAllMock.Calls | Where-Object { $_[0] -eq 'push' }).Count -eq 2) 'Failure must not hide or skip the other destination.'
        $otherRemote = if ($failureRemote -eq 'origin') { 'gitlab' } else { 'origin' }
        Assert-True (($captured -join "`n") -match ('Successfully updated and verified: ' + $otherRemote)) 'Partial success must name the already updated remote.'
        Assert-True (($captured -join "`n") -match 'NOT rolled back') 'Partial success must disclose non-atomic behavior.'
    }

    Reset-TestState
    $global:PushAllMock.MismatchRemote = 'gitlab'
    $captured = @(& $pushScript -RepoPath $testRepo *>&1)
    Assert-True ($LASTEXITCODE -ne 0) 'A SHA mismatch must fail even when push returns success.'

    Reset-TestState
    $captured = @(& $pushScript -RepoPath $testRepo -Tag v-test *>&1)
    Assert-True ($LASTEXITCODE -eq 0) 'An explicitly requested existing tag should succeed.'
    Assert-True ($global:PushAllMock.RemoteRefs['gitlab|refs/tags/v-test'] -eq $global:PushAllMock.Tag) 'Tag object SHA must be verified, preserving annotated tags.'

    Reset-TestState
    $global:PushAllMock.TagExists = $false
    $captured = @(& $pushScript -RepoPath $testRepo -Tag missing-tag *>&1)
    Assert-True ($LASTEXITCODE -ne 0) 'A missing local tag must be rejected.'
    Assert-True (@($global:PushAllMock.Calls | Where-Object { $_[0] -eq 'push' }).Count -eq 0) 'Missing tag must fail before either push.'

    Reset-TestState
    $global:PushAllMock.MultiUrl = $true
    $captured = @(& $pushScript -RepoPath $testRepo *>&1)
    Assert-True ($LASTEXITCODE -ne 0) 'Multiple push URLs must fail before writes.'
    Assert-True (@($global:PushAllMock.Calls | Where-Object { $_[0] -eq 'push' }).Count -eq 0) 'Multiple push URLs must not cause duplicate pushes.'

    Write-Host 'PASS: 10 dual-remote push mock cases (no network, commits, or Git config changes).'
}
finally {
    Remove-Item Function:\git -ErrorAction SilentlyContinue
    if ($priorGitFunction) { Set-Item Function:\git -Value $priorGitFunction.ScriptBlock }
    Remove-Variable PushAllMock -Scope Global -ErrorAction SilentlyContinue
}
