//go:build !windows

package session

import (
	"os"

	"srv/internal/srvutil"
)

// platformID returns the direct parent shell's pid as a string.
// Unix shells don't have the "transparent launcher" stack Windows
// has, so a single Getppid is enough.
func platformID() string {
	return srvutil.IntToStr(os.Getppid())
}
