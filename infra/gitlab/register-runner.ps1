[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
function Invoke-RunnerDocker {
    param([string[]]$DockerArguments)
    $previousPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        # Keep native stderr and registration output private, including failures.
        $output = @(& docker @DockerArguments 2>&1)
        $exitCode = $LASTEXITCODE
        return [pscustomobject]@{ ExitCode = $exitCode; Output = $output }
    }
    finally { $ErrorActionPreference = $previousPreference }
}

function Get-RunnerExecutionMode {
    param([string]$ComposePath)
    $listing = Invoke-RunnerDocker @('compose', '-f', $ComposePath, 'ps', '--all', '--quiet', 'runner')
    if ($listing.ExitCode -ne 0) { throw 'Cannot inspect the Compose runner; registration was not attempted.' }
    $ids = @($listing.Output | ForEach-Object { ([string]$_).Trim() } | Where-Object { $_ })
    if ($ids.Count -eq 0) { return 'run' }
    if ($ids.Count -ne 1 -or $ids[0] -notmatch '^[0-9a-f]{12,64}$') {
        throw 'Unexpected Compose runner inventory; registration was not attempted.'
    }
    $labels = Invoke-RunnerDocker @('inspect', '--format', '{{json .Config.Labels}}', $ids[0])
    $state = Invoke-RunnerDocker @('inspect', '--format', '{{.State.Status}}', $ids[0])
    if ($labels.ExitCode -ne 0 -or $state.ExitCode -ne 0) {
        throw 'Cannot verify runner ownership and state; registration was not attempted.'
    }
    try { $metadata = ($labels.Output -join "`n") | ConvertFrom-Json }
    catch { throw 'Invalid runner ownership metadata; registration was not attempted.' }
    if ($metadata.'com.docker.compose.project' -cne 'gpuflow-devops' -or
        $metadata.'com.docker.compose.service' -cne 'runner' -or
        $metadata.'com.docker.compose.oneoff' -ine 'False') {
        throw 'Runner does not belong to the expected Compose service; registration was not attempted.'
    }
    $status = ($state.Output -join '').Trim()
    if ($status -ceq 'running') { return 'exec' }
    if ($status -cin @('created', 'exited')) { return 'run' }
    throw 'Runner is not in a stable running or stopped state; registration was not attempted.'
}

function Get-RunnerConfigState {
    param([string]$ComposePath, [ValidateSet('run', 'exec')][string]$Mode)
    # A dedicated marker plus exit code distinguishes a genuinely absent file
    # from Docker/network errors. Empty, unreadable or non-regular files fail.
    $probe = 'if test -f /etc/gitlab-runner/config.toml && test -r /etc/gitlab-runner/config.toml && test -s /etc/gitlab-runner/config.toml; then echo GPUFLOW_RUNNER_CONFIG_PRESENT; exit 0; fi; if test ! -e /etc/gitlab-runner/config.toml && test ! -L /etc/gitlab-runner/config.toml; then echo GPUFLOW_RUNNER_CONFIG_MISSING; exit 42; fi; exit 43'
    if ($Mode -eq 'exec') {
        $result = Invoke-RunnerDocker @('compose', '-f', $ComposePath, 'exec', '-T', 'runner', 'sh', '-c', $probe)
    }
    else {
        $result = Invoke-RunnerDocker @('compose', '-f', $ComposePath, 'run', '--rm', '--no-deps', '--entrypoint', 'sh', 'runner', '-c', $probe)
    }
    $lines = @($result.Output | ForEach-Object { ([string]$_).Trim() })
    if ($result.ExitCode -eq 0 -and $lines -ccontains 'GPUFLOW_RUNNER_CONFIG_PRESENT') { return 'present' }
    if ($result.ExitCode -eq 42 -and $lines -ccontains 'GPUFLOW_RUNNER_CONFIG_MISSING') { return 'missing' }
    throw 'Cannot safely check runner configuration (Docker failure or invalid configuration); existing credentials were not changed.'
}

function Register-LocalRunner {
    param([string]$ComposePath, [string]$CredentialPath)
    $mode = Get-RunnerExecutionMode $ComposePath
    $builder = Invoke-RunnerDocker @('compose', '-f', $ComposePath, 'up', '-d', 'builder')
    if ($builder.ExitCode -ne 0) { throw 'Cannot start isolated builder.' }

    if ((Get-RunnerConfigState $ComposePath $mode) -eq 'missing') {
        if (-not (Test-Path -LiteralPath $CredentialPath)) { throw 'Run bootstrap.ps1 first as the same Windows account.' }
        $credential = Import-Clixml -LiteralPath $CredentialPath
        $previousToken = $env:CI_SERVER_TOKEN
        $registration = $null
        try {
            $env:CI_SERVER_TOKEN = $credential.GetNetworkCredential().Password
            # Reuse a running runner: a one-off container would inherit its .4
            # address and collide. The token is an environment name, not argv.
            $arguments = @('compose', '-f', $ComposePath)
            if ($mode -eq 'exec') { $arguments += @('exec', '-T', '-e', 'CI_SERVER_TOKEN', 'runner', 'gitlab-runner') }
            else { $arguments += @('run', '--rm', '--no-deps', '-e', 'CI_SERVER_TOKEN', 'runner') }
            $arguments += @('register', '--non-interactive',
                '--url', 'http://gitlab.gpuflow.test:8088', '--name', 'GPUFlow local serial Docker builds',
                '--executor', 'docker', '--docker-host', 'tcp://builder:2375', '--docker-image', 'docker:29.3.1-cli',
                '--docker-extra-hosts', 'gitlab.gpuflow.test:172.30.88.2', '--docker-extra-hosts', 'builder:172.30.88.3',
                '--docker-memory', '1536m', '--docker-cpus', '2', '--docker-pull-policy', 'if-not-present',
                '--env', 'DOCKER_HOST=tcp://builder:2375', '--env', 'DOCKER_TLS_CERTDIR=',
                '--env', 'GOMAXPROCS=2', '--env', 'GOFLAGS=-p=2', '--env', 'GOMEMLIMIT=768MiB',
                '--limit', '1', '--request-concurrency', '1', '--output-limit', '8192', '--debug-trace-disabled')
            $registration = Invoke-RunnerDocker $arguments
            if ($registration.ExitCode -ne 0) {
                throw 'Runner registration failed. Inspect locally without publishing credential-bearing output.'
            }
            Write-Output 'Runner registered; authentication output withheld.'
        }
        finally {
            if ($null -eq $previousToken) { Remove-Item Env:\CI_SERVER_TOKEN -ErrorAction SilentlyContinue }
            else { $env:CI_SERVER_TOKEN = $previousToken }
            $credential = $null
            $registration = $null
        }
    }
    $start = Invoke-RunnerDocker @('compose', '-f', $ComposePath, 'up', '-d', 'runner')
    if ($start.ExitCode -ne 0) { throw 'Cannot start registered runner.' }
    Write-Output 'Local serial runner started. Jobs use only the separate builder daemon.'
}

$compose = Join-Path $PSScriptRoot 'compose.yaml'
$credentialPath = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../.local-gitlab/runner.credential.xml'))
Register-LocalRunner -ComposePath $compose -CredentialPath $credentialPath
