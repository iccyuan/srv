@echo off
setlocal
set "SRV_SCRIPT=%~dp0src\srv.py"
where python >nul 2>nul
if %ERRORLEVEL%==0 (
    python "%SRV_SCRIPT%" %*
    exit /b %ERRORLEVEL%
)
where py >nul 2>nul
if %ERRORLEVEL%==0 (
    py -3 "%SRV_SCRIPT%" %*
    exit /b %ERRORLEVEL%
)
echo error: neither `python` nor `py` found in PATH. >&2
exit /b 127
