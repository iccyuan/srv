package sshx

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

func TestForwardStreamReader_MultiLine(t *testing.T) {
	src := strings.NewReader("line1\nline2\nline3\n")
	var buf bytes.Buffer
	var chunks []string
	var wg sync.WaitGroup
	wg.Add(1)
	forwardStreamReader(src, StreamStdout, &buf, func(kind StreamChunkKind, line string) {
		if kind != StreamStdout {
			t.Errorf("kind=%v, want stdout", kind)
		}
		chunks = append(chunks, line)
	}, &wg)
	wg.Wait()
	if len(chunks) != 3 {
		t.Errorf("got %d chunks, want 3", len(chunks))
	}
	if buf.String() != "line1\nline2\nline3\n" {
		t.Errorf("buf=%q", buf.String())
	}
	for i, want := range []string{"line1\n", "line2\n", "line3\n"} {
		if i < len(chunks) && chunks[i] != want {
			t.Errorf("chunk %d = %q, want %q", i, chunks[i], want)
		}
	}
}

func TestForwardStreamReader_TrailingPartialLine(t *testing.T) {
	// No trailing newline -- the final line should still be emitted
	// once the reader hits EOF.
	src := strings.NewReader("line1\nline2-no-newline")
	var buf bytes.Buffer
	var chunks []string
	var wg sync.WaitGroup
	wg.Add(1)
	forwardStreamReader(src, StreamStderr, &buf, func(kind StreamChunkKind, line string) {
		chunks = append(chunks, line)
	}, &wg)
	wg.Wait()
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %v", len(chunks), chunks)
	}
	if chunks[1] != "line2-no-newline" {
		t.Errorf("trailing partial = %q", chunks[1])
	}
	if buf.String() != "line1\nline2-no-newline" {
		t.Errorf("buf mismatch: %q", buf.String())
	}
}

func TestForwardStreamReader_Empty(t *testing.T) {
	src := strings.NewReader("")
	var buf bytes.Buffer
	var chunks []string
	var wg sync.WaitGroup
	wg.Add(1)
	forwardStreamReader(src, StreamStdout, &buf, func(kind StreamChunkKind, line string) {
		chunks = append(chunks, line)
	}, &wg)
	wg.Wait()
	if len(chunks) != 0 {
		t.Errorf("empty source: got %d chunks", len(chunks))
	}
	if buf.Len() != 0 {
		t.Errorf("buf should be empty: %q", buf.String())
	}
}

func TestForwardStreamReader_NilCallbackStillBuffers(t *testing.T) {
	// nil onChunk should not panic; buffer accumulation must still work.
	src := strings.NewReader("a\nb\n")
	var buf bytes.Buffer
	var wg sync.WaitGroup
	wg.Add(1)
	forwardStreamReader(src, StreamStdout, &buf, nil, &wg)
	wg.Wait()
	if buf.String() != "a\nb\n" {
		t.Errorf("buf=%q", buf.String())
	}
}

// Two forwarders (stdout + stderr) share one callback the way RunStream
// wires them. Without serializeChunks the unsynchronized counter below
// is a data race (-race flags it) and the count may come up short.
func TestSerializeChunks_ConcurrentForwarders(t *testing.T) {
	const n = 2000
	var sbOut, sbErr strings.Builder
	for i := 0; i < n; i++ {
		sbOut.WriteString("out\n")
		sbErr.WriteString("err\n")
	}
	var bufOut, bufErr bytes.Buffer
	var wg sync.WaitGroup
	calls := 0
	bytesSeen := 0
	onChunk := serializeChunks(func(_ StreamChunkKind, line string) {
		calls++
		bytesSeen += len(line)
	})
	wg.Add(2)
	go forwardStreamReader(strings.NewReader(sbOut.String()), StreamStdout, &bufOut, onChunk, &wg)
	go forwardStreamReader(strings.NewReader(sbErr.String()), StreamStderr, &bufErr, onChunk, &wg)
	wg.Wait()
	if calls != 2*n {
		t.Errorf("calls=%d want %d", calls, 2*n)
	}
	if bytesSeen != 2*n*4 {
		t.Errorf("bytesSeen=%d want %d", bytesSeen, 2*n*4)
	}
}

func TestSerializeChunks_NilStaysNil(t *testing.T) {
	if serializeChunks(nil) != nil {
		t.Fatal("nil callback must stay nil so forwardStreamReader skips it")
	}
}
