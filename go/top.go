package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmdTop streams remote `top -b -d N` (batch mode, refresh every N
// seconds) with auto-reconnect on SSH drop. Each frame scrolls
// into local stdout -- this is the "log of snapshots" view; for
// in-place curses-style top use `srv -t top` (gets a real pty).
//
// We hardcode batch mode + reasonable defaults so the user doesn't
// have to remember the dozen flags interactive top wants. -c shows
// command line (default on most distros but explicit beats implicit).
// -w 512 sets a wide enough output width that lines aren't truncated
// by top's own column logic before they reach us.
func cmdTop(args []string, cfg *Config, profileOverride string) error {
	interval := 2.0
	width := 512
	var extra []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--interval":
			if i+1 >= len(args) {
				return exitErr(2, "%s requires a value (seconds)", a)
			}
			n, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || n <= 0 {
				return exitErr(2, "bad %s value %q", a, args[i+1])
			}
			interval = n
			i++
		case strings.HasPrefix(a, "-n"):
			n, err := strconv.ParseFloat(a[2:], 64)
			if err != nil || n <= 0 {
				return exitErr(2, "bad -n value %q", a[2:])
			}
			interval = n
		case a == "-w" || a == "--width":
			if i+1 >= len(args) {
				return exitErr(2, "%s requires a value", a)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n <= 0 {
				return exitErr(2, "bad %s value %q", a, args[i+1])
			}
			width = n
			i++
		case a == "--":
			extra = append(extra, args[i+1:]...)
			i = len(args)
		default:
			// Anything we don't recognise is forwarded to top so
			// users can pass -u USER, -p PID, etc. without us having
			// to mirror top's whole flag surface.
			extra = append(extra, a)
		}
	}

	_, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}

	parts := []string{"top", "-b", "-c", "-w", strconv.Itoa(width), "-d", strconv.FormatFloat(interval, 'f', -1, 64)}
	parts = append(parts, extra...)
	remoteCmd := strings.Join(parts, " ")

	fmt.Fprintf(os.Stderr,
		"srv top: %s   (Ctrl-C to stop, auto-reconnect on drop)\n"+
			"  alternatives: `srv -t top` (pty, in-place)   `srv watch -n N <cmd>` (periodic any cmd)\n",
		profile.Host)

	onChunk := func(kind StreamChunkKind, line string) {
		if kind == StreamStderr {
			fmt.Fprint(os.Stderr, line)
		} else {
			fmt.Fprint(os.Stdout, line)
		}
	}
	return streamWithReconnect(profile, remoteCmd, onChunk)
}
