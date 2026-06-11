package transfer

import (
	"fmt"
	"io"
	"net/url"
	"path"
	"srv/internal/config"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strings"
)

// RelayResult reports how a relay download went and where the bytes
// landed on each side.
type RelayResult struct {
	ExitCode  int    // 0 on full success (download + pull both clean)
	LocalPath string // actual local landing path (post PullPath dir rule)
	RemoteTmp string // remote temp file path used, for diagnostics
	FileName  string // filename derived from the URL
}

// RelayDownload fetches rawURL *on the remote host* (curl, falling back
// to wget) into a throwaway remote temp dir, pulls the result down to
// localDest, then deletes the remote temp dir regardless of outcome.
//
// This is the "server as a download relay" path: the remote's network
// reaches the URL (faster line, or a source the local machine can't
// reach), and the bytes come home over the existing SSH transport. The
// remote keeps nothing -- the temp dir is removed on the way out, even
// when the download or the pull fails.
//
// showProgress streams curl/wget's native progress meter (carriage-
// return refreshed) straight to local stderr for the remote-fetch
// phase; the subsequent pull phase has its own progress.Meter. It is
// gated on a real stderr TTY so redirected CLI output and the MCP
// server (which must keep stderr clean for the model) fall back to a
// quiet, buffered fetch.
func RelayDownload(profile *config.Profile, rawURL, localDest string, showProgress bool) (RelayResult, error) {
	res := RelayResult{FileName: relayFileName(rawURL)}
	streamProgress := showProgress && srvtty.IsStderrTTY()

	c, err := sshx.Dial(profile)
	if err != nil {
		return res, err
	}
	defer c.Close()

	// Throwaway remote dir. mktemp -d isolates the download so the
	// cleanup is a single rm -rf with no chance of clobbering a real
	// path the user cares about.
	mk, err := c.RunCapture("mktemp -d 2>/dev/null || mktemp -d -t srv-get", "")
	if err != nil {
		return res, err
	}
	if mk.ExitCode != 0 {
		return res, fmt.Errorf("remote mktemp failed: %s", strings.TrimSpace(mk.Stderr))
	}
	tmpDir := strings.TrimSpace(mk.Stdout)
	if tmpDir == "" {
		return res, fmt.Errorf("remote mktemp returned an empty path")
	}
	remoteFile := tmpDir + "/" + res.FileName
	res.RemoteTmp = remoteFile
	// Always tear the temp dir down, success or failure. This defer runs
	// before the deferred c.Close() above, so the connection is still
	// open when the cleanup fires.
	defer func() { _, _ = c.RunCapture("rm -rf -- "+srvtty.ShQuotePath(tmpDir), "") }()

	dl := buildDownloadCmd(tmpDir, res.FileName, rawURL, streamProgress)
	if streamProgress {
		// Stream phase: curl/wget write their meter to stderr, which
		// RunStreamStdout pipes straight to local stderr (the file goes
		// to -o on the remote, so stdout is empty -> discard). We lose
		// the captured stderr for the error message, but the user has
		// already seen curl's own error on screen in this path.
		rc, rerr := c.RunStreamStdout(dl, "", io.Discard)
		if rerr != nil {
			return res, rerr
		}
		if rc != 0 {
			res.ExitCode = rc
			return res, fmt.Errorf("remote download failed (exit %d)", rc)
		}
	} else {
		run, rerr := c.RunCapture(dl, "")
		if rerr != nil {
			return res, rerr
		}
		if run.ExitCode != 0 {
			res.ExitCode = run.ExitCode
			msg := strings.TrimSpace(run.Stderr)
			if msg == "" {
				msg = fmt.Sprintf("exit %d", run.ExitCode)
			}
			return res, fmt.Errorf("remote download failed: %s", msg)
		}
	}

	// Pull the fetched file home. PullPath opens its own sftp connection;
	// the temp dir is still alive because our cleanup defer hasn't run.
	rc, finalLocal, perr := PullPath(profile, remoteFile, localDest, false)
	res.ExitCode = rc
	res.LocalPath = finalLocal
	if perr != nil {
		return res, perr
	}
	if rc != 0 {
		return res, fmt.Errorf("pull failed (exit %d)", rc)
	}
	return res, nil
}

// buildDownloadCmd assembles the remote shell command that fetches url
// into dir/fname, preferring curl and falling back to wget. -f makes
// curl fail on HTTP errors instead of saving a "404" page as the file;
// -S surfaces the error on stderr; -L follows redirects; --retry rides
// out transient drops.
//
// progress picks the meter style: --progress-bar (curl) /
// --progress=bar:force (wget) draw a live carriage-return bar for the
// streamed CLI path; the quiet path uses curl -s (silent meter, -S
// keeps errors) and wget -nv so a buffered capture stays parseable
// instead of full of bar redraws.
func buildDownloadCmd(dir, fname, url string, progress bool) string {
	qDir := srvtty.ShQuotePath(dir)
	qName := srvtty.ShQuote(fname)
	qURL := srvtty.ShQuote(url)
	curlFlags, wgetFlags := "-fsSL --retry 3", "-nv"
	if progress {
		curlFlags, wgetFlags = "-fSL --retry 3 --progress-bar", "--progress=bar:force"
	}
	return fmt.Sprintf(
		"cd %s && if command -v curl >/dev/null 2>&1; then curl %s -o %s %s; "+
			"elif command -v wget >/dev/null 2>&1; then wget %s -O %s %s; "+
			"else echo 'no curl or wget on remote' >&2; exit 127; fi",
		qDir,
		curlFlags, qName, qURL,
		wgetFlags, qName, qURL,
	)
}

// relayFileName derives the saved filename from a URL: the basename of
// the URL path with any query string stripped. Falls back to "download"
// when the URL has no usable path component (e.g. "https://host/").
func relayFileName(rawURL string) string {
	if u, err := url.Parse(rawURL); err == nil && u.Path != "" {
		// url.Parse already split the query into u.RawQuery, so u.Path
		// is query-free here.
		if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
			return base
		}
	}
	return "download"
}
