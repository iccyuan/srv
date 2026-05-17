package syncx

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/hooks"
	"srv/internal/mcplog"
	"srv/internal/remote"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"strconv"
	"strings"
	"time"
)

var DefaultExcludes = []string{
	".git", "node_modules", "__pycache__", ".venv", "venv",
	".idea", ".vscode", ".DS_Store", "*.pyc", "*.pyo", "*.swp",
}

type Options struct {
	RemoteRoot string
	Mode       string // git | mtime | glob | list (or empty = auto)
	GitScope   string
	NoGit      bool
	Since      string
	Include    []string
	Exclude    []string
	Files      []string
	Root       string
	DryRun     bool
	// Diff = print rsync-itemize style preview (size/mtime per file).
	// Implies DryRun (no actual transfer happens).
	Diff bool
	// Verbose = include unchanged "=" rows in the diff output.
	Verbose     bool
	Watch       bool
	Delete      bool
	Yes         bool
	DeleteLimit int
	// Pull = reverse direction (remote -> local). Mutually exclusive
	// with --watch (a pull watcher would need remote inotify and is
	// not in scope).
	Pull bool
}

func ParseOptions(args []string) *Options {
	o := &Options{GitScope: "all"}
	positional := []string{}
	requireValue := func(option string, index int) string {
		if index+1 >= len(args) {
			fmt.Fprintf(os.Stderr, "error: %s requires a value.\n", option)
			os.Exit(1)
		}
		return args[index+1]
	}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			if o.Mode == "" {
				o.Mode = "list"
			}
			o.Files = append(o.Files, args[i+1:]...)
			return o
		case a == "--git":
			o.Mode = "git"
			if i+1 < len(args) {
				next := args[i+1]
				if next == "all" || next == "staged" || next == "modified" || next == "untracked" {
					o.GitScope = next
					i += 2
					continue
				}
			}
			i++
			continue
		case a == "--all" || a == "--staged" || a == "--modified" || a == "--untracked":
			o.Mode = "git"
			o.GitScope = strings.TrimPrefix(a, "--")
			i++
			continue
		case a == "--no-git":
			o.NoGit = true
			i++
			continue
		case a == "--since":
			o.Mode = "mtime"
			o.Since = requireValue(a, i)
			i += 2
			continue
		case strings.HasPrefix(a, "--since="):
			o.Mode = "mtime"
			o.Since = strings.TrimPrefix(a, "--since=")
			i++
			continue
		case a == "--include":
			o.Mode = "glob"
			o.Include = append(o.Include, requireValue(a, i))
			i += 2
			continue
		case strings.HasPrefix(a, "--include="):
			o.Mode = "glob"
			o.Include = append(o.Include, strings.TrimPrefix(a, "--include="))
			i++
			continue
		case a == "--exclude":
			o.Exclude = append(o.Exclude, requireValue(a, i))
			i += 2
			continue
		case strings.HasPrefix(a, "--exclude="):
			o.Exclude = append(o.Exclude, strings.TrimPrefix(a, "--exclude="))
			i++
			continue
		case a == "--files":
			o.Mode = "list"
			o.Files = append(o.Files, requireValue(a, i))
			i += 2
			continue
		case a == "--root":
			o.Root = requireValue(a, i)
			i += 2
			continue
		case strings.HasPrefix(a, "--root="):
			o.Root = strings.TrimPrefix(a, "--root=")
			i++
			continue
		case a == "--dry-run":
			o.DryRun = true
			i++
			continue
		case a == "--diff":
			o.Diff = true
			i++
			continue
		case a == "-v" || a == "--verbose":
			o.Verbose = true
			i++
			continue
		case a == "--pull":
			o.Pull = true
			i++
			continue
		case a == "--watch":
			o.Watch = true
			i++
			continue
		case a == "--delete":
			o.Delete = true
			i++
			continue
		case a == "--yes" || a == "-y":
			o.Yes = true
			i++
			continue
		case a == "--delete-limit":
			n, err := strconv.Atoi(requireValue(a, i))
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "error: --delete-limit requires a non-negative integer")
				os.Exit(1)
			}
			o.DeleteLimit = n
			i += 2
			continue
		case strings.HasPrefix(a, "--delete-limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--delete-limit="))
			if err != nil || n < 0 {
				fmt.Fprintln(os.Stderr, "error: --delete-limit requires a non-negative integer")
				os.Exit(1)
			}
			o.DeleteLimit = n
			i++
			continue
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "error: unknown sync option %q\n", a)
			os.Exit(1)
		}
		positional = append(positional, a)
		i++
	}
	if len(positional) > 1 {
		fmt.Fprintf(os.Stderr, "error: only one remote root accepted, got %v\n", positional)
		os.Exit(1)
	}
	if len(positional) == 1 {
		o.RemoteRoot = positional[0]
	}
	return o
}

