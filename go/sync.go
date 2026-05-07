package main

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

var defaultSyncExcludes = []string{
	".git", "node_modules", "__pycache__", ".venv", "venv",
	".idea", ".vscode", ".DS_Store", "*.pyc", "*.pyo", "*.swp",
}

type syncOpts struct {
	remoteRoot string
	mode       string // git | mtime | glob | list (or empty = auto)
	gitScope   string
	noGit      bool
	since      string
	include    []string
	exclude    []string
	files      []string
	root       string
	dryRun     bool
	watch      bool
}

func parseSyncOpts(args []string) *syncOpts {
	o := &syncOpts{gitScope: "all"}
	positional := []string{}
	requireValue := func(option string, index int) string {
		if index+1 >= len(args) {
			fatal("error: %s requires a value.", option)
		}
		return args[index+1]
	}
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case a == "--":
			if o.mode == "" {
				o.mode = "list"
			}
			o.files = append(o.files, args[i+1:]...)
			return o
		case a == "--git":
			o.mode = "git"
			if i+1 < len(args) {
				next := args[i+1]
				if next == "all" || next == "staged" || next == "modified" || next == "untracked" {
					o.gitScope = next
					i += 2
					continue
				}
			}
			i++
			continue
		case a == "--all" || a == "--staged" || a == "--modified" || a == "--untracked":
			o.mode = "git"
			o.gitScope = strings.TrimPrefix(a, "--")
			i++
			continue
		case a == "--no-git":
			o.noGit = true
			i++
			continue
		case a == "--since":
			o.mode = "mtime"
			o.since = requireValue(a, i)
			i += 2
			continue
		case strings.HasPrefix(a, "--since="):
			o.mode = "mtime"
			o.since = strings.TrimPrefix(a, "--since=")
			i++
			continue
		case a == "--include":
			o.mode = "glob"
			o.include = append(o.include, requireValue(a, i))
			i += 2
			continue
		case strings.HasPrefix(a, "--include="):
			o.mode = "glob"
			o.include = append(o.include, strings.TrimPrefix(a, "--include="))
			i++
			continue
		case a == "--exclude":
			o.exclude = append(o.exclude, requireValue(a, i))
			i += 2
			continue
		case strings.HasPrefix(a, "--exclude="):
			o.exclude = append(o.exclude, strings.TrimPrefix(a, "--exclude="))
			i++
			continue
		case a == "--files":
			o.mode = "list"
			o.files = append(o.files, requireValue(a, i))
			i += 2
			continue
		case a == "--root":
			o.root = requireValue(a, i)
			i += 2
			continue
		case strings.HasPrefix(a, "--root="):
			o.root = strings.TrimPrefix(a, "--root=")
			i++
			continue
		case a == "--dry-run":
			o.dryRun = true
			i++
			continue
		case a == "--watch":
			o.watch = true
			i++
			continue
		case strings.HasPrefix(a, "-"):
			fatal("error: unknown sync option %q", a)
		}
		positional = append(positional, a)
		i++
	}
	if len(positional) > 1 {
		fatal("error: only one remote root accepted, got %v", positional)
	}
	if len(positional) == 1 {
		o.remoteRoot = positional[0]
	}
	return o
}

