package syncx

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"srv/internal/config"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strings"
)

// RemoteGitChangedFiles asks the remote git repo (rooted at remoteRoot)
// for changed files according to scope (matching the local-side
// GitChangedFiles semantics: all | modified | staged | untracked).
// Returns relative paths inside the repo, sorted.
func RemoteGitChangedFiles(profile *config.Profile, remoteRoot, scope string) ([]string, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	expanded, err := c.ExpandRemoteHome(remoteRoot)
	if err != nil {
		return nil, err
	}

	// Probe that the remote root is actually a git repo before issuing
	// the real queries -- otherwise the error message ("fatal: not a
	// git repository") is what we'd surface to the user, which is less
	// helpful than the targeted one we craft here.
	probe := fmt.Sprintf("cd %s && git rev-parse --is-inside-work-tree", srvtty.ShQuotePath(expanded))
	res, err := c.RunCapture(probe, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) != "true" {
		return nil, fmt.Errorf("remote %s is not a git repo (--pull git mode requires one)", expanded)
	}

	out := map[string]struct{}{}
	runRemoteGit := func(args string) (string, error) {
		cmd := fmt.Sprintf("cd %s && git %s", srvtty.ShQuotePath(expanded), args)
		r, err := c.RunCapture(cmd, "")
		if err != nil {
			return "", err
		}
		if r.ExitCode != 0 {
			return "", fmt.Errorf("remote git failed: %s", strings.TrimSpace(r.Stderr))
		}
		return r.Stdout, nil
	}
	if scope == "all" || scope == "modified" || scope == "untracked" {
		args := "ls-files -z"
		if scope == "all" || scope == "modified" {
			args += " --modified"
		}
		if scope == "all" || scope == "untracked" {
			args += " --others --exclude-standard"
		}
		body, err := runRemoteGit(args)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(body, "\x00") {
			if p != "" {
				out[p] = struct{}{}
			}
		}
	}
	if scope == "all" || scope == "staged" {
		body, err := runRemoteGit("diff --name-only --cached -z")
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(body, "\x00") {
			if p != "" {
				out[p] = struct{}{}
			}
		}
	}
	files := make([]string, 0, len(out))
	for k := range out {
		files = append(files, k)
	}
	sort.Strings(files)
	return files, nil
}

