//go:build windows

package tunnelproc

import (
	"os"
	"os/exec"
	"syscall"
)

// Same detach flags as the daemon spawn uses (see
// internal/daemon/daemon_spawn_windows.go). DETACHED_PROCESS hides
// the child from the parent's console; BREAKAWAY_FROM_JOB ensures
// terminals that group children into a Job object can't tear the
// tunnel down when the user closes the terminal.
const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
)

func applyDetachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob,
	}
}

// signalTerminate on Windows is effectively Kill -- the runtime
// can't deliver SIGTERM to a detached, no-console process. The
// tunnel subprocess won't run its cleanup, so Stop() also force-
// removes the status file after the kill confirms.
func signalTerminate(p *os.Process) error {
	return p.Kill()
}

// pidAliveImpl: OpenProcess via os.FindProcess always succeeds on
// Windows (it returns a process object even for a dead pid), so we
// have to query exit code. A non-STILL_ACTIVE code means the
// process is gone. We use the QUERY_LIMITED_INFORMATION right (0x1000)
// because it's the lowest-privilege handle that still lets
// GetExitCodeProcess succeed; the stdlib doesn't export the
// constant so we spell it inline.
func pidAliveImpl(pid int) bool {
	const (
		processQueryLimitedInformation uint32 = 0x1000
		stillActive                    uint32 = 259
	)
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	return code == stillActive
}
