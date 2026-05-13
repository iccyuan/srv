// Package launcher implements two "open this somewhere local"
// commands: `srv open <remote_file>` (pull into a temp dir, hand to
// the OS default opener) and `srv code [<remote_path>]` (point VS
// Code at the remote via the vscode-remote URI scheme).
//
// Both are local-side launchers -- they don't push anything back.
// For round-trip edits use `srv edit` instead.
package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/transfer"
	"strings"
)

// Open implements `srv open <remote_file>`. Downloads the file into
// a fresh temp dir and hands it to the OS' default opener
// (`xdg-open` on Linux, `open` on macOS, `start` on Windows). One-
// shot; the temp copy is not synced back.
func Open(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv open <remote_file>")
		return clierr.Code(2)
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return clierr.Code(1)
	}
	cwd := config.GetCwd(name, profile)
	rpath := remote.ResolvePath(args[0], cwd)
	tmpDir, err := os.MkdirTemp("", "srv-open-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return clierr.Code(1)
	}
	local := filepath.Join(tmpDir, path.Base(strings.TrimRight(rpath, "/")))
	if rc, _, err := transfer.PullPath(profile, rpath, local, false); err != nil || rc != 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "srv open:", err)
		}
		return clierr.Code(1)
	}
	fmt.Fprintln(os.Stderr, "opened local copy:", local)
	if err := openLocal(local); err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return clierr.Code(1)
	}
	return nil
}

// openLocal hands `p` to the platform's default-app launcher.
// Internal -- callers use Open instead.
func openLocal(p string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("cmd", "/c", "start", "", p).Start()
	case "darwin":
		return exec.Command("open", p).Start()
	default:
		return exec.Command("xdg-open", p).Start()
	}
}

// Code implements `srv code [<remote_path>]`. Launches VS Code (or
// echoes the command when `code` isn't on PATH) pointed at the
// remote profile via the vscode-remote URI scheme. With no arg it
// opens the active cwd.
func Code(args []string, cfg *config.Config, profileOverride string) error {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return clierr.Code(1)
	}
	cwd := config.GetCwd(name, profile)
	target := cwd
	if len(args) > 0 {
		target = remote.ResolvePath(args[0], cwd)
	}
	host := profile.Host
	if profile.User != "" {
		host = profile.User + "@" + profile.Host
	}
	uri := "vscode-remote://ssh-remote+" + host + target
	if code, err := exec.LookPath("code"); err == nil {
		return clierr.Code(runLocal(code, "--folder-uri", uri))
	}
	fmt.Println("code --folder-uri", uri)
	return nil
}

// runLocal exec's `name args...` with the user's stdio, returning
// the child's exit code. Used for the `code` launcher only -- other
// callers use cmd.Run / exec.Command directly when they need finer
// control.
func runLocal(name string, args ...string) int {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
