@echo off
setlocal
pushd "%~dp0"
if errorlevel 1 exit /b 1
if not exist "logs" mkdir "logs"
if errorlevel 1 goto failed
if not exist "data\transfers" mkdir "data\transfers"
if errorlevel 1 goto failed
"%~dp0droidharbor.exe" serve --log-file "logs\platform.jsonl" --transfer-temp-dir "data\transfers" %*
set "result=%errorlevel%"
popd
exit /b %result%
:failed
>&2 echo Failed to create runtime directories.
popd
exit /b 1
