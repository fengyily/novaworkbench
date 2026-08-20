# Cross-platform launcher for the NovaWorkbench single-binary release on
# Windows. Picks the .exe next to this script, ensures the data dir exists,
# forwards NOVA_PORT, and exec's into the server.
#
# Usage: .\start.ps1                      # default port 9527
#        $env:NOVA_PORT=9000; .\start.ps1
#        $env:CLAUDE_BIN="C:\tools\claude.exe"; .\start.ps1
$ErrorActionPreference = 'Stop'
Set-Location -Path (Split-Path -Parent $PSScriptRoot)

$arch = if ([System.Environment]::Is64BitOperatingSystem) { 'amd64' } else { '' }
$bin = "dist\novaworkbench-windows-$arch.exe"
if (-not (Test-Path $bin)) {
  Write-Error "No binary found at $bin. Build first: scripts\build-all.sh (or run from a Linux/macOS shell)."
  exit 1
}

$dataDir = Join-Path $env:USERPROFILE '.novaworkbench\data'
if (-not (Test-Path $dataDir)) { New-Item -ItemType Directory -Path $dataDir | Out-Null }

if (-not $env:NOVA_PORT) { $env:NOVA_PORT = '9527' }
Write-Host "NovaWorkbench starting on http://localhost:$($env:NOVA_PORT)"
Write-Host "Data dir: $dataDir"

& ".\$bin"
