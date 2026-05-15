//go:build windows

package tunnelproc

import (
	"syscall"
	"unsafe"
)

// pidStartTime queries the kernel for `pid`'s creation FILETIME via
// GetProcessTimes and converts it to unix nanoseconds. FILETIME is
// 100-ns ticks since 1601-01-01 UTC; the unix epoch is 1970-01-01
// UTC -- 11644473600 seconds later. Subtract that base, multiply
// by 100, and we have nanoseconds since unix epoch.
//
// Returns (0, false) on any error: missing rights to open the
// process, dead PID (FindProcess on Windows is non-failing but the
// handle's exit code will say so), etc. Callers fall back to
// PID-only liveness.
func pidStartTime(pid int) (int64, bool) {
	const processQueryLimitedInformation uint32 = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return 0, false
	}
	defer syscall.CloseHandle(h)
	var creation, exit, kernel, user syscall.Filetime
	// Calling syscall.GetProcessTimes directly: it's an unexported
	// helper in some Go versions, so we go through the lazy DLL
	// lookup that x/sys/windows would normally provide. Hand-rolled
	// here to avoid adding another package import for one syscall.
	procGetProcessTimes := kernel32GetProcessTimes()
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
	// FILETIME -> uint64 of 100-ns ticks since 1601-01-01.
	hi := uint64(creation.HighDateTime)
	lo := uint64(creation.LowDateTime)
	ticks := (hi << 32) | lo
	// 11644473600 seconds between 1601-01-01 and 1970-01-01.
	const epochDeltaSec uint64 = 11644473600
	epochDeltaTicks := epochDeltaSec * 10_000_000 // 10M 100-ns ticks per second
	if ticks < epochDeltaTicks {
		return 0, false
	}
	unixTicks := ticks - epochDeltaTicks
	// Each tick is 100 ns; nanos = ticks * 100.
	return int64(unixTicks * 100), true
}

// kernel32GetProcessTimes returns the lazy-loaded proc handle for
// kernel32!GetProcessTimes. Cached at package init so repeated
// pidStartTime calls don't hit the loader. Wrapped in a tiny helper
// instead of an init() so it lazy-loads only on the first call,
// keeping cold-start cost off the daemon's hot path.
var procGetProcessTimes *syscall.LazyProc

func kernel32GetProcessTimes() *syscall.LazyProc {
	if procGetProcessTimes == nil {
		mod := syscall.NewLazyDLL("kernel32.dll")
		procGetProcessTimes = mod.NewProc("GetProcessTimes")
	}
	return procGetProcessTimes
}
