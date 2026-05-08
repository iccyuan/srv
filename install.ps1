# srv installer for Windows (PowerShell).
#
# Usage:
#     .\install.ps1
#
# Adds the script's own directory to the **User** PATH (not System), so srv
# is callable from every new shell without admin rights. Idempotent --
# running it twice is fine; it won't duplicate the entry. Already-open
# PowerShell windows need to be restarted to pick up the new PATH.

[CmdletBinding()]
param(
    [switch]$Uninstall,
    [switch]$Gui
)

$ErrorActionPreference = 'Stop'

$here = $PSScriptRoot
if (-not $here) { $here = Split-Path -Parent $MyInvocation.MyCommand.Path }
$bin  = Join-Path $here 'srv.exe'

if (-not (Test-Path $bin)) {
    Write-Host "srv.exe not found at $bin" -ForegroundColor Yellow
    Write-Host "Build it first:" -ForegroundColor Yellow
    Write-Host "  cd `"$here\go`"; go build -o ..\srv.exe ." -ForegroundColor Yellow
    exit 1
}

# -Gui hands off to the cross-platform browser-based installer baked into
# the srv binary. Same UI on Windows / macOS / Linux; covers PATH +
# Claude Code MCP + first profile in one pass.
if ($Gui) {
    & $bin install
    exit $LASTEXITCODE
}

# Read current User PATH; split on ';' and normalize trailing slashes.
$current  = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $current) { $current = '' }
$entries  = $current -split ';' | ForEach-Object { $_.TrimEnd('\') } | Where-Object { $_ -ne '' }
$norm     = $here.TrimEnd('\')
$alreadyIn = $entries -contains $norm

if ($Uninstall) {
    if (-not $alreadyIn) {
        Write-Host "srv: not on User PATH, nothing to remove."
        exit 0
    }
    $kept = $entries | Where-Object { $_ -ne $norm }
    [Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
    Write-Host "srv: removed $here from User PATH." -ForegroundColor Green
    Write-Host "Open a new PowerShell to see the change."
    exit 0
}

if ($alreadyIn) {
    Write-Host "srv: $here is already on User PATH." -ForegroundColor Green
} else {
    $newPath = if ($current) { "$current;$here" } else { $here }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    Write-Host "srv: added $here to User PATH." -ForegroundColor Green
}

# Sanity check the binary works (don't depend on PATH update propagating).
try {
    $version = & $bin version 2>$null
    Write-Host ("srv: " + $version)
} catch {
    Write-Host "srv: warning -- could not run $bin -- $_" -ForegroundColor Yellow
}

Write-Host ""
Write-Host "Done. Open a new PowerShell window and run:  srv version"
Write-Host "(Existing windows still have the old PATH.)"
