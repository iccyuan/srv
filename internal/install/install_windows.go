//go:build windows

package install

import (
	"fmt"
	"os/exec"
	"strings"
)

// installAddToPath adds dir to the User PATH (HKCU\Environment\Path) and
// broadcasts WM_SETTINGCHANGE so new shells pick it up. Uses PowerShell's
// [Environment]::SetEnvironmentVariable -- handles long paths and the
// broadcast natively, avoiding `setx`'s 1024-char truncation.
//
// Idempotent: if dir is already in the User PATH (case-insensitive,
// trailing-slash insensitive), exits 0 without modifying anything.
func installAddToPath(dir string) error {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$current = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $current) { $current = '' }
$entries = $current -split ';' |
    ForEach-Object { $_.TrimEnd('\') } |
    Where-Object { $_ -ne '' }
$dir = '%s'.TrimEnd('\')
if ($entries -contains $dir) { exit 0 }
$new = if ($current) { "$current;$dir" } else { $dir }
[Environment]::SetEnvironmentVariable('Path', $new, 'User')
`, escapePSSingle(dir))
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("set PATH: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// installRemoveFromPath drops dir from the User PATH if present.
func installRemoveFromPath(dir string) error {
	script := fmt.Sprintf(`
$ErrorActionPreference = 'Stop'
$current = [Environment]::GetEnvironmentVariable('Path', 'User')
if ($null -eq $current) { exit 0 }
$dir = '%s'.TrimEnd('\')
$kept = $current -split ';' |
    ForEach-Object { $_.TrimEnd('\') } |
    Where-Object { $_ -ne '' -and $_ -ne $dir }
[Environment]::SetEnvironmentVariable('Path', ($kept -join ';'), 'User')
`, escapePSSingle(dir))
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		return fmt.Errorf("unset PATH: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// escapePSSingle escapes a string for safe use inside a PowerShell
// single-quoted literal: ' -> ”.
func escapePSSingle(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}
