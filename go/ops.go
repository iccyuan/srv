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
func runRemoteStream(profile *Profile, cwd, cmd string, tty bool) int {
	c, err := Dial(profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 255
	}
	defer c.Close()
	rc, err := c.RunInteractive(cmd, cwd, tty)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		if rc == 0 {
			return 255
		}
	}
	return rc
}

// runRemoteCapture opens a connection, runs `cmd` capturing output, closes.
func runRemoteCapture(profile *Profile, cwd, cmd string) (*RunCaptureResult, error) {
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

// changeRemoteCwd validates a target path on the remote (cd <current> ... ;
// cd <target> && pwd) and persists the absolute result for the current
// session+profile. Returns (newCwd, error). On failure, returns ("", err).
func changeRemoteCwd(profileName string, profile *Profile, target string) (string, error) {
	if target == "" {
		target = "~"
	}
	current := GetCwd(profileName, profile)
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
	newCwd := strings.TrimSpace(lines[len(lines)-1])
	if err := SetCwd(profileName, newCwd); err != nil {
		return "", err
	}
	return newCwd, nil
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

func uploadFile(s *sftp.Client, local, remote string) error {
	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()
	// Ensure remote parent exists.
	if dir := path.Dir(remote); dir != "" && dir != "." {
		_ = s.MkdirAll(dir)
	}
	dst, err := s.Create(remote)
	if err != nil {
		return err
	}
	defer dst.Close()
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

func downloadFile(s *sftp.Client, remote, local string) error {
	if dir := filepath.Dir(local); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	src, err := s.Open(remote)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(local)
	if err != nil {
		return err
	}
	defer dst.Close()
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
