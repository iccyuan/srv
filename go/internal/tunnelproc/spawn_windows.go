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

// signalTerminate sends a CTRL_BREAK_EVENT to the tunnel
// subprocess's process group. Because spawn_windows.go sets
// CREATE_NEW_PROCESS_GROUP at start, the child is the group leader
// and the event lands on it; the Go runtime translates the event
// into an os.Interrupt delivery that the subprocess's signal
// handler in run.go catches, runs its cleanup defer (status file
// removal, listener close), and exits.
//
// Falls back to Kill if the ctrl-break delivery fails -- this is
// the historical sledgehammer that gets the process down even when
// the group setup didn't work (e.g. the subprocess was reparented
// or the kernel rejected the event for some odd reason). Stop()
// still force-removes the status file after the kill so a stuck
// file doesn't ghost the next listing.
func signalTerminate(p *os.Process) error {
	if err := sendCtrlBreak(uint32(p.Pid)); err == nil {
		return nil
	}
	return p.Kill()
}

// sendCtrlBreak invokes kernel32!GenerateConsoleCtrlEvent with
// CTRL_BREAK_EVENT (1) and the given process group id. Lazy DLL
// lookup keeps cold-start cost off the daemon's hot path until the
// first tunnel stop actually needs it.
var procGenerateConsoleCtrlEvent *syscall.LazyProc

func sendCtrlBreak(pgid uint32) error {
	if procGenerateConsoleCtrlEvent == nil {
		mod := syscall.NewLazyDLL("kernel32.dll")
		procGenerateConsoleCtrlEvent = mod.NewProc("GenerateConsoleCtrlEvent")
	}
	const ctrlBreakEvent uintptr = 1
	r1, _, err := procGenerateConsoleCtrlEvent.Call(ctrlBreakEvent, uintptr(pgid))
	if r1 == 0 {
		return err
	}
	return nil
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
