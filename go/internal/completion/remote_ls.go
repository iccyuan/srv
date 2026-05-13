package completion

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/srvio"
	"srv/internal/srvpath"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strings"
	"time"
)

// Remote `ls`-style enumeration for tab completion and the MCP
// list_dir tool. Resolution order:
//   file cache (5s TTL) -> daemon (pooled SSH) -> auto-spawn daemon
//   -> direct dial fallback.
// Same hierarchy the internal `srv _ls` subcommand has always used;
// the MCP path reuses it so list_dir benefits from the same cache.

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
// Also called from the MCP `list_dir` handler; the file cache + the
// pooled daemon make repeat queries sub-100ms.
func ListEntries(prefix string, cfg *config.Config, profileOverride string) ([]string, error) {
	name, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return nil, err
	}
	cwd := config.GetCwd(name, profile)
	dirPart, basePart := sshx.SplitRemotePrefix(prefix)
	target := sshx.RemoteListTarget(dirPart, cwd)

	// File cache (instant, ~60ms even for misses-then-hits sequences).
	key := lsCacheKey(profile.Host, profile.User, target)
	if cached, ok := readLsCache(key, sshx.LsCacheTTL); ok {
		return matchLsEntries(cached, dirPart, basePart), nil
	}

	// Daemon (pooled SSH, ~500ms even when "cold" because no
	// handshake). Auto-spawn one in the background if none is
	// running -- next call will be warm. Send the CLI's cwd so
	// relative prefixes resolve against the right directory (the
	// daemon never reads its own session).
	if entries, ok := daemon.TryLs(name, cwd, prefix); ok {
		return entries, nil
	}
	if daemon.Ensure() {
		if entries, ok := daemon.TryLs(name, cwd, prefix); ok {
			return entries, nil
		}
	}

	// Direct dial fallback (~2.7s cold, full handshake).
	listing, err := remoteLs(profile, target, 10*time.Second)
	if err != nil {
		return nil, err
	}
	_ = writeLsCache(key, listing)
	return matchLsEntries(listing, dirPart, basePart), nil
}

// matchLsEntries filters entries by basePart prefix, prepends
// dirPart, returns.
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

func lsCacheKey(host, user, target string) string {
	h := sha1.Sum([]byte(host + "\x00" + user + "\x00" + target))
	return hex.EncodeToString(h[:10])
}

func lsCacheDir() string { return filepath.Join(srvpath.Dir(), "cache") }

func readLsCache(key string, ttl time.Duration) ([]string, bool) {
	p := filepath.Join(lsCacheDir(), "ls-"+key+".txt")
	st, err := os.Stat(p)
	if err != nil {
		return nil, false
	}
	if time.Since(st.ModTime()) > ttl {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return []string{}, true
	}
	return strings.Split(s, "\n"), true
}

func writeLsCache(key string, lines []string) error {
	p := filepath.Join(lsCacheDir(), "ls-"+key+".txt")
	return srvio.WriteFileAtomic(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
