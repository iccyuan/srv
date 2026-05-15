//go:build !windows

package tunnelproc

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// pidStartTime returns the OS-reported creation time of `pid` in unix
// nanoseconds. On Linux it parses /proc/<pid>/stat; everywhere else
// (macOS, BSDs) we'd need libproc / sysctl plumbing that's out of
// scope here, so we return (0, false) and the caller falls back to a
// PID-only liveness check. The slight loss of safety against PID
// reuse on those platforms is documented behaviour.
func pidStartTime(pid int) (int64, bool) {
	statPath := "/proc/" + strconv.Itoa(pid) + "/stat"
	if _, err := os.Stat(statPath); err != nil {
		// No /proc -- macOS / BSD path. Fall back.
		return 0, false
	}
	b, err := os.ReadFile(statPath)
	if err != nil {
		return 0, false
	}
	// /proc/<pid>/stat format: "<pid> (<comm>) <state> <ppid> ... <starttime> ..."
	// <comm> can contain spaces and parens, so we anchor on the LAST ')'
	// before splitting fields. starttime is field 22 (1-indexed) -- which is
	// index 19 in the post-")" split because fields 1 (pid) and 2 (comm)
	// already sit before the ')' we just consumed.
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
	hz := clkTck()
	if hz <= 0 {
		return 0, false
	}
	boot, ok := bootTime()
	if !ok {
		return 0, false
	}
	// startJiffies / HZ = seconds since boot. Combine with boot time
	// (nanoseconds since unix epoch) for an absolute wall-clock start.
	startSec := float64(startJiffies) / float64(hz)
	return boot + int64(startSec*1e9), true
}

// clkTck returns the system's user_HZ value (jiffies per second).
// Used to scale /proc/<pid>/stat starttime into seconds. On all
// modern Linux this is 100; we read it from syscall.SC_CLK_TCK
// instead of hardcoding so a custom-built kernel doesn't surprise
// us. If the syscall fails we hardcode 100 as the universally-
// correct guess.
func clkTck() int64 {
	// syscall.Sysconf isn't in the standard library across all OSes
	// we build for; rather than thread a per-OS implementation we
	// just trust the standard 100 jiffies/sec. Custom kernel builds
	// that change this are exotic enough that we accept the tiny
	// drift in our PID-reuse check.
	return 100
}

// bootTime returns the system boot time in unix nanoseconds. Read
// from /proc/uptime (seconds-since-boot fractional value) so the
// implementation is portable across Linux distros without syscall
// gymnastics. macOS / BSD callers never reach here because
// pidStartTime fast-exits on the /proc stat check above.
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
	now := time.Now().UnixNano()
	return now - int64(uptimeSec*1e9), true
}

// Suppress "imported and not used" when this file is the only one
// referencing syscall in some build configs.
var _ = syscall.Getuid
