@echo off
REM =========================================================================
REM SmartProxy - one-click runner (place in project root)
REM Usage: run-all.bat [start|stop|restart|status|logs]
REM =========================================================================

setlocal EnableDelayedExpansion
cd /d "%~dp0"

if "%~1"=="" (set "ACTION=start") else (set "ACTION=%~1")
set "ROOT=%cd%"
set "DIST=%ROOT%\dist"
set "RUN=%ROOT%\.run"

if not exist "%RUN%" mkdir "%RUN%"

REM Redis path candidates
set "REDIS_EXE="
if exist "D:\agent\other1\redis-tmp\Redis-7.4.4-Windows-x64-msys2\redis-server.exe" set "REDIS_EXE=D:\agent\other1\redis-tmp\Redis-7.4.4-Windows-x64-msys2\redis-server.exe"
if "%REDIS_EXE%"=="" if exist "C:\Redis\redis-server.exe" set "REDIS_EXE=C:\Redis\redis-server.exe"

set "REDIS_PORT=6379"
set "GW_PORT=8080"
set "DP_PORT=8081"
set "GW_NAME=smartproxy-gateway.exe"
set "DP_NAME=smartproxy-dispatcher.exe"

if "%ACTION%"=="start"   goto :do_start
if "%ACTION%"=="stop"    goto :do_stop
if "%ACTION%"=="restart" goto :do_restart
if "%ACTION%"=="status"  goto :do_status
if "%ACTION%"=="logs"    goto :do_logs
echo Usage: run-all.bat [start^|stop^|restart^|status^|logs]
exit /b 1

REM ==================================================================
REM START
REM ==================================================================
:do_start
echo.
echo ========== SmartProxy START ==========
echo.

if not exist "%DIST%\%GW_NAME%" (
    echo [STEP] Binaries not found, building...
    call "%ROOT%\scripts\build.bat" all
    if !errorlevel! neq 0 ( echo [FAIL] Build failed & exit /b 1 )
)
echo [OK] Binaries ready

if not exist "%ROOT%\.env" (
    if exist "%ROOT%\.env.example" (
        copy "%ROOT%\.env.example" "%ROOT%\.env" >nul
        echo [WARN] Generated .env from .env.example
    )
)

REM --- kill stale processes ---
echo [STEP] Killing stale processes...
taskkill /F /IM %GW_NAME%  >nul 2>&1
taskkill /F /IM %DP_NAME%  >nul 2>&1
verify >nul
timeout /t 1 /nobreak >nul

REM --- Redis ---
netstat -ano 2>nul | findstr ":%REDIS_PORT% " | findstr "LISTENING" >nul
if %errorlevel%==0 (
    echo [OK] Redis already on port %REDIS_PORT%
    verify >nul
    goto :redis_done
)
verify >nul

if "%REDIS_EXE%"=="" (
    echo [FAIL] redis-server.exe not found.
    echo        Put it at D:\agent\other1\redis-tmp\ or: docker run -p 6379:6379 redis:7-alpine
    exit /b 1
)
echo [STEP] Starting Redis...
start /B "" "%REDIS_EXE%" --port %REDIS_PORT% --save "" --appendonly no > "%RUN%\redis.log" 2> "%RUN%\redis-err.log"
set /a r=0
:wait_r
netstat -ano 2>nul | findstr ":%REDIS_PORT% " | findstr "LISTENING" >nul
if !errorlevel!==0 goto :r_ok
verify >nul
set /a r+=1
if !r! geq 20 ( echo [FAIL] Redis timeout & exit /b 1 )
timeout /t 0 /nobreak >nul
goto :wait_r
:r_ok
echo [OK] Redis started
:redis_done

