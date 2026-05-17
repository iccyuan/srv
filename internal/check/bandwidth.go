package check

import (
	"bytes"
	"fmt"
	"io"
	"srv/internal/config"
	"srv/internal/sshx"
	"time"
)

// RunBandwidthProbe measures the SSH link's throughput in both
// directions for `dur` per side and prints a short report.
//
//   - download (remote -> local): runs `dd if=/dev/zero bs=1M count=N`
//     on the remote, reads stdout, counts bytes.
//   - upload   (local -> remote): pipes /dev/zero from local stdin
//     into a remote `cat > /dev/null`, counts bytes shipped.
//
// The probe ignores SSH-level overhead (it measures what the user
// actually gets), so a "150 Mbps" verdict here is what `scp` would
// see if the pipe were always full.
//
// Bytes per side cap at 256 MiB to keep a runaway probe bounded on
// very fast links; on a typical gigabit dial that's the ceiling
// reached in ~2s.
func RunBandwidthProbe(profile *config.Profile, name string, dur time.Duration) int {
	if dur <= 0 {
		dur = 5 * time.Second
	}
	target := profile.Host
	if profile.User != "" {
		target = profile.User + "@" + profile.Host
	}
	fmt.Printf("bandwidth probe %s: %s:%d  (%v per direction)\n\n", name, target, profile.GetPort(), dur)

	c, err := sshx.DialOpts(profile, sshx.DialOptions{StrictHostKey: false})
	if err != nil {
		PrintDialError(err, profile)
		return 1
	}
	defer c.Close()

	const maxBytes = 256 * 1024 * 1024

	// Download: remote dd -> local /dev/null.
	dl, dlErr := bandwidthDown(c, dur, maxBytes)
	if dlErr != nil {
		fmt.Printf("download : FAILED (%v)\n", dlErr)
	} else {
		fmt.Printf("download : %s\n", formatRate(dl.bytes, dl.elapsed))
	}

	// Upload: local /dev/zero stream -> remote cat > /dev/null.
	ul, ulErr := bandwidthUp(c, dur, maxBytes)
	if ulErr != nil {
		fmt.Printf("upload   : FAILED (%v)\n", ulErr)
	} else {
		fmt.Printf("upload   : %s\n", formatRate(ul.bytes, ul.elapsed))
	}

	if dlErr != nil || ulErr != nil {
		return 1
	}
	dlMbps := mbps(dl.bytes, dl.elapsed)
	ulMbps := mbps(ul.bytes, ul.elapsed)
	fmt.Println()
	switch {
	case dlMbps < 1 || ulMbps < 1:
		fmt.Println("verdict  : single-digit Mbps; this is a slow link")
	case dlMbps < 25 || ulMbps < 25:
		fmt.Println("verdict  : usable for interactive work, but file sync will feel slow")
	default:
		fmt.Println("verdict  : link is fast enough that SSH overhead dominates for small commands")
	}
	asym := dlMbps / ulMbps
	if asym > 3 || asym < 0.33 {
		fmt.Printf("note     : directions differ by %.1fx -- one side is bottlenecked\n", asym)
	}
	return 0
}

type bwResult struct {
	bytes   int64
	elapsed time.Duration
}

// bandwidthDown asks the remote to spit zero bytes and counts what
// arrives until duration elapses. Uses `head -c <max>` to bound the
// remote process even when the client takes a while to close stdin.
func bandwidthDown(c *sshx.Client, dur time.Duration, maxBytes int64) (bwResult, error) {
	cmd := fmt.Sprintf("dd if=/dev/zero bs=65536 2>/dev/null | head -c %d", maxBytes)
	counter := &countingWriter{deadline: time.Now().Add(dur)}
	start := time.Now()
	_, runErr := c.RunStreamStdout(cmd, "", counter)
	elapsed := time.Since(start)
	// "deadline reached" -> closed pipe, RunStreamStdout returns
	// non-zero ssh.ExitError when the upstream is killed by SIGPIPE.
	// Treat that as success since we got what we asked for.
	if runErr != nil && counter.n == 0 {
		return bwResult{}, runErr
	}
	return bwResult{bytes: counter.n, elapsed: elapsed}, nil
}

// bandwidthUp streams a local /dev/zero-like reader into a remote
// `cat > /dev/null`, capping at maxBytes and dur. We measure how many
// bytes left the local side -- this is what an upload of a real file
// of the same size would experience.
func bandwidthUp(c *sshx.Client, dur time.Duration, maxBytes int64) (bwResult, error) {
	src := &zeroReader{deadline: time.Now().Add(dur), max: maxBytes}
	start := time.Now()
	_, runErr := c.RunStreamStdin("cat > /dev/null", src)
	elapsed := time.Since(start)
	if runErr != nil && src.served == 0 {
		return bwResult{}, runErr
	}
	return bwResult{bytes: src.served, elapsed: elapsed}, nil
}

// countingWriter discards bytes after a fixed wall-clock deadline.
// Once the deadline passes, Write returns io.EOF so the upstream
// reader (sftp / ssh session pipe) closes cleanly.
type countingWriter struct {
	n        int64
	deadline time.Time
}

func (w *countingWriter) Write(p []byte) (int, error) {
	if time.Now().After(w.deadline) {
		return 0, io.EOF
	}
	w.n += int64(len(p))
	return len(p), nil
}

// zeroReader serves zero bytes until either the byte budget or the
// wall-clock deadline is reached. Used as a /dev/zero substitute that
// works portably (Windows lacks /dev/zero) and that we can bound.
type zeroReader struct {
	served   int64
	max      int64
	deadline time.Time
	zeros    [65536]byte // reused per call to amortize allocation
}

func (r *zeroReader) Read(p []byte) (int, error) {
	if time.Now().After(r.deadline) || r.served >= r.max {
		return 0, io.EOF
	}
	// Bound this read to whichever is smaller: caller buffer, our
	// scratch, or remaining byte budget. Without the budget cap the
	// counter overshoots r.max by up to one buffer.
	remaining := r.max - r.served
	cap := int64(len(p))
	if cap > int64(len(r.zeros)) {
		cap = int64(len(r.zeros))
	}
	if cap > remaining {
		cap = remaining
	}
	n := copy(p[:cap], r.zeros[:cap])
	r.served += int64(n)
	return n, nil
}

func mbps(b int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	bits := float64(b * 8)
	return bits / d.Seconds() / 1_000_000
}

func formatRate(b int64, d time.Duration) string {
	mb := mbps(b, d)
	hb := humanBytes(b)
	return fmt.Sprintf("%-9s in %v  (%.1f Mbps)", hb, d.Round(time.Millisecond), mb)
}

func humanBytes(n int64) string {
	const (
		KB = 1 << 10
		MB = 1 << 20
		GB = 1 << 30
	)
	switch {
	case n >= GB:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(GB))
	case n >= MB:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(MB))
	case n >= KB:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(KB))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

// Silence "unused" if buf gets removed by a future tweak.
var _ = bytes.NewReader
