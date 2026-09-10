# Backward-compatible test entrypoint; all cases assert GitLab-only behavior.
$ErrorActionPreference = 'Stop'
& (Join-Path $PSScriptRoot 'test-push-gitlab.ps1')
exit $LASTEXITCODE