// RemoteGlobFiles asks the remote to enumerate files under remoteRoot
// that match any of the supplied glob patterns. Implemented via `find`
// with -name / -path filters chained by -o. Sorted output, NUL-safe.
//
// Patterns containing `/` are treated as path globs (-path); bare-name
// patterns use -name so `*.log` matches `foo/bar.log` deep in the tree
// (mirrors how `srv sync --include "*.log"` behaves locally).
func RemoteGlobFiles(profile *config.Profile, remoteRoot string, patterns []string) ([]string, error) {
	if len(patterns) == 0 {
		return nil, nil
	}
	c, err := sshx.Dial(profile)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	expanded, err := c.ExpandRemoteHome(remoteRoot)
	if err != nil {
		return nil, err
	}
	parts := make([]string, 0, len(patterns)*2)
	for i, pat := range patterns {
		if i > 0 {
			parts = append(parts, "-o")
		}
		flag := "-name"
		if strings.Contains(pat, "/") {
			flag = "-path"
			// Convert `src/**/*.go` to `src/*/*.go` etc. -- find doesn't
			// understand `**` so we fall back to a single-star wildcard
			// match against the path. Users wanting precise recursion
			// should pass `--files` instead.
			pat = strings.ReplaceAll(pat, "**", "*")
			pat = "./" + strings.TrimPrefix(pat, "./")
		}
		parts = append(parts, flag, srvtty.ShQuotePath(pat))
	}
	cmd := fmt.Sprintf("cd %s && find . -type f \\( %s \\) -printf '%%P\\0'",
		srvtty.ShQuotePath(expanded), strings.Join(parts, " "))
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("remote find failed: %s", strings.TrimSpace(res.Stderr))
	}
	files := []string{}
	for _, p := range strings.Split(res.Stdout, "\x00") {
		if p != "" {
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files, nil
}

// collectPullFiles picks the file list from the remote side based on
// mode. Mtime mode is intentionally rejected -- a remote `find -mmin`
// would work but it's confusing without a UI for "files modified in
// the last N hours on the *remote*", which is what the user would
// expect. List / glob / git cover the common cases.
func collectPullFiles(o *Options, profile *config.Profile) ([]string, error) {
	switch o.Mode {
	case "list":
		out := make([]string, 0, len(o.Files))
		for _, f := range o.Files {
			f = strings.TrimSpace(f)
			if f == "" || strings.HasPrefix(f, "/") || strings.HasPrefix(f, "..") {
				continue
			}
			out = append(out, f)
		}
		return out, nil
	case "glob":
		return RemoteGlobFiles(profile, o.RemoteRoot, o.Include)
	case "git":
		return RemoteGitChangedFiles(profile, o.RemoteRoot, o.GitScope)
	case "mtime":
		return nil, fmt.Errorf("--pull --since is not supported (use --include or --files)")
	}
	return nil, fmt.Errorf("--pull needs a mode: --files, --include, or git scope")
}

// BuildDiffPull is the symmetric BuildDiff for the pull direction:
// "would this pull change my local files?". Same rsync-itemize action
// codes but the interpretation flips:
//
//   - local does not yet have the file (pull creates)
//     > remote is newer (pull updates local)
//     < LOCAL is newer than remote (pull would clobber newer content)
//     = same size + mtime
func BuildDiffPull(localRoot string, files []string, remoteStats map[string]RemoteStat) []DiffEntry {
	entries := make([]DiffEntry, 0, len(files))
	for _, f := range files {
		local := localStatFor(localRoot, f)
		remote, ok := remoteStats[f]
		if !ok {
			// Pull mode: a missing remote stat means we couldn't read
			// the file's metadata (or the file vanished between
			// listing and stat). Treat as unchanged so the user gets a
			// hint rather than a confusing '+' / '>' row.
			entries = append(entries, DiffEntry{Action: '=', Path: f, Local: local})
			continue
		}
		entries = append(entries, classifyPull(f, local, remote))
	}
	return entries
}

func classifyPull(path string, local, remote RemoteStat) DiffEntry {
	if local.Missing {
		return DiffEntry{Action: '+', Path: path, Local: local, Remote: remote}
	}
	if local.Size == remote.Size && abs(local.MtimeUnix-remote.MtimeUnix) <= 2 {
		return DiffEntry{Action: '=', Path: path, Local: local, Remote: remote}
	}
	if local.MtimeUnix > remote.MtimeUnix {
		return DiffEntry{Action: '<', Path: path, Local: local, Remote: remote}
	}
	return DiffEntry{Action: '>', Path: path, Local: local, Remote: remote}
}

// TarDownloadStream is the inverse of TarUploadStream: the remote
// `tar -cf -` pipes the file list out, the local end un-tars into
// localRoot. Files are paths relative to remoteRoot (NOT absolute);
// the un-tar preserves the same tree on the local side.
//
// Gzip on the wire mirrors profile.CompressSync just like push does.
// Returns (remote exit code, error).
func TarDownloadStream(profile *config.Profile, remoteRoot string, files []string, localRoot string) (int, error) {
	if len(files) == 0 {
		return 0, nil
	}
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()
	expanded, err := c.ExpandRemoteHome(remoteRoot)
	if err != nil {
		return 1, err
	}
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return 1, err
	}

	tarFlag := "-cf"
	if profile.GetCompressSync() {
		tarFlag = "-czf"
	}
	// File list as args. For large lists, this could overflow ARG_MAX;
	// we keep it inline because the typical sync size is small. If
	// users hit ARG_MAX (~128k chars on Linux), they'll see a clear
	// "Argument list too long" error and can split via --files.
	quoted := make([]string, 0, len(files))
	for _, f := range files {
		quoted = append(quoted, srvtty.ShQuotePath(f))
	}
	remoteCmd := fmt.Sprintf("cd %s && tar %s - -- %s",
		srvtty.ShQuotePath(expanded), tarFlag, strings.Join(quoted, " "))

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		defer close(errCh)
		rc, err := c.RunStreamStdout(remoteCmd, "", pw)
		// Close the pipe so the reader unblocks. CloseWithError surfaces
		// non-zero exits as a tar-side error rather than treating them
		// as a clean EOF (which would silently truncate the local tree).
		if err != nil {
			_ = pw.CloseWithError(err)
		} else if rc != 0 {
			_ = pw.CloseWithError(fmt.Errorf("remote tar exit %d", rc))
		} else {
			_ = pw.Close()
		}
		errCh <- err
	}()

	var src io.Reader = pr
	if profile.GetCompressSync() {
		gz, gerr := gzip.NewReader(pr)
		if gerr != nil {
			<-errCh
			return 1, gerr
		}
		defer gz.Close()
		src = gz
	}
	tr := tar.NewReader(src)
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			<-errCh
			return 1, terr
		}
		if hdr.Name == "" {
			continue
		}
		// Defend against malicious paths from a compromised remote.
		// The hdr.Name should always be relative; reject ".." or
		// absolute paths up front.
		clean := filepath.Clean(hdr.Name)
		if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
			continue
		}
		dst := filepath.Join(localRoot, clean)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				<-errCh
				return 1, err
			}
		case tar.TypeReg, tar.TypeRegA: // nolint: staticcheck
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
				<-errCh
				return 1, err
			}
			f, err := os.Create(dst)
			if err != nil {
				<-errCh
				return 1, err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				<-errCh
				return 1, err
			}
			f.Close()
			_ = os.Chmod(dst, os.FileMode(hdr.Mode&0o777))
		default:
			// Symlinks / devices: skip silently. Sync stays a
			// content-only mirror; permission/symlink handling on
			// Windows is messy enough to be out of scope here.
		}
	}
	if rerr := <-errCh; rerr != nil {
		return 1, rerr
	}
	return 0, nil
}
