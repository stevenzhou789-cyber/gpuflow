[CmdletBinding()]
param(
    [string]$LocalStateDir,
    [string]$ApiBase = 'http://127.0.0.1:8088/api/v4'
)

# User-mode runner for trusted protected refs only. No service, scheduled task,
# hosts edit, machine-wide Git settings, or Windows elevation is performed.
# A shell executor runs with this user's permissions; it is not a sandbox.
$ErrorActionPreference = 'Stop'
if ($env:OS -ne 'Windows_NT') { throw 'This runner requires Windows.' }
if ($ApiBase -ne 'http://127.0.0.1:8088/api/v4') { throw 'Only the local GitLab API is allowed.' }
if (-not $LocalStateDir) { $LocalStateDir = Join-Path $PSScriptRoot '../../.local-gitlab' }
$stateRoot = (Resolve-Path -LiteralPath $LocalStateDir).Path
$bootstrapFile = Join-Path $stateRoot 'bootstrap-api.credential.xml'
if (-not (Test-Path -LiteralPath $bootstrapFile -PathType Leaf)) { throw 'Run local bootstrap first under the same Windows account.' }
$windowsPowerShell = Join-Path $env:SystemRoot 'System32/WindowsPowerShell/v1.0/powershell.exe'
if (-not (Test-Path -LiteralPath $windowsPowerShell -PathType Leaf)) { throw 'Windows PowerShell 5.1 is required.' }

function Protect-Directory([string]$Path) {
    if ((Test-Path -LiteralPath $Path) -and ((Get-Item -LiteralPath $Path).Attributes -band [IO.FileAttributes]::ReparsePoint)) {
        throw 'Runner private directories must not be junctions or symbolic links.'
    }
    [void](New-Item -ItemType Directory -Path $Path -Force)
    # Reuse the existing descriptor and modify only its DACL. A new descriptor
    # plus SetOwner can make Set-Acl request owner/SACL privileges on a second
    # run, although the current user already owns this private directory.
    $accessOnly = [Security.AccessControl.AccessControlSections]::Access
    $existingSecurity = Get-Acl -LiteralPath $Path
    $security = New-Object Security.AccessControl.DirectorySecurity
    $security.SetSecurityDescriptorSddlForm($existingSecurity.GetSecurityDescriptorSddlForm($accessOnly), $accessOnly)
    $security.SetAccessRuleProtection($true, $false)
    $currentSid = [Security.Principal.WindowsIdentity]::GetCurrent().User
    $systemSid = New-Object Security.Principal.SecurityIdentifier('S-1-5-18')
    foreach ($existingRule in @($security.GetAccessRules($true, $false, [Security.Principal.SecurityIdentifier]))) {
        [void]$security.RemoveAccessRuleSpecific($existingRule)
    }
    foreach ($sid in @($currentSid, $systemSid)) {
        $rule = New-Object Security.AccessControl.FileSystemAccessRule(
            $sid, [Security.AccessControl.FileSystemRights]::FullControl,
            ([Security.AccessControl.InheritanceFlags]::ContainerInherit -bor [Security.AccessControl.InheritanceFlags]::ObjectInherit),
            [Security.AccessControl.PropagationFlags]::None, [Security.AccessControl.AccessControlType]::Allow)
        $security.AddAccessRule($rule)
    }
    # Set-Acl can attempt to persist audit sections in Windows PowerShell.
    # Persist the DACL-only descriptor through the filesystem API instead.
    if ($PSVersionTable.PSVersion.Major -le 5) {
        [IO.Directory]::SetAccessControl($Path, $security)
    } else {
        [IO.FileSystemAclExtensions]::SetAccessControl((Get-Item -LiteralPath $Path), $security)
    }
}

function Invoke-PrivateNative([string]$Executable, [string[]]$Arguments, [string]$Label) {
    $preference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        $privateOutput = @(& $Executable @Arguments 2>&1)
        $code = $LASTEXITCODE
    }
    finally { $ErrorActionPreference = $preference }
    # Runner diagnostics can contain a token. Never print captured output.
    $privateOutput = $null
    if ($code -ne 0) { throw "$Label failed with exit code $code; details are withheld to protect authentication data." }
}

