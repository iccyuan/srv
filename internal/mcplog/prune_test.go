package mcplog

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// TestPruneTrimsToTail: an oversize log is cut down to <= keepBytes,
// the cut lands on a line boundary (no partial leading record), and
// the survivors are exactly the original tail.
func TestPruneTrimsToTail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)

	var src bytes.Buffer
	for i := 0; src.Len() < maxBytes+64*1024; i++ {
		src.WriteString("2026-05-16T10:00:00+08:00 [123] tool=run dur=0.1s ok line\n")
	}
	original := src.Bytes()
	if err := os.WriteFile(Path(), original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	kept, dropped, err := Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if dropped <= 0 {
		t.Fatalf("expected bytes dropped, got dropped=%d", dropped)
	}
	got, err := os.ReadFile(Path())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if int64(len(got)) != kept {
		t.Errorf("kept=%d but file is %d bytes", kept, len(got))
	}
	if int64(len(got)) > keepBytes {
		t.Errorf("post-prune file %d B exceeds keepBytes %d", len(got), keepBytes)
	}
	if !bytes.HasSuffix(original, got) {
		t.Error("survivors are not a suffix of the original log")
	}
	// Boundary: the byte immediately before the kept region is '\n',
	// so the file starts at a clean record edge.
	if cut := len(original) - len(got); cut <= 0 || original[cut-1] != '\n' {
		t.Errorf("cut at offset %d is not on a line boundary", cut)
	}
	if strings.HasPrefix(string(got), "2026") && !strings.HasSuffix(string(got), "\n") {
		t.Error("trimmed file should still end with a newline")
	}
}

// TestPruneWithinRetentionIsNoop: a small log is left exactly as-is
// and reports nothing dropped.
func TestPruneWithinRetentionIsNoop(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	content := []byte("2026-05-16T10:00:00+08:00 [123] start v=test\n")
	if err := os.WriteFile(Path(), content, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	kept, dropped, err := Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if dropped != 0 || kept != int64(len(content)) {
		t.Errorf("kept=%d dropped=%d, want kept=%d dropped=0", kept, dropped, len(content))
	}
	got, _ := os.ReadFile(Path())
	if !bytes.Equal(got, content) {
		t.Error("within-retention prune must not modify the file")
	}
}

// TestPruneAbsentFile: no log yet is success, not an error.
func TestPruneAbsentFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	kept, dropped, err := Prune()
	if err != nil || kept != 0 || dropped != 0 {
		t.Errorf("absent-file Prune = (%d,%d,%v), want (0,0,nil)", kept, dropped, err)
	}
}
