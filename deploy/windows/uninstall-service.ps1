# FileSync Windows service uninstaller. Run from an ELEVATED PowerShell.
$ErrorActionPreference = "Stop"

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "This script must run as Administrator (right-click PowerShell -> Run as administrator)."
    exit 1
}

$existing = Get-Service -Name FileSync -ErrorAction SilentlyContinue
if (-not $existing) {
    Write-Host "FileSync service is not installed."
    exit 0
}
Stop-Service -Name FileSync -Force -ErrorAction SilentlyContinue
sc.exe delete FileSync | Out-Null
Write-Host "FileSync service removed. (User data and config were left untouched.)"
