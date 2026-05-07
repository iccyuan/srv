package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"
)

// isStdinTTY returns true when stdin is connected to a terminal.
func isStdinTTY() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// terminalSize returns (cols, rows) of stdout's terminal, or (0, 0) on
// failure / non-tty.
func terminalSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

// makeStdinRaw puts the local stdin terminal in raw mode and returns a
// restore function. Returns nil restore if stdin isn't a terminal.
func makeStdinRaw() (func(), error) {
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

// promptPassphrase reads a passphrase from /dev/tty (or stdin) without echo.
func promptPassphrase(keyPath string) ([]byte, error) {
	fmt.Fprintf(os.Stderr, "Enter passphrase for %s: ", keyPath)
	defer fmt.Fprintln(os.Stderr)
	pass, err := term.ReadPassword(int(os.Stdin.Fd()))
	if err != nil {
		return nil, err
	}
	return pass, nil
}

// shQuote single-quotes a string for safe inclusion in a /bin/sh command.
// Replaces internal single quotes with the standard '\” dance.
func shQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !strings.ContainsAny(s, " \t\n\\\"'`$&|;<>*?(){}[]#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shQuotePath is shQuote for filesystem paths, but it preserves a leading
// `~` or `~/` so the remote shell still does tilde expansion. Without this,
// `cd '~'` would look for a literal directory named "~" and fail.
func shQuotePath(p string) string {
	switch {
	case p == "~":
		return "~"
	case strings.HasPrefix(p, "~/"):
		return "~/" + shQuote(p[2:])
	default:
		return shQuote(p)
	}
}

// base64Encode returns the base64 encoding of s.
func base64Encode(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// allDigits reports whether every rune in s is an ASCII digit.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
