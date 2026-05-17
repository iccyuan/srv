// Package srvtty bundles terminal / TTY interaction helpers used by
// srv: TTY detection, terminal-size lookup, raw-mode toggle, passphrase
// prompt, shell quoting (for safely interpolating into /bin/sh), and
// the in-place redraw primitive shared by `srv ui` and `srv watch`.
//
// Pure stdlib + golang.org/x/term -- no srv-internal deps -- so any
// subpackage can pull this in without dragging the rest of main along.
package srvtty

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"srv/internal/platform"
	"strings"
	"time"

	"golang.org/x/term"
)

// WatchWindowResize and EnableLocalVTOutput are thin wrappers around
// the platform package so callers that already speak srvtty don't
// have to learn a second namespace for tty-shaped helpers. The
// actual per-OS implementations live in internal/platform; srvtty
// is the user-facing facade.

// WatchWindowResize forwards local terminal-size changes to the
// supplied callback. See platform.Console.WatchWindowResize for the
// per-OS implementation notes.
func WatchWindowResize(onResize func(cols, rows int)) func() {
	return platform.Term.WatchWindowResize(onResize)
}

// EnableLocalVTOutput ensures the local console renders ANSI escape
// sequences. Returns a restore closure. See
// platform.Console.EnableLocalVTOutput for the per-OS details.
func EnableLocalVTOutput() func() {
	return platform.Term.EnableLocalVTOutput()
}

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

// KeyReader pumps stdin bytes into a buffered channel so the
// interactive picker / dashboard loop can read with a timeout
// (needed to disambiguate a standalone ESC from an arrow-key escape
// sequence). The pumping goroutine outlives the caller for the rest
// of the process; that's fine for a one-shot CLI tool.
type KeyReader struct {
	ch chan byte
}

// NewKeyReader starts the stdin-pump goroutine and returns a reader
// the caller can poll via Read / ReadWithTimeout.
func NewKeyReader() *KeyReader {
	kr := &KeyReader{ch: make(chan byte, 16)}
	go func() {
		rd := bufio.NewReader(os.Stdin)
		for {
			b, err := rd.ReadByte()
			if err != nil {
				close(kr.ch)
				return
			}
			kr.ch <- b
		}
	}()
	return kr
}

// Read blocks until the next byte or stdin EOF (returns 0, false).
func (kr *KeyReader) Read() (byte, bool) {
	b, ok := <-kr.ch
	return b, ok
}

// ReadWithTimeout returns (b, true) on a byte, (0, false) on timeout
// OR on stdin EOF. Callers that need to distinguish "no input yet"
// from "stdin closed" must observe Read separately.
func (kr *KeyReader) ReadWithTimeout(d time.Duration) (byte, bool) {
	select {
	case b, ok := <-kr.ch:
		if !ok {
			return 0, false
		}
		return b, true
	case <-time.After(d):
		return 0, false
	}
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
