package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/transfer"
	"strings"
)

// editJSONValue marshals v to a temp file, opens it in $EDITOR, and
// returns the (possibly modified) bytes after the editor exits. Used
// by `srv config edit` so users can hand-tweak a profile without
// escaping JSON on the command line.
func editJSONValue(v any, pattern string) ([]byte, error) {
	tmp, err := os.CreateTemp("", pattern)
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		tmp.Close()
		return nil, err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	editor, leadArgs, err := pickEditor()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(editor, append(leadArgs, tmpPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return os.ReadFile(tmpPath)
}

// cmdOpen downloads a remote file into a fresh temp dir and hands
// it to the local OS' default opener (xdg-open / open / start). One-
// shot; doesn't push the file back. For round-trip edits use `srv
// edit` instead.
func cmdOpen(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv open <remote_file>")
		return exitCode(2)
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCode(1)
	}
	cwd := config.GetCwd(name, profile)
	remote := remote.ResolvePath(args[0], cwd)
	tmpDir, err := os.MkdirTemp("", "srv-open-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return exitCode(1)
	}
	local := filepath.Join(tmpDir, path.Base(strings.TrimRight(remote, "/")))
	if rc, _, err := transfer.PullPath(profile, remote, local, false); err != nil || rc != 0 {
		if err != nil {
			fmt.Fprintln(os.Stderr, "srv open:", err)
		}
		return exitCode(1)
	}
	fmt.Fprintln(os.Stderr, "opened local copy:", local)
	if err := openLocal(local); err != nil {
		fmt.Fprintln(os.Stderr, "srv open:", err)
		return exitCode(1)
	}
	return nil
}

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

// cmdCode launches VS Code (or echoes the command when `code` isn't
// on PATH) pointed at the remote profile via the vscode-remote
// URI scheme. With no arg it opens the active cwd.
func cmdCode(args []string, cfg *config.Config, profileOverride string) error {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitCode(1)
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
		return exitCode(runLocal(code, "--folder-uri", uri))
	}
	fmt.Println("code --folder-uri", uri)
	return nil
}

func runLocal(name string, args ...string) int {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
