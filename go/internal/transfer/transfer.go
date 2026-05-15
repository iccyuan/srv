package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"srv/internal/config"
	"srv/internal/progress"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/pkg/sftp"
)

// defaultParallelWorkers caps how many files transfer concurrently
// during a recursive push/pull. pkg/sftp's Client is goroutine-safe
// and shares one SSH connection; each worker just shares the same
// underlying channel multiplexer.
//
// Picked empirically: 4 saturates a single gigabit link on mixed-size
// trees without overwhelming the SSH window. Override via the
// SRV_TRANSFER_WORKERS env var (1..32).
const defaultParallelWorkers = 4

// Single-file parallel-chunk transfer knobs. SSH/SFTP's per-channel
// window is ~256 KiB which is the bottleneck on high-RTT (>50ms)
// links: a single sequential stream can't keep the pipe full
// regardless of the actual bandwidth. Splitting a large file across
// N parallel WriteAt/ReadAt streams over the same SSH connection
// fills the pipe by overlapping window-refresh round-trips. Wins
// scale ~linearly with N up to roughly 8-16 streams.
//
// Threshold is the minimum file size that triggers the chunked path.
// Below that the per-stream setup cost outweighs the windowing win
// and sequential transfer is faster.
//
// Override via SRV_TRANSFER_CHUNK_THRESHOLD / _CHUNK_BYTES /
// _CHUNK_PARALLEL respectively; all parse as bytes-or-int with
// reasonable [min,max] clamps.
const (
	defaultChunkThreshold = 32 * 1024 * 1024 // files < 32 MiB stay sequential
	defaultChunkBytes     = 8 * 1024 * 1024  // 8 MiB per chunk
	defaultChunkParallel  = 4                // 4 concurrent streams
)

// parallelWorkers honours SRV_TRANSFER_WORKERS in the [1,32] range,
// defaulting to defaultParallelWorkers when unset or invalid. Used by
// uploadDir / downloadDir to size their goroutine pool.
func parallelWorkers() int {
	if v := strings.TrimSpace(os.Getenv("SRV_TRANSFER_WORKERS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 && n <= 32 {
			return n
		}
	}
	return defaultParallelWorkers
}

func envInt64(name string, def, lo, hi int64) int64 {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= lo && n <= hi {
			return n
		}
	}
	return def
}

func chunkThreshold() int64 {
	return envInt64("SRV_TRANSFER_CHUNK_THRESHOLD", defaultChunkThreshold, 1024, 1<<40)
}

func chunkBytes() int64 {
	return envInt64("SRV_TRANSFER_CHUNK_BYTES", defaultChunkBytes, 64*1024, 1<<30)
}

func chunkParallel() int {
	return int(envInt64("SRV_TRANSFER_CHUNK_PARALLEL", defaultChunkParallel, 1, 32))
}

// expandRemoteHome resolves a leading "~" by asking the remote `echo $HOME`
// once and substituting. Cached on the *sshx.Client.

// PushPath uploads a local file or directory (recursive) to a remote
// path. Returns (exitCode, finalRemotePath, err); finalRemotePath is the
// actual landing location after tilde-expansion and scp-style dir
// adjustment, so callers (CLI confirmation lines, MCP responses) can
// surface where the file really went rather than the user's raw input.
func PushPath(profile *config.Profile, local, remote string, recursive bool) (int, string, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, remote, err
	}
	defer c.Close()

	st, err := os.Stat(local)
	if err != nil {
		return 1, remote, err
	}
	if st.IsDir() && !recursive {
		recursive = true
	}

	resolved, err := c.ExpandRemoteHome(remote)
	if err != nil {
		return 1, remote, err
	}

	s, err := c.SFTP()
	if err != nil {
		return 1, resolved, err
	}
	// scp-style: if the remote target is an existing directory, place the
	// source inside it (file -> dir/basename, dir -> dir/source-name)
	// rather than trying to use the path verbatim. Without this,
	// `srv push foo.exe /existing-dir` calls SFTP Create("/existing-dir")
	// which returns the unhelpful "Failure" (SSH_FX_FAILURE) -- the SFTP
	// server can't overwrite a directory with a file. Mirrors the
	// symmetric handling PullPath has had since v1.
	if rstat, statErr := s.Stat(resolved); statErr == nil && rstat.IsDir() {
		resolved = path.Join(resolved, path.Base(local))
	}
	if recursive {
		if err := uploadDir(c, local, resolved); err != nil {
			return 1, resolved, err
		}
	} else {
		if err := Upload(c, local, resolved); err != nil {
			return 1, resolved, err
		}
	}
	return 0, resolved, nil
}

