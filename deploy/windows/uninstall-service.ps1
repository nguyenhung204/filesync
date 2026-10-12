# FileSync Windows service uninstaller. Run from an ELEVATED PowerShell.
$ErrorActionPreference = "Stop"

$principal = New-Object Security.Principal.WindowsPrincipal([Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $principal.IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
    Write-Error "Script này phải chạy với quyền Administrator (chuột phải vào PowerShell -> Run as administrator)."
    exit 1
}

$existing = Get-Service -Name FileSync -ErrorAction SilentlyContinue
if (-not $existing) {
    Write-Host "Service FileSync chưa được cài."
    exit 0
}
sc.exe stop FileSync | Out-Null
$stopExit = $LASTEXITCODE
# $stopExit is deliberately only read, not gated on: stop fails
# when the service is not running. The state poll below is the
# authoritative gate (same pattern as the Go uninstaller).
$lastStatus = "$($existing.Status)"
$deadline = (Get-Date).AddSeconds(15)
do {
    $svcNow = Get-Service -Name FileSync -ErrorAction SilentlyContinue
    if (-not $svcNow) { $lastStatus = "Stopped"; break }
    $lastStatus = "$($svcNow.Status)"
    if ($lastStatus -eq "Stopped") { break }
    Start-Sleep -Milliseconds 500
} while ((Get-Date) -lt $deadline)

$pendingStates = @("StartPending", "StopPending", "PausePending", "ContinuePending")
if (($lastStatus -ne "Stopped") -and ($pendingStates -contains $lastStatus)) {
    # Wedged in a pending state: the 0.2.0 service process never
    # speaks SCM, so it ignores stop forever. Kill exactly the
    # process sc.exe queryex attributes to this service, never
    # any other PID (same fallback as the Go uninstaller).
    $queryex = sc.exe queryex FileSync
    $queryexExit = $LASTEXITCODE
    $svcPid = 0
    if ($queryexExit -eq 0) {
        foreach ($line in $queryex) {
            if ($line -match "PID\s*:\s*(\d+)") { $svcPid = [int]$Matches[1] }
        }
    }
    if ($svcPid -gt 0) {
        Write-Host "Service FileSync đang treo ở trạng thái $lastStatus; đang tắt tiến trình của chính nó (PID $svcPid)."
        taskkill.exe /PID $svcPid /F | Out-Null
        if ($LASTEXITCODE -ne 0) { Write-Error "taskkill thất bại ($LASTEXITCODE)"; exit 1 }
        $deadline = (Get-Date).AddSeconds(5)
        do {
            $svcNow = Get-Service -Name FileSync -ErrorAction SilentlyContinue
            if (-not $svcNow) { $lastStatus = "Stopped"; break }
            $lastStatus = "$($svcNow.Status)"
            if ($lastStatus -eq "Stopped") { break }
            Start-Sleep -Milliseconds 500
        } while ((Get-Date) -lt $deadline)
    }
}

if ($lastStatus -ne "Stopped") {
    Write-Error "Service FileSync không dừng trong thời gian cho phép (trạng thái cuối: $lastStatus)."
    exit 1
}
sc.exe delete FileSync | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Error "sc.exe delete thất bại ($LASTEXITCODE)"; exit 1 }

# Do not claim removal until the service is really gone.
$deadline = (Get-Date).AddSeconds(15)
$remaining = $existing
do {
    $remaining = Get-Service -Name FileSync -ErrorAction SilentlyContinue
    if (-not $remaining) { break }
    Start-Sleep -Milliseconds 500
} while ((Get-Date) -lt $deadline)

if ($remaining) {
    Write-Error "Service FileSync vẫn còn sau 15 giây kể từ khi sc.exe delete (trạng thái: $($remaining.Status))."
    exit 1
}

# Remove the firewall rules the installer added (job 009
# addendum). Best-effort: a rule already deleted by hand is not
# an error.
foreach ($name in @("FileSync (TCP 22000)", "FileSync (UDP 5353)")) {
    netsh.exe advfirewall firewall delete rule name="$name" | Out-Null
}

Write-Host "Đã gỡ service FileSync. (Dữ liệu người dùng và cấu hình được giữ nguyên.)"
