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
	return StreamWithReconnectResumable(profile, newTailResumer(paths, initial), onChunk)
}

// newTailResumer picks the right resumer for a tail invocation:
//
//   - single file -> tailByteResumer: tracks bytes-of-stdout seen, on
//     reconnect uses `tail -F -c +<bytes+1>` so the remote re-opens at
//     the exact byte we stopped at and the user sees no repeats. If
//     the file was rotated during the gap, tail -F still picks the
//     new inode up; the byte offset becomes "from start of new file"
//     because the offset exceeds the new file's size only briefly.
//   - multi file  -> tailMultiResumer: there's no way to attribute
//     interleaved "==> path <==" output back to per-file byte offsets
//     reliably, so on reconnect we just drop the initial -n N backlog
//     (set to 0). The user loses the disconnect-window diff for
//     multi-file tails but at least doesn't see N lines of stale
//     backlog re-printed on every reconnect.
func newTailResumer(paths []string, initial int) StreamResumer {
	if len(paths) == 1 {
		return &tailByteResumer{path: paths[0], initial: initial}
	}
	return &tailMultiResumer{paths: paths, initial: initial}
}

type tailByteResumer struct {
	path     string
	initial  int   // -n N for first attempt
	bytes    int64 // stdout bytes seen so far (counts toward resume offset)
	consumed bool  // true once initial backlog has been emitted at least once
}

func (t *tailByteResumer) Cmd() string {
	quoted := srvtty.ShQuotePath(t.path)
	if !t.consumed {
		return fmt.Sprintf("tail -F -n %d %s", t.initial, quoted)
	}
	// +N is 1-indexed (byte position to start AT), so we add 1 to the
	// count of bytes already delivered.
	return fmt.Sprintf("tail -F -c +%d %s", t.bytes+1, quoted)
}

func (t *tailByteResumer) Observe(kind sshx.StreamChunkKind, line string) {
	if kind == sshx.StreamStdout {
		t.bytes += int64(len(line))
		t.consumed = true
	}
}

func (t *tailByteResumer) Suppress(sshx.StreamChunkKind, string) bool {
	// No boundary dupe to drop: -c +N skips the exact byte we already
	// counted, so the next chunk is naturally the NEW data.
	return false
}

type tailMultiResumer struct {
	paths    []string
	initial  int
	consumed bool
}

func (t *tailMultiResumer) Cmd() string {
	quoted := make([]string, len(t.paths))
	for i, p := range t.paths {
		quoted[i] = srvtty.ShQuotePath(p)
	}
	n := t.initial
	if t.consumed {
		// Multi-file resume: skip the backlog on reconnect so the user
		// doesn't see the trailing N lines from each file re-printed.
		// We can't recover the disconnect-window content either way.
		n = 0
	}
	return fmt.Sprintf("tail -F -n %d %s", n, strings.Join(quoted, " "))
}

func (t *tailMultiResumer) Observe(kind sshx.StreamChunkKind, _ string) {
	if kind == sshx.StreamStdout {
		t.consumed = true
	}
}

func (t *tailMultiResumer) Suppress(sshx.StreamChunkKind, string) bool { return false }

// StreamResumer is the per-stream policy that decides which remote
// command to issue on the first attempt and after each reconnect.
// `Cmd` is called once per loop iteration; `Observe` runs for every
// stdout/stderr line so the resumer can update its position (byte
// counter, last-seen timestamp, cursor, etc.). `Suppress` is consulted
// for the very first line after a reconnect so impls can drop the
// duplicate that resume mechanisms typically re-emit at the boundary.
type StreamResumer interface {
	Cmd() string
	Observe(kind sshx.StreamChunkKind, line string)
	Suppress(kind sshx.StreamChunkKind, line string) bool
}

// staticResumer is the no-resume implementation: same command every
// loop, no state. Used when the caller has a fixed command they want
// rerun verbatim after a reconnect (the original Tail behaviour).
type staticResumer struct{ cmd string }

func (s staticResumer) Cmd() string                                { return s.cmd }
func (s staticResumer) Observe(sshx.StreamChunkKind, string)       {}
func (s staticResumer) Suppress(sshx.StreamChunkKind, string) bool { return false }

// StreamWithReconnect runs `remoteCmd` on `profile`, forwarding every
// stdout/stderr line to onChunk, and redials on SSH disconnect with
// exponential backoff. Backwards-compatible thin wrapper over
// StreamWithReconnectResumable using a static resumer.
func StreamWithReconnect(profile *config.Profile, remoteCmd string, onChunk func(sshx.StreamChunkKind, string)) error {
	return StreamWithReconnectResumable(profile, staticResumer{cmd: remoteCmd}, onChunk)
}

// StreamWithReconnectResumable is the resumer-aware streamer. On each
// reconnect it asks the resumer for the next command (typically with
// a position cursor baked in) and wraps onChunk so the resumer also
// sees every line in order to update its state. The first chunk after
// a reconnect is checked against resumer.Suppress so boundary
// duplicates (e.g. journalctl --since=<ts> re-emits the seam line)
// can be filtered out before the user sees them.
//
// Stops cleanly on Ctrl-C / SIGTERM. Returns nil for clean shutdown;
// only a permanently-broken profile (auth, host key) bubbles up an
// error after the first dial fails non-retryably.
func StreamWithReconnectResumable(profile *config.Profile, resumer StreamResumer, onChunk func(sshx.StreamChunkKind, string)) error {
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
	// firstFrame tracks whether we're in the first chunk after a
	// reconnect. The resumer's Suppress is consulted at most once per
	// reconnect, on the very first emitted line in either stream --
	// after that, content flows through unfiltered.
	firstFrame := false
	for {
		select {
		case <-stopCh:
			return nil
		default:
		}

		c, err := sshx.Dial(profile)
		if err != nil {
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

		watcherDone := make(chan struct{})
		go func() {
			select {
			case <-stopCh:
				_ = c.Close()
			case <-watcherDone:
			}
		}()

		backoff = time.Second
		// Wrap onChunk so every emitted line also flows through the
		// resumer's state machine. Order matters: Observe runs first
		// so byte counters reflect *up-to-and-including* this line,
		// then Suppress consumes one resume-boundary duplicate.
		wrapped := func(kind sshx.StreamChunkKind, line string) {
			resumer.Observe(kind, line)
			if firstFrame {
				firstFrame = false
				if resumer.Suppress(kind, line) {
					return
				}
			}
			onChunk(kind, line)
		}
		cmd := resumer.Cmd()
		_, _, _, runErr := c.RunStream(cmd, "", wrapped)
		close(watcherDone)
		_ = c.Close()

		select {
		case <-stopCh:
			return nil
		default:
		}

		// Arm Suppress for the next reconnect iteration.
		firstFrame = true
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
