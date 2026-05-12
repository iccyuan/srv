package streams

import (
	"fmt"
	"os"
	"os/signal"
	"regexp"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Tail follows one or more remote files in real time. Distinct from
// `srv run "tail -F path"` in two ways:
//
//  1. Auto-reconnect: if the SSH connection drops mid-stream we redial
//     with exponential backoff (1s, 2s, 4s, ..., capped at 30s) and
//     resume. The remote-side `tail -F` re-opens files across log
//     rotation, so the combined effect is "watch this log forever,
//     survive network blips and server restarts."
//  2. Client-side --grep filter that runs after lines arrive locally.
//     Server-side `tail | grep` would buffer at the grep boundary and
//     hide partial lines; doing it locally keeps line-rate latency.
func Tail(args []string, cfg *config.Config, profileOverride string) error {
	if len(args) == 0 {
		return clierr.Errf(2, `usage: srv tail [-n LINES] [--grep REGEX] <remote-path>...  any remote file (auto-reconnect)
see also:
  srv journal -u UNIT [-f]                      systemd journal for a service
  srv logs <id> [-f]                            output of a detached srv job`)
	}

	initial := 10
	grepPat := ""
	var paths []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-n" || a == "--lines":
			if i+1 >= len(args) {
				return clierr.Errf(2, "%s requires a value", a)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return clierr.Errf(2, "bad %s value %q (want non-negative int)", a, args[i+1])
			}
			initial = n
			i++
		case strings.HasPrefix(a, "-n"):
			n, err := strconv.Atoi(a[2:])
			if err != nil || n < 0 {
				return clierr.Errf(2, "bad -n value %q", a[2:])
			}
			initial = n
		case a == "--grep":
			if i+1 >= len(args) {
				return clierr.Errf(2, "--grep requires a value")
			}
			grepPat = args[i+1]
			i++
		case a == "--":
			paths = append(paths, args[i+1:]...)
			i = len(args)
		default:
			paths = append(paths, a)
		}
	}
	if len(paths) == 0 {
		return clierr.Errf(2, "missing remote path")
	}

	var re *regexp.Regexp
	if grepPat != "" {
		r, err := regexp.Compile(grepPat)
		if err != nil {
			return clierr.Errf(2, "bad regex %q: %v", grepPat, err)
		}
		re = r
	}

	_, profile, err := config.Resolve(cfg, profileOverride)
	if err != nil {
		return clierr.Errf(1, "%v", err)
	}

	// Build the remote command. `tail -F` (capital F) follows by name
	// and reopens on truncate / rotation -- the safe default for log
	// files. -n N controls the initial backfill across all files.
	quoted := make([]string, len(paths))
	for i, p := range paths {
		quoted[i] = srvtty.ShQuotePath(p)
	}
	remoteCmd := fmt.Sprintf("tail -F -n %d %s", initial, strings.Join(quoted, " "))

	fmt.Fprintf(os.Stderr,
		"srv tail: %s   (Ctrl-C to stop, auto-reconnect on drop)\n",
		strings.Join(paths, " "))

	onChunk := func(kind sshx.StreamChunkKind, line string) {
		if re != nil && !re.MatchString(line) {
			return
		}
		if kind == sshx.StreamStderr {
			fmt.Fprint(os.Stderr, line)
		} else {
			fmt.Fprint(os.Stdout, line)
		}
	}
	return StreamWithReconnect(profile, remoteCmd, onChunk)
}

// StreamWithReconnect runs `remoteCmd` on `profile`, forwarding every
// stdout/stderr line to onChunk, and redials on SSH disconnect with
// exponential backoff. Stops cleanly on Ctrl-C / SIGTERM. Returns nil
// for clean shutdown; only a permanently-broken profile (auth, host
// key) bubbles up an error after the first dial fails non-retryably.
//
// Reusable: anything that wants "watch a remote command forever with
// reconnect" (tail, watch -n 0, journalctl -f) can call this.
func StreamWithReconnect(profile *config.Profile, remoteCmd string, onChunk func(sshx.StreamChunkKind, string)) error {
	stopCh := make(chan struct{})
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	var stopOnce sync.Once
	stop := func() {
		stopOnce.Do(func() {
			fmt.Fprintln(os.Stderr, "\nsrv tail: stopping.")
			close(stopCh)
		})
	}
	go func() {
		<-sigCh
		stop()
	}()

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		select {
		case <-stopCh:
			return nil
		default:
		}

		c, err := sshx.Dial(profile)
		if err != nil {
			// Auth / host-key errors are deterministic -- another redial
			// won't change the answer, so we surface immediately rather
			// than spin forever.
			if !sshx.IsRetryableDialErr(err) {
				return clierr.Errf(1, "tail: dial: %v", err)
			}
			fmt.Fprintf(os.Stderr, "srv tail: dial failed: %v (retry in %s)\n", err, backoff)
			if !waitOrStop(backoff, stopCh) {
				return nil
			}
			backoff = nextBackoff(backoff, maxBackoff)
			continue
		}

		// Spawn a watcher that closes the SSH client when stop is
		// signaled. Closing the client breaks the running session's
		// transport, so RunStream returns and we fall out of this
		// iteration to check stopCh.
		watcherDone := make(chan struct{})
		go func() {
			select {
			case <-stopCh:
				_ = c.Close()
			case <-watcherDone:
			}
		}()

		backoff = time.Second // reset on a successful connect
		_, _, _, runErr := c.RunStream(remoteCmd, "", onChunk)
		close(watcherDone)
		_ = c.Close()

		select {
		case <-stopCh:
			return nil
		default:
		}

		// Either tail exited (unusual for -F) or the SSH transport
		// dropped. Either way, the user wanted a long-running view, so
		// reconnect after a short wait.
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "srv tail: stream ended: %v (reconnect in %s)\n", runErr, backoff)
		} else {
			fmt.Fprintf(os.Stderr, "srv tail: stream ended cleanly (reconnect in %s)\n", backoff)
		}
		if !waitOrStop(backoff, stopCh) {
			return nil
		}
		backoff = nextBackoff(backoff, maxBackoff)
	}
}

// waitOrStop sleeps for d unless stopCh fires first. Returns true if
// the timer elapsed (caller should keep going), false if stopped.
func waitOrStop(d time.Duration, stopCh <-chan struct{}) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-stopCh:
		return false
	}
}

// nextBackoff doubles d up to max. Stays pure / branchless so we can
// unit-test the schedule without time-mocking.
func nextBackoff(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}