// PullPath downloads a remote file or directory to a local path. Returns
// (exitCode, finalLocalPath, err); finalLocalPath is the actual landing
// location after the "remote-source-name appended when local is an
// existing dir" rule fires, so callers can stat it to report transfer
// size or surface to the user where bytes really ended up.
func PullPath(profile *config.Profile, remote, local string, recursive bool) (int, string, error) {
	c, err := sshx.Dial(profile)
	if err != nil {
		return 255, local, err
	}
	defer c.Close()

	resolved, err := c.ExpandRemoteHome(remote)
	if err != nil {
		return 1, local, err
	}
	s, err := c.SFTP()
	if err != nil {
		return 1, local, err
	}
	st, err := s.Stat(resolved)
	if err != nil {
		return 1, local, err
	}
	if st.IsDir() {
		recursive = true
	}

	// If local target is an existing dir, drop the source's basename inside.
	finalLocal := local
	if li, err := os.Stat(local); err == nil && li.IsDir() {
		finalLocal = filepath.Join(local, path.Base(resolved))
	}
	if recursive {
		if err := downloadDir(c, resolved, finalLocal); err != nil {
			return 1, finalLocal, err
		}
		return 0, finalLocal, nil
	}
	if err := Download(c, resolved, finalLocal); err != nil {
		return 1, finalLocal, err
	}
	return 0, finalLocal, nil
}

// Upload copies local -> remote via SFTP. If a partial remote file
// exists (size strictly between 0 and the local file's size), it first
// verifies that the remote bytes are an exact prefix of the local file
// (via remote sha256 of the first N bytes -- ~80 byte network cost,
// not N bytes). Only then does it append the remainder. Mismatched
// partials are overwritten from scratch. Same-size remote files trigger
// the same prefix check; matching content is a no-op skip (with chmod
// sync so an unrelated permission change still lands).
//
// Large files with no usable partial route to chunkedUpload instead,
// which fills the SSH channel window by writing N parallel slices --
// a 5× win on high-RTT links and a no-op on local LAN where the
// single-stream rate already saturates.
func Upload(c *sshx.Client, local, remote string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}

	src, err := os.Open(local)
	if err != nil {
		return err
	}
	defer src.Close()

	localStat, err := src.Stat()
	if err != nil {
		return err
	}
	localSize := localStat.Size()

	// Ensure remote parent exists.
	if dir := path.Dir(remote); dir != "" && dir != "." {
		_ = s.MkdirAll(dir)
	}

	// Chunked fast path: big file AND no existing remote artefact to
	// resume against. The chunked path always writes from scratch, so
	// we leave the resume logic below in charge whenever there's
	// useful partial state on the other side.
	if localSize >= chunkThreshold() {
		rstat, statErr := s.Stat(remote)
		noPartial := statErr != nil || rstat.Size() == 0
		if noPartial {
			if err := chunkedUpload(c, src, localSize, remote); err != nil {
				return err
			}
			if st, err := os.Stat(local); err == nil {
				_ = s.Chmod(remote, st.Mode().Perm())
			}
			return nil
		}
	}

	var dst *sftp.File
	var startOffset int64
	if rstat, statErr := s.Stat(remote); statErr == nil && rstat.Size() > 0 {
		if rstat.Size() == localSize {
			if same, cmpErr := samePrefix(c, remote, local, localSize); cmpErr == nil && same {
				// Idempotent skip -- but still mirror local mode in case
				// the user changed permissions without touching content.
				if st, err := os.Stat(local); err == nil {
					_ = s.Chmod(remote, st.Mode().Perm())
				}
				return nil
			} else if cmpErr != nil {
				warnNotMCP("srv push: existing-file check failed for %s: %v; restarting\n", remote, cmpErr)
			} else {
				warnNotMCP("srv push: existing %s has same size but different content; restarting\n", remote)
			}
		} else if rstat.Size() < localSize {
			if same, cmpErr := samePrefix(c, remote, local, rstat.Size()); cmpErr == nil && same {
				f, openErr := s.OpenFile(remote, os.O_WRONLY|os.O_APPEND)
				if openErr == nil {
					if _, seekErr := src.Seek(rstat.Size(), io.SeekStart); seekErr == nil {
						dst = f
						startOffset = rstat.Size()
					} else {
						_ = f.Close()
					}
				}
			} else if cmpErr != nil {
				warnNotMCP("srv push: resume check failed for %s: %v; restarting\n", remote, cmpErr)
			} else {
				warnNotMCP("srv push: partial %s does not match source prefix; restarting\n", remote)
			}
		}
	}
	if dst == nil {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		dst, err = s.Create(remote)
		if err != nil {
			return err
		}
		startOffset = 0
	}
	defer dst.Close()

	if startOffset > 0 {
		warnNotMCP("srv push: resuming %s from %d/%d bytes\n", remote, startOffset, localSize)
	}
	// Progress meter -- silent under MCP and on non-TTY stderr (CI / pipes).
	// Resume mode pre-fills the counter so the bar shows the *true* total
	// progress, not just bytes transferred this call.
	meter := progress.NewMeter("push  "+progress.ShortLabel(remote), localSize)
	meter.Add(startOffset)
	if _, err := io.Copy(dst, progress.NewReader(src, meter)); err != nil {
		meter.Done()
		return err
	}
	meter.Done()
	if st, err := os.Stat(local); err == nil {
		_ = s.Chmod(remote, st.Mode().Perm())
	}
	return nil
}

