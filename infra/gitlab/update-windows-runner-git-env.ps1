[CmdletBinding()]
param(
    [Parameter(Mandatory = $true)][string]$LocalStateDir,
    [int]$ExpectedRunnerId = 2,
    [switch]$ConfirmRunnerStopped
)

# Runtime-config migration only: no registration, API call, credential reset,
# process termination, global Git configuration or system settings are changed.
$ErrorActionPreference = 'Stop'
function Convert-WindowsRunnerGitEnvironment([string]$Text, [int]$RunnerId) {
    if ([regex]::Matches($Text, '(?m)^\s*\[\[runners\]\]\s*$').Count -ne 1 -or
        [regex]::Matches($Text, '(?m)^\s*id\s*=\s*' + [regex]::Escape([string]$RunnerId) + '\s*$').Count -ne 1 -or
        $Text -notmatch '(?m)^\s*shell\s*=\s*"powershell"\s*$' -or
        $Text -notmatch '(?m)^\s*url\s*=\s*"http://127\.0\.0\.1:8088"\s*$') {
        throw 'Config is not the expected single existing local Windows Runner.'
    }
    $matches = [regex]::Matches($Text, '(?m)^[ \t]*environment[ \t]*=[ \t]*(?<array>\[[^\r\n]*\])[ \t]*\r?$')
    if ($matches.Count -ne 1) { throw 'Expected exactly one single-line Runner environment array; no config was changed.' }
    $match = $matches[0]
    $arrayText = $match.Groups['array'].Value
    try { $entries = ConvertFrom-Json -InputObject $arrayText }
    catch { throw 'Runner environment does not use the expected simple string-array format.' }
    foreach ($required in @('GIT_CONFIG_KEY_0=http.curloptResolve', 'GIT_CONFIG_VALUE_0=gitlab.gpuflow.test:8088:127.0.0.1',
            'GIT_CONFIG_KEY_1=core.longpaths', 'GIT_CONFIG_VALUE_1=true',
            'HTTP_PROXY=', 'HTTPS_PROXY=', 'ALL_PROXY=', 'NO_PROXY=127.0.0.1,localhost,gitlab.gpuflow.test')) {
        if (@($entries | Where-Object { $_ -ceq $required }).Count -ne 1) { throw 'Required nonsecret Runner Git/proxy setting is missing or duplicated.' }
    }
    $configEntries = @($entries | Where-Object { $_ -cmatch '^GIT_CONFIG_(COUNT|KEY_[0-9]+|VALUE_[0-9]+)=' })
    if (@($entries | Where-Object { $_ -ceq 'GIT_CONFIG_COUNT=2' }).Count -eq 1) {
        if ($configEntries.Count -ne 5) { throw 'COUNT=2 config contains unexpected extra Git entries.' }
        return $Text
    }
    if (@($entries | Where-Object { $_ -ceq 'GIT_CONFIG_COUNT=5' }).Count -ne 1 -or $configEntries.Count -ne 11) {
        throw 'Only the known COUNT=5 to COUNT=2 migration is supported.'
    }
    $remove = @('GIT_CONFIG_KEY_2=http.proxy', 'GIT_CONFIG_VALUE_2=',
        'GIT_CONFIG_KEY_3=http.http://gitlab.gpuflow.test:8088.proxy', 'GIT_CONFIG_VALUE_3=',
        'GIT_CONFIG_KEY_4=http.http://127.0.0.1:8088.proxy', 'GIT_CONFIG_VALUE_4=')
    $newArray = $arrayText.Replace('"GIT_CONFIG_COUNT=5"', '"GIT_CONFIG_COUNT=2"')
    foreach ($entry in $remove) {
        if (@($entries | Where-Object { $_ -ceq $entry }).Count -ne 1) { throw 'The old Git environment differs from the expected safe migration input.' }
        $pattern = ',[ \t]*"' + [regex]::Escape($entry) + '"'
        if ([regex]::Matches($newArray, $pattern).Count -ne 1) { throw 'Unexpected environment ordering/encoding; no config was changed.' }
        $newArray = [regex]::Replace($newArray, $pattern, '')
    }
    $newEntries = ConvertFrom-Json -InputObject $newArray
    $expectedEntries = @($entries | Where-Object { $_ -cnotin $remove } | ForEach-Object {
        if ($_ -ceq 'GIT_CONFIG_COUNT=5') { 'GIT_CONFIG_COUNT=2' } else { $_ }
    })
    if (($newEntries -join [char]0) -cne ($expectedEntries -join [char]0)) { throw 'Environment migration changed an unrelated setting.' }
    # All text outside the environment array, including token and runner ID,
    # is preserved character-for-character and is never printed.
    return $Text.Substring(0, $match.Groups['array'].Index) + $newArray +
        $Text.Substring($match.Groups['array'].Index + $match.Groups['array'].Length)
}

if ($env:OS -ne 'Windows_NT' -or -not $ConfirmRunnerStopped) {
    throw 'Pause the Runner, wait for all jobs, stop its verified process, then pass -ConfirmRunnerStopped on Windows.'
}
$state = (Resolve-Path -LiteralPath $LocalStateDir).Path
$runnerRoot = Join-Path $state 'windows-runner'
$config = Join-Path $runnerRoot 'config.toml'
foreach ($path in @($runnerRoot, $config)) {
    if (-not (Test-Path -LiteralPath $path) -or ((Get-Item -LiteralPath $path).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'Expected existing non-reparse private Runner directory/config.'
    }
}
$acl = Get-Acl -LiteralPath $runnerRoot
$allowedSids = @([Security.Principal.WindowsIdentity]::GetCurrent().User.Value, 'S-1-5-18')
if (-not $acl.AreAccessRulesProtected) { throw 'Runner private directory must have a protected DACL before migration.' }
foreach ($rule in $acl.GetAccessRules($true, $true, [Security.Principal.SecurityIdentifier])) {
    if ($rule.AccessControlType -eq 'Allow' -and $rule.IdentityReference.Value -notin $allowedSids) { throw 'Runner directory grants access outside the current user/SYSTEM.' }
}
$binary = Join-Path $runnerRoot 'gitlab-runner-windows-amd64.exe'
$running = @(Get-Process -Name 'gitlab-runner-windows-amd64' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $binary })
if ($running.Count) { throw 'The matching Windows Runner process is still running; this script will not stop it.' }
$before = Get-Content -LiteralPath $config -Raw
$after = Convert-WindowsRunnerGitEnvironment $before $ExpectedRunnerId
if ($before -ceq $after) { Write-Output 'Runner environment already uses the safe two-entry Git configuration; no change.'; exit 0 }
$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + [Guid]::NewGuid().ToString('N')
$temporary = Join-Path $runnerRoot ("config-env-$stamp.tmp")
$backup = Join-Path $runnerRoot ("config-env-before-$stamp.toml")
try {
    [IO.File]::WriteAllText($temporary, $after, [Text.UTF8Encoding]::new($false))
    if ((Get-Content -LiteralPath $config -Raw) -cne $before) { throw 'Config changed concurrently; refusing to overwrite it.' }
    [IO.File]::Replace($temporary, $config, $backup)
    if ((Get-Content -LiteralPath $config -Raw) -cne $after) { throw 'Post-write verification failed; inspect the private rollback copy.' }
}
finally {
    $before = $null; $after = $null
    if (Test-Path -LiteralPath $temporary) { Remove-Item -LiteralPath $temporary }
}
Write-Output 'Only Git environment COUNT/KEY/VALUE entries changed; existing Runner identity/token preserved. Private rollback copy retained beside config. Restart with the registration script after review.'
