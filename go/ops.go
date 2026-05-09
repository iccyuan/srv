package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
)

// runRemoteStream opens a connection, runs `cmd` interactively (streaming
// stdio), and closes. Returns remote exit code.
//
// Non-TTY runs go through the daemon when one is available -- the daemon's
// pooled SSH connection saves the ~2.7s handshake. The daemon streams
// stdout/stderr as base64 chunks (stream_run op) so commands like
// `tail -f` and `find /` produce real-time output, not buffered.
func runRemoteStream(profile *Profile, cwd, cmd string, tty bool) int {
	if !tty {
		if rc, ok := tryDaemonStreamRun(profile.Name, cwd, cmd); ok {
			return rc
		}
	}
	c, err := Dial(profile)
	if err != nil {
		printDiagError(err, profile)
		return 255
	}
	defer c.Close()
	rc, err := c.RunInteractive(cmd, cwd, tty)
	if err != nil {
		printDiagError(err, profile)
		if rc == 0 {
			return 255
		}
	}
	return rc
}

// remoteInitPrefix returns a leading shell snippet that sources a
// user-provided init file before the actual command runs, when one is
// configured. The path resolves in this order:
//
//  1. SRV_REMOTE_INIT env -- per-shell / per-MCP-launch override.
//  2. profile.init_file from ~/.srv/config.json -- per-profile default.
//  3. "" -- no file, no prefix; default behaviour preserved.
//
// The returned snippet uses `[ -f X ] && . X` so a missing file on the
// remote is a silent no-op (cheap test, doesn't fail the command).
// Functions / aliases / exports defined by the file stay in scope for
// the user's command in the same shell, which is the whole point.
func remoteInitPrefix(profile *Profile) string {
	p := os.Getenv("SRV_REMOTE_INIT")
	if p == "" && profile != nil {
		p = profile.InitFile
	}
	if p == "" {
		return ""
	}
	q := shQuotePath(p)
	return "[ -f " + q + " ] && . " + q + "; "
}

// runRemoteCapture opens a connection, runs `cmd` capturing output, closes.
//
// Tries the daemon first when the profile is named -- the pooled SSH
// connection reuses the handshake (~2.7s cold) and avoids spawning a
// fresh keepalive goroutine per call. Falls back to a direct dial when
// no daemon is reachable.
func runRemoteCapture(profile *Profile, cwd, cmd string) (*RunCaptureResult, error) {
	cmd = applyRemoteEnv(profile, cmd)
	if prefix := remoteInitPrefix(profile); prefix != "" {
		cmd = prefix + cmd
	}
	if profile.Name != "" {
		if res, ok := tryDaemonRunCapture(profile.Name, cwd, cmd); ok {
			return res, nil
		}
	}
	c, err := Dial(profile)
	if err != nil {
		return &RunCaptureResult{
			Stderr:   "ssh dial failed: " + err.Error(),
			ExitCode: 255,
			Cwd:      cwd,
		}, nil
	}
	defer c.Close()
	return c.RunCapture(cmd, cwd)
}

// changeRemoteCwd validates a target path on the remote and persists the
// absolute result for the current session+profile. Tries the daemon first
// (warm pooled SSH); falls back to a direct dial if no daemon. Returns
// (newCwd, error). On failure, returns ("", err).
func changeRemoteCwd(profileName string, profile *Profile, target string) (string, error) {
	if target == "" {
		target = "~"
	}
	current := GetCwd(profileName, profile)

	// Fast path via daemon.
	if newCwd, used, err := tryDaemonCd(profileName, current, target); used {
		if err != nil {
			return "", err
		}
		if perr := SetCwd(profileName, newCwd); perr != nil {
			return "", perr
		}
		return newCwd, nil
	}

	// Direct dial fallback.
	newCwd, err := validateRemoteCwd(profile, current, target)
	if err != nil {
		return "", err
	}
	if err := SetCwd(profileName, newCwd); err != nil {
		return "", err
	}
	return newCwd, nil
}

