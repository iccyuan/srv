package streams

import (
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/srvtty"
	"srv/internal/srvutil"
	"srv/internal/sshx"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// untilCond is the exit condition assembled from --until / --until-exit.
// Either, both, or neither can be set: when both are set the first one
// to match wins (OR semantics). Zero value = "never exit on output"
// (the original `watch` behavior; user Ctrl-C is the only exit path).
type untilCond struct {
	re      *regexp.Regexp // --until REGEX (nil if not requested)
	exit    int            // --until-exit N (when hasExit)
	hasExit bool
}

// matched reports whether the latest tick satisfies the user-supplied
// exit condition. Capture errors and nil results never match -- watch
// keeps polling through transient SSH drops, same as the original
// loop. The body scanned for --until is stdout + "\n" + stderr so a
// regex written against either stream Just Works; the frame's
// "--- stderr ---" fence is rendering-only and isn't part of the
// matched text.
func (u untilCond) matched(res *sshx.RunCaptureResult, err error) bool {
	if err != nil || res == nil {
		return false
	}
	if u.hasExit && res.ExitCode == u.exit {
		return true
	}
	if u.re != nil {
		body := res.Stdout
		if res.Stderr != "" {
			body += "\n" + res.Stderr
		}
		return u.re.MatchString(body)
	}
	return false
}

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
func Watch(args []string, cfg *config.Config, profileOverride string) error {
	interval := 2 * time.Second
	diff := false
	var until untilCond
	var cmdArgs []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--interval":
			if i+1 >= len(args) {
				return srvutil.Errf(2, "%s requires a value (seconds)", a)
			}
			n, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || n <= 0 {
				return srvutil.Errf(2, "bad %s value %q (want positive number)", a, args[i+1])
			}
			interval = time.Duration(n * float64(time.Second))
			i++
		case strings.HasPrefix(a, "-n"):
			n, err := strconv.ParseFloat(a[2:], 64)
			if err != nil || n <= 0 {
				return srvutil.Errf(2, "bad -n value %q", a[2:])
			}
			interval = time.Duration(n * float64(time.Second))
		case a == "-d" || a == "--diff":
			diff = true
		case a == "--until":
			if i+1 >= len(args) {
				return srvutil.Errf(2, "--until requires a regex")
			}
			re, err := regexp.Compile(args[i+1])
			if err != nil {
				return srvutil.Errf(2, "bad --until regex %q: %v", args[i+1], err)
			}
			until.re = re
			i++
		case strings.HasPrefix(a, "--until="):
			re, err := regexp.Compile(strings.TrimPrefix(a, "--until="))
			if err != nil {
				return srvutil.Errf(2, "bad --until regex: %v", err)
			}
			until.re = re
		case a == "--until-exit":
			if i+1 >= len(args) {
				return srvutil.Errf(2, "--until-exit requires an exit code")
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				return srvutil.Errf(2, "bad --until-exit value %q (want integer)", args[i+1])
			}
			until.exit = n
			until.hasExit = true
			i++
		case strings.HasPrefix(a, "--until-exit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--until-exit="))
			if err != nil {
				return srvutil.Errf(2, "bad --until-exit value: %v", err)
			}
			until.exit = n
			until.hasExit = true
		case a == "--":
			cmdArgs = append(cmdArgs, args[i+1:]...)
			i = len(args)
		default:
			cmdArgs = append(cmdArgs, a)
		}
	}
	if len(cmdArgs) == 0 {
		return srvutil.Errf(2, "usage: srv watch [-n SECONDS] [--diff] [--until RE | --until-exit N] <command>")
	}
	cmd := strings.Join(cmdArgs, " ")

	profName, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return srvutil.Errf(1, "%v", err)
	}

	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		<-sigCh
		close(stopCh)
	}()

	cwd := config.GetCwd(profName, profile)
	var prevOut string
	var prevLines int
	for {
		select {
		case <-stopCh:
			return nil
		default:
		}

		start := time.Now()
		res, runErr := remote.RunCapture(profile, cwd, cmd)
		latency := time.Since(start)

		frame := buildWatchFrame(cmd, profName, interval, latency, res, runErr, prevOut, diff)
		srvtty.RedrawInPlace(frame, prevLines)
		prevLines = strings.Count(frame, "\n")
		if res != nil {
			prevOut = res.Stdout
		}

		// --until / --until-exit exit AFTER rendering the matched frame
		// so the user can see what tripped the condition. Without the
		// render-first ordering, watch would clear the screen on exit
		// and leave the user with no evidence of the match.
		if until.matched(res, runErr) {
			return nil
		}

		if !waitOrStop(interval, stopCh) {
			return nil
		}
	}
}

// buildWatchFrame composes the header + body for one watch tick. Pulled
// out of Watch so the (otherwise side-effect-free) rendering can be
// covered by tests without driving a real SSH session.
func buildWatchFrame(cmd, profile string, interval, latency time.Duration, res *sshx.RunCaptureResult, runErr error, prev string, diff bool) string {
	var sb strings.Builder
	now := time.Now().Format("15:04:05")
	fmt.Fprintf(&sb, "%sEvery %s on %s%s   %s   %s$ %s%s\n\n",
		srvutil.Bold, fmtSecs(interval), profile, srvutil.Reset, now,
		srvutil.Dim, cmd, srvutil.Reset)
	if runErr != nil {
		fmt.Fprintf(&sb, "%s[srv watch: capture failed: %v]%s\n",
			srvutil.Dim, runErr, srvutil.Reset)
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
		srvutil.Dim, res.ExitCode, latency.Seconds(), srvutil.Reset)
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
			sb.WriteString(srvutil.Reverse)
			sb.WriteString(line)
			sb.WriteString(srvutil.Reset)
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
