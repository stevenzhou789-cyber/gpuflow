[CmdletBinding()]
param()

# Read function ASTs, not the installer entrypoint: no download, API request,
# credential read, registration or background process is performed by this test.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
if ($env:OS -ne 'Windows_NT' -or $PSVersionTable.PSEdition -ne 'Desktop' -or
    $PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1) {
    throw 'Run this regression with actual Windows PowerShell 5.1.'
}
$source = Join-Path $PSScriptRoot 'register-windows-runner.ps1'
$parseTokens = $null
$parseErrors = $null
$tree = [Management.Automation.Language.Parser]::ParseFile($source, [ref]$parseTokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw $parseErrors[0] }
foreach ($functionName in @('Protect-Directory', 'Invoke-LocalApi')) {
    $definition = $tree.Find({
        param($node)
        $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $functionName
    }, $true)
    if ($null -eq $definition) { throw "Missing regression target: $functionName" }
    Invoke-Expression $definition.Extent.Text
}

# Preserve quotes through a real nested PowerShell invocation. These assignments
# contain only the version check; no installer state or credentials are touched.
foreach ($variableName in @('$versionCheck', '$encodedVersionCheck')) {
    $assignment = $tree.Find({
        param($node)
        $node -is [Management.Automation.Language.AssignmentStatementAst] -and $node.Left.Extent.Text -eq $variableName
    }, $true)
    if ($null -eq $assignment) { throw 'Missing encoded version preflight.' }
    Invoke-Expression $assignment.Extent.Text
}
$powershell51 = Join-Path $env:SystemRoot 'System32/WindowsPowerShell/v1.0/powershell.exe'
& $powershell51 -NoLogo -NoProfile -OutputFormat Text -EncodedCommand $encodedVersionCheck
if ($LASTEXITCODE -ne 0) { throw 'Encoded PS5.1 version check failed.' }

# Reproduce Invoke-RestMethod JSON-array non-enumeration in Windows PS5.1.
$ApiBase = 'http://127.0.0.1:8088/api/v4'
$headers = @{}
$script:mockCalls = 0
$script:mockResponse = @([pscustomobject]@{ id = 1 }, [pscustomobject]@{ id = 2 })
function Invoke-RestMethod {
    param($Method, $Uri, $Headers, $UseBasicParsing, $ContentType, $Body)
    $script:mockCalls++
    Write-Output -NoEnumerate $script:mockResponse
}
$records = @(Invoke-LocalApi 'GET' 'groups/2/runners')
if ($records.Count -ne 2 -or [string]$records[1].id -ne '2' -or
    -not ($records | Where-Object { [string]$_.id -eq '2' })) {
    throw 'JSON array was not enumerated into separate runner records.'
}
$script:mockResponse = [pscustomobject]@{ id = 3 }
$single = Invoke-LocalApi 'GET' 'runners/3'
if ($single.id -ne 3 -or $script:mockCalls -ne 2) { throw 'Single-object API response changed.' }

# The historical failure occurred on the second call, with a private directory
# that already existed. Check creation, idempotence and exact DACL with no admin.
$fixture = Join-Path ([IO.Path]::GetTempPath()) ('gpuflow-runner-acl-test-' + [Guid]::NewGuid().ToString('N'))
try {
    Protect-Directory $fixture
    Protect-Directory $fixture
    $acl = Get-Acl -LiteralPath $fixture
    $rules = @($acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier]))
    $allowed = @([Security.Principal.WindowsIdentity]::GetCurrent().User.Value, 'S-1-5-18')
    if (-not $acl.AreAccessRulesProtected -or $rules.Count -ne 2) { throw 'Unexpected inherited or additional DACL rules.' }
    foreach ($rule in $rules) {
        if ($rule.IdentityReference.Value -notin $allowed -or $rule.AccessControlType -ne 'Allow' -or
            $rule.FileSystemRights -ne 'FullControl') { throw 'Unexpected private-directory access rule.' }
    }
}
finally {
    # Only this newly created, empty temporary directory is removed.
    if (Test-Path -LiteralPath $fixture) { Remove-Item -LiteralPath $fixture }
}
$migrationPath = Join-Path $PSScriptRoot 'update-windows-runner-git-env.ps1'
$migrationTree = [Management.Automation.Language.Parser]::ParseFile($migrationPath, [ref]$parseTokens, [ref]$parseErrors)
if ($parseErrors.Count) { throw $parseErrors[0] }
$migrationFunction = $migrationTree.Find({
    param($node)
    $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq 'Convert-WindowsRunnerGitEnvironment'
}, $true)
Invoke-Expression $migrationFunction.Extent.Text
$oldEntries = @('GIT_CONFIG_COUNT=5',
    'GIT_CONFIG_KEY_0=http.curloptResolve', 'GIT_CONFIG_VALUE_0=gitlab.gpuflow.test:8088:127.0.0.1',
    'GIT_CONFIG_KEY_1=core.longpaths', 'GIT_CONFIG_VALUE_1=true',
    'GIT_CONFIG_KEY_2=http.proxy', 'GIT_CONFIG_VALUE_2=',
    'GIT_CONFIG_KEY_3=http.http://gitlab.gpuflow.test:8088.proxy', 'GIT_CONFIG_VALUE_3=',
    'GIT_CONFIG_KEY_4=http.http://127.0.0.1:8088.proxy', 'GIT_CONFIG_VALUE_4=',
    'HTTP_PROXY=', 'HTTPS_PROXY=', 'ALL_PROXY=', 'NO_PROXY=127.0.0.1,localhost,gitlab.gpuflow.test',
    'GOMAXPROCS=2', 'GOFLAGS=-p=2')