// validateRemoteCwd is the side-effect-free "cd ... && pwd" probe that
// returns the resolved absolute path. Used both by direct-dial cwd
// changes and by the daemon's cd handler.
func validateRemoteCwd(profile *Profile, current, target string) (string, error) {
	if current == "" {
		current = "~"
	}
	if target == "" {
		target = "~"
	}
	cmd := fmt.Sprintf(
		"cd %s 2>/dev/null || cd ~; cd %s && pwd",
		shQuotePath(current), shQuotePath(target),
	)
	res, err := runRemoteCapture(profile, "", cmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		if stderr == "" {
			stderr = fmt.Sprintf("cd failed (exit %d)", res.ExitCode)
		}
		return "", errors.New(stderr)
	}
	lines := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[len(lines)-1]) == "" {
		return "", errors.New("remote did not return a path")
	}
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// resolveRemotePath anchors a remote path: absolute or ~-prefixed stays
// as-is, otherwise prepended with cwd.
func resolveRemotePath(remote, cwd string) string {
	if remote == "" {
		return cwd
	}
	if strings.HasPrefix(remote, "/") || strings.HasPrefix(remote, "~") {
		return remote
	}
	return strings.TrimRight(cwd, "/") + "/" + remote
}

// expandRemoteHome resolves a leading "~" by asking the remote `echo $HOME`
// once and substituting. Cached on the *Client.
func (c *Client) expandRemoteHome(p string) (string, error) {
	if !strings.HasPrefix(p, "~") {
		return p, nil
	}
	res, err := c.RunCapture("echo $HOME", "")
	if err != nil {
		return p, err
	}
	if res.ExitCode != 0 {
		return p, fmt.Errorf("remote $HOME lookup failed: %s", strings.TrimSpace(res.Stderr))
	}
	home := strings.TrimSpace(res.Stdout)
	if home == "" {
		return p, fmt.Errorf("remote $HOME empty")
	}
	if p == "~" {
		return home, nil
	}
	return home + p[1:], nil
}

// pushPath uploads a local file or directory (recursive) to a remote path.
func pushPath(profile *Profile, local, remote string, recursive bool) (int, error) {
	c, err := Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()

	st, err := os.Stat(local)
	if err != nil {
		return 1, err
	}
	if st.IsDir() && !recursive {
		recursive = true
	}

	resolved, err := c.expandRemoteHome(remote)
	if err != nil {
		return 1, err
	}

	s, err := c.SFTP()
	if err != nil {
		return 1, err
	}
	if recursive {
		if err := uploadDir(s, local, resolved); err != nil {
			return 1, err
		}
	} else {
		if err := uploadFile(s, local, resolved); err != nil {
			return 1, err
		}
	}
	return 0, nil
}

// pullPath downloads a remote file or directory to a local path.
func pullPath(profile *Profile, remote, local string, recursive bool) (int, error) {
	c, err := Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()

	resolved, err := c.expandRemoteHome(remote)
	if err != nil {
		return 1, err
	}
	s, err := c.SFTP()
	if err != nil {
		return 1, err
	}
	st, err := s.Stat(resolved)
	if err != nil {
		return 1, err
	}
	if st.IsDir() {
		recursive = true
	}

	// If local target is an existing dir, drop the source's basename inside.
	finalLocal := local
	if li, err := os.Stat(local); err == nil && li.IsDir() {
		finalLocal = filepath.Join(local, path.Base(resolved))
	}
	if recursive {
		return 0, downloadDir(s, resolved, finalLocal)
	}
	return 0, downloadFile(s, resolved, finalLocal)
}

