//go:build !linux

package moshx

import (
	"fmt"
	"os"
	"os/exec"
)

// On non-Unix targets the server side is unavailable -- Windows has
// no /dev/ptmx; ConPTY would work but isn't in scope for this v1.
// The client side still builds and runs on Windows; it just can't
// host the server end of a session.
//
// These stubs keep the package compileable across all OSes srv
// itself targets. Tests reach for the real implementation only on
// unix builds (gated by their own build tag if needed).

func openPTY() (*os.File, string, error) {
	return nil, "", fmt.Errorf("srv mosh-server is only supported on unix targets")
}

func startPTYCommand(name string, args []string, env []string, rows, cols uint16) (*os.File, *exec.Cmd, error) {
	return nil, nil, fmt.Errorf("srv mosh-server is only supported on unix targets")
}

func setWinsize(fd uintptr, rows, cols uint16) error {
	return fmt.Errorf("not supported on this platform")
}

func sendWinch(_ *exec.Cmd) {}
