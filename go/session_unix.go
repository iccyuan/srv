//go:build !windows

package main

import (
	"os"
	"syscall"
)

func platformSessionID() string {
	return intToStr(os.Getppid())
}

func platformPidAlive(pid int) bool {
	// Signal 0: no signal sent, but error tells us reachability.
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	// EPERM means it exists but we don't have permission.
	if err == syscall.EPERM {
		return true
	}
	return false
}
