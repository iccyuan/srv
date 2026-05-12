// Package srvtty bundles terminal / TTY interaction helpers used by
// srv: TTY detection, terminal-size lookup, raw-mode toggle, passphrase
// prompt, shell quoting (for safely interpolating into /bin/sh), and
// the in-place redraw primitive shared by `srv ui` and `srv watch`.
//
// Pure stdlib + golang.org/x/term -- no srv-internal deps -- so any
// subpackage can pull this in without dragging the rest of main along.
package srvtty

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// IsStdinTTY returns true when stdin is connected to a terminal.
func IsStdinTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// IsStderrTTY returns true when stderr is connected to a terminal.
// Used to gate human-only chrome (progress bars, refreshing status
// lines) so it never lands in MCP responses or piped logs.
func IsStderrTTY() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// Size returns (cols, rows) of stdout's terminal, or (0, 0) on
// failure / non-tty.
func Size() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

// MakeStdinRaw puts the local stdin terminal in raw mode and returns
// a restore function. Returns nil restore if stdin isn't a terminal.
func MakeStdinRaw() (func(), error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return nil, nil
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd, state) }, nil
}

// PromptPassphrase reads a passphrase from /dev/tty (or stdin) without
// echo. Always writes the prompt to stderr so stdout redirection
// doesn't swallow it.
func PromptPassphrase(keyPath string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", keyPath)
	defer fmt.Fprintln(os.Stderr)
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	return pass, nil
}

// ShQuote single-quotes a string for safe inclusion in a /bin/sh
// command. Replaces internal single quotes with the standard '\”
// dance.
func ShQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\\\"'`$&|;<>*?(){}[]#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ShQuotePath is ShQuote for filesystem paths, but it preserves a
// leading `~` or `~/` so the remote shell still does tilde expansion.
// Without this, `cd '~'` would look for a literal directory named "~"
// and fail.
func ShQuotePath(p string) string {
	switch {
	case p == "~":
		return "~"
	case strings.HasPrefix(p, "~/"):
		return "~/" + ShQuote(p[2:])
	default:
		return ShQuote(p)
	}
}

// Base64Encode returns the base64 encoding of s. Thin wrapper that
// removes one import + two casts at call sites.
func Base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// RedrawInPlace overwrites a previous N-line frame with `content` in
// a single Fprint, avoiding the "blank-then-refill" flash that an
// erase-first sequence would produce.
//
// Caller maintains prevLines (= line count of the last call's
// content) and passes 0 on the first render. Output goes to stderr so
// it stays out of the way when stdout is being piped / redirected.
//
// Shared between `srv ui` (the dashboard) and `srv watch` (periodic
// snapshot) so both get the same flicker-free repaint behaviour.
func RedrawInPlace(content string, prevLines int) {
	var sb strings.Builder
	if prevLines > 0 {
		// Cursor up, then carriage-return to column 0. No erase yet --
		// the per-line `\x1b[K` (erase-to-EOL) lands AFTER content so
		// each line only blanks the stale tail past the new content,
		// never the whole line.
		fmt.Fprintf(&sb, "\x1b[%dA\r", prevLines)
	}
	sb.WriteString(strings.ReplaceAll(content, "\n", "\x1b[K\r\n"))
	// Erase to end of screen handles the case where the new frame is
	// shorter than the old (orphan lines below would otherwise stay).
	sb.WriteString("\x1b[J")
	fmt.Fprint(os.Stderr, sb.String())
}
