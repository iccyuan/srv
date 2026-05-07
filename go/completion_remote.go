package main

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
func cmdInternalLs(args []string, cfg *Config, profileOverride string) int {
	if len(args) == 0 {
		return 0
	}
	prefix := args[0]

	name, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return 0
	}
	cwd := GetCwd(name, profile)

	dirPart, basePart := splitRemotePrefix(prefix)
	target := remoteListTarget(dirPart, cwd)

	// Try cache first.
	key := cacheKey(profile.Host, profile.User, target)
	if cached, ok := readLsCache(key, lsCacheTTL); ok {
		emitLsMatches(cached, dirPart, basePart)
		return 0
	}

	// Live ls. Tight 3s deadline so completion never feels stuck.
	listing, err := remoteList(profile, target, 3*time.Second)
	if err != nil {
		return 0
	}
	_ = writeLsCache(key, listing)
	emitLsMatches(listing, dirPart, basePart)
	return 0
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
	cmd := fmt.Sprintf("ls -1Ap %s 2>/dev/null", shQuotePath(target))
	c, err := DialOpts(profile, dialOpts{timeout: timeout})
	if err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		// Treat any non-zero (probably "no such directory") as empty.
		return nil, nil
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

// emitLsMatches prints entries that have basePart as prefix, prepending
// dirPart so the printed line is the full path the shell can substitute.
func emitLsMatches(entries []string, dirPart, basePart string) {
	for _, e := range entries {
		if !strings.HasPrefix(e, basePart) {
			continue
		}
		fmt.Println(dirPart + e)
	}
}

func cacheKey(host, user, target string) string {
	h := sha1.Sum([]byte(host + "\x00" + user + "\x00" + target))
	return hex.EncodeToString(h[:10])
}

func cacheDir() string { return filepath.Join(ConfigDir(), "cache") }

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
	if err := os.MkdirAll(cacheDir(), 0o755); err != nil {
		return err
	}
	p := filepath.Join(cacheDir(), "ls-"+key+".txt")
	return os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}
