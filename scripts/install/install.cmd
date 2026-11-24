@echo off
:: Olsyn Drive installer for Windows (per user, no administrator rights).
:: It runs install.ps1: the copy next to this file if there is one, otherwise
:: the one the drive serves. Usage: install.cmd [-Server <url>] [-Channel stable^|beta] [-NoLaunch] [-Force]
:: Exit codes: 0 installed, 1 failed, 2 not supported on this PC.
setlocal
:: The drive server fills this in when it serves the file; the default is used when it did not.
set "DRIVE_BASE=__DRIVE_BASE__"
if /i not "%DRIVE_BASE:~0,4%"=="http" set "DRIVE_BASE=https://drive.olsyn.com"
if exist "%~dp0install.ps1" (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -File "%~dp0install.ps1" %*
) else (
  powershell.exe -NoProfile -ExecutionPolicy Bypass -Command "$ProgressPreference='SilentlyContinue'; [Net.ServicePointManager]::SecurityProtocol=[Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12; $env:OLSYN_DRIVE_SERVER='%DRIVE_BASE%'; try { $s=Invoke-RestMethod -UseBasicParsing '%DRIVE_BASE%/install.ps1' } catch { Write-Host ('Could not download the installer script: ' + $_.Exception.Message); exit 1 }; & ([scriptblock]::Create($s)) %*; exit $LASTEXITCODE"
)
exit /b %ERRORLEVEL%