// fileJob is one src->dst pair queued for parallel transfer.
type fileJob struct{ src, dst string }

// chunkRange describes one [off, off+n) byte slice of the file that a
// chunked-transfer worker is responsible for moving. n is the chunk
// length (the last chunk is typically smaller than chunkBytes).
type chunkRange struct{ off, n int64 }

// chunkedUpload writes `localSize` bytes from `src` to `remote` using
// N parallel SFTP WriteAt streams over the same SSH connection. The
// destination is truncate-created up front; workers WriteAt into it
// at non-overlapping offsets. The motivation: a single SFTP stream
// fills its SSH channel window in one RTT but then idles waiting for
// the window-update ACK; opening N streams lets one stream's ACK
// arrive while another is still transmitting, so on a 100ms-RTT link
// throughput goes from window/RTT ≈ 2.5 MB/s to ~ N × that.
//
// Resume logic stays on the sequential Upload path -- this function
// always writes from scratch and the caller has already verified there
// was no partial worth preserving.
func chunkedUpload(c *sshx.Client, src *os.File, localSize int64, remote string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
	dst, err := s.Create(remote)
	if err != nil {
		return err
	}
	defer dst.Close()

	chunks := splitChunks(localSize, chunkBytes())
	parallel := chunkParallel()
	if parallel > len(chunks) {
		parallel = len(chunks)
	}
	if parallel < 1 {
		parallel = 1
	}

	meter := progress.NewMeter("push  "+progress.ShortLabel(remote), localSize)
	defer meter.Done()

	return runChunkWorkers(chunks, parallel, func(cr chunkRange, buf []byte) error {
		// os.File.ReadAt and *sftp.File.WriteAt are both safe for
		// concurrent use as long as the byte ranges don't overlap --
		// which our chunk split guarantees.
		off := cr.off
		remaining := cr.n
		for remaining > 0 {
			toRead := int64(len(buf))
			if remaining < toRead {
				toRead = remaining
			}
			n, rerr := src.ReadAt(buf[:toRead], off)
			if n > 0 {
				if _, werr := dst.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
				meter.Add(int64(n))
				off += int64(n)
				remaining -= int64(n)
			}
			if rerr != nil {
				if rerr == io.EOF {
					return nil
				}
				return rerr
			}
		}
		return nil
	})
}

