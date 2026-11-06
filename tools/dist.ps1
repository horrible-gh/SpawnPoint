#Requires -Version 5.1
<#
.SYNOPSIS
  Build the Windows x64 SpawnPoint executable.

.DESCRIPTION
  Run this script from anywhere. It compiles a self-contained Windows x64
  executable to dist\win64\spawnpoint.exe by default.
  No Git tags, command-line version override, or cgo toolchain is required.

.PARAMETER OutDir
  Output directory, relative to the repository root unless absolute.
  Defaults to dist.

.EXAMPLE
  powershell -ExecutionPolicy Bypass -File .\tools\dist.ps1
#>
[CmdletBinding()]
param(
  [string]$OutDir = "dist"
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$root = Split-Path -Parent $PSScriptRoot
$originalEnv = @{}
foreach ($name in @("GOOS", "GOARCH", "CGO_ENABLED")) {
  $originalEnv[$name] = [Environment]::GetEnvironmentVariable($name, "Process")
}

Push-Location -LiteralPath $root
try {
  if (-not (Get-Command "go" -ErrorAction SilentlyContinue)) {
    throw "Go is not installed or is not on PATH. SpawnPoint requires Go 1.26 or later."
  }

  if ([System.IO.Path]::IsPathRooted($OutDir)) {
    $outputRoot = $OutDir
  }
  else {
    $outputRoot = Join-Path $root $OutDir
  }

  $targetDir = Join-Path $outputRoot "win64"
  New-Item -ItemType Directory -Path $targetDir -Force | Out-Null
  $exe = Join-Path $targetDir "spawnpoint.exe"

  $env:GOOS = "windows"
  $env:GOARCH = "amd64"
  $env:CGO_ENABLED = "0"

  Write-Host "Building SpawnPoint for Windows x64..."
  & go build -trimpath -buildvcs=false -o $exe ./cmd/spawnpoint
  if ($LASTEXITCODE -ne 0) {
    throw "SpawnPoint build failed (go exit code $LASTEXITCODE)."
  }

  $file = Get-Item -LiteralPath $exe -ErrorAction Stop
  $hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $exe).Hash.ToLowerInvariant()
  Write-Host ("{0} ({1} bytes, SHA256 {2})" -f $file.FullName, $file.Length, $hash)
}
finally {
  foreach ($name in @("GOOS", "GOARCH", "CGO_ENABLED")) {
    [Environment]::SetEnvironmentVariable($name, $originalEnv[$name], "Process")
  }
  Pop-Location
}
