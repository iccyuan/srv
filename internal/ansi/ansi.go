// Package ansi holds the small set of ANSI escape sequences srv uses
// for colour and cursor control. Lives in its own package so feature
// modules (picker, ui, watch) can import just the codes without
// pulling in unrelated formatting helpers.
//
// Naming follows the ECMA-48 / xterm convention but in
// PascalCase to fit Go's export rules:
//
//	ansi.Reset / ansi.Bold / ansi.Dim / ansi.Reverse
//	ansi.Red / ansi.Green / ansi.Yellow / ansi.Blue / ansi.Magenta / ansi.Cyan
//	ansi.Hide / ansi.Show  -- cursor visibility
package ansi

const (
	Reset   = "\x1b[0m"
	Bold    = "\x1b[1m"
	Dim     = "\x1b[2m"
	Reverse = "\x1b[7m"
	Red     = "\x1b[31m"
	Green   = "\x1b[32m"
	Yellow  = "\x1b[33m"
	Blue    = "\x1b[34m"
	Magenta = "\x1b[35m"
	Cyan    = "\x1b[36m"
	Hide    = "\x1b[?25l"
	Show    = "\x1b[?25h"
)
