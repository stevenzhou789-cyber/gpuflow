[CmdletBinding()]
param(
    [string]$StateDirectory,
    [string]$ApiBase = 'http://127.0.0.1:8088/api/v4'
)
$ErrorActionPreference = 'Stop'
if (-not $StateDirectory) { $StateDirectory = Join-Path $PSScriptRoot '../../.local-gitlab' }
if ($ApiBase -ne 'http://127.0.0.1:8088/api/v4') { throw 'Bootstrap is restricted to this local GitLab instance.' }
$statePath = [IO.Path]::GetFullPath($StateDirectory)
$expectedStatePath = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '../../.local-gitlab'))
if ($statePath -ne $expectedStatePath) { throw 'Unexpected credential output directory.' }
New-Item -ItemType Directory -Path $statePath -Force | Out-Null
$taskAccount = [Security.Principal.WindowsIdentity]::GetCurrent().Name
& icacls.exe $statePath /inheritance:r /grant:r "${taskAccount}:(OI)(CI)F" 'SYSTEM:(OI)(CI)F' | Out-Null
if ($LASTEXITCODE -ne 0) { throw 'Cannot protect local credential directory.' }

function Assert-Native([string]$Label) {
    if ($LASTEXITCODE -ne 0) { throw "$Label failed with exit code $LASTEXITCODE" }
}
function Protect-LocalCredential([string]$UserName, [string]$Value, [string]$Name) {
    $secureValue = ConvertTo-SecureString -String $Value -AsPlainText -Force
    $credential = New-Object Management.Automation.PSCredential($UserName, $secureValue)
    $credential | Export-Clixml -LiteralPath (Join-Path $statePath $Name)
}

$passwordFile = Join-Path $statePath 'admin-login.credential.xml'
if (-not (Test-Path -LiteralPath $passwordFile)) {
    $initialOutput = & docker exec gpuflow-gitlab cat /etc/gitlab/initial_root_password
    Assert-Native 'Read local initial admin credential'
    $passwordLine = @($initialOutput | Where-Object { $_ -match '^Password: ' })
    if ($passwordLine.Count -ne 1) { throw 'Initial admin password unavailable; do not reset it automatically.' }
    Protect-LocalCredential 'root' ($passwordLine[0].Substring(10)) 'admin-login.credential.xml'
    $initialOutput = $null
    $passwordLine = $null
}
& docker cp (Join-Path $PSScriptRoot 'bootstrap-token.rb') 'gpuflow-gitlab:/tmp/gpuflow-bootstrap-token.rb'
Assert-Native 'Copy local bootstrap program'
& docker exec gpuflow-gitlab gitlab-rails runner /tmp/gpuflow-bootstrap-token.rb
Assert-Native 'Create local bootstrap credential'
$apiToken = ((& docker exec gpuflow-gitlab cat /var/opt/gitlab/gitlab-rails/gpuflow-bootstrap-token) -join '').Trim()
Assert-Native 'Load bootstrap credential'
if (-not $apiToken) { throw 'Empty bootstrap credential.' }
Protect-LocalCredential 'root' $apiToken 'bootstrap-api.credential.xml'
& docker exec gpuflow-gitlab rm -- /var/opt/gitlab/gitlab-rails/gpuflow-bootstrap-token
Assert-Native 'Remove the temporary plaintext bootstrap credential'
$apiHeaders = @{ 'PRIVATE-TOKEN' = $apiToken; Host = 'gitlab.gpuflow.test:8088' }

function Invoke-LocalGitLab([string]$Method, [string]$Path, $Body = $null) {
    $parameters = @{ Method = $Method; Uri = "$ApiBase/$Path"; Headers = $apiHeaders; UseBasicParsing = $true }
    if ($null -ne $Body) {
        $parameters.ContentType = 'application/json'
        $parameters.Body = ConvertTo-Json -InputObject $Body -Depth 12 -Compress
    }
    Invoke-RestMethod @parameters
}

$settings = Invoke-LocalGitLab 'PUT' 'application/settings' @{
    signup_enabled = $false
    default_project_visibility = 'private'
    default_group_visibility = 'private'
    default_snippet_visibility = 'private'
    max_artifacts_size = 1024
    default_artifacts_expire_in = '3 days'
}
if ($settings.signup_enabled) { throw 'Public registration remains enabled.' }

