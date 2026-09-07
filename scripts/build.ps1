param([switch]$Run,[switch]$Silent)
$ErrorActionPreference='Stop'
Set-Location (Split-Path $PSScriptRoot -Parent)
if (-not (Get-Command docker -ErrorAction SilentlyContinue)) { throw 'Docker Engine is required for this Linux container target. No native Windows installer is produced.' }
$revision=git rev-parse --short=12 HEAD
if ($LASTEXITCODE -ne 0) { throw 'Cannot resolve source revision' }
$recorded=git show -s --format=%cI HEAD
$env:VERSION="0.1.0-$revision"
$env:UPDATED_AT=$recorded
docker compose build --pull
if ($LASTEXITCODE -ne 0) { throw 'Container build failed' }
if ($Run) { docker compose up -d; if ($LASTEXITCODE -ne 0) { throw 'Startup failed; see deployment documentation for first-run key and owner setup' } }
