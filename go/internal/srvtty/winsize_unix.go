//go:build !windows

package srvtty

import (
	"os"
	"os/signal"
	"syscall"
)

// WatchWindowResize fires `onResize(cols, rows)` whenever the local
// terminal's size changes. On Unix this is SIGWINCH-driven: the
// kernel delivers the signal to any process whose controlling
// terminal got resized, and we translate that into a fresh GetSize
// callback. Returns a stop function the caller defers; missing the
// defer leaks a small goroutine but not a signal subscription
// (Restore on a closed-channel stop is fine).
//
// Called by srv shell / srv -t after RequestPty so the remote-side
// terminal layout stays in sync with the user's local window. Without
// this, resizing the terminal mid-session leaves vim/htop rendering
// at the original 80x24.
func WatchWindowResize(onResize func(cols, rows int)) func() {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGWINCH)
	stop := make(chan struct{})
	go func() {
		defer signal.Stop(sigCh)
		for {
			select {
			case <-sigCh:
				if w, h := Size(); w > 0 && h > 0 {
					onResize(w, h)
				}
			case <-stop:
				return
			}
		}
	}()
	return func() {
		select {
		case <-stop:
			// already closed
		default:
			close(stop)
		}
	}
}
