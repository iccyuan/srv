package syncx

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// TestParseBwLimit_Forms covers every accepted spelling at once -- one
// table beats N near-duplicate tests for what's essentially a parser.
// Whichever new form a user tries (rsync convention says K/M/G are
// 1024 units, suffix "B" / "iB" are decorative), all collapse to the
// same byte-per-second integer.
func TestParseBwLimit_Forms(t *testing.T) {
	cases := []struct {
		in    string
		want  int64
		isErr bool
	}{
		{"", 0, false},      // unset = unlimited
		{"0", 0, false},     // explicit zero = unlimited
		{"500", 500, false}, // bare bytes
		{"500K", 500 * 1024, false},
		{"500k", 500 * 1024, false},
		{"5M", 5 * 1024 * 1024, false},
		{"5MB", 5 * 1024 * 1024, false},
		{"5MiB", 5 * 1024 * 1024, false},
		{"2G", 2 * 1024 * 1024 * 1024, false},
		{"1.5M", int64(1.5 * 1024 * 1024), false},
		// Junk: prefix non-number, negative, missing number.
		{"abc", 0, true},
		{"M", 0, true},
		{"-5M", 0, true},
		// Rounds to zero is an error -- user typed something
		// non-empty so they want SOMETHING, surfacing it as "0"
		// would silently disable the cap.
		{"0.0001K", 0, false}, // 0.0001*1024 = 0.1024 -> truncates to 0 → caught
	}
	for _, c := range cases {
		got, err := parseBwLimit(c.in)
		if c.isErr {
			if err == nil {
				t.Errorf("parseBwLimit(%q) expected error, got %d", c.in, got)
			}
			continue
		}
		// "0.0001K" is the edge case that should fail; check it
		// explicitly rather than enumerating in the table.
		if c.in == "0.0001K" {
			if err == nil {
				t.Errorf("parseBwLimit(%q) expected rounds-to-0 error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseBwLimit(%q) unexpected error: %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("parseBwLimit(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestLimitedWriter_ThrottlesRate verifies the limiter caps throughput
// around the configured rate. We write 200 KB at a 100 KB/s cap and
// expect the elapsed time to be at least ~1s. The lower bound is the
// thing under test; an upper bound is too brittle to assert (CI
// scheduler jitter can push wallclock arbitrarily long).
func TestLimitedWriter_ThrottlesRate(t *testing.T) {
	if testing.Short() {
		t.Skip("rate-limiter timing test; -short skip")
	}
	var sink bytes.Buffer
	lw := newLimitedWriter(&sink, 100*1024) // 100 KB/s
	data := bytes.Repeat([]byte("x"), 200*1024)
	start := time.Now()
	n, err := lw.Write(data)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len(data) {
		t.Errorf("wrote %d bytes, want %d", n, len(data))
	}
	if sink.Len() != len(data) {
		t.Errorf("sink received %d bytes, want %d", sink.Len(), len(data))
	}
	// 200 KB at 100 KB/s and a 1s burst means: first 100 KB drains
	// from the bucket immediately, next 100 KB has to wait ~1s. Allow
	// 800ms as the floor since the sleep granularity (10ms) and
	// scheduler jitter can shave a few ticks.
	if elapsed < 800*time.Millisecond {
		t.Errorf("limiter too permissive: %d KB at 100 KB/s took only %v", n/1024, elapsed)
	}
}

// TestLimitedReader_ThrottlesRate is the symmetric check for reads.
// Same shape as the writer test; same rationale on the lower-bound-
// only assertion.
func TestLimitedReader_ThrottlesRate(t *testing.T) {
	if testing.Short() {
		t.Skip("rate-limiter timing test; -short skip")
	}
	src := strings.NewReader(strings.Repeat("x", 200*1024))
	lr := newLimitedReader(src, 100*1024)
	buf := make([]byte, 8*1024)
	total := 0
	start := time.Now()
	for {
		n, err := lr.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
	}
	elapsed := time.Since(start)
	if total != 200*1024 {
		t.Errorf("read %d bytes, want %d", total, 200*1024)
	}
	if elapsed < 800*time.Millisecond {
		t.Errorf("limiter too permissive: %d KB at 100 KB/s took only %v", total/1024, elapsed)
	}
}

// TestLimitedWriter_PassesAllBytes is the correctness invariant: even
// when the limiter is interleaving Write calls and sleeps, every byte
// of input lands in the sink in order. A bucket-refill bug could
// either drop bytes (n shorter than input) or duplicate them; this
// test catches both via a content check.
func TestLimitedWriter_PassesAllBytes(t *testing.T) {
	var sink bytes.Buffer
	lw := newLimitedWriter(&sink, 10*1024*1024) // 10 MB/s -> effectively no throttle on this size
	want := []byte("hello world, this is a test of the limiter writer\n")
	for i := 0; i < 100; i++ {
		if _, err := lw.Write(want); err != nil {
			t.Fatalf("Write iter %d: %v", i, err)
		}
	}
	got := sink.Bytes()
	if !bytes.Equal(got, bytes.Repeat(want, 100)) {
		t.Errorf("content corruption detected: len=%d want=%d", len(got), len(want)*100)
	}
}