// uploadFile copies local -> remote via SFTP. If a partial remote file
// exists (size strictly between 0 and the local file's size), seek both
// sides to the partial offset and append-mode write the remainder. Any
// other situation (no remote file, equal size, larger remote, error
// stating) falls back to a fresh full upload.
//
// The "remote shorter than local" heuristic is the only one that's safe
// without extra metadata: a partial write from a previous interrupted
// transfer is, by construction, smaller than the local source. Same-
// size means done; larger means we're not pushing what we think we are.
func uploadFile(s *sftp.Client, local, remote string) error {
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()

	localStat, err := src.Stat()
	if err != nil {
		return err
	}
	localSize := localStat.Size()

	// Ensure remote parent exists.
	if dir := path.Dir(remote); dir != "" && dir != "." {
		_ = s.MkdirAll(dir)
	}

	var dst *sftp.File
	var startOffset int64
	if rstat, statErr := s.Stat(remote); statErr == nil && rstat.Size() > 0 && rstat.Size() < localSize {
		f, openErr := s.OpenFile(remote, os.O_WRONLY|os.O_APPEND)
		if openErr == nil {
			if _, seekErr := src.Seek(rstat.Size(), io.SeekStart); seekErr == nil {
				dst = f
				startOffset = rstat.Size()
			} else {
				_ = f.Close()
			}
		}
	}
	if dst == nil {
		dst, err = s.Create(remote)
		if err != nil {
			return err
		}
		startOffset = 0
	}
	defer dst.Close()

	if startOffset > 0 {
		fmt.Fprintf(os.Stderr, "srv push: resuming %s from %d/%d bytes\n", remote, startOffset, localSize)
	}
	if _, err := io.Copy(dst, src); err != nil {
		return err
	}
	if st, err := os.Stat(local); err == nil {
		_ = s.Chmod(remote, st.Mode().Perm())
	}
	return nil
}

func uploadDir(s *sftp.Client, local, remote string) error {
	if err := s.MkdirAll(remote); err != nil {
		return err
	}
	return filepath.Walk(local, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(local, p)
		rel = filepath.ToSlash(rel)
		dst := remote
		if rel != "." {
			dst = path.Join(remote, rel)
		}
		if info.IsDir() {
			return s.MkdirAll(dst)
		}
		return uploadFile(s, p, dst)
	})
}

// downloadFile mirrors uploadFile's resume logic in the other direction.
// If the local file is a strict prefix of the remote (size 0 < L < R),
// seek the remote source to L and append the rest locally; otherwise
// download fresh.
func downloadFile(s *sftp.Client, remote, local string) error {
	if dir := filepath.Dir(local); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	src, err := s.Open(remote)
	if err != nil {
		return err
	}
	defer src.Close()

	rstat, err := src.Stat()
	if err != nil {
		return err
	}
	remoteSize := rstat.Size()

	var dst *os.File
	var startOffset int64
	if lstat, statErr := os.Stat(local); statErr == nil && lstat.Size() > 0 && lstat.Size() < remoteSize {
		f, openErr := os.OpenFile(local, os.O_WRONLY|os.O_APPEND, 0o644)
		if openErr == nil {
			if _, seekErr := src.Seek(lstat.Size(), io.SeekStart); seekErr == nil {
				dst = f
				startOffset = lstat.Size()
			} else {
				_ = f.Close()
			}
		}
	}
	if dst == nil {
		dst, err = os.Create(local)
		if err != nil {
			return err
		}
		startOffset = 0
	}
	defer dst.Close()

	if startOffset > 0 {
		fmt.Fprintf(os.Stderr, "srv pull: resuming %s from %d/%d bytes\n", local, startOffset, remoteSize)
	}
	_, err = io.Copy(dst, src)
	return err
}

func downloadDir(s *sftp.Client, remote, local string) error {
	if err := os.MkdirAll(local, 0o755); err != nil {
		return err
	}
	walker := s.Walk(remote)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return err
		}
		p := walker.Path()
		info := walker.Stat()
		rel := strings.TrimPrefix(p, remote)
		rel = strings.TrimPrefix(rel, "/")
		dst := local
		if rel != "" {
			dst = filepath.Join(local, rel)
		}
		if info.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := downloadFile(s, p, dst); err != nil {
			return err
		}
	}
	return nil
}
