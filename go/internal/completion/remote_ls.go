package completion

import (
	"fmt"
	"os"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strings"
	"time"
)

// Remote `ls`-style enumeration for tab completion and the MCP
// list_dir tool. Resolution order:
//
//	daemon (pooled SSH + in-memory cache) -> auto-spawn daemon ->
//	direct dial fallback.
//
// The daemon owns the only cache layer (s.lsCache, 5 s TTL, defensive
// copies, and a background sub-dir prefetcher). Clients always go
// through it: a previous srv invocation's tab-completion warms the
// daemon's memory, and every subsequent client benefits without any
// file-system round-trip. The only on-disk traffic in this path is
// the unix-socket call itself.
//
// Direct-dial fallback only fires when the daemon refuses to spawn
// (broken environment, permission issue) -- a cold SSH handshake
// (~2.7 s) is the user-visible cost. Acceptable for the rare path.

// LsCmd is the `srv _ls <prefix>` internal subcommand used by tab
// completion to enumerate remote entries. Output: one entry per
// line, each line is the full path the shell should substitute (so
// the user gets a complete absolute or relative path back).
// Directories carry a trailing "/" so completers can filter
// dirs-only when needed (e.g., `srv cd`).
//
// Failures are silent: completion mustn't blow up on errors.
//
// Calling with no arg is equivalent to passing an empty prefix
// (lists the current cwd). This matters because PowerShell 5.1's
// native command argument passing silently drops empty string
// arguments -- without this branch, `srv run <space><TAB>` from PS
// would invoke the binary as `srv _ls` (no args) and get nothing,
// leaving completion blank.
func LsCmd(args []string, cfg *config.Config, profileOverride string) error {
	prefix := ""
	if len(args) > 0 {
		prefix = args[0]
	}
	entries, err := ListEntries(prefix, cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv _ls:", err)
		return nil
	}
	for _, e := range entries {
		fmt.Println(e)
	}
	return nil
}

// ListEntries enumerates remote filesystem entries under `prefix`
// (empty = current cwd). Returns full paths in the same form tab
// completion uses -- absolute prefixes pass through, relative ones
// resolve against the active session's cwd, dirs carry a trailing
// "/".
//
// Also called from the MCP `list_dir` handler; the daemon's
// in-memory cache + sub-dir prefetcher make repeat queries
// sub-100ms.
func ListEntries(prefix string, cfg *config.Config, profileOverride string) ([]string, error) {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return nil, err
	}
	cwd := config.GetCwd(name, profile)

	// Daemon (pooled SSH, ~500 ms even when "cold" because no
	// handshake, ~10 ms when its in-memory cache holds the answer).
	// Auto-spawn one in the background if none is running -- the
	// retry will hit the cold-but-pooled daemon, and the next call
	// after that hits memory.
	if entries, ok := daemon.TryLs(name, cwd, prefix); ok {
		return entries, nil
	}
	if daemon.Ensure() {
		if entries, ok := daemon.TryLs(name, cwd, prefix); ok {
			return entries, nil
		}
	}

	// Direct dial fallback (~2.7 s cold, full handshake). Only
	// reached when the daemon refuses to spawn -- a broken-env
	// path that we'd rather make slow-but-correct than wrong-fast
	// via a stale cache.
	dirPart, basePart := sshx.SplitRemotePrefix(prefix)
	target := sshx.RemoteListTarget(dirPart, cwd)
	listing, err := remoteLs(profile, target, 10*time.Second)
	if err != nil {
		return nil, err
	}
	return matchLsEntries(listing, dirPart, basePart), nil
}

// matchLsEntries filters entries by basePart prefix, prepends
// dirPart, returns. Used only on the direct-dial fallback path;
// daemon-side filtering is done by the daemon itself in handleLs.
func matchLsEntries(entries []string, dirPart, basePart string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e, basePart) {
			continue
		}
		out = append(out, dirPart+e)
	}
	return out
}

// remoteLs runs `ls -1Ap <dir>` and returns one entry per line.
// Dirs carry a trailing "/". Hidden entries are included (so `srv
// cd .ssh/` completes), `.` and `..` are skipped.
func remoteLs(profile *config.Profile, target string, timeout time.Duration) ([]string, error) {
	cmd := fmt.Sprintf("ls -1Ap -- %s", srvtty.ShQuotePath(target))
	c, err := sshx.DialOpts(profile, sshx.DialOptions{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer c.Close()
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(res.Stderr)
		if stderr == "" {
			return nil, fmt.Errorf("ls exit %d", res.ExitCode)
		}
		return nil, fmt.Errorf("ls: %s", stderr)
	}
	out := []string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}
