// Package clierr is the tiny shared error envelope every subcommand
// uses to surface an explicit exit code (or an exit code + message)
// from inside the cmd handler without calling os.Exit directly.
//
// Living in its own package means feature subpackages (theme,
// daemon, ui, tunnel, ...) can produce ExitError values that
// package main's translateExit recognises -- previously errExit was
// pinned to main, so any extracted feature had to swallow exit codes
// or duplicate the type.
package clierr

import "fmt"

// ExitError carries an explicit numeric exit code plus an optional
// message. The cmd handler at the top of main (translateExit) checks
// for this type and propagates the code to os.Exit; non-Code values
// in Msg become a stderr line.
type ExitError struct {
	Code int
	Msg  string
}

// Error makes ExitError satisfy the error interface. When Msg is
// empty, the string includes the code so test failures stay
// informative; when Msg is set, that's what the user sees.
func (e *ExitError) Error() string {
	if e.Msg == "" {
		return fmt.Sprintf("exit %d", e.Code)
	}
	return e.Msg
}

// Errf builds an ExitError with a printf-formatted message. Use code
// 1 for ordinary failures, 2 for usage / argument errors (POSIX
// convention). Replaces main.exitErr.
func Errf(code int, format string, args ...any) error {
	return &ExitError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// Code wraps a bare numeric exit code into an error. Useful when a
// non-cmd helper (the daemon client, runRemoteStream, ...) already
// returned the right code and we just want to propagate it without
// an extra message. Code 0 returns nil. Replaces main.exitCode.
func Code(code int) error {
	if code == 0 {
		return nil
	}
	return &ExitError{Code: code}
}

// CodeOf is the inverse of Code -- pulls the numeric code out of an
// error. nil -> 0, *ExitError -> its code, anything else -> 1. Used
// by cmdRunWithHints to decide whether a remote command exited 127
// (the "did you mean a local subcommand?" hint trigger).
func CodeOf(err error) int {
	if err == nil {
		return 0
	}
	if ex, ok := err.(*ExitError); ok {
		return ex.Code
	}
	return 1
}
