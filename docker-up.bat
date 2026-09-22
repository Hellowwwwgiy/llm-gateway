@echo off
chcp 65001 >nul
setlocal EnableDelayedExpansion

:: ==================================================================
:: SmartProxy DOCKER UP  — one-click docker compose build + start
:: Prerequisite: Docker Desktop installed + running
:: Usage:        docker-up.bat
:: ==================================================================

cd /d "%~dp0"
set "COMPOSE=docker compose"

echo.
echo ========== SmartProxy DOCKER UP ==========
echo.

:: 1. Docker installed?
where docker >nul 2>&1
if errorlevel 1 (
    echo [FAIL] docker not found. Please install Docker Desktop first.
    echo        https://www.docker.com/products/docker-desktop/
    exit /b 1
)
echo [OK] docker found
docker --version 2>nul | findstr /r "Docker version" >nul
if errorlevel 1 (
    echo [FAIL] docker daemon not responding. Start Docker Desktop and try again.
    exit /b 1
)
echo [OK] docker daemon running

:: 2. docker compose available?
docker compose version >nul 2>&1
if errorlevel 1 (
    echo [FAIL] docker compose not available. Upgrade Docker Desktop to latest.
    exit /b 1
)
echo [OK] docker compose available

:: 3. .env present?
if not exist ".env" (
    if exist ".env.example" (
        echo [STEP] Copying .env.example -^> .env ...
        copy .env.example .env >nul
        echo        PLEASE edit .env and set OPENAI_API_KEY before the first run!
        echo        Press any key to continue (Ctrl+C to cancel)...
        pause >nul
    ) else (
        echo [FAIL] No .env or .env.example found.
        exit /b 1
    )
)
echo [OK] .env ready

:: 4. Build + up
echo.
echo [STEP] docker compose up -d --build
!COMPOSE! up -d --build
if errorlevel 1 (
    echo [FAIL] compose up failed. See errors above.
    exit /b 1
)

:: 5. Wait healthy
echo.
echo [STEP] Waiting for services to be healthy...
set "WAIT=0"
:wait_loop
!COMPOSE! ps --format json 2>nul | findstr /i "\"Health\":\"healthy\"" >nul 2>&1
if not errorlevel 1 (
    goto :healthy
)
set /a WAIT+=5
if !WAIT! geq 90 (
    echo [WARN] Health check timeout after 90s. Services may still be starting.
    goto :after_wait
)
timeout /t 5 /nobreak >nul
echo   ... waiting (!WAIT!s)
goto :wait_loop

:healthy
echo [OK] All services healthy

:after_wait

:: 6. Summary
echo.
echo ========== RUNNING ==========
!COMPOSE! ps
echo.
echo ========== ACCESS ==========
echo   Frontend + Gateway:    http://localhost:8080/
echo   Gateway /metrics:      http://localhost:8080/metrics
echo   Dispatcher /metrics:   http://localhost:8081/metrics
echo   Redis:                 localhost:6379
echo.
echo ========== COMMANDS ==========
echo   docker-down.bat        ← stop
echo   docker logs -f smartproxy-gateway
echo   docker logs -f smartproxy-dispatcher
echo.

exit /b 0
