//go:build linux || darwin || freebsd || netbsd || openbsd

package moshx

import (
	"os"
	"os/signal"
	"syscall"
)

// registerSigwinch returns a channel that fires whenever the local
// terminal is resized. Unix delivers SIGWINCH for this; we re-emit
// it as a generic channel signal so the client loop's select stays
// platform-agnostic.
func registerSigwinch() <-chan os.Signal {
	ch := make(chan os.Signal, 4)
	signal.Notify(ch, syscall.SIGWINCH)
	return ch
}
