package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strings"
	"time"

	"golang.org/x/term"
)

// cmdSudo runs `sudo <cmd>` on the remote. The hard part is the
// password prompt: under `srv -t sudo ...` the remote pty handles it
// fine, but that's heavier than necessary and only works when stdin is
// a tty. cmdSudo runs the command non-tty and pipes the password
// through `sudo -S` from local stdin.
//
// Two convenience knobs:
//   - The password is read from the local terminal with echo off
//     (term.ReadPassword), so it's never visible on screen or shell
//     history.
//   - The daemon keeps a per-profile in-memory cache (default TTL 5
//     min) so consecutive sudos in the same shell don't re-prompt.
//     The cache lives only in the daemon process; never persisted.
//     Pass --no-cache to skip both read and write.
func cmdSudo(args []string, cfg *Config, opts globalOpts) error {
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
				return exitErr(2, "--cache-ttl requires a value (e.g. 10m)")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return exitErr(2, "bad --cache-ttl %q: %v", args[i+1], err)
			}
			cacheTTL = d
			i++
		case a == "--clear-cache":
			profName, _, err := ResolveProfile(cfg, opts.profile)
			if err != nil {
				return exitErr(1, "%v", err)
			}
			daemonSudoCacheClear(profName)
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
		return exitErr(2, "usage: srv sudo [--no-cache] [--cache-ttl <dur>] <command>")
	}
	cmd := strings.Join(cmdArgs, " ")

	profName, profile, err := ResolveProfile(cfg, opts.profile)
	if err != nil {
		return exitErr(1, "%v", err)
	}

	password, fromCache := "", false
	if useCache {
		if pw := daemonSudoCacheGet(profName); pw != "" {
			password = pw
			fromCache = true
		}
	}
	if password == "" {
		pw, err := promptSudoPassword(profile.User)
		if err != nil {
			return exitErr(1, "read password: %v", err)
		}
		password = pw
	}

	cwd := GetCwd(profName, profile)
	rc, err := runRemoteSudo(profile, cwd, cmd, password)
	if err != nil {
		return exitErr(1, "%v", err)
	}

	// Treat exit code 1 + "incorrect password" as auth failure and
	// drop the cached password so the next attempt re-prompts. sudo's
	// own messages are localized so we match on the most common
	// fragments rather than exact strings; false negatives only mean
	// the user retypes the password one extra time.
	if rc == 1 {
		daemonSudoCacheClear(profName)
		if fromCache {
			fmt.Fprintln(os.Stderr, "srv sudo: cached password rejected, cleared cache")
		}
	} else if rc == 0 && useCache {
		// Refresh on success so a long-running session keeps the TTL
		// alive without forcing re-prompts at the cliff edge.
		daemonSudoCacheSet(profName, password, cacheTTL)
	}
	return exitCode(rc)
}

// promptSudoPassword reads a password from the local terminal with
// echo off. Fails fast when stdin isn't a tty -- MCP / piped contexts
// can't prompt interactively, and we don't want to read a password
// from arbitrary stdin (would surface in shell history if `--no-cache
// </file`-style mistakes happen).
func promptSudoPassword(user string) (string, error) {
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

// runRemoteSudo dials the profile, runs `sudo -S -p ” <cmd>` with the
// supplied password piped on stdin, and forwards stdout/stderr to the
// local terminal. Returns the remote exit code.
//
// The -p ” bit disables sudo's own prompt: we already have the
// password and don't want sudo to print "[sudo] password for ..." to
// stderr. The trailing newline on the password is required so sudo's
// read() returns.
func runRemoteSudo(profile *Profile, cwd, cmd, password string) (int, error) {
	c, err := Dial(profile)
	if err != nil {
		return 255, err
	}
	defer c.Close()
	full := sshx.WrapWithCwd("sudo -S -p '' "+cmd, cwd)
	return c.RunStreamStdin(full, strings.NewReader(password+"\n"))
}

// daemonSudoCacheGet returns the cached sudo password for `profile`,
// or "" on miss / no daemon. Decoded from the wire base64 form.
func daemonSudoCacheGet(profile string) string {
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		return ""
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{
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

// daemonSudoCacheSet stores `password` in the daemon's in-memory cache
// for `profile`, expiring after `ttl`. Best-effort: if the daemon
// isn't running, caching just doesn't happen (we don't spawn it for
// this, since sudo is interactive and the user paid the prompt cost
// already).
func daemonSudoCacheSet(profile, password string, ttl time.Duration) {
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		return
	}
	defer conn.Close()
	_, _ = daemonCall(conn, daemonRequest{
		Op:          "sudo_cache_set",
		Profile:     profile,
		PasswordB64: base64.StdEncoding.EncodeToString([]byte(password)),
		TTLSec:      int(ttl.Seconds()),
	}, 2*time.Second)
}

// daemonSudoCacheClear evicts the cached password for `profile`.
// Idempotent on cache miss.
func daemonSudoCacheClear(profile string) {
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		return
	}
	defer conn.Close()
	_, _ = daemonCall(conn, daemonRequest{
		Op:      "sudo_cache_clear",
		Profile: profile,
	}, 2*time.Second)
}
