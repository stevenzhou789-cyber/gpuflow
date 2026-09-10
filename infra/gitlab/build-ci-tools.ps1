[CmdletBinding()]
param([string]$LocalStateDir)
$ErrorActionPreference = 'Stop'
if (-not $LocalStateDir) { $LocalStateDir = Join-Path $PSScriptRoot '../../.local-gitlab' }
$stateRoot = (Resolve-Path -LiteralPath $LocalStateDir).Path
$credentialPath = Join-Path $stateRoot 'bootstrap-api.credential.xml'
$definition = Join-Path $PSScriptRoot 'Dockerfile.ci-tools'
$image = 'gitlab.gpuflow.test:5055/gpuflow/gpuflow/ci-tools:full-20260910'
$builder = 'gpuflow-gitlab-builder'
# Only a pinned, known isolated daemon may be used. No host Docker socket.
$daemon = @(& docker inspect $builder --format '{{.Config.Image}}|{{range .Mounts}}{{.Destination}}|{{end}}')
if ($LASTEXITCODE -ne 0 -or $daemon.Count -ne 1 -or $daemon[0] -notmatch '^docker:29\.3\.1-dind\|' -or $daemon[0] -match '/var/run/docker.sock') {
    throw 'Expected the dedicated GPUFlow Docker-in-Docker builder, without host socket mounts.'
}
$context = '/var/tmp/gpuflow-ci-tools-' + [Guid]::NewGuid().ToString('N')
if ($context -notmatch '^/var/tmp/gpuflow-ci-tools-[a-f0-9]{32}$') { throw 'Unsafe temporary build context.' }
& docker exec $builder mkdir -m 700 $context
if ($LASTEXITCODE -ne 0) { throw 'Could not create isolated build context.' }
$credential = $null
try {
    & docker cp $definition "${builder}:$context/Dockerfile"
    if ($LASTEXITCODE -ne 0) { throw 'Copy of tool definition failed.' }
    & docker exec $builder docker build --network host --progress plain -f "$context/Dockerfile" -t $image $context
    if ($LASTEXITCODE -ne 0) { throw 'Full CI tool image failed integrity checks or build.' }
    # Registry credentials are introduced only after the image is built. They
    # never enter Dockerfile, image layers, source, printed argv or artifacts.
    $credential = Import-Clixml -LiteralPath $credentialPath
    $credential.GetNetworkCredential().Password | & docker exec -i -e "DOCKER_CONFIG=$context/auth" $builder docker login gitlab.gpuflow.test:5055 --username root --password-stdin
    if ($LASTEXITCODE -ne 0) { throw 'Local Registry authentication failed.' }
    & docker exec -e "DOCKER_CONFIG=$context/auth" $builder docker push $image
    if ($LASTEXITCODE -ne 0) { throw 'CI tools registry push failed.' }
    $digest = @(& docker exec $builder docker image inspect $image --format '{{index .RepoDigests 0}}')
    if ($LASTEXITCODE -ne 0 -or $digest.Count -ne 1 -or $digest[0] -notmatch '^gitlab\.gpuflow\.test:5055/gpuflow/gpuflow/ci-tools@sha256:[a-f0-9]{64}$') {
        throw 'Registry digest missing; do not update pipeline image references.'
    }
    Write-Output ('Verified CI tools reference: ' + $digest[0])
    Write-Output 'Pin this exact digest in both pipeline definitions before pushing them.'
}
finally {
    $credential = $null
    # This is only our validated throwaway Linux context/auth directory. Never
    # prune builder caches, data volumes, application images or other containers.
    & docker exec $builder rm -rf -- $context
    if ($LASTEXITCODE -ne 0) { Write-Warning 'Temporary context cleanup failed; inspect the dedicated builder privately.' }
}
