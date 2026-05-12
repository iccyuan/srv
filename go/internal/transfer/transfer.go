package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"srv/internal/config"
	"srv/internal/progress"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strings"

	"github.com/pkg/sftp"
)

// expandRemoteHome resolves a leading "~" by asking the remote `echo $HOME`
// once and substituting. Cached on the *sshx.Client.

// PushPath uploads a local file or directory (recursive) to a remote
// path. Returns (exitCode, finalRemotePath, err); finalRemotePath is the
// actual landing location after tilde-expansion and scp-style dir
// adjustment, so callers (CLI confirmation lines, MCP responses) can
// surface where the file really went rather than the user's raw input.
func PushPath(profile *config.Profile, local, remote string, recursive bool) (int, string, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, remote, err
	}
	defer c.Close()

	st, err := os.Stat(local)
	if err != nil {
		return 1, remote, err
	}
	if st.IsDir() && !recursive {
		recursive = true
	}

	resolved, err := c.ExpandRemoteHome(remote)
	if err != nil {
		return 1, remote, err
	}

	s, err := c.SFTP()
	if err != nil {
		return 1, resolved, err
	}
	// scp-style: if the remote target is an existing directory, place the
	// source inside it (file -> dir/basename, dir -> dir/source-name)
	// rather than trying to use the path verbatim. Without this,
	// `srv push foo.exe /existing-dir` calls SFTP Create("/existing-dir")
	// which returns the unhelpful "Failure" (SSH_FX_FAILURE) -- the SFTP
	// server can't overwrite a directory with a file. Mirrors the
	// symmetric handling PullPath has had since v1.
	if rstat, statErr := s.Stat(resolved); statErr == nil && rstat.IsDir() {
		resolved = path.Join(resolved, path.Base(local))
	}
	if recursive {
		if err := uploadDir(c, local, resolved); err != nil {
			return 1, resolved, err
		}
	} else {
		if err := Upload(c, local, resolved); err != nil {
			return 1, resolved, err
		}
	}
	return 0, resolved, nil
}

// PullPath downloads a remote file or directory to a local path. Returns
// (exitCode, finalLocalPath, err); finalLocalPath is the actual landing
// location after the "remote-source-name appended when local is an
// existing dir" rule fires, so callers can stat it to report transfer
// size or surface to the user where bytes really ended up.
func PullPath(profile *config.Profile, remote, local string, recursive bool) (int, string, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, local, err
	}
	defer c.Close()

	resolved, err := c.ExpandRemoteHome(remote)
	if err != nil {
		return 1, local, err
	}
	s, err := c.SFTP()
	if err != nil {
		return 1, local, err
	}
	st, err := s.Stat(resolved)
	if err != nil {
		return 1, local, err
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
		if err := downloadDir(c, resolved, finalLocal); err != nil {
			return 1, finalLocal, err
		}
		return 0, finalLocal, nil
	}
	if err := Download(c, resolved, finalLocal); err != nil {
		return 1, finalLocal, err
	}
	return 0, finalLocal, nil
}

// Upload copies local -> remote via SFTP. If a partial remote file
// exists (size strictly between 0 and the local file's size), it first
// verifies that the remote bytes are an exact prefix of the local file
// (via remote sha256 of the first N bytes -- ~80 byte network cost,
// not N bytes). Only then does it append the remainder. Mismatched
// partials are overwritten from scratch. Same-size remote files trigger
// the same prefix check; matching content is a no-op skip (with chmod
// sync so an unrelated permission change still lands).
func Upload(c *sshx.Client, local, remote string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}

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
	if rstat, statErr := s.Stat(remote); statErr == nil && rstat.Size() > 0 {
		if rstat.Size() == localSize {
			if same, cmpErr := samePrefix(c, remote, local, localSize); cmpErr == nil && same {
				// Idempotent skip -- but still mirror local mode in case
				// the user changed permissions without touching content.
				if st, err := os.Stat(local); err == nil {
					_ = s.Chmod(remote, st.Mode().Perm())
				}
				return nil
			} else if cmpErr != nil {
				warnNotMCP("srv push: existing-file check failed for %s: %v; restarting\n", remote, cmpErr)
			} else {
				warnNotMCP("srv push: existing %s has same size but different content; restarting\n", remote)
			}
		} else if rstat.Size() < localSize {
			if same, cmpErr := samePrefix(c, remote, local, rstat.Size()); cmpErr == nil && same {
				f, openErr := s.OpenFile(remote, os.O_WRONLY|os.O_APPEND)
				if openErr == nil {
					if _, seekErr := src.Seek(rstat.Size(), io.SeekStart); seekErr == nil {
						dst = f
						startOffset = rstat.Size()
					} else {
						_ = f.Close()
					}
				}
			} else if cmpErr != nil {
				warnNotMCP("srv push: resume check failed for %s: %v; restarting\n", remote, cmpErr)
			} else {
				warnNotMCP("srv push: partial %s does not match source prefix; restarting\n", remote)
			}
		}
	}
	if dst == nil {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		dst, err = s.Create(remote)
		if err != nil {
			return err
		}
		startOffset = 0
	}
	defer dst.Close()

	if startOffset > 0 {
		warnNotMCP("srv push: resuming %s from %d/%d bytes\n", remote, startOffset, localSize)
	}
	// Progress meter -- silent under MCP and on non-TTY stderr (CI / pipes).
	// Resume mode pre-fills the counter so the bar shows the *true* total
	// progress, not just bytes transferred this call.
	meter := progress.NewMeter("push  "+progress.ShortLabel(remote), localSize)
	meter.Add(startOffset)
	if _, err := io.Copy(dst, progress.NewReader(src, meter)); err != nil {
		meter.Done()
		return err
	}
	meter.Done()
	if st, err := os.Stat(local); err == nil {
		_ = s.Chmod(remote, st.Mode().Perm())
	}
	return nil
}

