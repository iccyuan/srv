//go:build windows

package platform

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

func init() {
	Proc = windowsProcess{}
	Term = windowsConsole{}
	Sec = windowsCrypto{}
}

// --- Process -----------------------------------------------------

// Windows process creation flags. DETACHED_PROCESS hides the child
// from the parent's console; CREATE_NEW_PROCESS_GROUP gives it its
// own group (required for CTRL_BREAK_EVENT routing later);
// CREATE_BREAKAWAY_FROM_JOB escapes the parent's Job Object so
// closing the terminal doesn't propagate down.
const (
	detachedProcess        = 0x00000008
	createNewProcessGroup  = 0x00000200
	createBreakawayFromJob = 0x01000000
)

type windowsProcess struct{}

func (windowsProcess) Detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: detachedProcess | createNewProcessGroup | createBreakawayFromJob,
	}
}

// SignalTerminate sends CTRL_BREAK_EVENT to the child's process
// group (we put it in one via createNewProcessGroup in Detach). The
// Go runtime translates the event into an os.Interrupt delivery the
// subprocess's signal handler catches, so deferred cleanup runs
// before exit. Kill is the fallback when ctrl-break delivery fails
// (which happens if the target wasn't in the group we set up --
// e.g. an external process we didn't spawn).
func (windowsProcess) SignalTerminate(pid int) error {
	if err := sendCtrlBreak(uint32(pid)); err == nil {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return proc.Kill()
}

// procGenerateConsoleCtrlEvent is the lazy-loaded kernel32 proc for
// GenerateConsoleCtrlEvent. Cached at package level so repeated
// terminate calls don't re-resolve the symbol.
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

// PIDAlive probes via OpenProcess + GetExitCodeProcess. On Windows
// os.FindProcess always succeeds even for dead PIDs, so we go
// straight to the kernel for an authoritative answer.
func (windowsProcess) PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	const processQueryLimitedInformation uint32 = 0x1000
	const stillActive uint32 = 259
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

var procGetProcessTimes *syscall.LazyProc

// PIDStartTime asks the kernel for the process creation FILETIME
// via GetProcessTimes and converts to unix nanoseconds. FILETIME
// is 100-ns ticks since 1601-01-01 UTC; subtract the unix epoch
// delta (11644473600 seconds), multiply by 100 -> nanoseconds since
// unix epoch.
func (windowsProcess) PIDStartTime(pid int) (int64, bool) {
	const processQueryLimitedInformation uint32 = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	if procGetProcessTimes == nil {
		mod := syscall.NewLazyDLL("kernel32.dll")
		procGetProcessTimes = mod.NewProc("GetProcessTimes")
	}
	var creation, exit, kernel, user syscall.Filetime
	r1, _, _ := procGetProcessTimes.Call(
		uintptr(h),
		uintptr(unsafe.Pointer(&creation)),
		uintptr(unsafe.Pointer(&exit)),
		uintptr(unsafe.Pointer(&kernel)),
		uintptr(unsafe.Pointer(&user)),
	)
	if r1 == 0 {
		return 0, false
	}
	hi := uint64(creation.HighDateTime)
	lo := uint64(creation.LowDateTime)
	ticks := (hi << 32) | lo
	const epochDeltaSec uint64 = 11644473600
	epochDeltaTicks := epochDeltaSec * 10_000_000
	if ticks < epochDeltaTicks {
		return 0, false
	}
	return int64((ticks - epochDeltaTicks) * 100), true
}

// --- Console -----------------------------------------------------

type windowsConsole struct{}

func consoleSize() (int, int) {
	w, h, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil {
		return 0, 0
	}
	return w, h
}

// WatchWindowResize polls the console buffer dimensions every
// 250ms and fires onResize on transitions. Windows has no SIGWINCH
// equivalent for non-console signal delivery; the poll is cheap
// (one syscall per tick) and 250ms is the cadence the Windows
// Terminal team recommends for "feels live without burning CPU."
func (windowsConsole) WatchWindowResize(onResize func(cols, rows int)) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		lastW, lastH := consoleSize()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				w, h := consoleSize()
				if w == 0 && h == 0 {
					continue
				}
				if w != lastW || h != lastH {
					lastW, lastH = w, h
					onResize(w, h)
				}
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

// EnableLocalVTOutput turns on ENABLE_VIRTUAL_TERMINAL_PROCESSING on
// stdout so the local console actually renders ANSI escape sequences
// the remote PTY emits. Modern terminals (Windows Terminal, ConEmu,
// VSCode integrated) enable this automatically; cmd.exe and some
// older hosts don't. Restoring the original mode on session end
// keeps non-interactive callers' consoles in their original state.
func (windowsConsole) EnableLocalVTOutput() func() {
	h := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if err := windows.GetConsoleMode(h, &mode); err != nil {
		return func() {}
	}
	const enableVTProcessing uint32 = 0x0004
	if mode&enableVTProcessing != 0 {
		return func() {}
	}
	if err := windows.SetConsoleMode(h, mode|enableVTProcessing); err != nil {
		return func() {}
	}
	return func() {
		_ = windows.SetConsoleMode(h, mode)
	}
}

// --- Crypto ------------------------------------------------------

type windowsCrypto struct{}

// HardenKeyFile installs a protected DACL on `path` so only the
// current user's SID has access. The default ACL on a newly-created
// file under %USERPROFILE% is "inherit from parent", which may
// include "Authenticated Users" on shared / domain-joined machines;
// this function pins the file to "only me, no inheritance" regardless
// of where the home directory's ACL came from.
//
// Failures are non-fatal at the call site -- the encrypted file is
// usable with the inherited ACL, just less hardened. The caller
// (atrest.loadOrCreateKey) logs the message.
func (windowsCrypto) HardenKeyFile(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("get token user: %v", err)
	}
	sid := user.User.Sid
	ea := windows.EXPLICIT_ACCESS{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}
	acl, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{ea}, nil)
	if err != nil {
		return fmt.Errorf("acl from entries: %v", err)
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	); err != nil {
		return fmt.Errorf("set named security info: %v", err)
	}
	return nil
}
