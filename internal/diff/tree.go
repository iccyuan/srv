// Tree-level diff: walk a local directory and a remote directory,
// compare each by (size, mtime), and emit a per-file status table.
//
// Where this fits: `srv diff` (single file) and `srv sync --diff`
// (rsync-itemize preview of what a sync WOULD change) already exist.
// Tree fills the gap of "I just want to know how these directories
// differ, I'm not planning to sync" -- shape decisions like which
// files are stale on which side without committing to a transfer.
package diff

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"strconv"
	"strings"
)

// fileStat is the minimum the comparator needs: bytes and mtime as a
// unix timestamp. We deliberately don't carry mode/owner -- syncx
// already stays content-only, and a "mode mismatch" finding would be
// noisy on Windows-as-source cases (Git on Windows mangles +x).
type fileStat struct {
	size  int64
	mtime int64 // unix epoch seconds
}

// CmdTree implements `srv diff --tree <local_dir> [remote_dir]`. The
// remote path defaults to the same string as the local path, then
// gets anchored against the session cwd by remote.ResolvePath, which
// matches the existing single-file `srv diff` behavior.
//
// Verbose (`-v`) flips on emission of `=` rows (identical files);
// without it, only differing or one-sided files print. Same convention
// as `srv sync --diff -v`.
func CmdTree(args []string, cfg *config.Config, profileOverride string) error {
	verbose := false
	var positional []string
	for _, a := range args {
		switch a {
		case "-v", "--verbose":
			verbose = true
		default:
			if strings.HasPrefix(a, "-") {
				return srvutil.Errf(2, "unknown --tree option %q", a)
			}
			positional = append(positional, a)
		}
	}
	if len(positional) == 0 {
		return srvutil.Errf(2, "usage: srv diff --tree <local_dir> [remote_dir] [-v]")
	}
	local := positional[0]
	remoteArg := local
	if len(positional) > 1 {
		remoteArg = positional[1]
	}
	st, err := os.Stat(local)
	if err != nil {
		return srvutil.Errf(1, "local %q: %v", local, err)
	}
	if !st.IsDir() {
		return srvutil.Errf(1, "local %q is not a directory; use `srv diff` for single files", local)
	}
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return srvutil.Errf(1, "%v", err)
	}
	remotePath := remote.ResolvePath(remoteArg, config.GetCwd(name, profile))

	localTree, err := walkLocalTree(local)
	if err != nil {
		return srvutil.Errf(1, "walking local: %v", err)
	}
	remoteTree, err := walkRemoteTree(profile, remotePath)
	if err != nil {
		return srvutil.Errf(1, "walking remote: %v", err)
	}
	fmt.Print(renderTreeDiff(local, remotePath, localTree, remoteTree, verbose))
	return nil
}

// walkLocalTree returns POSIX-relative paths under root mapped to
// (size, mtime). Hidden files / directories are included -- this is
// a diff tool, not a sync, so showing dot-files is the safer default.
func walkLocalTree(root string) (map[string]fileStat, error) {
	out := map[string]fileStat{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		out[filepath.ToSlash(rel)] = fileStat{
			size:  info.Size(),
			mtime: info.ModTime().Unix(),
		}
		return nil
	})
	return out, err
}

