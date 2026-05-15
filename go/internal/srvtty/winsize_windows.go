//go:build windows

package srvtty

import "time"

// WatchWindowResize fires `onResize(cols, rows)` when the local
// terminal's size changes. Windows has no SIGWINCH (signals there are
// vestigial); we poll the console buffer every 250ms and emit the
// callback on transitions. The cost is one syscall per tick which
// barely registers next to the SSH I/O the same session is doing.
//
// 250ms is the sweet spot the Windows Terminal team recommends for
// "feels live but doesn't melt the CPU." If the user resizes the
// window in a continuous drag the remote will see N intermediate
// sizes; that's the same behaviour the Unix path produces because
// SIGWINCH also fires on every internal width change.
func WatchWindowResize(onResize func(cols, rows int)) func() {
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		lastW, lastH := Size()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				w, h := Size()
				if w == 0 && h == 0 {
					continue
				}
				if w != lastW || h != lastH {
					lastW, lastH = w, h
					onResize(w, h)
				}
			}
		}
	}()
	return func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
	}
}
