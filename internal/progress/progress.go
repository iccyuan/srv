// Package progress is the SFTP push/pull progress meter -- a
// refreshing single-line "filename  pct  bytes  rate" status on
// stderr while bytes flow, then a clean newline so the next log line
// lands cleanly.
//
// Two hard preconditions guard activation:
//
//  1. quiet == false. The MCP server's stderr is read by the client
//     (Claude Code) as part of every tool result, so any \r-driven
//     refresher would land in conversation history as a long single
//     line of escape-mangled garbage. mcp_loop.go calls SetQuiet(true)
//     at startup; there's intentionally no flag to flip this back on.
//
//  2. srvtty.IsStderrTTY() == true. CLI users with a redirected
//     stderr (`srv pull foo 2>/dev/null`, CI logs) get clean output
//     too.
//
// Updates are throttled to refreshInterval so a fast loopback
// transfer doesn't spend CPU on terminal escapes.
package progress

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"srv/internal/srvtty"
)

const refreshInterval = 100 * time.Millisecond

// quiet is set true under MCP so progress chrome doesn't leak into
// the model's conversation. Toggled via SetQuiet.
var quiet bool

// SetQuiet pins the meter to "disabled" regardless of TTY state.
// Called by mcp_loop.go at startup.
func SetQuiet(q bool) { quiet = q }

// Meter renders byte-transfer progress. Build one with NewMeter and
// feed it bytes via Add or a wrapped NewReader.
type Meter struct {
	label       string
	total       int64
	transferred atomic.Int64
	started     time.Time
	// lastRender is updated only inside Add (single goroutine in the
	// SFTP path). No locking; sloppy reads are fine.
	lastRender time.Time
	enabled    bool
	// labelWidth is the longest label text seen so far -- so an
	// alignment pad keeps the percent / bytes columns stable across a
	// multi-file transfer when filenames vary in length.
	labelWidth int
}

// NewMeter constructs a Meter ready for Add calls. The returned
// pointer is safe to share across the goroutine doing the io.Copy.
func NewMeter(label string, total int64) *Meter {
	return &Meter{
		label:      label,
		total:      total,
		started:    time.Now(),
		enabled:    !quiet && srvtty.IsStderrTTY(),
		labelWidth: len(label),
	}
}

// Add increments the byte counter and (throttled) re-renders the
// line. Safe to call frequently from io.Copy-driven hot paths.
func (p *Meter) Add(n int64) {
	if !p.enabled {
		return
	}
	p.transferred.Add(n)
	if time.Since(p.lastRender) < refreshInterval {
		return
	}
	p.render()
}

// Done renders one final 100% line and emits a newline so subsequent
// log output starts on a fresh row instead of overwriting the bar.
func (p *Meter) Done() {
	if !p.enabled {
		return
	}
	p.render()
	fmt.Fprintln(os.Stderr)
}

func (p *Meter) render() {
	p.lastRender = time.Now()
	n := p.transferred.Load()
	pct := 0
	if p.total > 0 {
		pct = int(n * 100 / p.total)
		if pct > 100 {
			pct = 100
		}
	}
	elapsed := time.Since(p.started).Seconds()
	speed := int64(0)
	if elapsed > 0 {
		speed = int64(float64(n) / elapsed)
	}
	// \x1b[K clears from cursor to EOL so a previous longer line
	// (e.g. just after switching to a shorter filename in dir mode)
	// doesn't leave residue characters past the new content.
	fmt.Fprintf(os.Stderr, "\r\x1b[K%-*s  %3d%%  %s/%s  %s/s",
		p.labelWidth, p.label, pct, FmtBytes(n), FmtBytes(p.total), FmtBytes(speed))
}

// FmtBytes humanizes a byte count: 1.2 KiB / 3.4 MiB / 5.6 GiB.
func FmtBytes(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KiB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1024*1024))
	default:
		return fmt.Sprintf("%.2f GiB", float64(n)/(1024*1024*1024))
	}
}

// reader wraps r so each Read pumps the byte count into the meter.
// Used on the source side of an io.Copy: read locally for push, read
// remotely for pull. Either side counts the same total; reading is
// the natural place because EOF semantics are clearest there.
type reader struct {
	r io.Reader
	m *Meter
}

// NewReader wraps r so each Read feeds m.Add. Returns r unchanged
// when m is nil or the meter is disabled (no overhead in the
// no-render path).
func NewReader(r io.Reader, m *Meter) io.Reader {
	if m == nil || !m.enabled {
		return r
	}
	return &reader{r: r, m: m}
}

func (pr *reader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	if n > 0 {
		pr.m.Add(int64(n))
	}
	return n, err
}

// ShortLabel trims a path to its trailing component(s) so the
// progress label stays compact regardless of how deep the user's
// path is. Keeps the basename and one parent dir for context (e.g.
// ".../assets/img.png" reads better than "img.png" alone in a
// multi-file run).
func ShortLabel(p string) string {
	p = strings.TrimRight(p, "/\\")
	parts := strings.Split(strings.ReplaceAll(p, "\\", "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2] + "/" + parts[len(parts)-1]
	}
	return parts[len(parts)-1]
}

// SumLocalSize returns the total byte size of `path` -- single file
// size for a regular file, recursive sum for a directory. Errors
// collapse to 0 because the metric is informational (post-transfer
// "you sent X bytes"); a missing-file failure already surfaced
// through the transfer rc, no point doubling up here.
func SumLocalSize(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if !st.IsDir() {
		return st.Size()
	}
	var total int64
	_ = filepath.Walk(path, func(_ string, info os.FileInfo, werr error) error {
		if werr != nil || info == nil {
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// FmtRate formats a transfer summary suffix:
// "(152.3 MiB in 24.7s, 6.2 MiB/s)". Returns "" when bytes is 0 or
// duration is zero -- nothing useful to say.
func FmtRate(bytes int64, d time.Duration) string {
	if bytes <= 0 || d <= 0 {
		return ""
	}
	rate := int64(float64(bytes) / d.Seconds())
	return fmt.Sprintf(" (%s in %.1fs, %s/s)", FmtBytes(bytes), d.Seconds(), FmtBytes(rate))
}
