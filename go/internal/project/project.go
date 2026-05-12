// Package project owns the `.srv-project` JSON pin file that a repo
// drops at its root to make a specific profile / cwd sticky for any
// srv call inside the tree.
//
// Surface: File type, Find (walk up looking for one), Resolve (the
// gateway ResolveProfile / GetCwd call), Cmd (the `srv project`
// subcommand). SetSilent gates the "ignoring malformed file" stderr
// warning so the MCP server can stay quiet.
package project

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// File is the on-disk shape of a project-local `.srv-project` JSON
// file. Lives at the root of a repo / workspace and pins the
// effective profile (and optional default cwd) for every srv
// invocation rooted inside that directory tree.
//
// The motivation is MCP: each Claude Code project launches its own
// srv MCP server with a fresh session, so without a project file the
// active profile defaults to the global one -- forcing the model to
// `srv use <profile>` on every conversation. Dropping a `.srv-project`
// in the repo makes the right profile sticky for that project alone.
type File struct {
	// Path is the absolute path the file was loaded from. Populated by
	// Find so diagnostics ("pinned by /repo/.srv-project") can surface
	// the source without an extra plumb.
	Path string `json:"-"`
	// Profile pins the active profile for any srv call inside the
	// project tree. Slots into ResolveProfile precedence between
	// $SRV_PROFILE and the config-level default.
	Profile string `json:"profile,omitempty"`
	// Cwd is the remote working directory new sessions land in for
	// this project. Slots into GetCwd precedence between $SRV_CWD and
	// profile.default_cwd.
	Cwd string `json:"cwd,omitempty"`
}

// FileName is the filename Find / Resolve look for. Single source of
// truth so docs and the lookup can't drift.
const FileName = ".srv-project"

// Per-process cache: walking up to a few directories is cheap, but
// resolving on every MCP tool call adds up. Keyed by start directory;
// we cache misses (nil entry) too so a project tree without a file
// doesn't pay the walk repeatedly.
var (
	cacheMu sync.Mutex
	cache   = map[string]*File{}
	cacheNo = map[string]bool{}

	// silent suppresses the "ignoring malformed file" stderr line.
	// Caller sets via SetSilent(true) under the MCP server so the
	// model's transcript doesn't pick up an unrelated warning every
	// tool call.
	silent bool
)

// SetSilent gates the diagnostic that Resolve prints when a
// .srv-project file fails to parse. Default is loud (suitable for
// the CLI); MCP server flips this on at startup.
func SetSilent(s bool) { silent = s }

// Find walks up from `startDir` looking for `.srv-project`, stopping
// at the filesystem root. Returns (nil, nil) when none found. Errors
// only on malformed JSON in a file we did find -- a stat failure or
// unreadable file mid-walk is treated as "not here, keep walking".
func Find(startDir string) (*File, error) {
	if startDir == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return nil, nil
	}

	cacheMu.Lock()
	if cached, ok := cache[abs]; ok {
		cacheMu.Unlock()
		return cached, nil
	}
	if cacheNo[abs] {
		cacheMu.Unlock()
		return nil, nil
	}
	cacheMu.Unlock()

	dir := abs
	for {
		candidate := filepath.Join(dir, FileName)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			pf, perr := load(candidate)
			if perr != nil {
				return nil, perr
			}
			cacheMu.Lock()
			cache[abs] = pf
			cacheMu.Unlock()
			return pf, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	cacheMu.Lock()
	cacheNo[abs] = true
	cacheMu.Unlock()
	return nil, nil
}

// load reads and parses a `.srv-project` JSON file. Empty or
// whitespace-only files are treated as missing rather than as an
// error, so an accidental `touch .srv-project` doesn't break srv
// across the whole subtree.
func load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := trimSpaceBytes(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var pf File
	if err := json.Unmarshal(trimmed, &pf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	pf.Path = path
	return &pf, nil
}

// trimSpaceBytes is a tiny helper avoiding an import of strings just
// to strip whitespace -- works in-place on the byte slice.
func trimSpaceBytes(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpaceByte(b[start]) {
		start++
	}
	for end > start && isSpaceByte(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// Resolve is the gateway used by ResolveProfile / GetCwd in package
// main. It looks up from the local cwd, swallows "file is malformed"
// errors after surfacing them once (so a typo doesn't block every
// command), and returns nil when no file is in scope.
func Resolve() *File {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	pf, err := Find(cwd)
	if err != nil {
		// Print the diagnostic but keep going -- the file exists but
		// won't influence resolution this run.
		if !silent {
			fmt.Fprintf(os.Stderr, "srv: ignoring malformed %s: %v\n", FileName, err)
		}
		return nil
	}
	return pf
}

// Cmd implements `srv project` -- prints the resolved project-file
// path and its effective pins, or reports that no file was found.
// Useful for diagnosing "why did srv pick that profile?" without
// grep'ing through $env / config / etc.
func Cmd(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	pf, err := Find(cwd)
	if err != nil {
		return err
	}
	if pf == nil {
		fmt.Printf("no %s found above %s\n", FileName, cwd)
		return nil
	}
	fmt.Printf("project file: %s\n", pf.Path)
	if pf.Profile != "" {
		fmt.Printf("  profile: %s\n", pf.Profile)
	}
	if pf.Cwd != "" {
		fmt.Printf("  cwd:     %s\n", pf.Cwd)
	}
	if pf.Profile == "" && pf.Cwd == "" {
		fmt.Println("  (file is present but pins nothing)")
	}
	return nil
}
