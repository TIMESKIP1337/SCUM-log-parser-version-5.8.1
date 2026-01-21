@echo off
setlocal enabledelayedexpansion

:: ตั้งชื่อไฟล์
set OUTPUT_NAME=scumbot_logs.exe

echo [1/3] Cleaning old builds...
del /f %OUTPUT_NAME% 2>nul

echo [2/3] Building optimized...
go build -ldflags="-s -w" -trimpath -o %OUTPUT_NAME% ./cmd/logs-bot

if errorlevel 1 (
    echo [ERROR] Build failed!
    exit /b 1
)

echo.
echo Before compression:
for %%A in (%OUTPUT_NAME%) do echo   Size: %%~zA bytes

echo.
echo [3/3] Compressing with UPX...
where upx >nul 2>nul
if errorlevel 1 (
    echo [SKIP] UPX not found. Install from: https://github.com/upx/upx/releases
    echo        Or run: winget install upx
) else (
    upx --best --lzma %OUTPUT_NAME%
)

echo.
echo Build completed: %OUTPUT_NAME%
echo After compression:
for %%A in (%OUTPUT_NAME%) do echo   Size: %%~zA bytes