REM --- Gateway ---
echo [STEP] Starting Gateway :%GW_PORT%...
start /B "" "%DIST%\%GW_NAME%" > "%RUN%\gateway.log" 2> "%RUN%\gateway-err.log"
set /a r=0
:wait_g
netstat -ano 2>nul | findstr ":%GW_PORT% " | findstr "LISTENING" >nul
if !errorlevel!==0 goto :g_ok
verify >nul
set /a r+=1
if !r! geq 40 ( echo [FAIL] Gateway timeout ^- Log: %RUN%\gateway-err.log & exit /b 1 )
timeout /t 0 /nobreak >nul
goto :wait_g
:g_ok
echo [OK] Gateway started on :%GW_PORT%

REM --- Dispatcher ---
echo [STEP] Starting Dispatcher :%DP_PORT%...
start /B "" "%DIST%\%DP_NAME%" > "%RUN%\dispatcher.log" 2> "%RUN%\dispatcher-err.log"
set /a r=0
:wait_d
netstat -ano 2>nul | findstr ":%DP_PORT% " | findstr "LISTENING" >nul
if !errorlevel!==0 goto :d_ok
verify >nul
set /a r+=1
if !r! geq 40 ( echo [WARN] Dispatcher timeout ^- check %RUN%\dispatcher-err.log & goto :d_done )
timeout /t 0 /nobreak >nul
goto :wait_d
:d_ok
echo [OK] Dispatcher started on :%DP_PORT%
:d_done

echo.
echo ========== ALL UP ==========
echo   Frontend:   http://localhost:%GW_PORT%/
echo   Gateway:    http://localhost:%GW_PORT%
echo   Dispatcher: http://localhost:%DP_PORT%/metrics
echo   Redis:      localhost:%REDIS_PORT%
echo.
echo   stop:   run-all.bat stop
echo   status: run-all.bat status
echo   logs:   type .run\gateway.log
echo.
echo [STEP] Opening browser...
start "" "http://localhost:%GW_PORT%/"
timeout /t 1 /nobreak >nul
start "" "http://localhost:%GW_PORT%/metrics"
timeout /t 1 /nobreak >nul
start "" "http://localhost:%DP_PORT%/metrics"
echo [OK] Browser tabs opened
exit /b 0

REM ==================================================================
REM STOP
REM ==================================================================
:do_stop
echo.
echo ========== SmartProxy STOP ==========
taskkill /F /IM %DP_NAME%  >nul 2>&1
taskkill /F /IM %GW_NAME%  >nul 2>&1
verify >nul
timeout /t 1 /nobreak >nul
tasklist /fi "imagename eq %GW_NAME%" 2>nul | findstr "%GW_NAME%" >nul
if !errorlevel!==0 (
    echo [WARN] %GW_NAME% still alive ^- killing again
    taskkill /F /IM %GW_NAME% >nul 2>&1
)
echo [OK] All app processes stopped
echo.
echo Note: Redis is NOT auto-killed (may be used by other tools).
exit /b 0

REM ==================================================================
REM RESTART
REM ==================================================================
:do_restart
call "%~f0" stop
timeout /t 1 /nobreak >nul
call "%~f0" start
exit /b %errorlevel%

REM ==================================================================
REM STATUS
REM ==================================================================
:do_status
echo.
echo ========== SmartProxy STATUS ==========
echo.
for %%p in (%REDIS_PORT% %GW_PORT% %DP_PORT%) do (
    set "found=0"
    for /f "tokens=5" %%a in ('netstat -ano 2^>nul ^| findstr ":%%p " ^| findstr "LISTENING"') do (
        set "found=1"
        echo   PORT %%p  RUNNING  PID=%%a
    )
    if "!found!"=="0" echo   PORT %%p  STOPPED
)
echo.
exit /b 0

REM ==================================================================
REM LOGS
REM ==================================================================
:do_logs
echo.
echo ========== Log Files ==========
for %%f in ("%RUN%\redis.log" "%RUN%\redis-err.log" "%RUN%\gateway.log" "%RUN%\gateway-err.log" "%RUN%\dispatcher.log" "%RUN%\dispatcher-err.log") do (
    if exist %%f ( echo   %%f  [%%~za bytes] )
)
echo.
exit /b 0
