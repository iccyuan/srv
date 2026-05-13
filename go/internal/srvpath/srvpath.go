// Package srvpath holds the path-helpers for srv's on-disk state.
// Concentrated in one tiny package so any subpackage that needs to
// know "where does srv put its files" can import this without
// dragging in config / session / etc.
//
// All paths derive from Dir() (= $SRV_HOME or ~/.srv).
package srvpath

import (
	"os"
	"path/filepath"
)

// Dir is the on-disk location of all srv state. Honors $SRV_HOME;
// falls back to ~/.srv (or "./.srv" in the rare case HOME is
// unreadable, so we never produce a relative path the caller can't
// distinguish from cwd).
func Dir() string {
	if v := os.Getenv("SRV_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".srv"
	}
	return filepath.Join(home, ".srv")
}

// Config returns the path to config.json.
func Config() string { return filepath.Join(Dir(), "config.json") }

// Sessions returns the path to sessions.json (the per-shell record file).
func Sessions() string { return filepath.Join(Dir(), "sessions.json") }

// Jobs returns the path to jobs.json (the detached-job ledger).
func Jobs() string { return filepath.Join(Dir(), "jobs.json") }

// MCPLog returns the path to mcp.log (the MCP server's lifecycle log).
func MCPLog() string { return filepath.Join(Dir(), "mcp.log") }

// MCPStats returns the path to mcp-stats.jsonl (one record per
// MCP tools/call: tool name, duration, in/out/progress bytes, ok).
// Append-only; rotated manually via `srv stats --clear`.
func MCPStats() string { return filepath.Join(Dir(), "mcp-stats.jsonl") }

// ColorPresetsDir returns ~/.srv/init/, the directory the user drops
// custom shell snippets into. Each *.sh file is one preset; the
// filename without extension is what `srv color use <name>` accepts.
func ColorPresetsDir() string { return filepath.Join(Dir(), "init") }

// ColorPreset resolves a preset name to its absolute *.sh file path
// under ColorPresetsDir(). Doesn't check existence; callers test
// with os.Stat so missing files surface a clear error.
func ColorPreset(name string) string {
	return filepath.Join(ColorPresetsDir(), name+".sh")
}
