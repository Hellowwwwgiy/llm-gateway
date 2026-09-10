@echo off
REM SmartProxy 本地快速启动（内存 fallback，不需要 Redis/RabbitMQ）
REM 依赖: 已执行 scripts\build.bat 且 gateway.exe 在 dist\

setlocal
cd /d "%~dp0\.."

if not exist "dist\smartproxy-gateway.exe" (
    echo [ERROR] dist\smartproxy-gateway.exe 不存在，请先运行 scripts\build.bat
    exit /b 1
)

if not exist ".env" (
    copy .env.example .env >nul
    echo [INFO] 已从 .env.example 生成 .env，记得填 OPENAI_API_KEY
)

REM --- 默认环境变量（指向项目内 mock LLM，你也可以改成真实 Key ---
if "%OPENAI_API_KEY%"==""    set "OPENAI_API_KEY=sk-mock"
if "%OPENAI_BASE_URL%"==""   set "OPENAI_BASE_URL=http://localhost:9999"
if "%OPENAI_MODEL%"==""     set "OPENAI_MODEL=mock-gpt"

echo ======================================================
echo  SmartProxy Gateway 启动中...
echo  默认 mock LLM: http://localhost:9999
echo  Gateway:       http://localhost:8080
echo  Metrics:       http://localhost:8080/metrics
echo ======================================================

dist\smartproxy-gateway.exe
