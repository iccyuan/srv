package syncx

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// Bandwidth limiting for tar streams. Wires up on push (writer side
// of the io.Pipe feeding `tar -xf -`) and pull (reader side of the
// io.Pipe draining `tar -cf -`). Either way the rate-limit point
// applies backpressure to the SSH session, and SSH's windowed flow
// control propagates that to the wire -- there's no separate "shape
// the socket" step.
//
// The token bucket is reified by need rather than refilled by a
// background goroutine: each call refills based on wall-clock delta
// since the last call. A bucket-empty Write or Read blocks via
// time.Sleep rather than spinning, keeping CPU near zero on the
// "saturated" path. Burst capacity == one second of credit so a
// single large Write doesn't see >1s of inserted latency.

// parseBwLimit accepts "5M", "500K", "2G", "1MiB", "5MB", bare bytes,
// or empty. Suffixes are case-insensitive and use 1024-based units
// (matches rsync's --bwlimit convention). Returns bytes-per-second.
// 0 / "" / "0" means "unlimited" -- caller skips wrapping.
func parseBwLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "0" {
		return 0, nil
	}
	mul := int64(1)
	body := s
	// Strip trailing "iB" / "B" to support "5MiB" / "5MB" forms.
	lower := strings.ToLower(body)
	switch {
	case strings.HasSuffix(lower, "ib"):
		body = body[:len(body)-2]
	case strings.HasSuffix(lower, "b"):
		body = body[:len(body)-1]
	}
	if body == "" {
		return 0, fmt.Errorf("--bwlimit value %q is missing a number", s)
	}
	last := body[len(body)-1]
	switch last {
	case 'k', 'K':
		mul = 1024
		body = body[:len(body)-1]
	case 'm', 'M':
		mul = 1024 * 1024
		body = body[:len(body)-1]
	case 'g', 'G':
		mul = 1024 * 1024 * 1024
		body = body[:len(body)-1]
	}
	var n float64
	if _, err := fmt.Sscanf(body, "%f", &n); err != nil || n < 0 {
		return 0, fmt.Errorf("--bwlimit value %q is not a non-negative number", s)
	}
	bps := int64(n * float64(mul))
	if bps == 0 {
		return 0, fmt.Errorf("--bwlimit value %q rounds to 0 bytes/sec", s)
	}
	return bps, nil
}

// limitedWriter throttles writes to no more than bps bytes per second
// via a token bucket. Used on the producer side of push tar streams.
//
// Not concurrent-safe: the tar producer is single-goroutine, so adding
// a mutex would only cost cycles. If a future caller needs to share
// the writer across goroutines, wrap externally.
type limitedWriter struct {
	w          io.Writer
	bps        int64
	tokens     int64
	lastRefill time.Time
}

func newLimitedWriter(w io.Writer, bps int64) *limitedWriter {
	return &limitedWriter{w: w, bps: bps, tokens: bps, lastRefill: time.Now()}
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	written := 0
	for written < len(p) {
		lw.refill()
		if lw.tokens <= 0 {
			// Sleep just long enough to earn one chunk's worth of
			// tokens -- 10ms granularity keeps the wakeup count bounded
			// and is below human-perceptible jitter.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		chunk := int64(len(p) - written)
		if chunk > lw.tokens {
			chunk = lw.tokens
		}
		n, err := lw.w.Write(p[written : written+int(chunk)])
		written += n
		lw.tokens -= int64(n)
		if err != nil {
			return written, err
		}
	}
	return written, nil
}

func (lw *limitedWriter) refill() {
	now := time.Now()
	elapsed := now.Sub(lw.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	lw.tokens += int64(elapsed * float64(lw.bps))
	if lw.tokens > lw.bps { // burst cap == 1s of credit
		lw.tokens = lw.bps
	}
	lw.lastRefill = now
}

// limitedReader is the read-side analogue. Used on the pull path where
// the network read is what we want to slow down; reading slower keeps
// the SSH channel's flow-control window from sliding open, which
// throttles the sender on the wire.
type limitedReader struct {
	r          io.Reader
	bps        int64
	tokens     int64
	lastRefill time.Time
}

func newLimitedReader(r io.Reader, bps int64) *limitedReader {
	return &limitedReader{r: r, bps: bps, tokens: bps, lastRefill: time.Now()}
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	for {
		lr.refill()
		if lr.tokens <= 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		max := int64(len(p))
		if max > lr.tokens {
			max = lr.tokens
		}
		n, err := lr.r.Read(p[:max])
		lr.tokens -= int64(n)
		return n, err
	}
}

func (lr *limitedReader) refill() {
	now := time.Now()
	elapsed := now.Sub(lr.lastRefill).Seconds()
	if elapsed <= 0 {
		return
	}
	lr.tokens += int64(elapsed * float64(lr.bps))
	if lr.tokens > lr.bps {
		lr.tokens = lr.bps
	}
	lr.lastRefill = now
}
