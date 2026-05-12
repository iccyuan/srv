package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"srv/internal/srvio"
	"srv/internal/srvpath"
	"srv/internal/srvtty"
	"strings"
	"time"
)

// Cache TTL for `srv _ls` outputs. Tab-tab on the same prefix should be
// instant; new content is rare on this timescale.
const lsCacheTTL = 5 * time.Second

// cmdInternalLs is the `srv _ls <prefix>` internal subcommand used by tab
// completion to enumerate remote entries. Output: one entry per line, each
// line is the full path the shell should substitute (so the user gets a
// complete absolute or relative path back). Directories carry a trailing
// "/" so completers can filter dirs-only when needed (e.g., `srv cd`).
//
// Failures are silent: completion mustn't blow up on errors.
//
// Calling with no arg is equivalent to passing an empty prefix (lists the
// current cwd). This matters because PowerShell 5.1's native command
// argument passing silently drops empty string arguments -- without this
// branch, `srv run <space><TAB>` from PS would invoke the binary as
// `srv _ls` (no args) and get nothing, leaving completion blank.
func cmdInternalLs(args []string, cfg *Config, profileOverride string) error {
	prefix := ""
	if len(args) > 0 {
		prefix = args[0]
	}
	entries, err := listRemoteEntries(prefix, cfg, profileOverride)
	if err != nil {
		fmt.Fprintln(os.Stderr, "srv _ls:", err)
		return nil
	}
	for _, e := range entries {
		fmt.Println(e)
	}
	return nil
}

// listRemoteEntries enumerates remote filesystem entries under `prefix`
// (empty = current cwd). Returns full paths in the same form tab
// completion uses -- absolute prefixes pass through, relative ones resolve
// against the active session's cwd, dirs carry a trailing "/".
//
// Resolution order: file cache (5s TTL) -> daemon (pooled SSH) ->
// auto-spawn daemon -> direct dial fallback. Same hierarchy cmdInternalLs
// has always used; pulled out so the MCP `list_dir` tool can reuse it.
func listRemoteEntries(prefix string, cfg *Config, profileOverride string) ([]string, error) {
	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return nil, err
	}
	cwd := GetCwd(name, profile)
	dirPart, basePart := splitRemotePrefix(prefix)
	target := remoteListTarget(dirPart, cwd)

	// File cache (instant, ~60ms even for misses-then-hits sequences).
	key := cacheKey(profile.Host, profile.User, target)
	if cached, ok := readLsCache(key, lsCacheTTL); ok {
		return matchEntries(cached, dirPart, basePart), nil
	}

	// Daemon (pooled SSH, ~500ms even when "cold" because no handshake).
	// Auto-spawn one in the background if none is running -- next call will
	// be warm. Send the CLI's cwd so relative prefixes resolve against the
	// right directory (the daemon never reads its own session).
	if entries, ok := tryDaemonLs(name, cwd, prefix); ok {
		return entries, nil
	}
	if ensureDaemon() {
		if entries, ok := tryDaemonLs(name, cwd, prefix); ok {
			return entries, nil
		}
	}

	// Direct dial fallback (~2.7s cold, full handshake).
	listing, err := remoteList(profile, target, 10*time.Second)
	if err != nil {
		return nil, err
	}
	_ = writeLsCache(key, listing)
	return matchEntries(listing, dirPart, basePart), nil
}

// matchEntries is emitLsMatches in slice form: filter by basePart prefix,
// prepend dirPart, return.
func matchEntries(entries []string, dirPart, basePart string) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !strings.HasPrefix(e, basePart) {
			continue
		}
		out = append(out, dirPart+e)
	}
	return out
}

// splitRemotePrefix divides "/opt/ap" into ("/opt/", "ap"); "/opt/" into
// ("/opt/", ""); "ap" into ("", "ap"); "" into ("", "").
func splitRemotePrefix(p string) (dir, base string) {
	if p == "" {
		return "", ""
	}
	i := strings.LastIndex(p, "/")
	if i < 0 {
		return "", p
	}
	return p[:i+1], p[i+1:]
}

// remoteListTarget resolves what directory to ls on the remote, given the
// dirPart from the user's prefix and the persisted cwd.
func remoteListTarget(dirPart, cwd string) string {
	if dirPart == "" {
		// Relative completion: list cwd.
		if cwd == "" {
			return "."
		}
		return cwd
	}
	// Absolute or ~-prefixed: pass through. Otherwise relative to cwd.
	if strings.HasPrefix(dirPart, "/") || strings.HasPrefix(dirPart, "~") {
		return dirPart
	}
	return strings.TrimRight(cwd, "/") + "/" + dirPart
}

// remoteList runs `ls -1Ap <dir>` and returns one entry per line. Dirs
// carry a trailing "/". Hidden entries are included (so `srv cd .ssh/`
// completes), `.` and `..` are skipped.
func remoteList(profile *Profile, target string, timeout time.Duration) ([]string, error) {
	cmd := fmt.Sprintf("ls -1Ap -- %s", srvtty.ShQuotePath(target))
	c, err := DialOpts(profile, DialOptions{Timeout: timeout})
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	defer c.Close()
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}
	if res.ExitCode != 0 {
		// Likely "no such directory" -- empty completion is the right answer,
		// but surface the cause via error so the user sees it on direct calls.
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

func cacheKey(host, user, target string) string {
	h := sha1.Sum([]byte(host + "\x00" + user + "\x00" + target))
	return hex.EncodeToString(h[:10])
}

func cacheDir() string { return filepath.Join(srvpath.Dir(), "cache") }

func readLsCache(key string, ttl time.Duration) ([]string, bool) {
	p := filepath.Join(cacheDir(), "ls-"+key+".txt")
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
	p := filepath.Join(cacheDir(), "ls-"+key+".txt")
	return srvio.WriteFileAtomic(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
