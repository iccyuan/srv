package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ProjectFile is the on-disk shape of a project-local `.srv-project`
// JSON file. Lives at the root of a repo / workspace and pins the
// effective profile (and optional default cwd) for every srv invocation
// rooted inside that directory tree.
//
// The motivation is MCP: each Claude Code project launches its own
// srv MCP server with a fresh session, so without a project file the
// active profile defaults to the global one -- forcing the model to
// `srv use <profile>` on every conversation. Dropping a `.srv-project`
// in the repo makes the right profile sticky for that project alone.
type ProjectFile struct {
	// Path is the absolute path the file was loaded from. Populated by
	// findProjectFile so diagnostics ("pinned by /repo/.srv-project")
	// can surface the source without an extra plumb.
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

// projectFileName is the filename we look for. Single source of truth
// so docs and the lookup can't drift.
const projectFileName = ".srv-project"

// Per-process cache: walking up to a few directories is cheap, but
// resolving on every MCP tool call adds up. Keyed by start directory;
// we cache misses (nil entry) too so a project tree without a file
// doesn't pay the walk repeatedly.
var (
	projectCacheMu sync.Mutex
	projectCache   = map[string]*ProjectFile{}
	projectCacheNo = map[string]bool{}
)

// findProjectFile walks up from `startDir` looking for `.srv-project`,
// stopping at the filesystem root. Returns (nil, nil) when none found.
// Errors only on malformed JSON in a file we did find -- a stat failure
// or unreadable file mid-walk is treated as "not here, keep walking".
func findProjectFile(startDir string) (*ProjectFile, error) {
	if startDir == "" {
		return nil, nil
	}
	abs, err := filepath.Abs(startDir)
	if err != nil {
		return nil, nil
	}

	projectCacheMu.Lock()
	if cached, ok := projectCache[abs]; ok {
		projectCacheMu.Unlock()
		return cached, nil
	}
	if projectCacheNo[abs] {
		projectCacheMu.Unlock()
		return nil, nil
	}
	projectCacheMu.Unlock()

	dir := abs
	for {
		candidate := filepath.Join(dir, projectFileName)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			pf, perr := loadProjectFile(candidate)
			if perr != nil {
				return nil, perr
			}
			projectCacheMu.Lock()
			projectCache[abs] = pf
			projectCacheMu.Unlock()
			return pf, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	projectCacheMu.Lock()
	projectCacheNo[abs] = true
	projectCacheMu.Unlock()
	return nil, nil
}

// loadProjectFile reads and parses a `.srv-project` JSON file. Empty
// or whitespace-only files are treated as missing rather than as an
// error, so an accidental `touch .srv-project` doesn't break srv
// across the whole subtree.
func loadProjectFile(path string) (*ProjectFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	trimmed := trimSpaceBytes(data)
	if len(trimmed) == 0 {
		return nil, nil
	}
	var pf ProjectFile
	if err := json.Unmarshal(trimmed, &pf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	pf.Path = path
	return &pf, nil
}

// trimSpaceBytes is a tiny helper avoiding an import of strings just to
// strip whitespace -- works in-place on the byte slice.
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

// resolveProjectFile is the gateway used by ResolveProfile / GetCwd.
// It looks up from the local cwd, swallows "file is malformed" errors
// after surfacing them once (so a typo doesn't block every command),
// and returns nil when no file is in scope.
func resolveProjectFile() *ProjectFile {
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	pf, err := findProjectFile(cwd)
	if err != nil {
		// Print the diagnostic but keep going -- the file exists but
		// won't influence resolution this run.
		if !mcpMode {
			fmt.Fprintf(os.Stderr, "srv: ignoring malformed %s: %v\n", projectFileName, err)
		}
		return nil
	}
	return pf
}

// cmdProject is `srv project` -- prints the resolved project-file path
// and its effective pins, or reports that no file was found. Useful for
// diagnosing "why did srv pick that profile?" without grep'ing through
// $env / config / etc.
func cmdProject(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	pf, err := findProjectFile(cwd)
	if err != nil {
		return err
	}
	if pf == nil {
		fmt.Printf("no %s found above %s\n", projectFileName, cwd)
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
