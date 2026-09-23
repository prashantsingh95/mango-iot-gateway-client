@echo off
REM ==========================================================================
REM Mango IoT Gateway Agent - One-click installer for Windows
REM No Docker needed (native Go binary). Auto-run on reboot via Task Scheduler.
REM
REM Usage: double-click install.bat, or:
REM   install.bat --server mqtts://xxx.s1.eu.hivemq.cloud:8883 --mqtt-user u --mqtt-pass p --token T --platform-url https://api.example.com --device-id gw-01
REM ==========================================================================
setlocal EnableDelayedExpansion

echo === Mango Gateway Agent installer (Windows) ===
cd /d %~dp0

REM --- 1. Go check (for source build) ---
where go >nul 2>nul
if errorlevel 1 (
  echo [!] Go not found - installing via winget...
  where winget >nul 2>nul
  if errorlevel 1 (
    echo [X] Install Go manually: https://go.dev/dl/  (or place gateway-agent.exe next to this file + set SKIP_BUILD=1)
    pause
    exit /b 1
  )
  winget install -e --id Go.Go --accept-source-agreements --accept-package-agreements
  echo [!] Go installed. Re-open terminal and re-run install.bat
  pause
  exit /b 0
)
for /f "tokens=3" %%v in ('go version') do echo [OK] Go %%v

REM --- 2. Parse simple args ---
set SERVER=
set MQTT_USER=
set MQTT_PASS=
set TOKEN=
set PLATFORM_URL=
set DEVICE_ID=
:parse
if "%~1"=="" goto :done_parse
if "%~1"=="--server" ( set SERVER=%~2 & shift & shift & goto :parse )
if "%~1"=="--mqtt-user" ( set MQTT_USER=%~2 & shift & shift & goto :parse )
if "%~1"=="--mqtt-pass" ( set MQTT_PASS=%~2 & shift & shift & goto :parse )
if "%~1"=="--token" ( set TOKEN=%~2 & shift & shift & goto :parse )
if "%~1"=="--platform-url" ( set PLATFORM_URL=%~2 & shift & shift & goto :parse )
if "%~1"=="--device-id" ( set DEVICE_ID=%~2 & shift & shift & goto :parse )
shift
goto :parse
:done_parse

REM --- 3. Build binary ---
set BIN=%~dp0gateway-agent.exe
if defined SKIP_BUILD (
  echo [i] SKIP_BUILD set - expecting %BIN% to exist
) else (
  echo [i] Building gateway-agent.exe...
  go mod download
  go build -ldflags="-s -w" -o "%BIN%" .
  if errorlevel 1 ( echo [X] Build failed & pause & exit /b 1 )
  echo [OK] Built %BIN%
)

REM --- 4. Config (prompt missing) ---
if "%SERVER%"=="" set /p SERVER="  MQTT broker URL [mqtts://xxx.s1.eu.hivemq.cloud:8883]: "
if "%SERVER%"=="" ( echo [X] Server URL required & pause & exit /b 1 )
if "%MQTT_USER%"=="" set /p MQTT_USER="  MQTT username (optional): "
if "%MQTT_PASS%"=="" set /p MQTT_PASS="  MQTT password (optional): "
if "%TOKEN%"=="" set /p TOKEN="  Provisioning token (optional): "
if "%PLATFORM_URL%"=="" set /p PLATFORM_URL="  Platform API URL [http://localhost:3001]: "
if "%PLATFORM_URL%"=="" set PLATFORM_URL=http://localhost:3001
if "%DEVICE_ID%"=="" (
  for /f %%m in ('getmac /fo csv /nh ^| findstr /r "[0-9A-F-]*"') do set RAWMAC=%%m
  set DEVICE_ID=gw-%COMPUTERNAME%
  set /p DEVICE_ID="  Device ID [%DEVICE_ID%]: "
)

set CFGDIR=%ProgramData%\mango-gateway
mkdir "%CFGDIR%" >nul 2>nul
set CFG=%CFGDIR%\config.yml
if not exist "%CFG%" (
  if exist "%~dp0config.yml" (
    copy "%~dp0config.yml" "%CFG%" >nul
    echo [i] Copied template config.yml - patching broker/device values...
    powershell -NoProfile -Command "(Get-Content '%CFG%') -replace 'broker_url:.*','broker_url: \"%SERVER%\"' -replace 'device_id:.*','device_id: \"%DEVICE_ID%\"' | Set-Content '%CFG%'"
  ) else (
    (
      echo gateway:
      echo   device_id: "%DEVICE_ID%"
      echo   name: "%COMPUTERNAME%"
      echo   provision_token: "%TOKEN%"
      echo   platform_url: "%PLATFORM_URL%"
      echo mqtt:
      echo   broker_url: "%SERVER%"
      echo   username: "%MQTT_USER%"
      echo   password: "%MQTT_PASS%"
      echo   qos: 1
      echo   keep_alive: 60
      echo   clean_session: false
    ) > "%CFG%"
  )
)
echo [OK] Config: %CFG%

REM --- 5. Auto-run on reboot (Scheduled Task, SYSTEM, on start) ---
echo [i] Creating scheduled task MangoGatewayAgent (auto-run on boot)...
schtasks /query /tn MangoGatewayAgent >nul 2>nul
if not errorlevel 1 schtasks /delete /tn MangoGatewayAgent /f >nul
schtasks /create /tn MangoGatewayAgent /tr "\"%BIN%\" --config \"%CFG%\"" /sc onstart /ru SYSTEM /rl highest /f
if errorlevel 1 ( echo [X] Task creation failed - run as Administrator & pause & exit /b 1 )
echo [OK] Task MangoGatewayAgent created (Restart on failure via Settings)

REM --- 6. Start now ---
echo [i] Starting agent...
schtasks /run /tn MangoGatewayAgent >nul 2>nul
start "" "%BIN%" --config "%CFG%"
echo.
echo [OK] Done. Reboot-safe: Task Scheduler starts agent on boot.
echo Config: %CFG%
echo Logs:   check agent output / Event Viewer
pause
