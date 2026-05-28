package history

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"srv/internal/atrest"
	"strings"
	"testing"
)

// resetAtrest forces the atrest package to re-read its key file. Each
// test gets a fresh SRV_HOME, but atrest caches the key behind a
// sync.Once -- without this reset, tests would all share the first
// SRV_HOME's key and writes/reads would mismatch.
func resetAtrest(t *testing.T) {
	t.Helper()
	atrest.ResetForTest()
}

func TestAppendAndReadAll(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)

	Append(Entry{Profile: "prod", Cwd: "/opt", Cmd: "ls", Exit: 0})
	Append(Entry{Profile: "prod", Cwd: "/opt", Cmd: "false", Exit: 1})
	// Empty cmd is a no-op.
	Append(Entry{Profile: "prod", Cmd: ""})

	got, err := ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries; want 2 (%v)", len(got), got)
	}
	if got[0].Cmd != "ls" || got[1].Cmd != "false" || got[1].Exit != 1 {
		t.Fatalf("entries mismatch: %+v", got)
	}
	if got[0].Time == "" {
		t.Errorf("auto Time fill missing")
	}
}

func TestClear(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	Append(Entry{Profile: "p", Cmd: "x"})
	if err := Clear(); err != nil {
		t.Fatal(err)
	}
	got, _ := ReadAll()
	if len(got) != 0 {
		t.Errorf("Clear left %d entries", len(got))
	}
	// Clear on missing file should be a no-op, not an error.
	_ = os.Remove(filepath.Join(dir, "history.jsonl"))
	if err := Clear(); err != nil {
		t.Errorf("Clear on missing file: %v", err)
	}
}

func TestEncryptedAppendRoundtrip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	t.Setenv("SRV_AT_REST_ENCRYPT", "1")
	// Reset atrest's key cache so this test's SRV_HOME picks up the
	// fresh key file rather than a stale one from a previous test.
	resetAtrest(t)

	Append(Entry{Profile: "prod", Cwd: "/x", Cmd: "echo s3cr3t", Exit: 0})

	// Raw file should NOT contain "s3cr3t" in plain text.
	raw, err := os.ReadFile(filepath.Join(dir, "history.jsonl"))
	if err != nil {
		t.Fatalf("read raw: %v", err)
	}
	if contains(raw, []byte("s3cr3t")) {
		t.Errorf("encrypted history file leaks plaintext: %s", raw)
	}
	// Public read path must decrypt and surface the entry intact.
	got, err := ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 1 || got[0].Cmd != "echo s3cr3t" {
		t.Errorf("decrypted entry wrong: %+v", got)
	}
}

func TestMixedEncryptedAndPlaintextRead(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	// First entry is plaintext (env unset)...
	t.Setenv("SRV_AT_REST_ENCRYPT", "")
	resetAtrest(t)
	Append(Entry{Profile: "p", Cmd: "plain"})
	// ...then flip encryption on for the next.
	t.Setenv("SRV_AT_REST_ENCRYPT", "1")
	resetAtrest(t)
	Append(Entry{Profile: "p", Cmd: "encrypted"})

	got, err := ReadAll()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d, want 2: %+v", len(got), got)
	}
	if got[0].Cmd != "plain" || got[1].Cmd != "encrypted" {
		t.Errorf("mixed read order/content wrong: %+v", got)
	}
}

// TestMaybeRotateKeepsTail builds a synthetic JSONL exceeding
// rotateThreshold and verifies that maybeRotate trims to the LAST
// MaxEntries lines exactly, preserving byte content (no re-encode).
func TestMaybeRotateKeepsTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	// rotateThreshold + 100 lines, each carrying its index so we can
	// assert which slice survives. Lines don't need to be valid JSON --
	// the new rotate path slices on '\n' alone and never decodes.
	var buf bytes.Buffer
	total := rotateThreshold + 100
	for i := range total {
		fmt.Fprintf(&buf, "line-%07d\n", i)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	maybeRotate(path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	gotLines := bytes.Count(got, []byte{'\n'})
	if gotLines != MaxEntries {
		t.Fatalf("after rotate: %d lines, want %d", gotLines, MaxEntries)
	}
	// First surviving line must be the (total - MaxEntries)-th original.
	firstIdx := total - MaxEntries
	wantFirst := fmt.Sprintf("line-%07d\n", firstIdx)
	if !strings.HasPrefix(string(got), wantFirst) {
		t.Fatalf("first surviving line = %q, want prefix %q",
			firstLineOf(got), wantFirst)
	}
	// Last line must be the original last one (unchanged).
	wantLast := fmt.Sprintf("line-%07d\n", total-1)
	if !strings.HasSuffix(string(got), wantLast) {
		t.Fatalf("last surviving line = %q, want suffix %q",
			lastLineOf(got), wantLast)
	}
}

// TestMaybeRotateBelowThresholdNoop guards against accidental
// rewriting when the file is small enough that rotation should skip.
func TestMaybeRotateBelowThresholdNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history.jsonl")
	original := []byte("a\nb\nc\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	maybeRotate(path)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("below-threshold rotate mutated file: got %q, want %q", got, original)
	}
}

func firstLineOf(b []byte) string {
	if i := bytes.IndexByte(b, '\n'); i >= 0 {
		return string(b[:i+1])
	}
	return string(b)
}

func lastLineOf(b []byte) string {
	// b ends in '\n'; find the previous one.
	if len(b) == 0 {
		return ""
	}
	end := len(b)
	if b[end-1] == '\n' {
		// scan back from end-2 to find prior newline
		i := bytes.LastIndexByte(b[:end-1], '\n')
		return string(b[i+1:])
	}
	i := bytes.LastIndexByte(b, '\n')
	return string(b[i+1:])
}

func contains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
outer:
	for i := 0; i+len(needle) <= len(haystack); i++ {
		for j := 0; j < len(needle); j++ {
			if haystack[i+j] != needle[j] {
				continue outer
			}
		}
		return true
	}
	return false
}
