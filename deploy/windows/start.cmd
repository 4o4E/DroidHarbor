@echo off
setlocal
pushd "%~dp0"
if errorlevel 1 exit /b 1
"%~dp0droidharbor.exe"
set "result=%errorlevel%"
if not "%result%"=="0" pause
popd
exit /b %result%
