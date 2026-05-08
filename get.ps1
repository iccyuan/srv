# srv one-shot installer for Windows (PowerShell).
#
#   iwr -useb https://raw.githubusercontent.com/iccyuan/srv/main/get.ps1 | iex
#
# Pin a version or change the install dir via env vars:
#
#   $env:SRV_VERSION = '2.6.5'
#   $env:SRV_INSTALL_DIR = "$env:USERPROFILE\bin"
#   iwr -useb https://raw.githubusercontent.com/iccyuan/srv/main/get.ps1 | iex
#
# What it does:
#   1. Detect arch (x86_64; bails on Windows ARM64 -- no release for it).
#   2. Resolve latest release version (via /releases/latest redirect)
#      unless $env:SRV_VERSION is set.
#   3. Download the matching .zip from GitHub Releases.
#   4. Extract srv.exe into $env:SRV_INSTALL_DIR (default $env:USERPROFILE\.srv\bin).
#   5. Add that dir to the User PATH (idempotent).
#   6. Tell the user to run `srv install` for MCP / first-profile setup.

$ErrorActionPreference = 'Stop'

$Repo = if ($env:SRV_REPO) { $env:SRV_REPO } else { 'iccyuan/srv' }
$InstallDir = if ($env:SRV_INSTALL_DIR) { $env:SRV_INSTALL_DIR } else { "$env:USERPROFILE\.srv\bin" }
$Version = $env:SRV_VERSION

function Say([string]$msg) { Write-Host "[srv get] $msg" }

# --- arch detection ---
$archLabel = $env:PROCESSOR_ARCHITECTURE
if (-not $archLabel) { $archLabel = $env:PROCESSOR_ARCHITEW6432 }
switch -Wildcard ($archLabel) {
    'AMD64' { $Arch = 'x86_64' }
    'ARM64' {
        Say "Windows ARM64 isn't shipped as a release."
        Say "Build from source instead:  cd go; go build -o ..\srv.exe ."
        exit 1
    }
    default {
        Say "unsupported arch: $archLabel"
        exit 1
    }
}

# --- resolve latest version (redirect-based, no API rate limit) ---
if (-not $Version) {
    Say 'resolving latest release...'
    $resp = $null
    try {
        $resp = Invoke-WebRequest -UseBasicParsing -MaximumRedirection 0 `
            -Uri "https://github.com/$Repo/releases/latest" -ErrorAction SilentlyContinue
    } catch [System.Net.WebException] {
        $resp = $_.Exception.Response
    }
    $location = $null
    if ($resp -and $resp.Headers) {
        $location = $resp.Headers.Location
        if (-not $location -and $resp.Headers['Location']) {
            $location = $resp.Headers['Location']
        }
    }
    if ($location) {
        $Version = ($location.ToString() -split '/v')[-1]
    }
    if (-not $Version) {
        try {
            $api = Invoke-RestMethod "https://api.github.com/repos/$Repo/releases/latest"
            $Version = ($api.tag_name -replace '^v', '')
        } catch {
            Say "couldn't resolve latest version. Set `$env:SRV_VERSION = '<x.y.z>' first."
            exit 1
        }
    }
}

$Archive = "srv_${Version}_windows_${Arch}.zip"
$Url = "https://github.com/$Repo/releases/download/v${Version}/${Archive}"

Say "downloading srv $Version for windows/$Arch"
Say "  url:   $Url"
Say "  dest:  $InstallDir\srv.exe"

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) "srv-get-$([guid]::NewGuid().ToString('N'))"
New-Item -ItemType Directory -Force -Path $tmp | Out-Null
try {
    $zipPath = Join-Path $tmp $Archive
    Invoke-WebRequest -UseBasicParsing -Uri $Url -OutFile $zipPath
    Expand-Archive -Path $zipPath -DestinationPath $tmp -Force
    Move-Item -Force "$tmp\srv.exe" "$InstallDir\srv.exe"
    Say ("installed: " + (& "$InstallDir\srv.exe" version))
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# --- User PATH (no admin needed) ---
$current = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $current) { $current = '' }
$entries = $current -split ';' | ForEach-Object { $_.TrimEnd('\') } | Where-Object { $_ -ne '' }
$norm = $InstallDir.TrimEnd('\')
if ($entries -contains $norm) {
    Say "$InstallDir already on User PATH"
} else {
    $newPath = if ($current) { "$current;$InstallDir" } else { $InstallDir }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    Say "added $InstallDir to User PATH"
}

Write-Host ''
Write-Host '[srv get] done.' -ForegroundColor Green
Write-Host '[srv get] Open a new PowerShell, or refresh PATH in this session:'
Write-Host '[srv get]   $env:Path = [Environment]::GetEnvironmentVariable("Path","User") + ";" + [Environment]::GetEnvironmentVariable("Path","Machine")'
Write-Host ''
Write-Host '[srv get] Then to register Claude Code MCP / set up your first profile:'
Write-Host '[srv get]   srv install     (opens a browser-based wizard)'
Write-Host ''
