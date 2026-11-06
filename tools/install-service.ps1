#Requires -Version 5.1
#Requires -RunAsAdministrator
<#
.SYNOPSIS
  Register SpawnPoint as an automatic-start Windows service.

.DESCRIPTION
  Installs dist\win64\spawnpoint.exe to Program Files and registers a service
  that runs as NT AUTHORITY\LocalService (not LocalSystem).
  The installer does not start the service unless -Start is supplied.
  Existing service registrations are never overwritten.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\tools\install-service.ps1

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\tools\install-service.ps1 -Start
#>
[CmdletBinding()]
param(
  [string]$SourceExe,
  [string]$InstallDir = "$env:ProgramFiles\SpawnPoint",
  [switch]$Start
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$serviceName = "SpawnPoint"
$root = Split-Path -Parent $PSScriptRoot

if (Get-Service -Name $serviceName -ErrorAction SilentlyContinue) {
  throw "Service '$serviceName' already exists. Refusing to overwrite an existing registration."
}
if (-not $SourceExe) {
  $SourceExe = Join-Path $root "dist\win64\spawnpoint.exe"
}
if (-not (Test-Path -LiteralPath $SourceExe -PathType Leaf)) {
  throw "SpawnPoint executable not found: $SourceExe. Run tools\dist.ps1 first."
}
$sourcePath = (Resolve-Path -LiteralPath $SourceExe).Path
if (-not [System.IO.Path]::IsPathRooted($InstallDir)) {
  throw "InstallDir must be an absolute path."
}

$destination = Join-Path $InstallDir "spawnpoint.exe"
$dataDir = Join-Path $env:ProgramData "SpawnPoint"
$logDir = Join-Path $dataDir "logs"

New-Item -ItemType Directory -Path $InstallDir, $dataDir, $logDir -Force | Out-Null
if (-not [string]::Equals($sourcePath, $destination, [System.StringComparison]::OrdinalIgnoreCase)) {
  Copy-Item -LiteralPath $sourcePath -Destination $destination -Force
}

# LocalService needs write access only to SpawnPoint's own data and logs.
& icacls.exe $dataDir /grant '*S-1-5-19:(OI)(CI)M' /T | Out-Null
if ($LASTEXITCODE -ne 0) { throw "Failed to grant LocalService access to $dataDir." }

# sc.exe has the unusual 'option= value' argument convention.
& sc.exe create $serviceName 'binPath=' ('"' + $destination + '"') 'start=' 'auto' 'obj=' 'NT AUTHORITY\LocalService' 'DisplayName=' 'SpawnPoint'
if ($LASTEXITCODE -ne 0) { throw "Failed to create $serviceName service." }

$serviceKey = "HKLM:\SYSTEM\CurrentControlSet\Services\$serviceName"
$serviceEnvironment = @(
  'SPAWNPOINT_HOST=127.0.0.1',
  'SPAWNPOINT_PORT=7527',
  ("SPAWNPOINT_DB_PATH=" + (Join-Path $dataDir "spawnpoint.db")),
  ("SPAWNPOINT_LOG_DIR=" + $logDir),
  'SPAWNPOINT_KILL_CHILDREN_ON_EXIT=1'
)
New-ItemProperty -Path $serviceKey -Name Environment -PropertyType MultiString -Value $serviceEnvironment -Force | Out-Null

& sc.exe failure $serviceName 'reset=' '86400' 'actions=' 'restart/5000/restart/10000/restart/30000'
if ($LASTEXITCODE -ne 0) { throw "Failed to configure service recovery." }
& sc.exe failureflag $serviceName '1'
if ($LASTEXITCODE -ne 0) { throw "Failed to configure service failure actions." }

if ($Start) {
  & sc.exe start $serviceName
  if ($LASTEXITCODE -ne 0) { throw "Service registered, but start failed. Check the Windows Event Log and $logDir." }
}

Write-Host "SpawnPoint service registered (account: NT AUTHORITY\LocalService)."
Write-Host "Executable: $destination"
Write-Host "Database: $(Join-Path $dataDir 'spawnpoint.db')"
if (-not $Start) {
  Write-Host "Start when ready: sc.exe start SpawnPoint"
}
