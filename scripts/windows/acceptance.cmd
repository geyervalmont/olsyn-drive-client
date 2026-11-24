@echo off
rem Scripted acceptance of a dev build against the fake drive.
rem See docs\testing-windows.md. Synthetic data only; nothing outside %TEMP% is touched.
rem
rem   acceptance.cmd <folder holding olsyn-drive.exe and fakedrive-server.exe>
rem
rem Exit code: the number of failed checks.
setlocal EnableExtensions EnableDelayedExpansion
if "%~1"=="" (
  echo usage: %~nx0 ^<folder holding olsyn-drive.exe and fakedrive-server.exe^>
  exit /b 2
)
set "BIN=%~f1"
set "OD=%BIN%\olsyn-drive.exe"
set "FD=%BIN%\fakedrive-server.exe"
if not exist "%OD%" (echo missing %OD% & exit /b 2)
if not exist "%FD%" (echo missing %FD% & exit /b 2)
where curl >nul 2>&1 || (echo curl.exe is needed ^(it ships with Windows 11^) & exit /b 2)

set "WORK=%TEMP%\olsyn-acceptance"
if exist "%WORK%" rmdir /s /q "%WORK%"
mkdir "%WORK%\src\Exports\Sub" "%WORK%\src\Later" "%WORK%\src\Offline" "%WORK%\fd-data" || exit /b 2
set "OLSYN_DRIVE_HOME=%WORK%\home"
rem Over SSH (a network logon) Windows Credential Manager cannot store
rem anything: a dev build keeps the token in a file under the test home instead.
if defined SSH_CONNECTION set "OLSYN_DRIVE_TOKEN_STORE=file"
if defined SSH_CLIENT set "OLSYN_DRIVE_TOKEN_STORE=file"
set "SERVER=http://127.0.0.1:8787"
set "FAULTS=http://127.0.0.1:8788"
set "MARKER=OLSYN-ACCEPTANCE-CONTENT-MARKER"
set FAILS=0

echo Making synthetic files...
for /l %%i in (1,1,200) do echo synthetic file %%i %MARKER%> "%WORK%\src\Exports\f-%%i.txt"
for /l %%i in (1,1,20) do echo later file %%i> "%WORK%\src\Later\l-%%i.txt"
for /l %%i in (1,1,20) do echo offline file %%i> "%WORK%\src\Offline\o-%%i.txt"
(for /l %%i in (1,1,16) do @echo 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcde) > "%WORK%\big.tmp"
for /l %%i in (1,1,15) do (
  copy /b "%WORK%\big.tmp"+"%WORK%\big.tmp" "%WORK%\big2.tmp" >nul
  move /y "%WORK%\big2.tmp" "%WORK%\big.tmp" >nul
)
move /y "%WORK%\big.tmp" "%WORK%\src\Exports\Sub\big.bin" >nul

echo Starting the fake drive...
start "olsyn fake drive" /min "%FD%" --auto-approve --data "%WORK%\fd-data"
call :waitfor "curl -s -f -o nul %FAULTS%/stats" 15
call :check "fake drive answers" %ERRORLEVEL% 0

echo Starting the agent...
start "" "%OD%" agent --server %SERVER%
call :waitfor "call :quiet status" 15
call :check "agent answers" %ERRORLEVEL% 0

"%OD%" login --no-browser > "%WORK%\login.txt" 2>&1
call :check "sign in" %ERRORLEVEL% 0
"%OD%" status > "%WORK%\status.txt" 2>&1
findstr /c:"Account:" "%WORK%\status.txt" >nul
call :check "status shows the account" %ERRORLEVEL% 0

if exist "%ProgramFiles(x86)%\WinFsp\bin\winfsp-x64.dll" (
  call :waitfor "call :mounted" 30
  call :check "W: is mounted" !ERRORLEVEL! 0
  set N=0
  for /f %%n in ('dir /b /a-d "W:\Large folder" 2^>nul ^| find /c /v ""') do set N=%%n
  if "!N!"=="12000" (call :check "W:\Large folder lists 12000 files" 0 0) else (call :check "W:\Large folder lists 12000 files, got !N!" 1 0)
  echo %MARKER% saved through W:> "W:\Projects\Example Tower\acceptance-note.txt"
  call :check "write a file through W:" !ERRORLEVEL! 0
  call :waitfor "call :idle" 60
  call :check "the save reaches the drive" !ERRORLEVEL! 0
) else (
  echo SKIP  mount checks: WinFsp is not installed
)

