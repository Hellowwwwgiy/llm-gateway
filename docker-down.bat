@echo off
chcp 65001 >nul
setlocal EnableDelayedExpansion

:: ==================================================================
:: SmartProxy DOCKER DOWN  — compose stop
:: Usage:
::   docker-down.bat          ← stop containers, keep volumes (Redis data)
::   docker-down.bat --clean  ← also remove volumes (fresh next run)
:: ==================================================================

cd /d "%~dp0"
set "COMPOSE=docker compose"
set "CLEAN=0"
if "%~1"=="--clean" set "CLEAN=1"

echo.
echo ========== SmartProxy DOCKER DOWN ==========
echo.

!COMPOSE! ps >nul 2>&1
if errorlevel 1 (
    echo [INFO] No compose project running in this dir.
    exit /b 0
)

if "%CLEAN%"=="1" (
    echo [STEP] docker compose down --volumes
    !COMPOSE! down --volumes
) else (
    echo [STEP] docker compose down
    !COMPOSE! down
)

if errorlevel 1 (
    echo [FAIL] compose down failed.
    exit /b 1
)

echo.
echo [OK] Stopped.
if "%CLEAN%"=="1" echo      (volumes removed)
echo.

exit /b 0
