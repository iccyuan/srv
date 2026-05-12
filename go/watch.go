package main

import (
	"fmt"
	"os"
	"os/signal"
	"srv/internal/ansi"
	"srv/internal/srvtty"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Watch runs a remote command repeatedly with an in-place refresh,
// like the BSD/Linux `watch` utility but over SSH. Reuses the daemon
// connection pool when available so each tick doesn't pay a fresh
// handshake.
//
//	srv watch [-n SECONDS] [--diff] [--] <command...>
//
// --diff highlights lines that differ from the previous frame. We
// intentionally do line-granularity diffing (not character-level like
// GNU watch -d) -- it's noisier for fields with rolling counters but
// keeps the code small and the highlight readable for typical
// ps/df/free output.
func cmdWatch(args []string, cfg *Config, profileOverride string) error {
	interval := 2 * time.Second
	diff := false
	var cmdArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--interval":
			if i+1 >= len(args) {
				return exitErr(2, "%s requires a value (seconds)", a)
			}
			n, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || n <= 0 {
				return exitErr(2, "bad %s value %q (want positive number)", a, args[i+1])
			}
			interval = time.Duration(n * float64(time.Second))
			i++
		case strings.HasPrefix(a, "-n"):
			n, err := strconv.ParseFloat(a[2:], 64)
			if err != nil || n <= 0 {
				return exitErr(2, "bad -n value %q", a[2:])
			}
			interval = time.Duration(n * float64(time.Second))
		case a == "-d" || a == "--diff":
			diff = true
		case a == "--":
			cmdArgs = append(cmdArgs, args[i+1:]...)
			i = len(args)
		default:
			cmdArgs = append(cmdArgs, a)
		}
	}
	if len(cmdArgs) == 0 {
		return exitErr(2, "usage: srv watch [-n SECONDS] [--diff] <command>")
	}
	cmd := strings.Join(cmdArgs, " ")

	profName, profile, err := ResolveProfile(cfg, profileOverride)
	if err != nil {
		return exitErr(1, "%v", err)
	}

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		close(stopCh)
	}()

	cwd := GetCwd(profName, profile)
	var prevOut string
	var prevLines int
	for {
		select {
		case <-stopCh:
			return nil
		default:
		}

		start := time.Now()
		res, runErr := runRemoteCapture(profile, cwd, cmd)
		latency := time.Since(start)

		frame := buildWatchFrame(cmd, profName, interval, latency, res, runErr, prevOut, diff)
		srvtty.RedrawInPlace(frame, prevLines)
		prevLines = strings.Count(frame, "\n")
		if res != nil {
			prevOut = res.Stdout
		}

		if !waitOrStop(interval, stopCh) {
			return nil
		}
	}
}

// buildWatchFrame composes the header + body for one watch tick. Pulled
// out of Watch so the (otherwise side-effect-free) rendering can be
// covered by tests without driving a real SSH session.
func buildWatchFrame(cmd, profile string, interval, latency time.Duration, res *RunCaptureResult, runErr error, prev string, diff bool) string {
	var sb strings.Builder
	now := time.Now().Format("15:04:05")
	fmt.Fprintf(&sb, "%sEvery %s on %s%s   %s   %s$ %s%s\n\n",
		ansi.Bold, fmtSecs(interval), profile, ansi.Reset, now,
		ansi.Dim, cmd, ansi.Reset)
	if runErr != nil {
		fmt.Fprintf(&sb, "%s[srv watch: capture failed: %v]%s\n",
			ansi.Dim, runErr, ansi.Reset)
		return sb.String()
	}
	if res == nil {
		return sb.String()
	}
	body := res.Stdout
	if res.Stderr != "" {
		if body != "" {
			body += "\n"
		}
		body += "--- stderr ---\n" + res.Stderr
	}
	if diff && prev != "" {
		body = highlightDiffLines(body, prev)
	}
	sb.WriteString(body)
	if !strings.HasSuffix(body, "\n") {
		sb.WriteByte('\n')
	}
	fmt.Fprintf(&sb, "%s[exit %d  capture %.2fs]%s\n",
		ansi.Dim, res.ExitCode, latency.Seconds(), ansi.Reset)
	return sb.String()
}

// highlightDiffLines walks the current output and previous output
// line-by-line; current lines that don't appear at the SAME INDEX in
// prev get wrapped in reverse video. Cheap, no-LCS heuristic -- works
// well for stable-row tables (ps, df, top batch), noisy for sorted
// outputs where row order shifts (a flagged line is just "this row
// looked different last tick").
func highlightDiffLines(current, prev string) string {
	curLines := strings.Split(current, "\n")
	prevLines := strings.Split(prev, "\n")
	var sb strings.Builder
	for i, line := range curLines {
		if i < len(prevLines) && prevLines[i] == line {
			sb.WriteString(line)
		} else {
			sb.WriteString(ansi.Reverse)
			sb.WriteString(line)
			sb.WriteString(ansi.Reset)
		}
		if i < len(curLines)-1 {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// fmtSecs renders an interval as a compact "2s" / "0.5s" / "1m"
// string for the watch header. Sub-second values keep one decimal.
func fmtSecs(d time.Duration) string {
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d >= time.Second {
		if d%time.Second == 0 {
			return fmt.Sprintf("%ds", int(d.Seconds()))
		}
	}
	return strconv.FormatFloat(d.Seconds(), 'f', 1, 64) + "s"
}