$runnerRoot = Join-Path $stateRoot 'windows-runner'
Protect-Directory $runnerRoot
$builds = Join-Path $runnerRoot 'builds'
$cache = Join-Path $runnerRoot 'cache'
$logs = Join-Path $runnerRoot 'logs'
foreach ($directory in @($builds, $cache, $logs)) { Protect-Directory $directory }
$binary = Join-Path $runnerRoot 'gitlab-runner-windows-amd64.exe'
$config = Join-Path $runnerRoot 'config.toml'
$runnerCredentialFile = Join-Path $runnerRoot 'runner.credential.xml'
$processRecord = Join-Path $runnerRoot 'process.json'
$expectedHash = 'bc4ac4eb1b566020888867c183d2d510efe3878b394e7b914de67f2302482984'
$downloadUrl = 'https://gitlab-runner-downloads.s3.amazonaws.com/v19.3.0/binaries/gitlab-runner-windows-amd64.exe'

if (-not (Test-Path -LiteralPath $binary -PathType Leaf)) {
    $download = Join-Path $runnerRoot ('runner-download-' + [Guid]::NewGuid().ToString('N') + '.tmp')
    $previousProtocol = [Net.ServicePointManager]::SecurityProtocol
    try {
        [Net.ServicePointManager]::SecurityProtocol = $previousProtocol -bor [Net.SecurityProtocolType]::Tls12
        Invoke-WebRequest -UseBasicParsing -Uri $downloadUrl -OutFile $download
        if ((Get-FileHash -LiteralPath $download -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expectedHash) {
            throw 'Downloaded Windows Runner SHA256 does not match the pinned official binary.'
        }
        Move-Item -LiteralPath $download -Destination $binary
    }
    finally {
        [Net.ServicePointManager]::SecurityProtocol = $previousProtocol
        if (Test-Path -LiteralPath $download) { Remove-Item -LiteralPath $download -Force }
    }
}
if ((Get-FileHash -LiteralPath $binary -Algorithm SHA256).Hash.ToLowerInvariant() -ne $expectedHash) {
    throw 'Existing Windows Runner binary has an unexpected SHA256; it will not be overwritten or executed.'
}
# PS 5.1 native argument forwarding removes nested quotes from -Command.
# UTF-16LE EncodedCommand preserves the exact check; no credentials are used.
$versionCheck = '$ProgressPreference = "SilentlyContinue"; Write-Output ("Detected PowerShell {0} / {1}" -f $PSVersionTable.PSVersion, $PSVersionTable.PSEdition); if ($PSVersionTable.PSEdition -ne "Desktop" -or $PSVersionTable.PSVersion.Major -ne 5 -or $PSVersionTable.PSVersion.Minor -ne 1) { exit 1 }'
$encodedVersionCheck = [Convert]::ToBase64String([Text.Encoding]::Unicode.GetBytes($versionCheck))
& $windowsPowerShell -NoLogo -NoProfile -OutputFormat Text -EncodedCommand $encodedVersionCheck
if ($LASTEXITCODE -ne 0) { throw 'Windows PowerShell 5.1 verification failed; the Windows Desktop 5.1 runtime is required.' }

$bootstrapCredential = Import-Clixml -LiteralPath $bootstrapFile
$headers = @{ 'PRIVATE-TOKEN' = $bootstrapCredential.GetNetworkCredential().Password; Host = 'gitlab.gpuflow.test:8088' }
function Invoke-LocalApi([string]$Method, [string]$Path, $Body = $null) {
    $parameters = @{ Method = $Method; Uri = "$ApiBase/$Path"; Headers = $headers; UseBasicParsing = $true }
    if ($null -ne $Body) {
        $parameters.ContentType = 'application/json'
        $parameters.Body = ConvertTo-Json -InputObject $Body -Depth 8 -Compress
    }
    try {
        $response = Invoke-RestMethod @parameters
        # Invoke-RestMethod emits a JSON array as a single pipeline object in
        # Windows PS 5.1. Explicitly enumerate it so callers using @() receive
        # runner/group records, not one nested array with an array-valued id.
        foreach ($record in $response) { Write-Output $record }
    }
    catch { throw 'Local GitLab API request failed. Check that GitLab is ready and the one-day bootstrap credential is still valid; authentication details are withheld.' }
}

try {
    $groups = @(Invoke-LocalApi 'GET' 'groups?search=gpuflow&per_page=100')
    $group = @($groups | Where-Object { $_.full_path -eq 'gpuflow' -and $_.visibility -eq 'private' })
    if ($group.Count -ne 1) { throw 'Exactly one private gpuflow group is required.' }
    if (-not (Test-Path -LiteralPath $runnerCredentialFile -PathType Leaf)) {
        if (Test-Path -LiteralPath $config) { throw 'Runner config exists without its matching encrypted credential; refusing an ambiguous registration.' }
        $created = Invoke-LocalApi 'POST' 'user/runners' @{
            runner_type = 'group_type'; group_id = $group[0].id
            description = 'GPUFlow local Windows PowerShell 5.1 protected builds'
            tag_list = @('windows-powershell51'); run_untagged = $false
            access_level = 'ref_protected'; maximum_timeout = 7200
        }
        if (-not $created.token -or -not $created.id) { throw 'GitLab did not return a complete new runner credential.' }
        $secureToken = ConvertTo-SecureString -String $created.token -AsPlainText -Force
        $saved = New-Object Management.Automation.PSCredential([string]$created.id, $secureToken)
        $saved | Export-Clixml -LiteralPath $runnerCredentialFile
        $created = $null
        $saved = $null
        $secureToken = $null
    }
    $runnerCredential = Import-Clixml -LiteralPath $runnerCredentialFile
    $runnerId = $runnerCredential.UserName
    if ($runnerId -notmatch '^[0-9]+$') { throw 'Invalid stored Windows Runner identity.' }
    $serverRunner = Invoke-LocalApi 'GET' "runners/$runnerId"
    if ($serverRunner.runner_type -ne 'group_type' -or $serverRunner.access_level -ne 'ref_protected' -or
        $serverRunner.run_untagged -or 'windows-powershell51' -notin @($serverRunner.tag_list)) {
        throw 'Existing runner does not meet the protected Windows-only scheduling restrictions.'
    }
    $groupRunners = @(Invoke-LocalApi 'GET' "groups/$($group[0].id)/runners?type=group_type&per_page=100")
    if (-not ($groupRunners | Where-Object { [string]$_.id -eq $runnerId })) { throw 'Stored Windows Runner does not belong to the private gpuflow group.' }
    if (-not (Test-Path -LiteralPath $config -PathType Leaf)) {
        $previousToken = $env:CI_SERVER_TOKEN
        try {
            $env:CI_SERVER_TOKEN = $runnerCredential.GetNetworkCredential().Password
            Invoke-PrivateNative $binary @(
                'register', '--non-interactive', '--config', $config,
                '--url', 'http://127.0.0.1:8088', '--name', 'GPUFlow Windows PowerShell 5.1',
                '--executor', 'shell', '--shell', 'powershell', '--builds-dir', $builds, '--cache-dir', $cache,
                '--limit', '1', '--request-concurrency', '1', '--output-limit', '8192', '--debug-trace-disabled',
                # Windows PS5.1 deletes an env var when assigned an empty value.
                # Git COUNT must therefore reference only nonempty values.
                '--env', 'GIT_CONFIG_COUNT=2',
                '--env', 'GIT_CONFIG_KEY_0=http.curloptResolve', '--env', 'GIT_CONFIG_VALUE_0=gitlab.gpuflow.test:8088:127.0.0.1',
                '--env', 'GIT_CONFIG_KEY_1=core.longpaths', '--env', 'GIT_CONFIG_VALUE_1=true',
                '--env', 'HTTP_PROXY=', '--env', 'HTTPS_PROXY=', '--env', 'ALL_PROXY=', '--env', 'NO_PROXY=127.0.0.1,localhost,gitlab.gpuflow.test',
                '--env', 'GOMAXPROCS=2', '--env', 'GOFLAGS=-p=2'
            ) 'Windows Runner registration'
        }
        finally {
            if ($null -eq $previousToken) { Remove-Item Env:CI_SERVER_TOKEN -ErrorAction SilentlyContinue }
            else { $env:CI_SERVER_TOKEN = $previousToken }
        }
    }
    # config.toml necessarily contains Runner's token in plaintext. Its parent
    # ACL is private to this user and SYSTEM; never copy it into repositories.
    $configText = Get-Content -LiteralPath $config -Raw
    foreach ($required in @('(?m)^\s*concurrent\s*=\s*1\s*$', '(?m)^\s*executor\s*=\s*"shell"\s*$', '(?m)^\s*shell\s*=\s*"powershell"\s*$')) {
        if ($configText -notmatch $required) { throw 'Existing runner configuration is not the expected serial Windows PowerShell executor.' }
    }
    if ([regex]::Matches($configText, '(?m)^\s*\[\[runners\]\]\s*$').Count -ne 1) { throw 'Exactly one runner entry is permitted in this private config.' }
    if ($configText -notmatch '"GIT_CONFIG_COUNT=2"' -or $configText -match '"GIT_CONFIG_(?:KEY|VALUE)_[2-9][0-9]*=') {
        throw 'Existing Windows Runner Git environment needs the controlled update-windows-runner-git-env.ps1 migration before restart.'
    }
    $configText = $null
    Invoke-PrivateNative $binary @('verify', '--config', $config) 'Windows Runner server verification'
}
finally {
    $bootstrapCredential = $null
    $runnerCredential = $null
    $headers = $null
}

if (Test-Path -LiteralPath $processRecord -PathType Leaf) {
    $record = Get-Content -LiteralPath $processRecord -Raw | ConvertFrom-Json
    $existing = Get-Process -Id ([int]$record.pid) -ErrorAction SilentlyContinue
    if ($existing) {
        if ($existing.Path -eq $binary -and $existing.StartTime.ToUniversalTime().ToString('o') -eq $record.started_utc) {
            Write-Output ("Windows PowerShell 5.1 Runner is already running in hidden user mode (PID {0})." -f $existing.Id)
            exit 0
        }
        throw 'Recorded PID belongs to another process; no process was stopped or replaced.'
    }
}
$untrackedProcesses = @(Get-Process -Name 'gitlab-runner-windows-amd64' -ErrorAction SilentlyContinue | Where-Object { $_.Path -eq $binary })
if ($untrackedProcesses.Count -gt 0) { throw 'A runner from this private directory is already running without the matching PID record. Inspect it manually; no duplicate or stop was attempted.' }
$stamp = (Get-Date).ToUniversalTime().ToString('yyyyMMddTHHmmssZ') + '-' + [Guid]::NewGuid().ToString('N')
$stdout = Join-Path $logs ("runner-$stamp.stdout.log")
$stderr = Join-Path $logs ("runner-$stamp.stderr.log")
$oldProxies = @{}
foreach ($key in @('HTTP_PROXY', 'HTTPS_PROXY', 'ALL_PROXY', 'NO_PROXY')) { $oldProxies[$key] = [Environment]::GetEnvironmentVariable($key, 'Process') }
try {
    $env:HTTP_PROXY = ''; $env:HTTPS_PROXY = ''; $env:ALL_PROXY = ''
    $env:NO_PROXY = '127.0.0.1,localhost,gitlab.gpuflow.test'
    $started = Start-Process -FilePath $binary -ArgumentList @('run', '--config', ('"{0}"' -f $config), '--working-directory', ('"{0}"' -f $runnerRoot)) `
        -WorkingDirectory $runnerRoot -WindowStyle Hidden -RedirectStandardOutput $stdout -RedirectStandardError $stderr -PassThru
}
finally {
    foreach ($key in $oldProxies.Keys) { [Environment]::SetEnvironmentVariable($key, $oldProxies[$key], 'Process') }
}
Start-Sleep -Seconds 2
$started.Refresh()
if ($started.HasExited) { throw "Windows Runner exited early; inspect only the private logs under $logs." }
$record = @{ pid = $started.Id; started_utc = $started.StartTime.ToUniversalTime().ToString('o'); binary = $binary; config = $config; stdout = $stdout; stderr = $stderr }
[IO.File]::WriteAllText($processRecord, ($record | ConvertTo-Json), [Text.UTF8Encoding]::new($false))
Write-Output ("Windows PowerShell 5.1 Runner started hidden in user mode (PID {0}); config, credentials, cache and logs are private." -f $started.Id)
Write-Output 'No Windows service or logon task was installed. Rerun this script after a logout/reboot. Only trusted protected-ref jobs may use this host-user shell executor.'
