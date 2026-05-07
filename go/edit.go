package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// cmdEdit downloads a remote file to a temp dir, opens it in $EDITOR, and
// uploads it back if the local copy was modified after the editor exits.
//
// Concurrency caveat: srv does not lock the remote file. Concurrent edits
// from another session will be silently overwritten on save-back. For
// shared boxes prefer SSH'ing in directly.
//
// $EDITOR / $VISUAL is split on whitespace so wrappers like "code --wait"
// work as-is. Without --wait, VS Code returns immediately and srv will
// observe "no changes" -- the user is responsible for configuring their
// editor to block until close.
func cmdEdit(args []string, cfg *Config, profileOverride string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv edit <remote_path>")
		return 2
	}
	remote := args[0]

	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	cwd := GetCwd(name, profile)
	abs := resolveRemotePath(remote, cwd)

	c, err := Dial(profile)
	if err != nil {
		printDiagError(err, profile)
		return 255
	}
	defer c.Close()

	resolved, err := c.expandRemoteHome(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit:", err)
		return 1
	}

	s, err := c.SFTP()
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: sftp:", err)
		return 1
	}
	st, err := s.Stat(resolved)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: stat:", err)
		return 1
	}
	if st.IsDir() {
		fmt.Fprintln(os.Stderr, "srv edit: target is a directory:", resolved)
		return 1
	}

	tmpDir, err := os.MkdirTemp("", "srv-edit-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: mkdtemp:", err)
		return 1
	}
	defer os.RemoveAll(tmpDir)

	base := path.Base(resolved)
	if base == "" || base == "/" || base == "." {
		base = "remote-file"
	}
	localPath := filepath.Join(tmpDir, base)

	if err := downloadFile(s, resolved, localPath); err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: download:", err)
		return 1
	}

	before, err := os.Stat(localPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit:", err)
		return 1
	}

	editor, leadArgs, err := pickEditor()
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit:", err)
		return 1
	}

	cmd := exec.Command(editor, append(leadArgs, localPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		// Editor exited non-zero. Don't bail -- user may have :wq'd
		// successfully and a wrapper hit a typo afterwards. Surface a
		// warning, then check mtime as usual.
		fmt.Fprintln(os.Stderr, "srv edit: editor returned non-zero:", err)
	}

	after, err := os.Stat(localPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: local file gone, not uploading.")
		return 1
	}
	if after.ModTime().Equal(before.ModTime()) && after.Size() == before.Size() {
		fmt.Fprintln(os.Stderr, "srv edit: no changes; not uploading.")
		return 0
	}

	if err := uploadFile(s, localPath, resolved); err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: upload:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "srv edit: saved %s\n", resolved)
	return 0
}

// pickEditor returns (executable, leadingArgs, err). Resolution order:
// $VISUAL, $EDITOR (each split on whitespace so "code --wait" works), then
// platform fallbacks: notepad on Windows; vim/vi/nano elsewhere.
func pickEditor() (string, []string, error) {
	for _, env := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			fields := strings.Fields(v)
			return fields[0], fields[1:], nil
		}
	}
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("notepad.exe"); err == nil {
			return "notepad.exe", nil, nil
		}
	}
	for _, candidate := range []string{"vim", "vi", "nano"} {
		if _, err := exec.LookPath(candidate); err == nil {
			return candidate, nil, nil
		}
	}
	return "", nil, errors.New("no editor found; set $VISUAL or $EDITOR (e.g. 'code --wait')")
}
