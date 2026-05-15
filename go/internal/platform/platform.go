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

// SystemStats samples the local machine's CPU load + memory
// percentage. Used by the `srv ui` dashboard's STATS panel; each OS
// uses its native interface (/proc on Linux, sysctl+vm_stat on
// macOS, Get-CimInstance on Windows). Implementations should set
// Err on failure and return the partial sample so the UI can render
// a "stats unavailable: <err>" line instead of going blank.
type SystemStats interface {
	Sample() Sample
}

// Sample carries one CPU + memory measurement. CPULoad is the
// 1-minute load average on Unix or LoadPercentage/100 on Windows
// (which doesn't map exactly but is the closest cheap signal). When
// Err is non-empty the other fields may be zero or partial; the UI
// renders Err inline below the chart.
type Sample struct {
	CPULoad    float64
	MemPercent float64
	Err        string
}

// Notifier pops a native OS notification. Best-effort: missing
// notifier tools (notify-send not installed, osascript disabled,
// PowerShell missing) return an error the caller logs. Used by the
// daemon's job-completion watcher and any future "long task done"
// callsite.
type Notifier interface {
	// Toast displays title + body via the OS's native notification
	// path. Returns an error iff the OS-side tool failed.
	Toast(title, body string) error
}

// Opener hands a path (or URL) to the OS's default-app launcher.
// `srv open` and `srv code` use this to spawn the user's browser /
// editor / explorer for a remote resource that's been materialised
// locally. Implementations spawn the launcher and return without
// waiting -- the foreground program doesn't block on the spawned
// app.
type Opener interface {
	// Open hands `path` (file path or URL) to the OS's default-app
	// launcher. Returns the exec error if the launcher itself
	// failed to start; whether the launcher then succeeded at
	// opening the file is the launcher's business.
	Open(path string) error
}

// Proc, Term, Sec, Stats, Notif are the package-level instances
// callers reach for. Initialised exactly once per process by the
// platform_<goos>.go init() that matches the build target. Tests
// may overwrite these to inject stubs; restore the original value
// via defer to keep the rest of the suite running against the real
// implementation.
var (
	Proc  Process
	Term  Console
	Sec   Crypto
	Stats SystemStats
	Notif Notifier
	Open  Opener
)
