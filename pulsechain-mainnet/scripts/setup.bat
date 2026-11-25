@echo off
setlocal EnableExtensions

REM Thin launcher so Windows users can double-click or run from cmd
REM without hitting PowerShell execution-policy errors.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0setup.ps1"
exit /b %ERRORLEVEL%
