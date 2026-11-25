@echo off
rem Windows launcher for scripts\setup.ps1: double-click it, or run
rem   setup.cmd [-Lan] [-Proxy] [-NoWait]
rem from a command prompt. Bypasses the PowerShell execution policy for this
rem script only (no system setting is changed).
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0scripts\setup.ps1" %*
set RC=%ERRORLEVEL%
echo %cmdcmdline% | find /i "%~0" >nul && pause
exit /b %RC%
