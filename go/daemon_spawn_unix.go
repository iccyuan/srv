//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// applyDetachAttrs makes the spawned child a new session leader, so it
// won't receive SIGHUP when the parent's terminal closes.
func applyDetachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
