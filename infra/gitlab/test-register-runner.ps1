[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'

# Load functions only: never run the real entrypoint or native Docker wrapper.
# Credentials below are synthetic in-memory fixtures; no filesystem is probed.
$tokens = $null
$errors = $null
$tree = [Management.Automation.Language.Parser]::ParseFile((Join-Path $PSScriptRoot 'register-runner.ps1'), [ref]$tokens, [ref]$errors)
if ($errors.Count) { throw $errors[0] }
foreach ($name in @('Get-RunnerExecutionMode', 'Get-RunnerConfigState', 'Register-LocalRunner')) {
    $definition = $tree.Find({
        param($node)
        $node -is [Management.Automation.Language.FunctionDefinitionAst] -and $node.Name -eq $name
    }, $true)
    if ($null -eq $definition) { throw "Missing test target: $name" }
    Invoke-Expression $definition.Extent.Text
}

$script:fixtureToken = 'synthetic-runner-credential-not-a-real-secret'
$script:fixtureCredentialPath = 'mock-only-runner-credential.xml'
$script:fixtureComposePath = 'mock-only-compose.yaml'
$script:containerId = 'abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789'

function New-MockResult {
    param([int]$ExitCode, [object[]]$Output = @())
    return [pscustomobject]@{ ExitCode = $ExitCode; Output = $Output }
}

function Test-Path {
    param([string]$LiteralPath)
    if ($LiteralPath -cne $script:fixtureCredentialPath) { throw 'Unexpected filesystem access.' }
    return $script:scenario.CredentialExists
}

function Import-Clixml {
    param([string]$LiteralPath)
    if ($LiteralPath -cne $script:fixtureCredentialPath) { throw 'Unexpected credential access.' }
    $script:credentialReads++
    $secure = ConvertTo-SecureString $script:fixtureToken -AsPlainText -Force
    return New-Object Management.Automation.PSCredential('fixture', $secure)
}

function Invoke-RunnerDocker {
    param([string[]]$DockerArguments)
    $script:calls.Add([string[]]$DockerArguments)
    if (($DockerArguments -join ' ').Contains($script:fixtureToken)) { throw 'Token leaked into Docker arguments.' }
    if ($DockerArguments[0] -eq 'inspect') {
        if ($script:scenario.InspectFail) { return New-MockResult 1 @('synthetic inspect error') }
        if ($DockerArguments[2] -eq '{{json .Config.Labels}}') {
            $project = 'gpuflow-devops'
            if ($script:scenario.WrongOwner) { $project = 'unrelated-project' }
            return New-MockResult 0 @((@{
                'com.docker.compose.project' = $project
                'com.docker.compose.service' = 'runner'
                'com.docker.compose.oneoff' = 'False'
            } | ConvertTo-Json -Compress))
        }
        if ($DockerArguments[2] -eq '{{.State.Status}}') { return New-MockResult 0 @($script:scenario.ContainerState) }
        throw 'Unexpected inspect command.'
    }
    if ($DockerArguments[0] -ne 'compose' -or $DockerArguments[2] -cne $script:fixtureComposePath) { throw 'Unexpected Docker target.' }
    $operation = $DockerArguments[3]
    if ($operation -eq 'ps') {
        if ($script:scenario.ListFail) { return New-MockResult 1 @('synthetic Docker daemon error') }
        if ($script:scenario.ContainerState -eq 'absent') { return New-MockResult 0 }
        return New-MockResult 0 @($script:containerId)
    }
    if ($operation -eq 'up') {
        if ($DockerArguments[-1] -eq 'builder' -and $script:scenario.BuilderFail) { return New-MockResult 1 }
        return New-MockResult 0
    }
    if ($operation -notin @('exec', 'run')) { throw 'Unexpected Compose operation.' }
    if ($operation -eq 'exec' -and $DockerArguments -notcontains '-T') { throw 'exec must disable TTY.' }
    if ($script:scenario.ContainerState -eq 'running' -and $operation -eq 'run') { throw 'One-off would collide with the running runner address.' }
    if ($DockerArguments -contains 'register') {
        $script:registrationCalls++
        $registerIndex = [Array]::IndexOf($DockerArguments, 'register')
        if ($operation -eq 'exec' -and $DockerArguments[$registerIndex - 1] -cne 'gitlab-runner') { throw 'exec registration must name the gitlab-runner executable.' }
        if ($operation -eq 'run' -and $DockerArguments[$registerIndex - 1] -cne 'runner') { throw 'one-off registration must preserve the image entrypoint.' }
        if ($env:CI_SERVER_TOKEN -cne $script:fixtureToken) { throw 'Registration did not receive the fixture token through environment.' }
        if ($DockerArguments -notcontains '-e' -or $DockerArguments -notcontains 'CI_SERVER_TOKEN') { throw 'Missing environment-only token forwarding.' }
        if ($script:scenario.RegisterFail) { return New-MockResult 1 @($script:fixtureToken) }
        return New-MockResult 0 @($script:fixtureToken)
    }
    if ($DockerArguments[-1] -notlike '*GPUFLOW_RUNNER_CONFIG_MISSING*') { throw 'Unexpected configuration probe.' }
    switch ($script:scenario.ConfigState) {
        'present' { return New-MockResult 0 @('GPUFLOW_RUNNER_CONFIG_PRESENT') }
        'missing' { return New-MockResult 42 @('GPUFLOW_RUNNER_CONFIG_MISSING') }
        'docker-error' { return New-MockResult 1 @('synthetic address already in use') }
        'empty' { return New-MockResult 43 }
        'unmarked-missing' { return New-MockResult 42 @('synthetic unrelated error') }
        'missing-marker-wrong-exit' { return New-MockResult 1 @('GPUFLOW_RUNNER_CONFIG_MISSING') }
        default { throw 'Unexpected mock configuration state.' }
    }
}

function Invoke-Scenario {
    param([string]$Name, [hashtable]$Overrides, [bool]$ExpectFailure, [int]$ExpectedRegistrations = 0)
    $script:scenario = @{
        ContainerState = 'running'; ConfigState = 'present'; CredentialExists = $true
        ListFail = $false; InspectFail = $false; WrongOwner = $false
        BuilderFail = $false; RegisterFail = $false
    }
    foreach ($key in $Overrides.Keys) { $script:scenario[$key] = $Overrides[$key] }
    $script:calls = New-Object 'Collections.Generic.List[object]'
    $script:credentialReads = 0
    $script:registrationCalls = 0
    $failure = $null
    $output = @()
    try { $output = @(Register-LocalRunner $script:fixtureComposePath $script:fixtureCredentialPath) }
    catch { $failure = $_ }
    if (($null -ne $failure) -ne $ExpectFailure) { throw "Unexpected success/failure: $Name ($failure)" }
    if ($script:registrationCalls -ne $ExpectedRegistrations -or $script:credentialReads -ne $ExpectedRegistrations) {
        throw "Unexpected registration or credential read: $Name"
    }
    if ((($output -join ' ') + [string]$failure).Contains($script:fixtureToken)) { throw "Credential output leaked: $Name" }
    if ($env:CI_SERVER_TOKEN -cne 'synthetic-preexisting-token') { throw "Prior environment was not restored: $Name" }
    if (-not $ExpectFailure) {
        $last = $script:calls[$script:calls.Count - 1]
        if ($last[3] -ne 'up' -or $last[-1] -ne 'runner') { throw "Successful runner was not started: $Name" }
    }
    elseif ($ExpectedRegistrations -eq 0) {
        foreach ($call in $script:calls) {
            if ($call -contains 'register' -or ($call[0] -eq 'compose' -and $call[3] -eq 'up' -and $call[-1] -eq 'runner')) {
                throw "Unsafe follow-up after failure: $Name"
            }
        }
    }
    Write-Output "PASS: $Name"
}

$previousToken = $env:CI_SERVER_TOKEN
try {
    $env:CI_SERVER_TOKEN = 'synthetic-preexisting-token'
    Invoke-Scenario 'running configured runner uses exec without reading a credential' @{ CredentialExists = $false } $false
    Invoke-Scenario 'stopped configured runner uses one-off without registration' @{ ContainerState = 'exited' } $false
    Invoke-Scenario 'absent configured runner uses one-off without registration' @{ ContainerState = 'absent' } $false
    Invoke-Scenario 'running runner with missing config registers through exec' @{ ConfigState = 'missing' } $false 1
    Invoke-Scenario 'absent runner with missing config registers through one-off' @{ ContainerState = 'absent'; ConfigState = 'missing' } $false 1
    Invoke-Scenario 'Docker inventory failure does not register' @{ ListFail = $true } $true
    Invoke-Scenario 'Docker inspect failure does not register' @{ InspectFail = $true } $true
    Invoke-Scenario 'foreign Compose ownership does not register' @{ WrongOwner = $true } $true
    Invoke-Scenario 'restarting runner does not register' @{ ContainerState = 'restarting' } $true
    Invoke-Scenario 'builder failure does not register' @{ BuilderFail = $true } $true
    Invoke-Scenario 'Docker probe failure does not register' @{ ConfigState = 'docker-error' } $true
    Invoke-Scenario 'empty or invalid config does not replace a token' @{ ConfigState = 'empty' } $true
    Invoke-Scenario 'exit 42 without a marker does not register' @{ ConfigState = 'unmarked-missing' } $true
    Invoke-Scenario 'missing marker without exit 42 does not register' @{ ConfigState = 'missing-marker-wrong-exit' } $true
    Invoke-Scenario 'missing credential fails without import' @{ ConfigState = 'missing'; CredentialExists = $false } $true
    Invoke-Scenario 'registration failure withholds token and restores environment' @{ ConfigState = 'missing'; RegisterFail = $true } $true 1
}
finally {
    if ($null -eq $previousToken) { Remove-Item Env:\CI_SERVER_TOKEN -ErrorAction SilentlyContinue }
    else { $env:CI_SERVER_TOKEN = $previousToken }
}
Write-Output 'PASS: 16 mocked Linux runner regressions; no Docker, network or real credential access.'
