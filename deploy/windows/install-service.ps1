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
    Write-Error "Script này phải chạy với quyền Administrator (chuột phải vào PowerShell -> Run as administrator)."
    exit 1
}

if (-not (Test-Path $BinaryPath)) {
    Write-Error "Không thấy filesync.exe tại $BinaryPath - hãy build/chép file trước, hoặc truyền -BinaryPath."
    exit 1
}

if ($ConfigDir -eq "") {
    $ConfigDir = Join-Path $env:APPDATA "filesync"
}

$binPath = "`"$BinaryPath`" serve --config-dir `"$ConfigDir`""

$existing = Get-Service -Name FileSync -ErrorAction SilentlyContinue
if ($existing) {
    Write-Host "Service FileSync đã tồn tại; đang dừng và cấu hình lại."
    sc.exe stop FileSync | Out-Null
    $stopExit = $LASTEXITCODE
    # $stopExit is deliberately only read, not gated on: stop fails
    # when the service is not running. The state poll below is the
    # authoritative gate (same pattern as the Go installer).
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
        # any other PID (same fallback as the Go installer).
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

    # Do not recreate until the old service is really gone.
    $deadline = (Get-Date).AddSeconds(15)
    do {
        $svcNow = Get-Service -Name FileSync -ErrorAction SilentlyContinue
        if (-not $svcNow) { break }
        Start-Sleep -Milliseconds 500
    } while ((Get-Date) -lt $deadline)
    if ($svcNow) { Write-Error "Service FileSync vẫn còn sau 15 giây kể từ khi sc.exe delete."; exit 1 }
}

sc.exe create FileSync binPath= $binPath start= auto DisplayName= "FileSync"
if ($LASTEXITCODE -ne 0) { Write-Error "sc.exe create thất bại ($LASTEXITCODE)"; exit 1 }

sc.exe failure FileSync reset= 86400 actions= restart/5000 | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Error "sc.exe failure thất bại ($LASTEXITCODE)"; exit 1 }

sc.exe description FileSync "Two-way file sync without a cloud (LAN, phase 1)" | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Error "sc.exe description thất bại ($LASTEXITCODE)"; exit 1 }

sc.exe start FileSync | Out-Null
if ($LASTEXITCODE -ne 0) { Write-Error "sc.exe start thất bại ($LASTEXITCODE)"; exit 1 }

# Do not claim success until the Service Control Manager reports RUNNING.
$deadline = (Get-Date).AddSeconds(15)
$lastStatus = "unknown"
do {
    $svc = Get-Service -Name FileSync -ErrorAction SilentlyContinue
    if ($svc) { $lastStatus = "$($svc.Status)" }
    if ($lastStatus -eq "Running") { break }
    Start-Sleep -Milliseconds 500
} while ((Get-Date) -lt $deadline)

if ($lastStatus -ne "Running") {
    Write-Error "Service FileSync không đạt trạng thái RUNNING trong 15 giây (trạng thái cuối: $lastStatus). Hãy chạy `"$BinaryPath`" serve trong console để xem log của daemon."
    exit 1
}

# Firewall (job 009 addendum): inbound rules for the daemon
# program only, Private profile only - sync (TCP 22000) and
# mDNS discovery (UDP 5353). Without them Windows Firewall can
# silently block inbound sync/discovery, especially for the
# service. Failures only warn: the service is already RUNNING.
foreach ($rule in @(
    @{ Name = "FileSync (TCP 22000)"; Protocol = "TCP"; Port = "22000" },
    @{ Name = "FileSync (UDP 5353)"; Protocol = "UDP"; Port = "5353" }
)) {
    netsh.exe advfirewall firewall delete rule name="$($rule.Name)" | Out-Null
    netsh.exe advfirewall firewall add rule name="$($rule.Name)" dir=in action=allow program="$BinaryPath" enable=yes profile=private protocol=$($rule.Protocol) localport=$($rule.Port) | Out-Null
    if ($LASTEXITCODE -ne 0) {
        Write-Host "CẢNH BÁO: không tạo được rule tường lửa '$($rule.Name)' (netsh exit $LASTEXITCODE). Nếu máy khác không thấy máy này, hãy cho phép filesync.exe trong Windows Firewall ở mạng Private."
    }
}

Write-Host "Service FileSync đã cài và đang RUNNING. Thư mục cấu hình: $ConfigDir"
Write-Host "Kiểm tra bằng lệnh: filesync --config-dir `"$ConfigDir`" status"
