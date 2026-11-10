@echo off
setlocal
pushd "%~dp0"
if errorlevel 1 exit /b 1
if not exist "data" mkdir "data"
if errorlevel 1 goto failed
icacls "data" /inheritance:r /grant:r "%USERDOMAIN%\%USERNAME%:(OI)(CI)F" "*S-1-5-18:(OI)(CI)F" "*S-1-5-32-544:(OI)(CI)F"
if errorlevel 1 goto failed
"%~dp0droidharbor.exe" admin-init --admin-file "data\admin.json"
set "result=%errorlevel%"
popd
exit /b %result%
:failed
>&2 echo Failed to initialize private data directory.
popd
exit /b 1
