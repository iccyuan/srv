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
