//go:build windows

package daemon

import (
	"os/exec"
	"syscall"
)

// Windows process creation flags.
//
//	DETACHED_PROCESS         = 0x00000008  -- no inherited console
//	CREATE_NEW_PROCESS_GROUP = 0x00000200  -- own process group
//	CREATE_BREAKAWAY_FROM_JOB= 0x01000000  -- escape parent's job object
//
// Without breakaway, terminals like Windows Terminal that put their child
// into a Job will tear our daemon down when the user closes the terminal.
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