// findGitRoot walks upward from start until it finds a directory containing
// a .git entry. Returns "" if not in a repo.
func findGitRoot(start string) string {
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

// gitChangedFiles runs `git ls-files`/`git diff` and returns relative paths.
func gitChangedFiles(repoRoot, scope string) ([]string, error) {
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
		return 0, fmt.Errorf("empty duration")
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
	return regexMatch(regex, rel)
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

// matchesAnyExclude returns true if any pattern matches the path. Supports:
//   - direct fnmatch on the full path
//   - matching a single path component (so 'node_modules' excludes 'a/node_modules/b')
func matchesAnyExclude(path string, patterns []string) bool {
	norm := strings.ReplaceAll(path, "\\", "/")
	parts := strings.Split(norm, "/")
	for _, raw := range patterns {
		pat := strings.TrimRight(raw, "/")
		if strings.ContainsAny(pat, "/*") && regexMatch(globToRegex(pat), norm) {
			return true
		}
		for _, part := range parts {
			if regexMatch(globToRegex(pat), part) {
				return true
			}
		}
	}
	return false
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

func collectSyncFiles(o *syncOpts, localRoot string, allExcludes []string) ([]string, error) {
	var files []string
	switch o.mode {
	case "git":
		var err error
		files, err = gitChangedFiles(localRoot, o.gitScope)
		if err != nil {
			return nil, err
		}
	case "mtime":
		var err error
		files, err = mtimeChangedFiles(localRoot, o.since, allExcludes)
		if err != nil {
			return nil, err
		}
	case "glob":
		if len(o.include) == 0 {
			return nil, fmt.Errorf("--include requires at least one pattern")
		}
		files = globFiles(localRoot, o.include)
	case "list":
		for _, p := range o.files {
			rel := normalizeForTar(localRoot, p)
			if rel == "" {
				fmt.Fprintf(os.Stderr, "warning: skipping %q (outside local root)\n", p)
				continue
			}
			files = append(files, rel)
		}
	default:
		return nil, fmt.Errorf("internal: no sync mode resolved")
	}

	excludes := o.exclude
	if o.mode != "list" {
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

// tarUploadStream builds a tar stream of files (rooted at localRoot) entirely
// in Go and pipes it into a remote `tar -xf -` running in remoteRoot.
func tarUploadStream(profile *Profile, localRoot string, files []string, remoteRoot string) (int, error) {
	c, err := Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()

	expanded, err := c.expandRemoteHome(remoteRoot)
	if err != nil {
		return 1, err
	}
	remoteCmd := fmt.Sprintf("mkdir -p %s && cd %s && tar -xf -", shQuotePath(expanded), shQuotePath(expanded))

	// Build tar in-memory (for typical use cases this is fine; for very
	// large transfers we could pipe via io.Pipe and a goroutine).
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		defer pw.Close()
		tw := tar.NewWriter(pw)
		defer tw.Close()
		for _, f := range files {
			full := filepath.Join(localRoot, f)
			info, err := os.Stat(full)
			if err != nil {
				errCh <- err
				return
			}
			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				errCh <- err
				return
			}
			hdr.Name = f // relative path with forward slashes
			if err := tw.WriteHeader(hdr); err != nil {
				errCh <- err
				return
			}
			if !info.IsDir() {
				src, err := os.Open(full)
				if err != nil {
					errCh <- err
					return
				}
				if _, err := io.Copy(tw, src); err != nil {
					src.Close()
					errCh <- err
					return
				}
				src.Close()
			}
		}
		errCh <- nil
	}()

	rc, runErr := c.RunStreamStdin(remoteCmd, pr)
	tarErr := <-errCh
	if tarErr != nil {
		return 1, tarErr
	}
	return rc, runErr
}

func cmdSync(args []string, cfg *Config, profileOverride string) int {
	o := parseSyncOpts(args)
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		fatal("%v", err)
	}

	// local root
	localRoot := o.root
	if localRoot == "" {
		if o.mode == "git" || (o.mode == "" && !o.noGit) {
			localRoot = findGitRoot(mustCwd())
		}
		if localRoot == "" {
			localRoot = mustCwd()
		}
	}
	abs, err := filepath.Abs(localRoot)
	if err != nil {
		fatal("error: bad local root: %v", err)
	}
	localRoot = abs
	if st, err := os.Stat(localRoot); err != nil || !st.IsDir() {
		fatal("error: local root not a directory: %s", localRoot)
	}

	// auto-detect mode
	if o.mode == "" {
		if !o.noGit && findGitRoot(localRoot) != "" {
			o.mode = "git"
		} else {
			reason := "not in a git repo"
			if o.noGit {
				reason = "git auto-detect disabled (--no-git)"
			}
			fatal("error: %s. Specify --include / --since / --files.", reason)
		}
	}

	// remote root
	cwd := GetCwd(name, profile)
	remoteRoot := cwd
	if o.remoteRoot != "" {
		remoteRoot = resolveRemotePath(o.remoteRoot, cwd)
	} else if profile.SyncRoot != "" {
		remoteRoot = resolveRemotePath(profile.SyncRoot, cwd)
	}

	allExcludes := append([]string{}, o.exclude...)
	allExcludes = append(allExcludes, profile.SyncExclude...)
	allExcludes = append(allExcludes, defaultSyncExcludes...)

	files, err := collectSyncFiles(o, localRoot, allExcludes)
	if err != nil {
		fatal("error: %v", err)
	}

	target := profile.Host
	if profile.User != "" {
		target = profile.User + "@" + profile.Host
	}
	header := fmt.Sprintf("mode    : %s", o.mode)
	if o.mode == "git" {
		header += " (" + o.gitScope + ")"
	} else if o.mode == "mtime" {
		header += " since " + o.since
	}
	fmt.Fprintln(os.Stderr, header)
	fmt.Fprintf(os.Stderr, "local   : %s\n", localRoot)
	fmt.Fprintf(os.Stderr, "remote  : %s:%s\n", target, remoteRoot)
	fmt.Fprintf(os.Stderr, "files   : %d\n", len(files))
	if len(files) == 0 {
		fmt.Fprintln(os.Stderr, "(nothing to sync)")
		return 0
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
	if o.dryRun {
		fmt.Fprintln(os.Stderr, "(dry-run, not transferred)")
		return 0
	}
	rc, err := tarUploadStream(profile, localRoot, files, remoteRoot)
	if err != nil {
		printDiagError(err, profile)
	}
	if o.watch {
		fmt.Fprintln(os.Stderr)
		return runSyncWatch(o, profile, localRoot, remoteRoot, allExcludes)
	}
	return rc
}

func mustCwd() string {
	wd, err := os.Getwd()
	if err != nil {
		fatal("error: %v", err)
	}
	return wd
}
