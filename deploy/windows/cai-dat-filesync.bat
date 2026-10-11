@echo off
chcp 65001 >nul
title FileSync - Cai dat
setlocal

rem ============================================================
rem  FileSync - trinh cai dat bang 1 cu double-click
rem  Dat file nay CUNG THU MUC voi filesync.exe va install-service.ps1
rem  Viec no lam:
rem    1. Chep filesync.exe vao %LOCALAPPDATA%\Programs\filesync
rem    2. Chay init (tao cau hinh + dinh danh may, in device ID)
rem    3. Xin quyen admin de dang ky Windows service (tu chay khi mo may)
rem    4. Kiem tra trang thai daemon
rem ============================================================

set "SRC=%~dp0filesync.exe"
set "PS1=%~dp0install-service.ps1"
set "DST=%LOCALAPPDATA%\Programs\filesync"

if not exist "%SRC%" (
  echo [LOI] Khong thay filesync.exe cung thu muc voi file cai dat nay.
  echo Hay tai filesync.exe ve va dat cung cho roi chay lai.
  pause
  exit /b 1
)
if not exist "%PS1%" (
  echo [LOI] Khong thay install-service.ps1 cung thu muc.
  echo Hay tai install-service.ps1 ve va dat cung cho roi chay lai.
  pause
  exit /b 1
)

echo Dang chep filesync.exe vao %DST% ...
mkdir "%DST%" 2>nul
copy /Y "%SRC%" "%DST%\filesync.exe" >nul
if errorlevel 1 (
  echo [LOI] Chep file that bai. Kiem tra quyen ghi thu muc dich.
  pause
  exit /b 1
)

echo.
echo Dang khoi tao FileSync (chi tao moi lan dau, chay lai khong mat du lieu)...
"%DST%\filesync.exe" init

echo.
echo Dang dang ky Windows service (se co hop thoai xin quyen admin, chon Yes)...
powershell -NoProfile -ExecutionPolicy Bypass -Command "Start-Process powershell -Verb RunAs -Wait -ArgumentList '-NoProfile -ExecutionPolicy Bypass -File \"%PS1%\" -BinaryPath \"%DST%\filesync.exe\"'"

echo.
echo Dang kiem chung service FileSync...
sc query FileSync >nul 2>&1
if errorlevel 1 (
  echo [LOI] Service FileSync chua duoc tao - buoc dang ky bang quyen admin da that bai hoac bi huy.
  echo Hay chay lai file nay va chon Yes o hop thoai xin quyen admin.
  pause
  exit /b 1
)
sc query FileSync | findstr /C:"RUNNING" >nul
if errorlevel 1 (
  echo [LOI] Service FileSync chua chay - trang thai chua phai RUNNING.
  echo Hay mo PowerShell quyen admin va chay lenh sc start FileSync de xem ma loi cu the.
  pause
  exit /b 1
)

echo.
echo Kiem tra trang thai daemon:
"%DST%\filesync.exe" status

echo.
echo ============================================================
echo  Cai dat xong. FileSync se tu chay moi khi mo may.
echo  Xem trang thai bat ky luc nao bang lenh: filesync status
echo  (file exe nam tai %DST%)
echo ============================================================
pause
