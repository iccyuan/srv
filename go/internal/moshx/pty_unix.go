//go:build linux

package moshx

// Linux-only PTY allocation. The TIOCSPTLCK / TIOCGPTN ioctls used
// below are Linux-kernel-specific; BSDs (including macOS) use the
// POSIX path `posix_openpt` / `grantpt` / `unlockpt` / `ptsname`,
// which Go doesn't expose directly without cgo. For v1 we restrict
// the mosh server side to Linux remotes -- that's the typical SSH
// target anyway. macOS / BSD servers fall through to the stub in
// pty_other.go which returns a clear "not supported" error.

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY allocates a pseudo-terminal pair via /dev/ptmx and returns
// the master file plus the slave path. Closing master + slave is
// the caller's responsibility.
//
// Implemented without a third-party PTY dep because the dance is
// only ~10 lines of ioctls and golang.org/x/sys/unix already gives
// us the constants we need on every BSD-ish target.
func openPTY() (master *os.File, slavePath string, err error) {
	master, err = os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /dev/ptmx: %w", err)
	}
	// Unlock the slave side.
	if err := unix.IoctlSetPointerInt(int(master.Fd()), unix.TIOCSPTLCK, 0); err != nil {
		master.Close()
		return nil, "", fmt.Errorf("unlockpt: %w", err)
	}
	// Ask for the slave number.
	n, err := unix.IoctlGetInt(int(master.Fd()), unix.TIOCGPTN)
	if err != nil {
		master.Close()
		return nil, "", fmt.Errorf("ptsname: %w", err)
	}
	slavePath = fmt.Sprintf("/dev/pts/%d", n)
	return master, slavePath, nil
}

// startPTYCommand fork-execs cmd with stdin/stdout/stderr wired to a
// freshly-allocated PTY slave. Returns the master end (which the
// caller pumps to/from the user) and the process for SIGWINCH /
// signal forwarding / final reap.
//
// The setsid + TIOCSCTTY dance makes the child the controlling
// process for the new PTY -- without it, Ctrl-C from the user
// wouldn't translate into SIGINT for the child.
func startPTYCommand(name string, args []string, env []string, rows, cols uint16) (*os.File, *exec.Cmd, error) {
	master, slavePath, err := openPTY()
	if err != nil {
		return nil, nil, err
	}
	slave, err := os.OpenFile(slavePath, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		return nil, nil, fmt.Errorf("open slave: %w", err)
	}

	c := exec.Command(name, args...)
	c.Env = env
	c.Stdin = slave
	c.Stdout = slave
	c.Stderr = slave
	c.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		// Ctty is the slave fd inside the child process. After
		// dup-mapping stdin/stdout/stderr, those file descriptors
		// (0,1,2) point at the slave -- which is exactly what
		// Ctty:0 selects (the post-dup fd index).
		Ctty: 0,
	}
	if err := c.Start(); err != nil {
		slave.Close()
		master.Close()
		return nil, nil, err
	}
	// Parent doesn't need the slave fd anymore.
	slave.Close()

	if rows > 0 && cols > 0 {
		_ = setWinsize(master.Fd(), rows, cols)
	}
	return master, c, nil
}

// sendWinch fires SIGWINCH at the child so curses apps notice the
// terminal size change. Returns silently on no-process / non-unix.
func sendWinch(proc *exec.Cmd) {
	if proc != nil && proc.Process != nil {
		_ = proc.Process.Signal(syscall.SIGWINCH)
	}
}

// setWinsize updates the PTY's reported window size so applications
// like `top` / `htop` re-render against the new geometry.
func setWinsize(fd uintptr, rows, cols uint16) error {
	ws := struct {
		Row, Col, X, Y uint16
	}{Row: rows, Col: cols}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return errno
	}
	return nil
}
