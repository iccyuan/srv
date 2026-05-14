package history

import (
	"os"
	"path/filepath"
	"testing"
)

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
