//go:build !windows

package srvutil

import "syscall"

// platformPidAlive uses signal 0 (no signal sent) to probe
// reachability. err==nil means the process exists and we can signal
// it; EPERM means it exists but we don't have permission (still
// alive). Anything else (ESRCH etc.) means dead.
func platformPidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	if err == syscall.EPERM {
		return true
	}
	return false
}
