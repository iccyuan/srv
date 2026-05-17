package syncx

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"srv/internal/config"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strconv"
	"strings"
)

// RemoteStat captures the minimum metadata needed to decide whether a
// file would change on the remote. Size is bytes; MtimeUnix is the
// remote-reported mtime in seconds since the epoch. Missing=true means
// the file does not exist on the remote (so a sync would create it).
type RemoteStat struct {
	Missing   bool
	Size      int64
	MtimeUnix int64
}

// DiffEntry is one row of the `sync --diff` itemize output. Action
// follows a rsync-ish convention:
//
//	'+'   new on remote (file would be created)
//	'>'   would update (size or mtime differ; local newer or sizes mismatch)
//	'<'   local appears OLDER than remote (push still overwrites; warning)
//	'='   identical (size + mtime match within FS tolerance)
//	'-'   tracked-deletion: file would be removed on remote (only when --delete is on)
type DiffEntry struct {
	Action byte
	Path   string
	Local  RemoteStat // local stat (Missing unused)
	Remote RemoteStat // remote stat
}

// FetchRemoteStats stat()s every file in `files` (relative to
// remoteRoot) using one ssh exec. NUL-separated stdin keeps it safe for
// odd file names. Output is `EXIST|<size>|<mtime>|<path>\n` for files
// that exist, or `MISS|||<path>\n` for files that don't. Missing
// entries surface as RemoteStat.Missing=true rather than as an error so
// callers can present "new file" rows in the diff.
//
// Cost: one SSH session, one remote shell loop. ~50ms over a warm
// pooled SSH connection for ~1000 files. We deliberately don't batch
// into multiple round-trips -- the loop runs entirely on the remote.
func FetchRemoteStats(profile *config.Profile, remoteRoot string, files []string) (map[string]RemoteStat, error) {
	if len(files) == 0 {
		return map[string]RemoteStat{}, nil
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

	// The remote loop uses `printf '%s\0'` between cells so any embedded
	// pipe / newline in a path doesn't confuse the local parser. The
	// trailing literal NUL also delimits records.
	script := `cd ` + srvtty.ShQuotePath(expanded) + ` && while IFS= read -r -d "" f; do
  if [ -e "$f" ]; then
    s=$(stat -c "%s|%Y" -- "$f" 2>/dev/null || stat -f "%z|%m" -- "$f" 2>/dev/null)
    printf "EXIST|%s|%s\x00" "$s" "$f"
  else
    printf "MISS|||%s\x00" "$f"
  fi
done`

	// NUL-separated stdin.
	var buf bytes.Buffer
	for _, f := range files {
		buf.WriteString(f)
		buf.WriteByte(0)
	}
	res, err := c.RunCaptureStdin(script, "", &buf)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("remote stat: exit %d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
	}

	out := make(map[string]RemoteStat, len(files))
	for _, rec := range strings.Split(res.Stdout, "\x00") {
		rec = strings.TrimRight(rec, "\n")
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "|", 4)
		if len(parts) < 4 {
			continue
		}
		kind, size, mtime, name := parts[0], parts[1], parts[2], parts[3]
		switch kind {
		case "MISS":
			out[name] = RemoteStat{Missing: true}
		case "EXIST":
			sz, _ := strconv.ParseInt(size, 10, 64)
			mt, _ := strconv.ParseInt(mtime, 10, 64)
			out[name] = RemoteStat{Size: sz, MtimeUnix: mt}
		}
	}
	return out, nil
}

// BuildDiff compares local and remote stats for each file and returns
// an ordered slice of DiffEntry. localRoot is the on-disk root; files
// are relative paths.
//
// deletes (non-nil only when --delete is active) get '-' rows appended
// at the end so the preview surfaces them in the same view.
func BuildDiff(localRoot string, files []string, remoteStats map[string]RemoteStat, deletes []string) []DiffEntry {
	entries := make([]DiffEntry, 0, len(files)+len(deletes))
	for _, f := range files {
		local := localStatFor(localRoot, f)
		remote, ok := remoteStats[f]
		if !ok {
			// Map miss = treat as missing on remote (new file). This
			// covers the case where FetchRemoteStats returned a partial
			// result (or wasn't called at all because the stat ssh
			// failed); a missing entry is the safer guess than "same".
			remote = RemoteStat{Missing: true}
		}
		entries = append(entries, classify(f, local, remote))
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	for _, d := range deletes {
		entries = append(entries, DiffEntry{Action: '-', Path: d})
	}
	return entries
}

func localStatFor(root, rel string) RemoteStat {
	st, err := os.Stat(filepath.Join(root, rel))
	if err != nil {
		return RemoteStat{Missing: true}
	}
	return RemoteStat{Size: st.Size(), MtimeUnix: st.ModTime().Unix()}
}

func classify(path string, local, remote RemoteStat) DiffEntry {
	if remote.Missing {
		return DiffEntry{Action: '+', Path: path, Local: local, Remote: remote}
	}
	if local.Size == remote.Size {
		// 2-second tolerance covers FAT/NTFS rounding when working
		// across Windows hosts; same-size + same-mtime is good
		// enough to call "identical" without burning bandwidth on a
		// content hash.
		if abs(local.MtimeUnix-remote.MtimeUnix) <= 2 {
			return DiffEntry{Action: '=', Path: path, Local: local, Remote: remote}
		}
	}
	if local.MtimeUnix < remote.MtimeUnix {
		return DiffEntry{Action: '<', Path: path, Local: local, Remote: remote}
	}
	return DiffEntry{Action: '>', Path: path, Local: local, Remote: remote}
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}

// PrintDiff renders entries to w in a compact rsync-itemize style.
// `verbose` controls whether unchanged ('=') entries are shown.
func PrintDiff(entries []DiffEntry, verbose bool) string {
	var sb strings.Builder
	var nNew, nMod, nOld, nSame, nDel int
	for _, e := range entries {
		switch e.Action {
		case '+':
			nNew++
		case '>':
			nMod++
		case '<':
			nOld++
		case '=':
			nSame++
		case '-':
			nDel++
		}
		if e.Action == '=' && !verbose {
			continue
		}
		switch e.Action {
		case '+':
			fmt.Fprintf(&sb, "+ %s   (new, %s)\n", e.Path, humanBytes(e.Local.Size))
		case '>':
			fmt.Fprintf(&sb, "> %s   (update, %s -> %s)\n", e.Path,
				humanBytes(e.Remote.Size), humanBytes(e.Local.Size))
		case '<':
			fmt.Fprintf(&sb, "< %s   (LOCAL OLDER, would overwrite remote %s)\n",
				e.Path, humanBytes(e.Remote.Size))
		case '=':
			fmt.Fprintf(&sb, "= %s\n", e.Path)
		case '-':
			fmt.Fprintf(&sb, "- %s   (delete)\n", e.Path)
		}
	}
	fmt.Fprintf(&sb, "\nsummary: %d new, %d modified, %d local-older, %d unchanged, %d to delete\n",
		nNew, nMod, nOld, nSame, nDel)
	return sb.String()
}

func humanBytes(n int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.1fG", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.1fM", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1fK", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%dB", n)
	}
}
