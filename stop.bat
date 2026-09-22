@echo off
chcp 65001 >nul
setlocal EnableDelayedExpansion

:: ==================================================================
:: SmartProxy STOP - kill gateway + dispatcher + release ports
:: Usage:
::   stop.bat              stop gateway + dispatcher (keep Redis)
::   stop.bat --redis      also stop Redis
:: ==================================================================

set "GW_NAME=smartproxy-gateway.exe"
set "DP_NAME=smartproxy-dispatcher.exe"
set "GW_PORT=8080"
set "DP_PORT=8081"
set "REDIS_PORT=6379"

if "%~1"=="--redis" (set "STOP_REDIS=1") else (set "STOP_REDIS=0")

echo.
echo ========== SmartProxy STOP ==========
echo.

:: Phase 1: kill by exe name
echo [1/3] Killing SmartProxy processes by exe name...
taskkill /F /IM %GW_NAME%  >nul 2>&1
taskkill /F /IM %DP_NAME%  >nul 2>&1
verify >nul
timeout /t 1 /nobreak >nul

:: Phase 2: kill any process still holding default ports
echo [2/3] Releasing default ports %GW_PORT% + %DP_PORT%...
for /f "tokens=5" %%a in ('netstat -ano 2^>nul ^| findstr ":%GW_PORT% " ^| findstr "LISTENING"') do (
    echo   :%GW_PORT%  held by PID %%a  - killing...
    taskkill /F /PID %%a >nul 2>&1
)
for /f "tokens=5" %%a in ('netstat -ano 2^>nul ^| findstr ":%DP_PORT% " ^| findstr "LISTENING"') do (
    echo   :%DP_PORT%  held by PID %%a  - killing...
    taskkill /F /PID %%a >nul 2>&1
)
verify >nul
timeout /t 1 /nobreak >nul

:: Phase 3: optional Redis
if "%STOP_REDIS%"=="1" (
    echo [3/3] Stopping Redis on :%REDIS_PORT%...
    for /f "tokens=5" %%a in ('netstat -ano 2^>nul ^| findstr ":%REDIS_PORT% " ^| findstr "LISTENING"') do (
        echo   :%REDIS_PORT%  held by PID %%a  - killing...
        taskkill /F /PID %%a >nul 2>&1
    )
    taskkill /F /IM redis-server.exe >nul 2>&1
) else (
    echo [3/3] Skipping Redis  (use "stop.bat --redis" to also kill Redis)
)
verify >nul
timeout /t 1 /nobreak >nul

:: Summary
echo.
echo ---------- Ports after stop ----------
netstat -ano 2>nul | findstr ":%GW_PORT% :%DP_PORT% :%REDIS_PORT%" | findstr "LISTENING" >nul
if errorlevel 1 (
    echo   :%GW_PORT%   FREE
    echo   :%DP_PORT%   FREE
    if "%STOP_REDIS%"=="1" (echo   :%REDIS_PORT%   FREE) else (echo   :%REDIS_PORT%   kept running)
) else (
    echo   WARNING: some ports still held!
    netstat -ano 2>nul | findstr ":%GW_PORT% :%DP_PORT% :%REDIS_PORT%" | findstr "LISTENING"
)
echo.
echo ========== DONE ==========
echo.

exit /b 0