// chunkedDownload mirrors chunkedUpload in the other direction: each
// worker ReadAt's its slice of the remote file and WriteAt's into the
// local file. pkg/sftp's File.ReadAt and os.File.WriteAt are both
// concurrent-safe; the bottleneck is the SSH channel window, which is
// what we're filling by going parallel.
func chunkedDownload(c *sshx.Client, remote string, remoteSize int64, local string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
	src, err := s.Open(remote)
	if err != nil {
		return err
	}
	defer src.Close()
	dst, err := os.Create(local)
	if err != nil {
		return err
	}
	defer dst.Close()
	// Preallocate so concurrent WriteAt's can land out of order
	// without growing the file unpredictably.
	if err := dst.Truncate(remoteSize); err != nil {
		return err
	}

	chunks := splitChunks(remoteSize, chunkBytes())
	parallel := chunkParallel()
	if parallel > len(chunks) {
		parallel = len(chunks)
	}
	if parallel < 1 {
		parallel = 1
	}

	meter := progress.NewMeter("pull  "+progress.ShortLabel(local), remoteSize)
	defer meter.Done()

	return runChunkWorkers(chunks, parallel, func(cr chunkRange, buf []byte) error {
		off := cr.off
		remaining := cr.n
		for remaining > 0 {
			toRead := int64(len(buf))
			if remaining < toRead {
				toRead = remaining
			}
			n, rerr := src.ReadAt(buf[:toRead], off)
			if n > 0 {
				if _, werr := dst.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
				meter.Add(int64(n))
				off += int64(n)
				remaining -= int64(n)
			}
			if rerr != nil {
				if rerr == io.EOF {
					return nil
				}
				return rerr
			}
		}
		return nil
	})
}

// splitChunks splits a [0, size) range into chunks of `chunk` bytes
// each, with the trailing chunk truncated to whatever's left. Returns
// at least one range even for size 0 (an empty chunk) so workers
// always have something to do; callers that care about empty files
// should short-circuit before this.
func splitChunks(size, chunk int64) []chunkRange {
	if chunk <= 0 {
		chunk = defaultChunkBytes
	}
	if size <= 0 {
		return []chunkRange{{0, 0}}
	}
	out := make([]chunkRange, 0, (size+chunk-1)/chunk)
	for off := int64(0); off < size; off += chunk {
		n := chunk
		if off+n > size {
			n = size - off
		}
		out = append(out, chunkRange{off, n})
	}
	return out
}

// runChunkWorkers fans `chunks` across `parallel` goroutines, calling
// `do(chunk, buf)` for each. Each worker owns a 256 KiB scratch buffer
// re-used across its chunks so we're not allocating per-Range. First
// error wins (atomic.Pointer keeps the read on the worker's hot path
// race-free); peers drain the channel after that without spending
// network bytes on doomed work.
func runChunkWorkers(chunks []chunkRange, parallel int, do func(chunkRange, []byte) error) error {
	if len(chunks) == 0 {
		return nil
	}
	if parallel < 1 {
		parallel = 1
	}
	jobCh := make(chan chunkRange)
	var wg sync.WaitGroup
	var errOnce sync.Once
	var firstErr atomic.Pointer[error]
	setErr := func(e error) { errOnce.Do(func() { firstErr.Store(&e) }) }

	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 256*1024)
			for cr := range jobCh {
				if firstErr.Load() != nil {
					continue
				}
				if err := do(cr, buf); err != nil {
					setErr(err)
				}
			}
		}()
	}
	for _, cr := range chunks {
		jobCh <- cr
	}
	close(jobCh)
	wg.Wait()
	if p := firstErr.Load(); p != nil {
		return *p
	}
	return nil
}

func uploadDir(c *sshx.Client, local, remote string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
	if err := s.MkdirAll(remote); err != nil {
		return err
	}
	// Two-phase walk: create every dir sequentially first so concurrent
	// file uploads never race against a parent that doesn't exist yet.
	// MkdirAll is idempotent so the cost is one round-trip per dir.
	var files []fileJob
	walkErr := filepath.Walk(local, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(local, p)
		rel = filepath.ToSlash(rel)
		dst := remote
		if rel != "." {
			dst = path.Join(remote, rel)
		}
		if info.IsDir() {
			return s.MkdirAll(dst)
		}
		files = append(files, fileJob{src: p, dst: dst})
		return nil
	})
	if walkErr != nil {
		return walkErr
	}
	return runParallel(files, parallelWorkers(), func(j fileJob) error {
		return Upload(c, j.src, j.dst)
	})
}

