// Package platform is srv's single home for OS-specific behaviour.
// Three interfaces -- Process, Console, Crypto -- cover everything
// the rest of the codebase needs to know about the operating system:
//
//   - Process: detached spawn, graceful termination, PID liveness +
//     creation-time queries. Used by internal/daemon and
//     internal/tunnelproc to run sub-processes that outlive their
//     parent.
//   - Console: terminal-related primitives that differ per OS -- the
//     local window-resize signal (SIGWINCH on Unix, polled console
//     buffer on Windows) and the VT-output mode toggle (no-op on
//     Unix, an explicit SetConsoleMode call on Windows).
//   - Crypto: at-rest hardening hooks. On Unix the POSIX file mode
//     covers what we need; on Windows we apply an explicit DACL.
//
// Open/closed principle: adding a new operating system means
// implementing the three interfaces in a new platform_<goos>.go file
// (build-tagged appropriately) and adding a one-line init that
// installs the implementation into the package-level Proc / Term /
// Sec vars. NO existing file needs to change. Removing the
// abstractions or changing their signatures means touching every
// platform impl AND every caller, so the cost asymmetry is exactly
// what open/closed wants: cheap to extend, expensive to break.
//
// Test mockability: package-level vars are reassignable, so tests
// that want to drive failure paths (HardenKeyFile rejects, SignalTerminate
// reports EPERM) can swap in a stub implementation, restore via defer.
// The interface methods stay pure (no hidden state on the receiver),
// which keeps the stub story short.
package platform

import "os/exec"

// Process is the spawn + signal + liveness surface every long-lived
// subprocess in srv goes through. The historical duplication --
// applyDetachAttrs lived in both daemon/daemon_spawn_*.go and
// tunnelproc/spawn_*.go -- collapsed when both callers started
// depending on this interface instead of their own per-OS file.
type Process interface {
	// Detach applies the OS-specific exec.Cmd.SysProcAttr fields
	// that make the spawned child outlive the parent (Setsid on
	// Unix; DETACHED_PROCESS + new process group + breakaway-from-
	// job on Windows).
	Detach(cmd *exec.Cmd)
	// SignalTerminate sends a "please clean up and exit" signal to
	// `pid`. SIGTERM on Unix; CTRL_BREAK_EVENT on Windows (with a
	// Kill fallback if the ctrl-break delivery fails because the
	// target wasn't in a process group we set up). The child must
	// have been spawned via Detach to receive ctrl-break.
	SignalTerminate(pid int) error
	// PIDAlive reports whether a process with the given PID is
	// currently running. Encapsulates the OS-specific probe (signal
	// 0 on Unix; OpenProcess + GetExitCodeProcess on Windows).
	PIDAlive(pid int) bool
	// PIDStartTime returns the OS-reported creation time of `pid`
	// in unix nanoseconds, or (0, false) if the OS doesn't make
	// that available cheaply (macOS, BSDs without /proc). Used to
	// detect PID reuse on long-uptime hosts.
	PIDStartTime(pid int) (int64, bool)
}

// Console is the local-terminal-shaped interface used by sshx for
// interactive sessions. The Unix story is "the kernel does most of
// the work" (SIGWINCH, native ANSI); the Windows story is "we have
// to poll for resize and explicitly opt into VT processing." Both
// look the same from the caller's perspective.
type Console interface {
	// WatchWindowResize fires `onResize(cols, rows)` whenever the
	// local terminal's size changes. Returns a stop closure callers
	// MUST defer; missing the defer leaks a single goroutine, not
	// a signal subscription.
	WatchWindowResize(onResize func(cols, rows int)) func()
	// EnableLocalVTOutput ensures the local console renders ANSI
	// escape sequences. Returns a restore closure that reverts the
	// console mode when the interactive session ends. No-op on Unix
	// and on modern Windows terminals that already have the bit set.
	EnableLocalVTOutput() func()
}

// Crypto wraps file-level secret-protection hardening that POSIX
// modes can't express. Specifically: the at-rest key file at
// ~/.srv/secret/key gets a tightened DACL on Windows so it doesn't
// inherit parent-directory permissions that may include
// Authenticated Users on shared / domain-joined boxes.
type Crypto interface {
	// HardenKeyFile applies OS-specific tightening beyond the
	// 0600 mode the caller already requested. No-op on Unix
	// (the mode is already the strongest constraint POSIX has).
	// Errors are non-fatal at the call site -- the file is usable
	// even with the inherited ACL, just less hardened.
	HardenKeyFile(path string) error
}

// Proc, Term, Sec are the package-level instances callers reach
// for. Initialised exactly once per process by the platform_<goos>.go
// init() that matches the build target. Tests may overwrite these
// to inject stubs; restore the original value via defer to keep the
// rest of the suite running against the real implementation.
var (
	Proc Process
	Term Console
	Sec  Crypto
)
