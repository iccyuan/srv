//go:build !windows

package platform

import (
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"
)

// init wires the Unix implementations into the package-level vars
// the rest of the codebase consumes. A sibling platform_windows.go
// does the same with Windows-flavoured structs; the matching build
// tag ensures exactly one init runs per binary.
func init() {
	Proc = unixProcess{}
	Term = unixConsole{}
	Sec = unixCrypto{}
}

// --- Process -----------------------------------------------------

type unixProcess struct{}

// Detach makes the spawned child a new session leader so it doesn't
// receive SIGHUP when the parent's terminal closes. Setsid is the
// minimum-viable detach on Unix; nothing else (job control,
// process-group membership, std-handle inheritance) needs altering.
func (unixProcess) Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// SignalTerminate uses SIGTERM so the child can clean up its
// listener + status file. tunnel-proc's runtime installs a handler
// for this; the deferred RemoveStatus fires before the process exits.
func (unixProcess) SignalTerminate(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Signal(syscall.SIGTERM)
}

// PIDAlive probes with signal 0 -- a permissions/existence check
// that doesn't deliver anything. ESRCH means the process is gone;
// EPERM ("exists but not yours") we treat as alive so we don't
// auto-evict a status file whose owning process is still up.
func (unixProcess) PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		if err == syscall.ESRCH {
			return false
		}
	}
	return true
}

// PIDStartTime parses /proc/<pid>/stat for the kernel-recorded
// starttime (field 22, in jiffies since boot). Combined with the
// boot time read out of /proc/uptime we get a wall-clock-equivalent
// timestamp in unix nanoseconds.
//
// macOS / BSD don't have /proc, so the function short-circuits and
// returns (0, false). Callers fall back to PID-only liveness on
// those platforms; the loss is detection of PID reuse, which is
// rare enough that we accept it as a documented limitation.
func (unixProcess) PIDStartTime(pid int) (int64, bool) {
	statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
	if _, err := os.Stat(statPath); err != nil {
		return 0, false
	}
	b, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	// Format: "<pid> (<comm>) <state> <ppid> ... <starttime> ...".
	// <comm> can carry spaces and parens, so anchor on the LAST ')'
	// before field-splitting. starttime is field 22, which is index
	// 19 in the rest-of-line split (fields 1 and 2 are pre-")").
	close := strings.LastIndexByte(string(b), ')')
	if close < 0 || close+2 >= len(b) {
		return 0, false
	}
	rest := strings.Fields(string(b[close+2:]))
	if len(rest) < 20 {
		return 0, false
	}
	startJiffies, err := strconv.ParseInt(rest[19], 10, 64)
	if err != nil {
		return 0, false
	}
	const hz = 100 // user_HZ is 100 on every modern Linux kernel
	boot, ok := bootTime()
	if !ok {
		return 0, false
	}
	startSec := float64(startJiffies) / float64(hz)
	return boot + int64(startSec*1e9), true
}

// bootTime returns the system boot time in unix nanoseconds, read
// from /proc/uptime. macOS/BSD callers never reach here because
// PIDStartTime short-exits on the /proc check above.
func bootTime() (int64, bool) {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return 0, false
	}
	uptimeSec, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, false
	}
	return time.Now().UnixNano() - int64(uptimeSec*1e9), true
}

// --- Console -----------------------------------------------------

type unixConsole struct{}

// consoleSize reads the stdout terminal's column/row count. Pulled
// from golang.org/x/term directly rather than going through
// internal/srvtty so the platform package stays at the bottom of
// the dependency graph (srvtty depends on platform, not the other
// way around). Returns (0, 0) when stdout isn't a terminal.
func consoleSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

// WatchWindowResize plumbs SIGWINCH into the onResize callback so
// the remote PTY learns about local terminal-size changes mid-
// session. SIGWINCH is delivered exactly when the kernel sees a
// resize, no polling needed.
func (unixConsole) WatchWindowResize(onResize func(cols, rows int)) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	stop := make(chan struct{})
	go func() {
		defer signal.Stop(sigCh)
		for {
			select {
			case <-sigCh:
				if w, h := consoleSize(); w > 0 && h > 0 {
					onResize(w, h)
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
}

// EnableLocalVTOutput is a no-op on Unix: ANSI escape rendering is
// the native behaviour, no equivalent of Windows's
// ENABLE_VIRTUAL_TERMINAL_PROCESSING flag exists to toggle. We
// still return a no-op restore so the cross-platform call site can
// defer it uniformly without conditional logic.
func (unixConsole) EnableLocalVTOutput() func() {
	return func() {}
}

// --- Crypto ------------------------------------------------------

type unixCrypto struct{}

// HardenKeyFile is a no-op on Unix: the at-rest key file is created
// with mode 0600 which the kernel enforces. Nothing further to do.
// The function exists so the Windows implementation has a sibling
// to slot into the same interface.
func (unixCrypto) HardenKeyFile(_ string) error {
	return nil
}
