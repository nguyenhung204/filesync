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
if ($LASTEXITCODE -ne 0) { Write-Error "sc.exe delete failed ($LASTEXITCODE)"; exit 1 }

# Do not claim removal until the service is really gone.
$deadline = (Get-Date).AddSeconds(15)
$remaining = $existing
do {
    $remaining = Get-Service -Name FileSync -ErrorAction SilentlyContinue
    if (-not $remaining) { break }
    Start-Sleep -Milliseconds 500
} while ((Get-Date) -lt $deadline)

if ($remaining) {
    Write-Error "FileSync service still exists 15 seconds after sc.exe delete (status: $($remaining.Status))."
    exit 1
}

Write-Host "FileSync service removed. (User data and config were left untouched.)"
