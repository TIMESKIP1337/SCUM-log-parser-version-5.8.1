@echo off
title scumbot_logs Auto Restart
color 0A

REM Allow X.509 certs with negative serial number (some FTPS hosts use self-signed certs that violate RFC 5280)
set GODEBUG=x509negativeserial=1

:start
cls
echo ===============================================
echo Starting Discord Bot at %date% %time%
echo ===============================================
echo.

scumbot_logs.exe

echo.
echo ===============================================
echo Bot stopped at %date% %time%
echo Restarting in 10 seconds...
echo Press Ctrl+C to stop auto-restart
echo ===============================================
timeout /t 10 /nobreak > nul
goto start