package check

import (
	"io"
	"testing"
	"time"
)

func TestZeroReaderRespectsByteCap(t *testing.T) {
	r := &zeroReader{max: 100, deadline: time.Now().Add(time.Second)}
	buf := make([]byte, 30)
	total := 0
	for total < 200 {
		n, err := r.Read(buf)
		total += n
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("unexpected err: %v", err)
		}
	}
	if total != 100 {
		t.Errorf("served %d, want 100", total)
	}
}

func TestZeroReaderRespectsDeadline(t *testing.T) {
	r := &zeroReader{max: 1 << 30, deadline: time.Now().Add(-time.Second)}
	buf := make([]byte, 30)
	n, err := r.Read(buf)
	if err != io.EOF {
		t.Errorf("past-deadline read should EOF; got n=%d err=%v", n, err)
	}
}

func TestCountingWriterEOFsAfterDeadline(t *testing.T) {
	w := &countingWriter{deadline: time.Now().Add(-time.Second)}
	if _, err := w.Write(make([]byte, 10)); err != io.EOF {
		t.Errorf("past-deadline write should EOF; got %v", err)
	}
}

func TestMbpsAndHumanBytes(t *testing.T) {
	// 10 MB in 1s should be 80 Mbps.
	got := mbps(10*1024*1024, time.Second)
	if got < 83 || got > 84 {
		t.Errorf("10MB/s ~ 83.9 Mbps; got %.2f", got)
	}
	if got := humanBytes(1 << 30); got != "1.00 GB" {
		t.Errorf("1GB humanBytes: %s", got)
	}
	if got := humanBytes(500); got != "500 B" {
		t.Errorf("500B humanBytes: %s", got)
	}
}
