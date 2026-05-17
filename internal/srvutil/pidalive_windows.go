//go:build windows

package srvutil

import "golang.org/x/sys/windows"

// platformPidAlive opens the process with PROCESS_QUERY_LIMITED_INFORMATION
// to test existence. The handle is closed immediately; we only care
// that the open succeeded.
func platformPidAlive(pid int) bool {
	const PROCESS_QUERY_LIMITED_INFORMATION = 0x1000
	h, err := windows.OpenProcess(PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = windows.CloseHandle(h)
	return true
}
