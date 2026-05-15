//go:build !windows

package tunnelproc

import (
	"os"
	"os/exec"
	"syscall"
)

// applyDetachAttrs puts the spawned tunnel subprocess into its own
// session so it doesn't pick up SIGHUP when the parent terminal
// closes. Mirrors daemon.applyDetachAttrs; duplicated rather than
// shared to keep the daemon and tunnelproc packages independent.
func applyDetachAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// signalTerminate sends SIGTERM so the tunnel can clean up its
// listener + status file. The runtime body installs a handler that
// closes the listener and removes the status file before returning,
// so a clean shutdown is the normal case.
func signalTerminate(p *os.Process) error {
	return p.Signal(syscall.SIGTERM)
}

// pidAliveImpl uses signal 0 -- it's a permissions/existence probe
// that doesn't actually deliver anything to the target. ESRCH means
// the pid is gone; EPERM means it exists but isn't ours, which we
// also treat as "alive" so we don't auto-clean a status file whose
// owning process is reachable to *some* user.
func pidAliveImpl(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		if err == syscall.ESRCH {
			return false
		}
	}
	return true
}
