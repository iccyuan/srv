// Package editcmd implements `srv edit <remote_path>` (round-trip
// edit: pull, open in $EDITOR, push back if changed) and the
// EditJSON helper that `srv config edit` uses to hand-tweak JSON
// profile records.
//
// Both flows share pickEditor's resolution logic: $VISUAL, then
// $EDITOR, then platform fallbacks (notepad on Windows; vim/vi/nano
// elsewhere). $VISUAL / $EDITOR are split on whitespace so wrappers
// like "code --wait" work as-is.
package editcmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"srv/internal/check"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/sshx"
	"srv/internal/transfer"
	"strings"
)

// Cmd implements `srv edit <remote_path>`. Downloads the file to a
// temp dir, opens it in $EDITOR, and uploads it back if the local
// copy was modified after the editor exits.
//
// Conflict caveat: srv does not lock the remote file. Before
// save-back it re-stats the remote path and refuses to overwrite if
// size or mtime changed since the initial pull.
//
// Without --wait, VS Code returns immediately and srv will observe
// "no changes" -- the user is responsible for configuring their
// editor to block until close.
func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: srv edit <remote_path>")
		return clierr.Code(2)
	}
	rpath := args[0]

	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return clierr.Code(1)
	}
	cwd := config.GetCwd(name, profile)
	abs := remote.ResolvePath(rpath, cwd)

	c, err := sshx.Dial(profile)
	if err != nil {
		check.PrintDialError(err, profile)
		return clierr.Code(255)
	}
	defer c.Close()

	resolved, err := c.ExpandRemoteHome(abs)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit:", err)
		return clierr.Code(1)
	}

	s, err := c.SFTP()
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: sftp:", err)
		return clierr.Code(1)
	}
	st, err := s.Stat(resolved)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: stat:", err)
		return clierr.Code(1)
	}
	if st.IsDir() {
		fmt.Fprintln(os.Stderr, "srv edit: target is a directory:", resolved)
		return clierr.Code(1)
	}
	remoteSize := st.Size()
	remoteMod := st.ModTime()

	tmpDir, err := os.MkdirTemp("", "srv-edit-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: mkdtemp:", err)
		return clierr.Code(1)
	}
	defer os.RemoveAll(tmpDir)

	base := path.Base(resolved)
	if base == "" || base == "/" || base == "." {
		base = "remote-file"
	}
	localPath := filepath.Join(tmpDir, base)

	if err := transfer.Download(c, resolved, localPath); err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: download:", err)
		return clierr.Code(1)
	}

	before, err := os.Stat(localPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit:", err)
		return clierr.Code(1)
	}

	editor, leadArgs, err := pickEditor()
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit:", err)
		return clierr.Code(1)
	}

	cmd := exec.Command(editor, append(leadArgs, localPath)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		// Editor exited non-zero. Don't bail -- user may have :wq'd
		// successfully and a wrapper hit a typo afterwards. Surface
		// a warning, then check mtime as usual.
		fmt.Fprintln(os.Stderr, "srv edit: editor returned non-zero:", err)
	}

	after, err := os.Stat(localPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: local file gone, not uploading.")
		return clierr.Code(1)
	}
	if after.ModTime().Equal(before.ModTime()) && after.Size() == before.Size() {
		fmt.Fprintln(os.Stderr, "srv edit: no changes; not uploading.")
		return nil
	}

	latest, err := s.Stat(resolved)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: restat:", err)
		return clierr.Code(1)
	}
	if latest.Size() != remoteSize || !latest.ModTime().Equal(remoteMod) {
		fmt.Fprintln(os.Stderr, "srv edit: remote file changed while editing; refusing to overwrite.")
		fmt.Fprintf(os.Stderr, "srv edit: initial size=%d mtime=%s, current size=%d mtime=%s\n",
			remoteSize, remoteMod.Format("2006-01-02T15:04:05"),
			latest.Size(), latest.ModTime().Format("2006-01-02T15:04:05"))
		return clierr.Code(1)
	}

	if err := transfer.Upload(c, localPath, resolved); err != nil {
		fmt.Fprintln(os.Stderr, "srv edit: upload:", err)
		return clierr.Code(1)
	}
	fmt.Fprintf(os.Stderr, "srv edit: saved %s\n", resolved)
	return nil
}

// EditJSON marshals v to a temp file, opens it in $EDITOR, and
// returns the (possibly modified) bytes after the editor exits.
// Used by `srv config edit` so users can hand-tweak a profile
// without escaping JSON on the command line. `pattern` is the
// os.CreateTemp filename pattern (e.g. "srv-profile-*.json") so the
// editor's syntax highlighting picks up the right mode.
func EditJSON(v any, pattern string) ([]byte, error) {
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

// pickEditor returns (executable, leadingArgs, err). Resolution
// order: $VISUAL, $EDITOR (each split on whitespace so "code --wait"
// works), then platform fallbacks: notepad on Windows; vim/vi/nano
// elsewhere.
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