// walkRemoteTree runs a single SSH command to enumerate files under
// remoteRoot with (size, mtime). Linux uses GNU find's `-printf`;
// macOS uses BSD find with `-exec stat -f`. The platform comes from
// the profile (auto-detected and cached -- see remote.GetPlatform).
//
// One round trip, one parse. Falls back across both formats on
// PlatformUnknown so a non-Linux/non-Darwin remote (illumos, WSL with
// busybox) still works without needing the user to set `platform`.
func walkRemoteTree(profile *config.Profile, remoteRoot string) (map[string]fileStat, error) {
	quoted := srvtty.ShQuotePath(remoteRoot)
	plat := remote.GetPlatform(profile)

	gnu := fmt.Sprintf(`cd %s && find . -type f -printf '%%P\t%%s\t%%T@\n'`, quoted)
	bsd := fmt.Sprintf(`cd %s && find . -type f -exec stat -f '%%N	%%z	%%m' {} +`, quoted)

	scripts := []string{gnu, bsd}
	if plat == remote.PlatformDarwin {
		scripts = []string{bsd, gnu}
	}

	var lastErr error
	for _, script := range scripts {
		res, err := remote.RunCapture(profile, "", script)
		if err != nil {
			lastErr = err
			continue
		}
		if res.ExitCode != 0 {
			lastErr = fmt.Errorf("remote find exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
			continue
		}
		return parseRemoteTreeLines(res.Stdout), nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no remote find variant succeeded")
	}
	return nil, lastErr
}

// parseRemoteTreeLines accepts the tab-separated output of either
// `find -printf '%P\t%s\t%T@\n'` (GNU) or
// `find ... -exec stat -f '%N\t%z\t%m' {} +` (BSD).
//
// Tolerant of:
//   - leading "./" from BSD stat (%N echoes the find arg, which starts
//     with "./" because we cd'd in)
//   - fractional epoch (GNU's %T@ has .NNN suffix; we truncate)
//   - blank lines / extra whitespace (BSD prints trailing newline)
//
// Lines that don't parse are dropped silently rather than aborting --
// the result is a best-effort tree view, not a transactional manifest.
func parseRemoteTreeLines(s string) map[string]fileStat {
	out := map[string]fileStat{}
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r ")
		if line == "" {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimPrefix(parts[0], "./")
		size, err := strconv.ParseInt(parts[1], 10, 64)
		if err != nil {
			continue
		}
		mtime := parts[2]
		if dot := strings.IndexByte(mtime, '.'); dot >= 0 {
			mtime = mtime[:dot]
		}
		mt, err := strconv.ParseInt(mtime, 10, 64)
		if err != nil {
			continue
		}
		out[name] = fileStat{size: size, mtime: mt}
	}
	return out
}

// renderTreeDiff produces the output table. Pure function -- no I/O,
// no profile -- so it's easily covered by the test alongside the
// classify and humanizeBytes helpers.
//
// Status legend (mirrors the existing `srv sync --diff` shape so users
// don't have to learn a second vocabulary):
//
//   - remote-only file (would be NEW if pulled)
//   - local-only file  (would be NEW if pushed)
//     < local is newer   (size/mtime differ; local mtime > remote mtime)
//     > remote is newer  (size/mtime differ; remote mtime > local mtime)
//     ~ both differ but mtimes are within 2s of each other (ambiguous side)
//     = identical (only with -v)
func renderTreeDiff(localRoot, remoteRoot string, local, remote map[string]fileStat, verbose bool) string {
	type row struct {
		action rune
		path   string
		ldesc  string
		rdesc  string
	}
	keys := unionKeys(local, remote)
	rows := make([]row, 0, len(keys))
	var added, removed, changed, same int
	for _, k := range keys {
		l, lok := local[k]
		r, rok := remote[k]
		switch {
		case !lok && rok:
			rows = append(rows, row{action: '+', path: k, ldesc: "-", rdesc: humanizeBytes(r.size)})
			added++
		case lok && !rok:
			rows = append(rows, row{action: '-', path: k, ldesc: humanizeBytes(l.size), rdesc: "-"})
			removed++
		default:
			if l.size == r.size && abs64(l.mtime-r.mtime) <= 2 {
				same++
				if verbose {
					rows = append(rows, row{action: '=', path: k, ldesc: humanizeBytes(l.size), rdesc: humanizeBytes(r.size)})
				}
				continue
			}
			act := '~'
			switch {
			case l.mtime > r.mtime+2:
				act = '<'
			case r.mtime > l.mtime+2:
				act = '>'
			}
			rows = append(rows, row{action: act, path: k,
				ldesc: humanizeBytes(l.size), rdesc: humanizeBytes(r.size)})
			changed++
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "local:  %s\nremote: %s\n\n", localRoot, remoteRoot)
	if len(rows) == 0 {
		sb.WriteString("(no differences)\n")
	} else {
		// Compute column widths from observed rows.
		maxPath := 4
		maxL := 5
		maxR := 6
		for _, r := range rows {
			if len(r.path) > maxPath {
				maxPath = len(r.path)
			}
			if len(r.ldesc) > maxL {
				maxL = len(r.ldesc)
			}
			if len(r.rdesc) > maxR {
				maxR = len(r.rdesc)
			}
		}
		fmt.Fprintf(&sb, "  %-*s  %-*s  %-*s\n", maxPath, "path", maxL, "local", maxR, "remote")
		for _, r := range rows {
			fmt.Fprintf(&sb, "%c %-*s  %-*s  %-*s\n",
				r.action, maxPath, r.path, maxL, r.ldesc, maxR, r.rdesc)
		}
	}
	fmt.Fprintf(&sb, "\nsummary: +%d  -%d  ~%d  =%d\n", added, removed, changed, same)
	return sb.String()
}

func unionKeys(a, b map[string]fileStat) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// humanizeBytes renders a byte count as "123B" / "4.5K" / "1.2M" /
// "3.4G". Powers of 1024. The threshold for "K" is 1024 so the
// boundary case of exactly 1024 bytes reads as "1.0K", not "1024B".
func humanizeBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%dB", n)
	}
	units := []string{"K", "M", "G", "T", "P"}
	v := float64(n) / 1024.0
	idx := 0
	for v >= 1024 && idx < len(units)-1 {
		v /= 1024
		idx++
	}
	return fmt.Sprintf("%.1f%s", v, units[idx])
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