$fixtureConfig = @'
concurrent = 1
[[runners]]
  id = 2
  token = "fixture-token-not-secret"
  url = "http://127.0.0.1:8088"
  shell = "powershell"
  environment = PLACEHOLDER
'@
$fixtureConfig = $fixtureConfig.Replace('PLACEHOLDER', (ConvertTo-Json -InputObject $oldEntries -Compress))
$migrated = Convert-WindowsRunnerGitEnvironment $fixtureConfig 2
if ($migrated -notmatch '"GIT_CONFIG_COUNT=2"' -or $migrated -match '"GIT_CONFIG_(KEY|VALUE)_[2-4]=' -or
    $migrated -notmatch 'token = "fixture-token-not-secret"' -or $migrated -notmatch '"GOFLAGS=-p=2"') {
    throw 'Known environment migration changed identity or failed to remove empty Git entries.'
}
if ((Convert-WindowsRunnerGitEnvironment $migrated 2) -cne $migrated) { throw 'Environment migration is not idempotent.' }
foreach ($badConfig in @(
    $fixtureConfig.Replace('id = 2', 'id = 3'),
    $fixtureConfig.Replace('GIT_CONFIG_VALUE_2=', 'GIT_CONFIG_VALUE_2=unexpected'),
    $fixtureConfig.Replace('NO_PROXY=127.0.0.1,localhost,gitlab.gpuflow.test', 'NO_PROXY=other'),
    $fixtureConfig.Replace('GIT_CONFIG_COUNT=5', 'GIT_CONFIG_COUNT=9')
)) {
    $rejected = $false
    try { [void](Convert-WindowsRunnerGitEnvironment $badConfig 2) } catch { $rejected = $true }
    if (-not $rejected) { throw 'Unsafe config migration input was accepted.' }
}
$runtimeFixture = Join-Path ([IO.Path]::GetTempPath()) ('gpuflow-runner-migration-test-' + [Guid]::NewGuid().ToString('N'))
$runtimeRunner = Join-Path $runtimeFixture 'windows-runner'
try {
    [void](New-Item -ItemType Directory -Path $runtimeFixture)
    Protect-Directory $runtimeRunner
    $runtimeConfig = Join-Path $runtimeRunner 'config.toml'
    [IO.File]::WriteAllText($runtimeConfig, $fixtureConfig, [Text.UTF8Encoding]::new($false))
    & $powershell51 -NoLogo -NoProfile -ExecutionPolicy Bypass -File $migrationPath -LocalStateDir $runtimeFixture -ExpectedRunnerId 2 -ConfirmRunnerStopped
    if ($LASTEXITCODE -ne 0 -or (Get-Content -LiteralPath $runtimeConfig -Raw) -cne $migrated) { throw 'Real private-file atomic migration failed.' }
    $rollbackFiles = @(Get-ChildItem -LiteralPath $runtimeRunner -Filter 'config-env-before-*.toml')
    if ($rollbackFiles.Count -ne 1 -or (Get-Content -LiteralPath $rollbackFiles[0].FullName -Raw) -cne $fixtureConfig) {
        throw 'Private rollback copy did not preserve the old fixture config.'
    }
}
finally {
    if (Test-Path -LiteralPath $runtimeRunner) {
        foreach ($item in Get-ChildItem -LiteralPath $runtimeRunner) {
            if ($item.PSIsContainer -or -not $item.FullName.StartsWith($runtimeRunner + [IO.Path]::DirectorySeparatorChar)) { throw 'Unexpected temporary migration cleanup target.' }
            Remove-Item -LiteralPath $item.FullName
        }
        Remove-Item -LiteralPath $runtimeRunner
    }
    if (Test-Path -LiteralPath $runtimeFixture) { Remove-Item -LiteralPath $runtimeFixture }
}
Write-Output 'PASS: PS5.1 preflight/API/DACL, Git-env migration/refusals and real atomic private-file rollback; no real API or credential access.'
