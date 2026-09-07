param([switch]$Run,[switch]$Silent)
$ErrorActionPreference='Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw 'Docker Engine is required for this Linux container target. No native Windows installer is produced.' }
$revision=git rev-parse --short=12 HEAD
if ($LASTEXITCODE -ne 0) { throw 'Cannot resolve source revision' }
$recorded=[DateTime]::UtcNow.ToString('yyyy-MM-ddTHH:mm:ssZ')
$env:VERSION="0.1.0-$revision"
$env:UPDATED_AT=$recorded
New-Item -ItemType Directory -Force work | Out-Null
@{sourceRevision=(git rev-parse HEAD);version=$env:VERSION;buildStartedAt=$recorded;status='building'} | ConvertTo-Json | Set-Content -Encoding utf8 work/build-receipt.json
docker compose build --pull
if ($LASTEXITCODE -ne 0) { throw 'Container build failed' }
@{sourceRevision=(git rev-parse HEAD);version=$env:VERSION;buildStartedAt=$recorded;status='built'} | ConvertTo-Json | Set-Content -Encoding utf8 work/build-receipt.json
if ($Run) { docker compose up -d; if ($LASTEXITCODE -ne 0) { throw 'Startup failed; see deployment documentation for first-run key and owner setup' } }
