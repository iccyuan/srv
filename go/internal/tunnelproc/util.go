package tunnelproc

import (
	"fmt"
	"time"
)

// sleepMillis is a thin readability wrapper around time.Sleep. The
// callers spell out poll intervals in ms because that's how the
// timeouts are documented in the package preamble; converting to
// time.Duration at every callsite obscures the unit.
func sleepMillis(ms int) {
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

// formatStartedAgo turns a unix timestamp into a compact relative
// duration string used by `srv tunnel list` for independent tunnels.
// Matches the daemon-hosted format the existing CLI uses so users
// can't tell the two paths apart by appearance.
//
//	formatStartedAgo(now - 7s)    -> "7s"
//	formatStartedAgo(now - 4m)    -> "4m"
//	formatStartedAgo(now - 1h6m)  -> "1h6m"
//	formatStartedAgo(now - 26h)   -> "1d2h"
func formatStartedAgo(started int64) string {
	if started <= 0 {
		return ""
	}
	d := time.Since(time.Unix(started, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		h := int(d.Hours())
		m := int(d.Minutes()) - h*60
		if m == 0 {
			return fmt.Sprintf("%dh", h)
		}
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		d2 := int(d.Hours())
		days := d2 / 24
		hours := d2 - days*24
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	}
}
