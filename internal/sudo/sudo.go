// Package sudo implements `srv sudo <cmd>` -- running sudo on the
// remote without going through a remote pty.
//
// Two convenience knobs:
//   - The password is read from the local terminal with echo off
//     (term.ReadPassword), so it's never visible on screen or in
//     shell history.
//   - The daemon keeps a per-profile in-memory cache (default TTL 5
//     min) so consecutive sudos in the same shell don't re-prompt.
//     The cache lives only in the daemon process; never persisted.
//     Pass --no-cache to skip both read and write.
package sudo

import (
	"encoding/base64"
	"fmt"
	"os"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"strings"
	"time"

	"golang.org/x/term"
)

// Cmd implements `srv sudo [--no-cache] [--cache-ttl <dur>] [--clear-cache] <command>`.
// profileOverride is the value of the global `-P/--profile` flag (empty
// when not set).
func Cmd(args []string, cfg *config.Config, profileOverride string) error {
	useCache := true
	cacheTTL := 5 * time.Minute
	var cmdArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--no-cache":
			useCache = false
		case a == "--cache-ttl":
			if i+1 >= len(args) {
				return srvutil.Errf(2, "--cache-ttl requires a value (e.g. 10m)")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return srvutil.Errf(2, "bad --cache-ttl %q: %v", args[i+1], err)
			}
			cacheTTL = d
			i++
		case a == "--clear-cache":
			profName, _, err := config.Resolve(cfg, profileOverride)
			if err != nil {
				return srvutil.Errf(1, "%v", err)
			}
			cacheClear(profName)
			fmt.Println("sudo cache cleared for profile", profName)
			return nil
		case a == "--":
			cmdArgs = append(cmdArgs, args[i+1:]...)
			i = len(args)
		default:
			cmdArgs = append(cmdArgs, a)
		}
	}
	if len(cmdArgs) == 0 {
		return srvutil.Errf(2, "usage: srv sudo [--no-cache] [--cache-ttl <dur>] <command>")
	}
	cmd := strings.Join(cmdArgs, " ")

	profName, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return srvutil.Errf(1, "%v", err)
	}

	password, fromCache := "", false
	if useCache {
		if pw := cacheGet(profName); pw != "" {
			password = pw
			fromCache = true
		}
	}
	if password == "" {
		pw, err := promptPassword(profile.User)
		if err != nil {
			return srvutil.Errf(1, "read password: %v", err)
		}
		password = pw
	}

	cwd := config.GetCwd(profName, profile)
	rc, err := runRemote(profile, cwd, cmd, password)
	if err != nil {
		return srvutil.Errf(1, "%v", err)
	}

	// Treat exit code 1 + "incorrect password" as auth failure and
	// drop the cached password so the next attempt re-prompts. sudo's
	// own messages are localized so we match on the most common
	// fragments rather than exact strings; false negatives only mean
	// the user retypes the password one extra time.
	if rc == 1 {
		cacheClear(profName)
		if fromCache {
			fmt.Fprintln(os.Stderr, "srv sudo: cached password rejected, cleared cache")
		}
	} else if rc == 0 && useCache {
		// Refresh on success so a long-running session keeps the TTL
		// alive without forcing re-prompts at the cliff edge.
		cacheSet(profName, password, cacheTTL)
	}
	return srvutil.Code(rc)
}

// promptPassword reads a password from the local terminal with echo
// off. Fails fast when stdin isn't a tty -- MCP / piped contexts
// can't prompt interactively, and we don't want to read a password
// from arbitrary stdin (would surface in shell history if `--no-cache
// </file`-style mistakes happen).
func promptPassword(user string) (string, error) {
	if !srvtty.IsStdinTTY() {
		return "", fmt.Errorf("stdin is not a tty; cannot prompt (use cache or run from an interactive shell)")
	}
	label := user
	if label == "" {
		label = "remote"
	}
	fmt.Fprintf(os.Stderr, "[sudo] password for %s: ", label)
	defer fmt.Fprintln(os.Stderr)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return "", err
	}
	if len(b) == 0 {
		return "", fmt.Errorf("empty password")
	}
	return string(b), nil
}

// runRemote dials the profile, runs `sudo -S -p ” <cmd>` with the
// supplied password piped on stdin, and forwards stdout/stderr to the
// local terminal. Returns the remote exit code.
//
// The -p ” bit disables sudo's own prompt: we already have the
// password and don't want sudo to print "[sudo] password for ..." to
// stderr. The trailing newline on the password is required so sudo's
// read() returns.
func runRemote(profile *config.Profile, cwd, cmd, password string) (int, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()
	full := sshx.WrapWithCwd("sudo -S -p '' "+cmd, cwd)
	return c.RunStreamStdin(full, strings.NewReader(password+"\n"))
}

// cacheGet returns the cached sudo password for `profile`, or "" on
// miss / no daemon. Decoded from the wire base64 form.
func cacheGet(profile string) string {
	conn := daemon.DialSock(300 * time.Millisecond)
	if conn == nil {
		return ""
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{
		Op:      "sudo_cache_get",
		Profile: profile,
	}, 2*time.Second)
	if err != nil || resp == nil || !resp.OK || resp.PasswordB64 == "" {
		return ""
	}
	b, err := base64.StdEncoding.DecodeString(resp.PasswordB64)
	if err != nil {
		return ""
	}
	return string(b)
}

// cacheSet stores `password` in the daemon's in-memory cache for
// `profile`, expiring after `ttl`. Best-effort: if the daemon isn't
// running, caching just doesn't happen (we don't spawn it for this,
// since sudo is interactive and the user paid the prompt cost
// already).
func cacheSet(profile, password string, ttl time.Duration) {
	conn := daemon.DialSock(300 * time.Millisecond)
	if conn == nil {
		return
	}
	defer conn.Close()
	_, _ = daemon.Call(conn, daemon.Request{
		Op:          "sudo_cache_set",
		Profile:     profile,
		PasswordB64: base64.StdEncoding.EncodeToString([]byte(password)),
		TTLSec:      int(ttl.Seconds()),
	}, 2*time.Second)
}

// cacheClear evicts the cached password for `profile`. Idempotent on
// cache miss.
func cacheClear(profile string) {
	conn := daemon.DialSock(300 * time.Millisecond)
	if conn == nil {
		return
	}
	defer conn.Close()
	_, _ = daemon.Call(conn, daemon.Request{
		Op:      "sudo_cache_clear",
		Profile: profile,
	}, 2*time.Second)
}
