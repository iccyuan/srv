package history

import (
	"os"
	"path/filepath"
	"srv/internal/atrest"
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
