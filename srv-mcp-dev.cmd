@echo off
REM ============================================================
REM  srv MCP dev wrapper -- DEV ONLY, do not point production
REM  MCP config at this (it rebuilds on every connect).
REM
REM  Committed to the repo: SRV_ROOT is derived from this script's
REM  own location (%~dp0), so it works from any clone path with no
REM  per-machine edits. The script must live at the repo root
REM  (sibling of the `go/` source dir and the built `srv.exe`).
REM
REM  Registered as the `srv` MCP server command so that a single
REM  `/mcp` reconnect in Claude Code rebuilds from source AND
REM  refreshes the daemon, with no manual kill / build steps.
REM
REM  Why it also stops the daemon: srv.exe daemon and srv.exe mcp
REM  are the SAME binary. Windows write-locks a running .exe, so
REM  `go build -o srv.exe` fails while the daemon holds it. The
REM  daemon respawns lazily (daemon.Ensure) from the freshly built
REM  binary on the first tool call -- so mcp-side AND daemon-side
REM  changes both go live on reconnect.
REM ============================================================
setlocal

REM %~dp0 = drive+path of this script, with a trailing backslash.
REM Strip the trailing backslash so path joins below are clean.
set "SRV_ROOT=%~dp0"
if "%SRV_ROOT:~-1%"=="\" set "SRV_ROOT=%SRV_ROOT:~0,-1%"
set "SRV_EXE=%SRV_ROOT%\srv.exe"

REM 1) Stop any running daemon to release the srv.exe write lock.
REM    Running a write-locked exe is allowed (run != overwrite),
REM    so this works even though srv.exe is locked. No-op if no
REM    daemon / no srv.exe yet.
if exist "%SRV_EXE%" "%SRV_EXE%" daemon stop >nul 2>&1

REM 2) Rebuild from source. stdout must stay clean (it becomes the
REM    JSON-RPC channel once mcp starts); build diagnostics go to
REM    build.log. On failure, exit non-zero so Claude Code surfaces
REM    the connection failure instead of running stale code.
go build -C "%SRV_ROOT%\go" -o ..\srv.exe . 2> "%SRV_ROOT%\build.log"
if errorlevel 1 exit /b 1

REM 3) Exec the MCP server. First remote tool call lazily respawns
REM    the daemon from THIS newly built binary.
"%SRV_EXE%" mcp