// Download mirrors Upload's resume logic in the other direction.
// If the local file is a strict prefix of the remote (size 0 < L < R),
// verify the prefix matches via remote sha256 then append the rest.
// Mismatched partials are overwritten from scratch. Large files with
// no usable local partial route to chunkedDownload so the SSH window
// is filled by N parallel ReadAt streams on high-RTT links.
func Download(c *sshx.Client, remote, local string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
	if dir := filepath.Dir(local); dir != "" && dir != "." {
		_ = os.MkdirAll(dir, 0o755)
	}
	src, err := s.Open(remote)
	if err != nil {
		return err
	}
	defer src.Close()

	rstat, err := src.Stat()
	if err != nil {
		return err
	}
	remoteSize := rstat.Size()

	// Chunked fast path: big remote AND no usable local partial.
	if remoteSize >= chunkThreshold() {
		lstat, statErr := os.Stat(local)
		noPartial := statErr != nil || lstat.Size() == 0
		if noPartial {
			// We still need the remote handle to drive ReadAt across
			// goroutines; close the existing one so chunkedDownload's
			// workers can each open their own.
			_ = src.Close()
			return chunkedDownload(c, remote, remoteSize, local)
		}
	}

	var dst *os.File
	var startOffset int64
	if lstat, statErr := os.Stat(local); statErr == nil && lstat.Size() > 0 {
		if lstat.Size() == remoteSize {
			if same, cmpErr := samePrefix(c, remote, local, remoteSize); cmpErr == nil && same {
				return nil
			} else if cmpErr != nil {
				warnNotMCP("srv pull: existing-file check failed for %s: %v; restarting\n", local, cmpErr)
			} else {
				warnNotMCP("srv pull: existing %s has same size but different content; restarting\n", local)
			}
		} else if lstat.Size() < remoteSize {
			if same, cmpErr := samePrefix(c, remote, local, lstat.Size()); cmpErr == nil && same {
				f, openErr := os.OpenFile(local, os.O_WRONLY|os.O_APPEND, 0o644)
				if openErr == nil {
					if _, seekErr := src.Seek(lstat.Size(), io.SeekStart); seekErr == nil {
						dst = f
						startOffset = lstat.Size()
					} else {
						_ = f.Close()
					}
				}
			} else if cmpErr != nil {
				warnNotMCP("srv pull: resume check failed for %s: %v; restarting\n", local, cmpErr)
			} else {
				warnNotMCP("srv pull: partial %s does not match remote prefix; restarting\n", local)
			}
		}
	}
	if dst == nil {
		if _, err := src.Seek(0, io.SeekStart); err != nil {
			return err
		}
		dst, err = os.Create(local)
		if err != nil {
			return err
		}
		startOffset = 0
	}
	defer dst.Close()

	if startOffset > 0 {
		warnNotMCP("srv pull: resuming %s from %d/%d bytes\n", local, startOffset, remoteSize)
	}
	meter := progress.NewMeter("pull  "+progress.ShortLabel(local), remoteSize)
	meter.Add(startOffset)
	if _, err := io.Copy(dst, progress.NewReader(src, meter)); err != nil {
		meter.Done()
		return err
	}
	meter.Done()
	return nil
}

// samePrefix asks the remote to sha256 the first n bytes of `remote`,
// hashes the same range of `local`, and returns true iff the hex digests
// match. Cost: ~80 byte network reply (one short SSH command exec) +
// disk read of n bytes on each side. The byte-stream comparison this
// replaces had to ship n bytes back across the network just to verify
// -- a 5GB resume would download 5GB just to confirm the partial was
// a real prefix. The hash version keeps that on-disk.
func samePrefix(c *sshx.Client, remote, local string, n int64) (bool, error) {
	rh, err := remoteHashFirstN(c, remote, n)
	if err != nil {
		return false, err
	}
	lh, err := localHashFirstN(local, n)
	if err != nil {
		return false, err
	}
	return rh == lh, nil
}

// sha256EmptyHex is sha256("") -- short-circuit when n=0 so we don't
// even bother shelling out.
const sha256EmptyHex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