"%OD%" send "%WORK%\src\Exports" --to /Projects --wait > "%WORK%\send1.txt" 2>&1
call :check "send a folder (201 files)" %ERRORLEVEL% 0
rem (a label with parentheses: the case that broke :check on the first run)
"%OD%" send "%WORK%\src\Exports" --to /Projects --wait > "%WORK%\send2.txt" 2>&1
call :check "send again" %ERRORLEVEL% 0
findstr /c:"0 sent, 201 already there" "%WORK%\send2.txt" >nul
call :check "the second send skips everything" %ERRORLEVEL% 0

curl -s -X POST "%FAULTS%/deploy?seconds=8" >nul
"%OD%" send "%WORK%\src\Later" --to /Projects --wait > "%WORK%\send3.txt" 2>&1
call :check "send during a deploy waits and finishes" %ERRORLEVEL% 0

curl -s -X POST "%FAULTS%/offline?on=1" >nul
start "" /b cmd /c "ping -n 5 127.0.0.1 >nul & curl -s -X POST %FAULTS%/offline?on=0 >nul"
"%OD%" send "%WORK%\src\Offline" --to /Projects --wait > "%WORK%\send4.txt" 2>&1
call :check "send while offline for 4 s waits and finishes" %ERRORLEVEL% 0

"%OD%" send "%WORK%\src\Later" --to "/Managed outputs" --wait > "%WORK%\send5.txt" 2>&1
call :check "send into a managed folder exits 1" %ERRORLEVEL% 1
findstr /c:"20 held" "%WORK%\send5.txt" >nul
call :check "the managed folder holds all 20" %ERRORLEVEL% 0

"%OD%" send "%WORK%\src\Later" --to "/Issued for construction" > "%WORK%\send6.txt" 2>&1
call :check "send into a sealed folder is refused" %ERRORLEVEL% 1

curl -s -X POST "%FAULTS%/revoke" >nul
call :waitfor "call :signedout" 60
call :check "a revoked token signs out" %ERRORLEVEL% 0

findstr /s /m /c:"odt_" "%OLSYN_DRIVE_HOME%\logs\*" >nul 2>&1
call :check "no token in the logs" %ERRORLEVEL% 1
findstr /s /m /c:"%MARKER%" "%OLSYN_DRIVE_HOME%\logs\*" >nul 2>&1
call :check "no file content in the logs" %ERRORLEVEL% 1
if exist "%OLSYN_DRIVE_HOME%\logs\agent.log" (call :check "agent.log exists" 0 0) else (call :check "agent.log exists" 1 0)

echo Stopping...
"%OD%" quit >nul 2>&1
taskkill /f /fi "WINDOWTITLE eq olsyn fake drive*" >nul 2>&1
taskkill /f /im fakedrive-server.exe >nul 2>&1
ping -n 3 127.0.0.1 >nul
rmdir /s /q "%WORK%" 2>nul
echo.
if %FAILS%==0 (echo All checks passed.) else (echo %FAILS% check^(s^) failed.)
exit /b %FAILS%

rem ---- helpers ----

rem :check <name> <got> <want>
rem The name is only ever expanded late (!CHECK_NAME!) and outside any
rem parenthesised block, so parentheses, ampersands or carets in it cannot
rem end a block or start a command.
:check
set "CHECK_NAME=%~1"
if not "%~2"=="%~3" goto :check_fail
echo PASS  !CHECK_NAME!
exit /b 0
:check_fail
echo FAIL  !CHECK_NAME! [exit %~2, wanted %~3]
set /a FAILS+=1
exit /b 0

rem :waitfor "<command>" <seconds>: runs the command once a second until it exits 0.
:waitfor
for /l %%s in (1,1,%~2) do (
  %~1 >nul 2>&1 && exit /b 0
  ping -n 2 127.0.0.1 >nul
)
exit /b 1

:quiet
"%OD%" %* >nul 2>&1
if errorlevel 3 exit /b 1
exit /b 0

:mounted
if exist W:\Projects\ exit /b 0
exit /b 1

:idle
"%OD%" status 2>nul | findstr /c:"Transfers: none" >nul
exit /b %ERRORLEVEL%

:signedout
"%OD%" status 2>nul | findstr /c:"Signed out" >nul
exit /b %ERRORLEVEL%
