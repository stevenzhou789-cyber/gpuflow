[CmdletBinding()]
param()
$ErrorActionPreference = 'Stop'
$compose = Join-Path $PSScriptRoot 'compose.yaml'
$credentialPath = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../.local-gitlab/runner.credential.xml'))
if (-not (Test-Path -LiteralPath $credentialPath)) { throw 'Run bootstrap.ps1 first as the same Windows account.' }

& docker compose -f $compose up -d builder
if ($LASTEXITCODE -ne 0) { throw 'Cannot start isolated builder.' }
& docker compose -f $compose run --rm --no-deps --entrypoint sh runner -c 'test -s /etc/gitlab-runner/config.toml'
if ($LASTEXITCODE -ne 0) {
    $credential = Import-Clixml -LiteralPath $credentialPath
    $env:CI_SERVER_TOKEN = $credential.GetNetworkCredential().Password
    $previousPreference = $ErrorActionPreference
    try {
        $ErrorActionPreference = 'Continue'
        # The token travels through the process environment, not argv or source.
        $registration = @(& docker compose -f $compose run --rm --no-deps -e CI_SERVER_TOKEN runner register --non-interactive `
            --url 'http://gitlab.gpuflow.test:8088' --name 'GPUFlow local serial Docker builds' `
            --executor docker --docker-host 'tcp://builder:2375' --docker-image 'docker:29.3.1-cli' `
            --docker-extra-hosts 'gitlab.gpuflow.test:172.30.88.2' --docker-extra-hosts 'builder:172.30.88.3' `
            --docker-memory 1536m --docker-cpus 2 --docker-pull-policy if-not-present `
            --env 'DOCKER_HOST=tcp://builder:2375' --env 'DOCKER_TLS_CERTDIR=' `
            --env 'GOMAXPROCS=2' --env 'GOFLAGS=-p=2' --env 'GOMEMLIMIT=768MiB' `
            --limit 1 --request-concurrency 1 --output-limit 8192 --debug-trace-disabled 2>&1)
        $registrationExit = $LASTEXITCODE
        $ErrorActionPreference = $previousPreference
        if ($registrationExit -ne 0) {
            # Runner output can contain authentication data; keep it out of logs.
            throw 'Runner registration failed. Inspect locally without publishing credential-bearing output.'
        }
        Write-Output 'Runner registered; authentication output withheld.'
    }
    finally {
        $ErrorActionPreference = $previousPreference
        Remove-Item Env:\CI_SERVER_TOKEN -ErrorAction SilentlyContinue
        $credential = $null
        $registration = $null
    }
}
& docker compose -f $compose up -d runner
if ($LASTEXITCODE -ne 0) { throw 'Cannot start registered runner.' }
Write-Output 'Local serial runner started. Jobs use only the separate builder daemon.'