// remoteHashFirstN runs `head -c n -- path | sha256sum` on the remote
// and returns the 64-char hex digest. Falls through sha256sum →
// shasum -a 256 → openssl so it works on Linux, BSD/macOS, and minimal
// Alpine-style images. The grep filter strips the formatting noise each
// tool prints alongside the hex (e.g. `<hex>  -` vs `(stdin)= <hex>`).
func remoteHashFirstN(c *sshx.Client, p string, n int64) (string, error) {
	if n == 0 {
		return sha256EmptyHex, nil
	}
	cmd := fmt.Sprintf(
		"head -c %d -- %s | { sha256sum 2>/dev/null || shasum -a 256 2>/dev/null || openssl dgst -sha256 -hex 2>/dev/null; } | grep -oE '[0-9a-f]{64}' | head -n1",
		n, srvtty.ShQuotePath(p),
	)
	res, err := c.RunCapture(cmd, "")
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(res.Stdout)
	if h == "" {
		return "", fmt.Errorf("no usable hash command on remote (need sha256sum / shasum / openssl)")
	}
	return h, nil
}

func localHashFirstN(p string, n int64) (string, error) {
	if n == 0 {
		return sha256EmptyHex, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.CopyN(h, f, n); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// quiet is set true under MCP so progress-style stderr noise
// ("restarting partial upload at X", etc.) doesn't leak into tool
// results. mcp_loop.go pins this on startup via SetQuiet.
var quiet bool

// SetQuiet pins warn output off; called from the MCP server's
// startup alongside i18n.SetMCPMode / progress.SetQuiet.
func SetQuiet(q bool) { quiet = q }

// warnNotMCP prints to stderr, but stays silent under MCP -- the
// client there reads stderr as part of the tool result and noisy
// "restarting" / "resuming" lines pollute the model's context.
func warnNotMCP(format string, args ...any) {
	if quiet {
		return
	}
	fmt.Fprintf(os.Stderr, format, args...)
}

func downloadDir(c *sshx.Client, remote, local string) error {
	s, err := c.SFTP()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(local, 0o755); err != nil {
		return err
	}
	var files []fileJob
	walker := s.Walk(remote)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return err
		}
		p := walker.Path()
		info := walker.Stat()
		rel := strings.TrimPrefix(p, remote)
		rel = strings.TrimPrefix(rel, "/")
		dst := local
		if rel != "" {
			dst = filepath.Join(local, rel)
		}
		if info.IsDir() {
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
			continue
		}
		files = append(files, fileJob{src: p, dst: dst})
	}
	return runParallel(files, parallelWorkers(), func(j fileJob) error {
		return Download(c, j.src, j.dst)
	})
}

// runParallel fans `jobs` across `workers` goroutines calling `do(job)`.
// First error wins: as soon as one worker returns non-nil, the rest
// drain the channel quickly (still consuming jobs but skipping work)
// so the caller's wait completes without leaking goroutines.
//
// Falls back to sequential when len(jobs) == 0 or workers <= 1, which
// also keeps the test suite single-threaded for the small tree
// fixtures used in transfer_test.go.
func runParallel(jobs []fileJob, workers int, do func(fileJob) error) error {
	if len(jobs) == 0 {
		return nil
	}
	if workers < 1 {
		workers = 1
	}
	if workers > len(jobs) {
		workers = len(jobs)
	}
	if workers == 1 {
		for _, j := range jobs {
			if err := do(j); err != nil {
				return err
			}
		}
		return nil
	}

	jobCh := make(chan fileJob)
	// firstErr holds the first non-nil error any worker observed.
	// atomic.Pointer keeps the read on the hot "should I skip this
	// job?" path lock-free AND race-detector clean. Plain sync.Once
	// would order the WRITE but not the concurrent READs on line
	// `if firstErr != nil`, which is what `go test -race` flagged
	// on CI.
	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr atomic.Pointer[error]
	)
	setErr := func(e error) {
		errOnce.Do(func() { firstErr.Store(&e) })
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				// Skip remaining work after a peer reported an error --
				// we still drain the channel so the producer's `<-jobCh`
				// loop terminates, just stop spending bandwidth on it.
				if firstErr.Load() != nil {
					continue
				}
				if err := do(j); err != nil {
					setErr(err)
				}
			}
		}()
	}
	for _, j := range jobs {
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()
	if p := firstErr.Load(); p != nil {
		return *p
	}
	return nil
}
