package transfer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestLocalHashFirstN(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.bin")
	if err := os.WriteFile(p, []byte("abcdef"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Hash of first 3 bytes ("abc") -- compare against the std-lib
	// computed value so a regression in our impl is caught even if both
	// callsites happen to drift the same way.
	want := sha256.Sum256([]byte("abc"))
	wantHex := hex.EncodeToString(want[:])
	got, err := localHashFirstN(p, 3)
	if err != nil {
		t.Fatalf("localHashFirstN: %v", err)
	}
	if got != wantHex {
		t.Errorf("hash mismatch: got=%s want=%s", got, wantHex)
	}
}

func TestLocalHashFirstNZero(t *testing.T) {
	// n=0 short-circuits to the empty-string sha256 constant without
	// touching the filesystem -- file may not even exist.
	got, err := localHashFirstN("/nonexistent", 0)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != sha256EmptyHex {
		t.Errorf("zero-n: got=%s want=%s", got, sha256EmptyHex)
	}
}

func TestLocalHashFirstNShort(t *testing.T) {
	// File has 2 bytes; asking for 3 should surface the io.CopyN short
	// read so callers (samePrefix) know to fall back to fresh upload.
	dir := t.TempDir()
	p := filepath.Join(dir, "short.bin")
	if err := os.WriteFile(p, []byte("ab"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := localHashFirstN(p, 3); err == nil {
		t.Fatal("expected short-read error, got nil")
	}
}

func TestRunParallelEmptyAndSequential(t *testing.T) {
	// Empty input: no work, no error.
	if err := runParallel(nil, 4, func(fileJob) error { return os.ErrInvalid }); err != nil {
		t.Errorf("nil jobs: got %v, want nil", err)
	}
	// workers=1 short-circuits to sequential; verify by counting.
	jobs := []fileJob{{src: "a"}, {src: "b"}, {src: "c"}}
	count := 0
	if err := runParallel(jobs, 1, func(fileJob) error {
		count++
		return nil
	}); err != nil {
		t.Errorf("sequential: %v", err)
	}
	if count != 3 {
		t.Errorf("sequential count: got %d want 3", count)
	}
}

func TestRunParallelFirstErrorWins(t *testing.T) {
	jobs := []fileJob{{src: "a"}, {src: "b"}, {src: "fail"}, {src: "d"}, {src: "e"}}
	// atomic counter -- multiple worker goroutines invoke the function
	// concurrently; a plain int++ would race the test on go test -race.
	var calls atomic.Int32
	myErr := os.ErrPermission
	err := runParallel(jobs, 4, func(j fileJob) error {
		calls.Add(1)
		if j.src == "fail" {
			return myErr
		}
		return nil
	})
	if err != myErr {
		t.Errorf("expected first error, got %v", err)
	}
	if calls.Load() == 0 {
		t.Errorf("worker should have been invoked at least once")
	}
}

func TestParallelWorkersEnv(t *testing.T) {
	t.Setenv("SRV_TRANSFER_WORKERS", "")
	if got := parallelWorkers(); got != defaultParallelWorkers {
		t.Errorf("unset env: got %d want %d", got, defaultParallelWorkers)
	}
	t.Setenv("SRV_TRANSFER_WORKERS", "8")
	if got := parallelWorkers(); got != 8 {
		t.Errorf("env=8: got %d want 8", got)
	}
	t.Setenv("SRV_TRANSFER_WORKERS", "99")
	if got := parallelWorkers(); got != defaultParallelWorkers {
		t.Errorf("env out of range: got %d want default %d", got, defaultParallelWorkers)
	}
	t.Setenv("SRV_TRANSFER_WORKERS", "notanumber")
	if got := parallelWorkers(); got != defaultParallelWorkers {
		t.Errorf("env invalid: got %d want default %d", got, defaultParallelWorkers)
	}
}

func TestSplitChunksCoversFullRange(t *testing.T) {
	// 17 MiB into 8 MiB chunks -> three pieces: 8, 8, 1.
	chunks := splitChunks(17*1024*1024, 8*1024*1024)
	if len(chunks) != 3 {
		t.Fatalf("got %d chunks, want 3", len(chunks))
	}
	var covered int64
	for i, c := range chunks {
		if c.off != covered {
			t.Errorf("chunk[%d] off=%d, expected contiguous start at %d", i, c.off, covered)
		}
		covered += c.n
	}
	if covered != 17*1024*1024 {
		t.Errorf("total covered=%d, want 17MiB", covered)
	}
	// Last chunk should be the partial 1 MiB tail.
	if last := chunks[len(chunks)-1].n; last != 1024*1024 {
		t.Errorf("last chunk n=%d, want 1MiB", last)
	}
}

func TestSplitChunksZeroSize(t *testing.T) {
	// Zero-sized files still get one zero-length chunk so workers
	// have something to consume; caller decides whether to bother.
	chunks := splitChunks(0, 8*1024*1024)
	if len(chunks) != 1 || chunks[0].n != 0 {
		t.Errorf("zero-size: got %+v, want [{0 0}]", chunks)
	}
}

func TestSplitChunksExactMultiple(t *testing.T) {
	// 16 MiB / 8 MiB = exactly 2 chunks, no partial tail.
	chunks := splitChunks(16*1024*1024, 8*1024*1024)
	if len(chunks) != 2 {
		t.Fatalf("exact: got %d chunks, want 2", len(chunks))
	}
	if chunks[0].n != 8*1024*1024 || chunks[1].n != 8*1024*1024 {
		t.Errorf("exact chunks not equal: %+v", chunks)
	}
}

func TestRunChunkWorkersAllRangesProcessed(t *testing.T) {
	// Build 20 distinct chunks; assert every range is visited
	// exactly once with the right buffer scratch reuse semantics.
	chunks := make([]chunkRange, 20)
	for i := range chunks {
		chunks[i] = chunkRange{off: int64(i) * 1024, n: 1024}
	}
	var visited atomic.Int32
	seen := make([]atomic.Int32, len(chunks))
	err := runChunkWorkers(chunks, 4, func(cr chunkRange, buf []byte) error {
		if len(buf) != 256*1024 {
			t.Errorf("buf len=%d, expected 256KiB scratch", len(buf))
		}
		idx := int(cr.off / 1024)
		seen[idx].Add(1)
		visited.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("runChunkWorkers: %v", err)
	}
	if got := visited.Load(); int(got) != len(chunks) {
		t.Errorf("visited %d, want %d", got, len(chunks))
	}
	for i := range seen {
		if v := seen[i].Load(); v != 1 {
			t.Errorf("chunk[%d] visited %d times, want 1", i, v)
		}
	}
}

func TestRunChunkWorkersFirstErrorWins(t *testing.T) {
	chunks := []chunkRange{{0, 1}, {1, 1}, {2, 1}, {3, 1}, {4, 1}}
	myErr := os.ErrPermission
	err := runChunkWorkers(chunks, 4, func(cr chunkRange, _ []byte) error {
		if cr.off == 2 {
			return myErr
		}
		return nil
	})
	if err != myErr {
		t.Errorf("got %v, want first error %v", err, myErr)
	}
}
