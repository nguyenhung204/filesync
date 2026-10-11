# FileSync Windows service installer (phase 1).
# Run from an ELEVATED PowerShell. Creates the FileSync service
# (auto-start, restart on failure) running `filesync.exe serve`.
# Usage:
#   .\install-service.ps1 [-BinaryPath C:\path\to\filesync.exe] [-ConfigDir C:\path\to\config]
param(
    [string]$BinaryPath = "$env:LOCALAPPDATA\Programs\filesync\filesync.exe",
    [string]$ConfigDir = ""
)

$ErrorActionPreference = "Stop"

# Admin check: sc.exe needs elevation; fail loudly and early.
$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "This script must run as Administrator (right-click PowerShell -> Run as administrator)."
    exit 1
}

if (-not (Test-Path $BinaryPath)) {
    Write-Error "filesync.exe not found at $BinaryPath — build/copy it first, or pass -BinaryPath."
    exit 1
}

if ($ConfigDir -eq "") {
    $ConfigDir = Join-Path $env:APPDATA "filesync"
}

$binPath = "`"$BinaryPath`" serve --config-dir `"$ConfigDir`""

$existing = Get-Service -Name FileSync -ErrorAction SilentlyContinue
if ($existing) {
    Write-Host "FileSync service already exists; stopping and reconfiguring it."
    Stop-Service -Name FileSync -Force -ErrorAction SilentlyContinue
    sc.exe delete FileSync | Out-Null
    Start-Sleep -Seconds 1
}

sc.exe create FileSync binPath= $binPath start= auto DisplayName= "FileSync"
if ($LASTEXITCODE -ne 0) { Write-Error "sc.exe create failed ($LASTEXITCODE)"; exit 1 }

sc.exe failure FileSync reset= 86400 actions= restart/5000 | Out-Null
sc.exe description FileSync "Two-way file sync without a cloud (LAN, phase 1)" | Out-Null
sc.exe start FileSync | Out-Null

Write-Host "FileSync service installed and started. Config dir: $ConfigDir"
Write-Host "Check it with: filesync --config-dir `"$ConfigDir`" status"
