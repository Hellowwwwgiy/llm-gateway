@echo off
REM =========================================================================
REM SmartProxy - Windows build script
REM Builds both gateway and dispatcher into dist/
REM =========================================================================
setlocal EnableDelayedExpansion

REM --- find Go ---
set "GOBIN="
where go >nul 2>&1
if %errorlevel%==0 (
    for /f "tokens=*" %%i in ('where go') do set "GOBIN=%%i"
) else if exist "D:\Go\bin\go.exe" (
    set "GOBIN=D:\Go\bin\go.exe"
    set "GOROOT=D:\Go"
) else if exist "C:\Go\bin\go.exe" (
    set "GOBIN=C:\Go\bin\go.exe"
    set "GOROOT=C:\Go"
) else (
    echo [ERROR] Go not found. Install Go 1.21+ first.
    exit /b 1
)

echo [INFO] Go: %GOBIN%
if defined GOROOT echo [INFO] GOROOT=%GOROOT%

cd /d "%~dp0\.."
set "TARGET=%~1"
if "%TARGET%"=="" set "TARGET=all"

echo [INFO] target=%TARGET%

if not exist "dist" mkdir dist

"%GOBIN%" clean -cache -testcache >nul 2>&1

set "FAIL=0"

REM --- gateway ---
if "%TARGET%"=="all" (
    echo [BUILD] gateway ...
    "%GOBIN%" build -trimpath -ldflags "-s -w" -o "dist\smartproxy-gateway.exe" "./cmd/gateway"
    if %errorlevel% neq 0 (
        echo [FAIL] gateway
        set "FAIL=1"
    ) else (
        echo [OK]   dist\smartproxy-gateway.exe
    )
) else if "%TARGET%"=="gateway" (
    echo [BUILD] gateway ...
    "%GOBIN%" build -trimpath -ldflags "-s -w" -o "dist\smartproxy-gateway.exe" "./cmd/gateway"
    if %errorlevel% neq 0 ( echo [FAIL] gateway & set "FAIL=1" ) else ( echo [OK] done )
)

REM --- dispatcher ---
if "%TARGET%"=="all" (
    echo [BUILD] dispatcher ...
    "%GOBIN%" build -trimpath -ldflags "-s -w" -o "dist\smartproxy-dispatcher.exe" "./cmd/dispatcher"
    if %errorlevel% neq 0 (
        echo [FAIL] dispatcher
        set "FAIL=1"
    ) else (
        echo [OK]   dist\smartproxy-dispatcher.exe
    )
) else if "%TARGET%"=="disp" (
    echo [BUILD] dispatcher ...
    "%GOBIN%" build -trimpath -ldflags "-s -w" -o "dist\smartproxy-dispatcher.exe" "./cmd/dispatcher"
    if %errorlevel% neq 0 ( echo [FAIL] dispatcher & set "FAIL=1" ) else ( echo [OK] done )
)

if %FAIL% neq 0 (
    echo.
    echo [FAIL] some targets failed
    exit /b 1
)

echo.
echo [DONE] artifacts in dist\:
dir /b dist\
echo.
echo [TIP] copy .env.example .env and set OPENAI_API_KEY before running
exit /b 0