func uploadDir(c *sshx.Client, local, remote string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
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
		return Upload(c, p, dst)
	})
}

// Download mirrors Upload's resume logic in the other direction.
// If the local file is a strict prefix of the remote (size 0 < L < R),
// verify the prefix matches via remote sha256 then append the rest.
// Mismatched partials are overwritten from scratch.
func Download(c *sshx.Client, remote, local string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
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
	if lstat, statErr := os.Stat(local); statErr == nil && lstat.Size() > 0 {
		if lstat.Size() == remoteSize {
			if same, cmpErr := samePrefix(c, remote, local, remoteSize); cmpErr == nil && same {
				return nil
			} else if cmpErr != nil {
				warnNotMCP("srv pull: existing-file check failed for %s: %v; restarting\n", local, cmpErr)
			} else {
				warnNotMCP("srv pull: existing %s has same size but different content; restarting\n", local)
			}
		} else if lstat.Size() < remoteSize {
			if same, cmpErr := samePrefix(c, remote, local, lstat.Size()); cmpErr == nil && same {
				f, openErr := os.OpenFile(local, os.O_WRONLY|os.O_APPEND, 0o644)
				if openErr == nil {
					if _, seekErr := src.Seek(lstat.Size(), io.SeekStart); seekErr == nil {
						dst = f
						startOffset = lstat.Size()
					} else {
						_ = f.Close()
					}
				}
			} else if cmpErr != nil {
				warnNotMCP("srv pull: resume check failed for %s: %v; restarting\n", local, cmpErr)
			} else {
				warnNotMCP("srv pull: partial %s does not match remote prefix; restarting\n", local)
			}
		}
	}
	if dst == nil {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		dst, err = os.Create(local)
		if err != nil {
			return err
		}
		startOffset = 0
	}
	defer dst.Close()

	if startOffset > 0 {
		warnNotMCP("srv pull: resuming %s from %d/%d bytes\n", local, startOffset, remoteSize)
	}
	meter := progress.NewMeter("pull  "+progress.ShortLabel(local), remoteSize)
	meter.Add(startOffset)
	if _, err := io.Copy(dst, progress.NewReader(src, meter)); err != nil {
		meter.Done()
		return err
	}
	meter.Done()
	return nil
}

// samePrefix asks the remote to sha256 the first n bytes of `remote`,
// hashes the same range of `local`, and returns true iff the hex digests
// match. Cost: ~80 byte network reply (one short SSH command exec) +
// disk read of n bytes on each side. The byte-stream comparison this
// replaces had to ship n bytes back across the network just to verify
// -- a 5GB resume would download 5GB just to confirm the partial was
// a real prefix. The hash version keeps that on-disk.
func samePrefix(c *sshx.Client, remote, local string, n int64) (bool, error) {
	rh, err := remoteHashFirstN(c, remote, n)
	if err != nil {
		return false, err
	}
	lh, err := localHashFirstN(local, n)
	if err != nil {
		return false, err
	}
	return rh == lh, nil
}

// sha256EmptyHex is sha256("") -- short-circuit when n=0 so we don't
// even bother shelling out.
const sha256EmptyHex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// remoteHashFirstN runs `head -c n -- path | sha256sum` on the remote
// and returns the 64-char hex digest. Falls through sha256sum →
// shasum -a 256 → openssl so it works on Linux, BSD/macOS, and minimal
// Alpine-style images. The grep filter strips the formatting noise each
// tool prints alongside the hex (e.g. `<hex>  -` vs `(stdin)= <hex>`).
func remoteHashFirstN(c *sshx.Client, p string, n int64) (string, error) {
	if n == 0 {
		return sha256EmptyHex, nil
	}
	cmd := fmt.Sprintf(
		"head -c %d -- %s | { sha256sum 2>/dev/null || shasum -a 256 2>/dev/null || openssl dgst -sha256 -hex 2>/dev/null; } | grep -oE '[0-9a-f]{64}' | head -n1",
		n, srvtty.ShQuotePath(p),
	)
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(res.Stdout)
	if h == "" {
		return "", fmt.Errorf("no usable hash command on remote (need sha256sum / shasum / openssl)")
	}
	return h, nil
}

func localHashFirstN(p string, n int64) (string, error) {
	if n == 0 {
		return sha256EmptyHex, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// quiet is set true under MCP so progress-style stderr noise
// ("restarting partial upload at X", etc.) doesn't leak into tool
// results. mcp_loop.go pins this on startup via SetQuiet.
var quiet bool

// SetQuiet pins warn output off; called from the MCP server's
// startup alongside i18n.SetMCPMode / progress.SetQuiet.
func SetQuiet(q bool) { quiet = q }

// warnNotMCP prints to stderr, but stays silent under MCP -- the
// client there reads stderr as part of the tool result and noisy
// "restarting" / "resuming" lines pollute the model's context.
func warnNotMCP(format string, args ...any) {
	if quiet {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

func downloadDir(c *sshx.Client, remote, local string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
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
		if err := Download(c, p, dst); err != nil {
			return err
		}
	}
	return nil
}