func GitDeletedFiles(repoRoot string) ([]string, error) {
	cmd := exec.Command("git", "-C", repoRoot, "ls-files", "--deleted", "-z")
	b, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(b))
		if detail == "" {
			detail = err.Error()
		}
		return nil, fmt.Errorf("git command failed: %s", detail)
	}
	var out []string
	for _, p := range strings.Split(string(b), "\x00") {
		if p != "" {
			out = append(out, filepath.ToSlash(p))
		}
	}
	sort.Strings(out)
	return out, nil
}

// FindGitRoot walks upward from start until it finds a directory containing
// a .git entry. Returns "" if not in a repo.
func FindGitRoot(start string) string {
	p, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(p, ".git")); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return ""
		}
		p = parent
	}
}

// GitChangedFiles runs `git ls-files`/`git diff` and returns relative paths.
func GitChangedFiles(repoRoot, scope string) ([]string, error) {
	out := map[string]struct{}{}
	runGit := func(args ...string) ([]byte, error) {
		cmd := exec.Command("git", args...)
		b, err := cmd.CombinedOutput()
		if err != nil {
			detail := strings.TrimSpace(string(b))
			if detail == "" {
				detail = err.Error()
			}
			return nil, fmt.Errorf("git command failed: %s", detail)
		}
		return b, nil
	}
	if scope == "all" || scope == "modified" || scope == "untracked" {
		flags := []string{"-C", repoRoot, "ls-files", "-z"}
		if scope == "all" || scope == "modified" {
			flags = append(flags, "--modified")
		}
		if scope == "all" || scope == "untracked" {
			flags = append(flags, "--others", "--exclude-standard")
		}
		b, err := runGit(flags...)
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(string(b), "\x00") {
			if p != "" {
				out[p] = struct{}{}
			}
		}
	}
	if scope == "all" || scope == "staged" {
		b, err := runGit("-C", repoRoot, "diff", "--name-only", "--cached", "-z")
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(string(b), "\x00") {
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

// parseDuration parses '2h', '30m', '1d', '90s' (or bare digits = seconds).
func parseDuration(s string) (time.Duration, error) {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return 0, fmt.Errorf("--since requires a duration (e.g. \"2h\", \"30m\")")
	}
	last := s[len(s)-1]
	mul := time.Second
	body := s
	switch last {
	case 's':
		mul = time.Second
		body = s[:len(s)-1]
	case 'm':
		mul = time.Minute
		body = s[:len(s)-1]
	case 'h':
		mul = time.Hour
		body = s[:len(s)-1]
	case 'd':
		mul = 24 * time.Hour
		body = s[:len(s)-1]
	}
	n, err := strconv.ParseFloat(body, 64)
	if err != nil {
		return 0, fmt.Errorf("bad duration %q (expected like '2h', '30m', '1d', '90s')", s)
	}
	return time.Duration(n * float64(mul)), nil
}

func mtimeChangedFiles(root, since string, excludes []string) ([]string, error) {
	dur, err := parseDuration(since)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-dur)
	skipDirs := map[string]bool{
		"__pycache__": true, ".git": true, "node_modules": true,
		".venv": true, "venv": true, ".idea": true, ".vscode": true,
	}
	var out []string
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().Before(cutoff) {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if !matchesAnyExclude(rel, excludes) {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

func globFiles(root string, patterns []string) []string {
	out := map[string]struct{}{}
	for _, pat := range patterns {
		// filepath.Glob doesn't support **; use doublestar-like manual walk.
		matches := globMatches(root, pat)
		for _, m := range matches {
			rel, _ := filepath.Rel(root, m)
			out[filepath.ToSlash(rel)] = struct{}{}
		}
	}
	files := make([]string, 0, len(out))
	for k := range out {
		files = append(files, k)
	}
	sort.Strings(files)
	return files
}

// globMatches returns absolute paths matching `pat` rooted at `root`.
// Supports ** in patterns by walking the tree.
func globMatches(root, pat string) []string {
	var out []string
	full := filepath.Join(root, pat)
	full = filepath.ToSlash(full)

	// Simple case: no '**' -> use filepath.Glob directly.
	if !strings.Contains(pat, "**") {
		matches, _ := filepath.Glob(full)
		for _, m := range matches {
			st, err := os.Stat(m)
			if err == nil && !st.IsDir() {
				out = append(out, m)
			}
		}
		return out
	}
	// Walk and match -- treat '**' as zero-or-more path components.
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if matchDoubleStar(pat, rel) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// matchDoubleStar tests `rel` against `pat`, where ** matches any sequence
// of characters (including /).
func matchDoubleStar(pat, rel string) bool {
	regex := globToRegex(pat)
	return srvutil.RegexMatch(regex, rel)
}

// globToRegex converts a shell glob with ** into a regex.
func globToRegex(pat string) string {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pat) {
		c := pat[i]
		switch c {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				b.WriteString(".*")
				i += 2
				if i < len(pat) && pat[i] == '/' {
					i++
				}
			} else {
				b.WriteString("[^/]*")
				i++
			}
		case '?':
			b.WriteString("[^/]")
			i++
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	b.WriteString("$")
	return b.String()
}

// matchesAnyExclude returns true if `path` should be excluded after
// evaluating every pattern in `patterns` in order. Supports:
//   - direct fnmatch on the full path
//   - matching a single path component (so 'node_modules' excludes 'a/node_modules/b')
//   - gitignore-style negation: a leading "!" turns the pattern into a
//     re-include rule. Later matches override earlier ones, so a
//     `.srvignore` of `*.log\n!important.log` keeps important.log.
func matchesAnyExclude(path string, patterns []string) bool {
	norm := strings.ReplaceAll(path, "\\", "/")
	parts := strings.Split(norm, "/")
	excluded := false
	for _, raw := range patterns {
		neg := false
		pat := raw
		if strings.HasPrefix(pat, "!") {
			neg = true
			pat = pat[1:]
		}
		pat = strings.TrimRight(pat, "/")
		if pat == "" {
			continue
		}
		match := false
		if strings.ContainsAny(pat, "/*") && srvutil.RegexMatch(globToRegex(pat), norm) {
			match = true
		}
		if !match {
			for _, part := range parts {
				if srvutil.RegexMatch(globToRegex(pat), part) {
					match = true
					break
				}
			}
		}
		if match {
			excluded = !neg
		}
	}
	return excluded
}

// normalizeForTar makes the path relative to root (forward slashes), or
// returns "" if outside.
func normalizeForTar(root, p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return ""
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return ""
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func CollectFiles(o *Options, localRoot string, allExcludes []string) ([]string, error) {
	var files []string
	switch o.Mode {
	case "git":
		var err error
		files, err = GitChangedFiles(localRoot, o.GitScope)
		if err != nil {
			return nil, err
		}
	case "mtime":
		var err error
		files, err = mtimeChangedFiles(localRoot, o.Since, allExcludes)
		if err != nil {
			return nil, err
		}
	case "glob":
		if len(o.Include) == 0 {
			return nil, fmt.Errorf("--include requires at least one pattern")
		}
		files = globFiles(localRoot, o.Include)
	case "list":
		if len(o.Files) == 0 {
			return nil, fmt.Errorf("--files requires at least one path")
		}
		for _, p := range o.Files {
			// Resolve a relative file against localRoot, NOT the
			// process CWD. glob/mtime/git modes all operate within
			// localRoot; list used to Abs() against CWD, so passing
			// `root` + relative `files` (the MCP sync tool's normal
			// shape) silently dropped every file as "outside root"
			// and returned "(nothing to sync)" with no error.
			fp := p
			if !filepath.IsAbs(fp) {
				fp = filepath.Join(localRoot, fp)
			}
			rel := normalizeForTar(localRoot, fp)
			if rel == "" {
				fmt.Fprintf(os.Stderr, "warning: skipping %q (outside local root)\n", p)
				continue
			}
			files = append(files, rel)
		}
	default:
		return nil, fmt.Errorf("internal: no sync mode resolved")
	}

	excludes := o.Exclude
	if o.Mode != "list" {
		excludes = allExcludes
	}
	if len(excludes) > 0 {
		filtered := files[:0]
		for _, f := range files {
			if !matchesAnyExclude(f, excludes) {
				filtered = append(filtered, f)
			}
		}
		files = filtered
	}

	// Drop entries that no longer exist.
	out := files[:0]
	for _, f := range files {
		if st, err := os.Stat(filepath.Join(localRoot, f)); err == nil && !st.IsDir() {
			out = append(out, f)
		}
	}
	return out, nil
}

func CollectDeletes(o *Options, localRoot string, allExcludes []string) ([]string, error) {
	if !o.Delete {
		return nil, nil
	}
	if o.Mode != "git" {
		return nil, fmt.Errorf("--delete currently requires git mode")
	}
	deletes, err := GitDeletedFiles(localRoot)
	if err != nil {
		return nil, err
	}
	if len(allExcludes) > 0 {
		filtered := deletes[:0]
		for _, f := range deletes {
			if !matchesAnyExclude(f, allExcludes) {
				filtered = append(filtered, f)
			}
		}
		deletes = filtered
	}
	return deletes, nil
}

// TarUploadStream builds a tar stream of files (rooted at localRoot) entirely
// in Go and pipes it into a remote `tar -xf -` running in o.RemoteRoot.
// Gzips the stream when profile.CompressSync is true (default) -- typical
// 70% reduction on text/code, ~ms-level CPU cost.
func TarUploadStream(profile *config.Profile, localRoot string, files []string, remoteRoot string) (int, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()

	expanded, err := c.ExpandRemoteHome(remoteRoot)
	if err != nil {
		return 1, err
	}
	tarFlag := "-xf"
	if profile.GetCompressSync() {
		tarFlag = "-xzf"
	}
	remoteCmd := fmt.Sprintf("mkdir -p %s && cd %s && tar %s -",
		srvtty.ShQuotePath(expanded), srvtty.ShQuotePath(expanded), tarFlag)

	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		// Recover any panic in here -- without this, an OS-level IO
		// hiccup or malformed file metadata would crash the entire
		// process. Especially important under MCP, where the parent
		// process death looks to Claude Code like an unrecoverable
		// "tools no longer available" event. We pump the panic into
		// errCh as an error so the caller sees the same channel signal
		// it would for an ordinary tar/copy failure.
		defer func() {
			if r := recover(); r != nil {
				_ = pw.CloseWithError(fmt.Errorf("tar producer panic: %v", r))
				select {
				case errCh <- fmt.Errorf("tar producer panic: %v", r):
				default:
				}
				mcplog.Logf("tar producer panic: %v", r)
			}
		}()
		defer pw.Close()
		// Sink chain: tar -> [gzip ->] pw
		var sink io.WriteCloser = pw
		if profile.GetCompressSync() {
			sink = gzip.NewWriter(pw)
		}
		tw := tar.NewWriter(sink)
		writeFiles := func() error {
			for _, f := range files {
				full := filepath.Join(localRoot, f)
				info, err := os.Stat(full)
				if err != nil {
					return err
				}
				hdr, err := tar.FileInfoHeader(info, "")
				if err != nil {
					return err
				}
				hdr.Name = f
				if err := tw.WriteHeader(hdr); err != nil {
					return err
				}
				if info.IsDir() {
					continue
				}
				src, oerr := os.Open(full)
				if oerr != nil {
					return oerr
				}
				_, copyErr := io.Copy(tw, src)
				src.Close()
				if copyErr != nil {
					return copyErr
				}
			}
			return nil
		}
		err := writeFiles()
		// Order matters: tar.Writer.Close flushes its trailer; the gzip
		// writer must Close after that to write its own trailer; only
		// then can pw close so the remote tar sees clean EOF.
		if cerr := tw.Close(); err == nil {
			err = cerr
		}
		if sink != pw {
			if cerr := sink.Close(); err == nil {
				err = cerr
			}
		}
		errCh <- err
	}()

	rc, runErr := c.RunStreamStdin(remoteCmd, pr)
	tarErr := <-errCh
	if tarErr != nil {
		return 1, tarErr
	}
	return rc, runErr
}

func DeleteRemoteFiles(profile *config.Profile, remoteRoot string, files []string) (int, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()
	expanded, err := c.ExpandRemoteHome(remoteRoot)
	if err != nil {
		return 1, err
	}
	parts := make([]string, 0, len(files))
	for _, f := range files {
		if f == "" || strings.HasPrefix(f, "../") || filepath.IsAbs(f) {
			continue
		}
		parts = append(parts, srvtty.ShQuotePath(f))
	}
	if len(parts) == 0 {
		return 0, nil
	}
	cmd := fmt.Sprintf("cd %s && rm -f -- %s", srvtty.ShQuotePath(expanded), strings.Join(parts, " "))
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return 1, err
	}
	if res.ExitCode != 0 {
		return res.ExitCode, fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return 0, nil
}

func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	o := ParseOptions(args)
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}

	// local root
	localRoot := o.Root
	if localRoot == "" {
		if o.Mode == "git" || (o.Mode == "" && !o.NoGit) {
			localRoot = FindGitRoot(MustCwd())
		}
		if localRoot == "" {
			localRoot = MustCwd()
		}
	}
	abs, err := filepath.Abs(localRoot)
	if err != nil {
		return clierr.Errf(1, "error: bad local root: %v", err)
	}
	localRoot = abs
	if st, err := os.Stat(localRoot); err != nil || !st.IsDir() {
		return clierr.Errf(1, "error: local root not a directory: %s", localRoot)
	}

	// auto-detect mode
	if o.Mode == "" {
		if !o.NoGit && FindGitRoot(localRoot) != "" {
			o.Mode = "git"
		} else {
			reason := "not in a git repo"
			if o.NoGit {
				reason = "git auto-detect disabled (--no-git)"
			}
			return clierr.Errf(1, "error: %s. Specify --include / --since / --files.", reason)
		}
	}

	// remote root
	cwd := config.GetCwd(name, profile)
	if o.RemoteRoot != "" {
		o.RemoteRoot = remote.ResolvePath(o.RemoteRoot, cwd)
	} else if profile.SyncRoot != "" {
		o.RemoteRoot = remote.ResolvePath(profile.SyncRoot, cwd)
	}
	if o.RemoteRoot == "" {
		// Final fallback: session cwd (or `~` if no session). Both push
		// and pull need a concrete remote root, and historically users
		// running `srv sync` from inside their working dir expected it
		// to land in the active remote cwd. Pull especially relies on
		// this -- otherwise RemoteGitChangedFiles has nothing to cd to.
		o.RemoteRoot = cwd
		if o.RemoteRoot == "" {
			o.RemoteRoot = "~"
		}
	}

	allExcludes := append([]string{}, o.Exclude...)
	allExcludes = append(allExcludes, profile.SyncExclude...)
	allExcludes = append(allExcludes, DefaultExcludes...)
	// .srvignore goes LAST so negation (`!pattern`) can re-include files
	// the user otherwise wants to skip via DefaultExcludes / profile.
	allExcludes = append(allExcludes, LoadIgnoreFile(localRoot)...)

	// Validate flag combos that don't make sense for the chosen direction.
	if o.Pull {
		if o.Delete {
			return clierr.Errf(1, "error: --pull --delete is not supported (would clobber local files; remove manually instead)")
		}
		if o.Watch {
			return clierr.Errf(1, "error: --pull --watch is not supported (needs remote inotify)")
		}
	}

	var files []string
	if o.Pull {
		files, err = collectPullFiles(o, profile)
	} else {
		files, err = CollectFiles(o, localRoot, allExcludes)
	}
	if err != nil {
		return clierr.Errf(1, "error: %v", err)
	}
	// Post-filter pull results through the same exclude pipeline so
	// .srvignore and --exclude still keep junk out of the local tree.
	if o.Pull && len(allExcludes) > 0 {
		filtered := files[:0]
		for _, f := range files {
			if !matchesAnyExclude(f, allExcludes) {
				filtered = append(filtered, f)
			}
		}
		files = filtered
	}

	var deletes []string
	if o.Delete && !o.Pull {
		limit := o.DeleteLimit
		if limit == 0 {
			limit = 20
		}
		deletes, err = CollectDeletes(o, localRoot, allExcludes)
		if err != nil {
			return clierr.Errf(1, "error: %v", err)
		}
		if len(deletes) > limit && !o.DryRun && !o.Yes {
			return clierr.Errf(1, "error: --delete would remove %d files (limit %d). Re-run with --dry-run, --yes, or --delete-limit N.", len(deletes), limit)
		}
	}

	target := profile.Host
	if profile.User != "" {
		target = profile.User + "@" + profile.Host
	}
	direction := "push"
	if o.Pull {
		direction = "pull"
	}
	header := fmt.Sprintf("mode    : %s (%s)", o.Mode, direction)
	if o.Mode == "git" {
		header += " (" + o.GitScope + ")"
	} else if o.Mode == "mtime" {
		header += " since " + o.Since
	}
	fmt.Fprintln(os.Stderr, header)
	fmt.Fprintf(os.Stderr, "local   : %s\n", localRoot)
	fmt.Fprintf(os.Stderr, "remote  : %s:%s\n", target, o.RemoteRoot)
	fmt.Fprintf(os.Stderr, "files   : %d\n", len(files))
	if len(deletes) > 0 {
		fmt.Fprintf(os.Stderr, "delete  : %d\n", len(deletes))
	}
	if len(files) == 0 && len(deletes) == 0 {
		fmt.Fprintln(os.Stderr, "(nothing to sync)")
		return nil
	}
	// --diff is a richer dry-run: skip the plain file list, print the
	// itemize preview, and never transfer. --dry-run keeps the cheap
	// path because users sometimes pipe `srv sync --dry-run` into
	// scripts and don't want the extra remote stat round-trip.
	if o.Diff {
		remoteStats, ferr := FetchRemoteStats(profile, o.RemoteRoot, files)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "warning: remote stat failed (%v); showing local-only view\n", ferr)
			remoteStats = map[string]RemoteStat{}
		}
		var entries []DiffEntry
		if o.Pull {
			entries = BuildDiffPull(localRoot, files, remoteStats)
		} else {
			entries = BuildDiff(localRoot, files, remoteStats, deletes)
		}
		fmt.Print(PrintDiff(entries, o.Verbose))
		fmt.Fprintln(os.Stderr, "(--diff: preview only, nothing transferred)")
		return nil
	}
	listed := files
	if len(listed) > 200 {
		listed = listed[:200]
	}
	for _, f := range listed {
		fmt.Fprintf(os.Stderr, "  %s\n", f)
	}
	if len(files) > 200 {
		fmt.Fprintf(os.Stderr, "  ... (%d more)\n", len(files)-200)
	}
	for _, f := range deletes {
		fmt.Fprintf(os.Stderr, "  delete %s\n", f)
	}
	if o.DryRun {
		fmt.Fprintln(os.Stderr, "(dry-run, not transferred)")
		return nil
	}
	hookBase := hooks.Event{
		Profile: name,
		Host:    profile.Host,
		User:    profile.User,
		Port:    profile.GetPort(),
		Cwd:     localRoot,
		Local:   localRoot,
		Target:  o.RemoteRoot,
	}
	hookBase.Name = "pre-sync"
	hooks.Run(hookBase)
	var rc int
	if o.Pull {
		rc, err = TarDownloadStream(profile, o.RemoteRoot, files, localRoot)
	} else {
		rc, err = TarUploadStream(profile, localRoot, files, o.RemoteRoot)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if rc == 0 && len(deletes) > 0 {
		if drc, derr := DeleteRemoteFiles(profile, o.RemoteRoot, deletes); derr != nil {
			fmt.Fprintln(os.Stderr, derr)
			return clierr.Code(1)
		} else if drc != 0 {
			return clierr.Code(drc)
		}
	}
	hookBase.Name = "post-sync"
	hookBase.Exit = rc
	hooks.Run(hookBase)
	if o.Watch {
		fmt.Fprintln(os.Stderr)
		return clierr.Code(runWatch(o, profile, localRoot, o.RemoteRoot, allExcludes))
	}
	return clierr.Code(rc)
}

func MustCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	return wd
}
