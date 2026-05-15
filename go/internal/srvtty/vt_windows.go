//go:build windows

package srvtty

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnableLocalVTOutput turns on ENABLE_VIRTUAL_TERMINAL_PROCESSING on
// stdout so the local console actually renders ANSI escape sequences
// the remote PTY emits (colour, cursor positioning, screen erase)
// instead of leaving them as visible garbage on the screen.
//
// Background: x/term.MakeRaw covers stdin (input-side VT for arrow
// keys / mouse via VT_INPUT) but NOT stdout. Modern terminals --
// Windows Terminal, ConEmu, the VSCode integrated terminal -- enable
// this flag automatically when they spawn a child, so for most users
// this function is a no-op confirmation. But cmd.exe and some older
// hosts don't, and srv shouldn't depend on the terminal getting it
// right.
//
// Returns a restore closure the caller defers so the console mode
// reverts when the interactive session ends. Restoring is important
// even on the no-op fast path because we conditionally call this
// only when interactive: a non-interactive caller that doesn't want
// VT on stdout shouldn't have it survive.
//
// On Windows < 10 1809 the constant doesn't exist and SetConsoleMode
// just ignores the bit; the function returns harmlessly.
func EnableLocalVTOutput() func() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		// Not a console (redirected stdout, MSYS pty, etc.): nothing
		// to enable, nothing to restore.
		return func() {}
	}
	const enableVTProcessing uint32 = 0x0004
	if mode&enableVTProcessing != 0 {
		// Already on (the common case in modern terminals). No
		// restore needed.
		return func() {}
	}
	if err := windows.SetConsoleMode(h, mode|enableVTProcessing); err != nil {
		// Older Windows that doesn't recognise the bit: just leave
		// things alone. Remote ANSI escapes will look ugly but the
		// session is otherwise usable.
		return func() {}
	}
	return func() {
		_ = windows.SetConsoleMode(h, mode)
	}
}
