package sshx

import (
	"strings"
	"time"
)

// Remote-path helpers shared by the daemon's ls RPC handler and the
// CLI-side completion_remote shim. Living in sshx rather than each
// caller's file keeps them as one source of truth and avoids a third
// cyclic-friendly package.

// LsCacheTTL bounds how long a `_ls` cache entry is considered fresh.
// 5s is short enough that a `mv foo bar` immediately re-tab-completes
// to the new name, but long enough that mashing TAB inside the same
// shell prompt doesn't pay a fresh SSH round-trip per keystroke.
const LsCacheTTL = 5 * time.Second

// SplitRemotePrefix divides "/opt/ap" into ("/opt/", "ap"); "/opt/"
// into ("/opt/", ""); "ap" into ("", "ap"). Used to decide which
// directory to ls on the remote vs. which basename to filter the
// listing by locally.
func SplitRemotePrefix(p string) (dir, base string) {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[:i+1], p[i+1:]
	}
	return "", p
}

// RemoteListTarget resolves what directory to ls on the remote, given
// the split prefix's directory part (`dirPart`) and the session's
// current remote cwd (`cwd`). Absolute dirPart wins; relative joins
// to cwd; empty dirPart defaults to cwd (or "~" when cwd itself is
// empty).
func RemoteListTarget(dirPart, cwd string) string {
	if dirPart == "" {
		if cwd == "" {
			return "~"
		}
		return cwd
	}
	if strings.HasPrefix(dirPart, "/") || strings.HasPrefix(dirPart, "~") {
		return dirPart
	}
	if cwd == "" {
		return dirPart
	}
	// Relative path: join to cwd. Trailing slash matters for ls so
	// preserve it.
	if strings.HasSuffix(cwd, "/") {
		return cwd + dirPart
	}
	return cwd + "/" + dirPart
}
