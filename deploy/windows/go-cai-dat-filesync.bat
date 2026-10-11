@echo off
chcp 65001 >nul
title FileSync - Go cai dat
setlocal

rem ============================================================
rem  FileSync - go Windows service bang 1 cu double-click
rem  Dat file nay CUNG THU MUC voi uninstall-service.ps1
rem  Chi go service; cau hinh + du lieu dong bo KHONG bi xoa.
rem ============================================================

set "PS1=%~dp0uninstall-service.ps1"

if not exist "%PS1%" (
  echo [LOI] Khong thay uninstall-service.ps1 cung thu muc.
  pause
  exit /b 1
)

echo Dang go Windows service FileSync (se co hop thoai xin quyen admin, chon Yes)...
powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process powershell -Verb RunAs -Wait -ArgumentList '-NoProfile -ExecutionPolicy Bypass -File \"%PS1%\"'"

sc query FileSync >nul 2>&1
if not errorlevel 1 (
  echo [LOI] Service FileSync van con - buoc go bang quyen admin da that bai hoac bi huy.
  echo Hay chay lai file nay va chon Yes o hop thoai xin quyen admin.
  pause
  exit /b 1
)

echo.
echo Da go service. Cau hinh va du lieu dong bo van duoc giu nguyen.
echo Muon xoa han, xoa thu muc %LOCALAPPDATA%\Programs\filesync va thu muc cau hinh cua FileSync.
pause