$groups = @(Invoke-LocalGitLab 'GET' 'groups?search=gpuflow&per_page=100')
$group = @($groups | Where-Object { $_.full_path -eq 'gpuflow' })
if ($group.Count -eq 0) {
    $group = @(Invoke-LocalGitLab 'POST' 'groups' @{ name = 'GPUFlow'; path = 'gpuflow'; visibility = 'private' })
}
if ($group.Count -ne 1 -or $group[0].visibility -ne 'private') { throw 'Unexpected local group visibility or identity.' }
$groupId = $group[0].id
$projects = @()
foreach ($projectName in @('gpuflow', 'gpuflow-enterprise', 'gpuflow-license-issuer')) {
    $matches = @(Invoke-LocalGitLab 'GET' "groups/$groupId/projects?search=$projectName&per_page=100")
    $project = @($matches | Where-Object { $_.path_with_namespace -eq "gpuflow/$projectName" })
    if ($project.Count -eq 0) {
        $project = @(Invoke-LocalGitLab 'POST' 'projects' @{
            name = $projectName; path = $projectName; namespace_id = $groupId
            visibility = 'private'; initialize_with_readme = $false
            shared_runners_enabled = $false; auto_devops_enabled = $false
            build_timeout = 7200
        })
    }
    if ($project.Count -ne 1 -or $project[0].visibility -ne 'private') { throw "Unexpected project: $projectName" }
    $project = @(Invoke-LocalGitLab 'PUT' "projects/$($project[0].id)" @{
        builds_access_level = 'private'; keep_latest_artifact = $false
    })
    $projects += $project[0]
}
$community = $projects | Where-Object { $_.path -eq 'gpuflow' }
$enterprise = $projects | Where-Object { $_.path -eq 'gpuflow-enterprise' }
$allowlist = @(Invoke-LocalGitLab 'GET' "projects/$($community.id)/job_token_scope/allowlist?per_page=100")
if (-not ($allowlist | Where-Object { $_.id -eq $enterprise.id })) {
    Invoke-LocalGitLab 'POST' "projects/$($community.id)/job_token_scope/allowlist" @{ target_project_id = $enterprise.id } | Out-Null
}

$gitCredentialPath = Join-Path $statePath 'git-push.credential.xml'
if (-not (Test-Path -LiteralPath $gitCredentialPath)) {
    $currentUser = Invoke-LocalGitLab 'GET' 'user'
    $gitToken = Invoke-LocalGitLab 'POST' "users/$($currentUser.id)/personal_access_tokens" @{
        name = 'gpuflow-local-dual-push'
        scopes = @('read_repository', 'write_repository')
        expires_at = (Get-Date).AddDays(90).ToString('yyyy-MM-dd')
    }
    Protect-LocalCredential 'root' $gitToken.token 'git-push.credential.xml'
    $gitToken = $null
}
$gitCredential = Import-Clixml -LiteralPath $gitCredentialPath
$credentialInput = "protocol=http`nhost=gitlab.gpuflow.test:8088`nusername=root`npassword=$($gitCredential.GetNetworkCredential().Password)`n`n"
$credentialInput | & git -c credential.provider=generic credential approve
Assert-Native 'Store local Git credential in the existing Git credential manager'
$credentialInput = $null
$gitCredential = $null

$runnerCredentialPath = Join-Path $statePath 'runner.credential.xml'
if (-not (Test-Path -LiteralPath $runnerCredentialPath)) {
    $newRunner = Invoke-LocalGitLab 'POST' 'user/runners' @{
        runner_type = 'group_type'; group_id = $groupId
        description = 'GPUFlow local serial Docker builds'
        tag_list = @('linux-docker'); run_untagged = $false
        maximum_timeout = 7200; access_level = 'not_protected'
    }
    Protect-LocalCredential ([string]$newRunner.id) $newRunner.token 'runner.credential.xml'
    $newRunner = $null
}

$projects | Select-Object id,path_with_namespace,visibility,web_url,http_url_to_repo |
    Export-Clixml -LiteralPath (Join-Path $statePath 'projects.xml')
Write-Output 'Local projects verified private; credentials encrypted for the current Windows account.'
$projects | Select-Object id,path_with_namespace,visibility,web_url | Format-Table -AutoSize
$apiToken = $null
$apiHeaders = $null
