package transfer

import (
	"fmt"
	"os"
	"sync/atomic"
	"time"
)

// Per-phase wall-clock breakdown for push/pull, off by default.
// Two ways to enable, picked up at runtime so no process restart:
//
//   - touch ~/.srv/transfer-timing.on   (flag file; works for an
//     already-running MCP server since the env-var path can't be
//     altered without re-registering the server with the client)
//   - export SRV_TRANSFER_TIMING=1      (env-var path; suitable for
//     CLI debugging where the user spawns srv themselves)
//
// When on, output goes to ~/.srv/transfer-timing.log (append) AND
// stderr -- the log file persists across calls so an MCP session's
// transfers stay readable after the fact; stderr surfaces in the
// CLI / streaming MCP paths for interactive debugging. When off,
// every call is a single stat()+miss on the flag file (cheap) plus
// no allocations on the timing path.
//
// Output shape (one line per phase, prefixed with a monotonic call
// counter so interleaved push/pull calls don't get confused):
//
//	srv-transfer #3 [push] acquire-client          12.4ms (cache-hit)
//	srv-transfer #3 [push] expand-home            312.7ms
//	srv-transfer #3 [push] sftp-init               31.2ms (cache-hit)
//	srv-transfer #3 [push] stat-remote-target     297.4ms
//	srv-transfer #3 [push] upload                 4.215s
//	srv-transfer #3 [push] total                  4.869s

// envTimingEnabled is read once at package init.
var envTimingEnabled = func() bool {
	v := os.Getenv("SRV_TRANSFER_TIMING")
	return v == "1" || v == "true" || v == "yes" || v == "on"
}()

// timingFlagPath is the file whose mere existence enables timing
// for the next push/pull call. Checked per-call so an MCP server
// can be toggled live by touching the file.
func timingFlagPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + string(os.PathSeparator) + ".srv" + string(os.PathSeparator) + "transfer-timing.on"
}

// timingLogPath is where the per-call breakdowns get appended when
// timing is on. One file shared across calls; one line per phase.
func timingLogPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home + string(os.PathSeparator) + ".srv" + string(os.PathSeparator) + "transfer-timing.log"
}

func timingEnabledNow() bool {
	if envTimingEnabled {
		return true
	}
	p := timingFlagPath()
	if p == "" {
		return false
	}
	_, err := os.Stat(p)
	return err == nil
}

var timingCallCounter int64

// newCallID returns a monotonically increasing id per push/pull call
// so the lines from concurrent transfers don't visually mix.
func newCallID() int64 { return atomic.AddInt64(&timingCallCounter, 1) }

// phaseTimer collects per-phase durations for one push/pull call and
// flushes them in a stable order at the end. The collect-then-flush
// shape means a panicking phase still gets its parents printed (via
// the deferred Flush in the caller) so partial timings survive
// crashes. Zero-overhead when timingEnabled is false: phases just
// record into a slice that never gets read.
type phaseTimer struct {
	id        int64
	op        string // "push" / "pull"
	started   time.Time
	phaseName []string
	phaseDur  []time.Duration
	phaseNote []string
}

func newPhaseTimer(op string) *phaseTimer {
	return &phaseTimer{id: newCallID(), op: op, started: time.Now()}
}

// Record adds a phase. `note` is optional ("" suppressed) -- typically
// used to tag cache-hit / cache-miss / retry-N so post-mortem doesn't
// require correlation with separate log lines.
func (p *phaseTimer) Record(name string, dur time.Duration, note string) {
	if p == nil {
		return
	}
	p.phaseName = append(p.phaseName, name)
	p.phaseDur = append(p.phaseDur, dur)
	p.phaseNote = append(p.phaseNote, note)
}

// Time runs fn, records its duration under `name`, and returns
// whatever fn returned. Convenience wrapper for the common case
// where the entire phase is one function call.
func (p *phaseTimer) Time(name string, fn func() error) error {
	if p == nil {
		return fn()
	}
	start := time.Now()
	err := fn()
	p.Record(name, time.Since(start), "")
	return err
}

// Flush writes all recorded phases to stderr AND ~/.srv/transfer-
// timing.log (append) when timing is on. Safe to call when off
// (no-op). Designed for `defer p.Flush()`.
func (p *phaseTimer) Flush() {
	if p == nil || !timingEnabledNow() {
		return
	}
	total := time.Since(p.started)
	var w *os.File
	if logPath := timingLogPath(); logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			w = f
			defer w.Close()
		}
	}
	emit := func(line string) {
		fmt.Fprint(os.Stderr, line)
		if w != nil {
			_, _ = w.WriteString(line)
		}
	}
	stamp := time.Now().Format("15:04:05.000")
	for i, name := range p.phaseName {
		emit(fmt.Sprintf("%s srv-transfer #%d [%s] %-22s %s%s\n",
			stamp, p.id, p.op, name, fmtDur(p.phaseDur[i]), fmtNote(p.phaseNote[i])))
	}
	emit(fmt.Sprintf("%s srv-transfer #%d [%s] %-22s %s\n",
		stamp, p.id, p.op, "total", fmtDur(total)))
}

func fmtDur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.3fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d.Microseconds())/1000)
	default:
		return fmt.Sprintf("%dµs", d.Microseconds())
	}
}

func fmtNote(note string) string {
	if note == "" {
		return ""
	}
	return " (" + note + ")"
}
