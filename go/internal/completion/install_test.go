package completion

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStripCompletionBlock_NoBlock(t *testing.T) {
	in := []byte("alias ll='ls -la'\nexport FOO=bar\n")
	got := stripCompletionBlock(in)
	if string(got) != string(in) {
		t.Errorf("input without block was modified:\nwant %q\n got %q", in, got)
	}
}

func TestStripCompletionBlock_SingleBlockAtEnd(t *testing.T) {
	in := []byte("export FOO=bar\n" +
		completionInstallStart + "\nsource <(srv completion bash)\n" + completionInstallEnd + "\n")
	got := stripCompletionBlock(in)
	want := "export FOO=bar\n"
	if string(got) != want {
		t.Errorf("strip mismatch:\nwant %q\n got %q", want, got)
	}
}

func TestStripCompletionBlock_BlockInMiddle(t *testing.T) {
	in := []byte("line1\n" +
		completionInstallStart + "\nfoo\n" + completionInstallEnd + "\nline2\n")
	got := stripCompletionBlock(in)
	want := "line1\nline2\n"
	if string(got) != want {
		t.Errorf("strip middle:\nwant %q\n got %q", want, got)
	}
}

func TestStripCompletionBlock_MultipleStrayBlocks(t *testing.T) {
	in := []byte("a\n" +
		completionInstallStart + "\nold1\n" + completionInstallEnd + "\nb\n" +
		completionInstallStart + "\nold2\n" + completionInstallEnd + "\nc\n")
	got := stripCompletionBlock(in)
	want := "a\nb\nc\n"
	if string(got) != want {
		t.Errorf("strip multiple:\nwant %q\n got %q", want, got)
	}
}

func TestStripCompletionBlock_MalformedKeepsRest(t *testing.T) {
	// No end marker -- we leave the file alone rather than gobble to EOF.
	in := []byte("a\n" + completionInstallStart + "\nno-end\nb\n")
	got := stripCompletionBlock(in)
	if string(got) != string(in) {
		t.Errorf("malformed should be untouched:\nwant %q\n got %q", in, got)
	}
}

func TestWriteBlockToRC_CreatesMissingFile(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, "subdir", ".bashrc")
	if err := writeBlockToRC(rc, "echo hello"); err != nil {
		t.Fatalf("writeBlockToRC: %v", err)
	}
	data, err := os.ReadFile(rc)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, completionInstallStart) || !strings.Contains(s, completionInstallEnd) {
		t.Errorf("missing markers: %q", s)
	}
	if !strings.Contains(s, "echo hello") {
		t.Errorf("missing payload: %q", s)
	}
}

func TestWriteBlockToRC_AppendsToExisting(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".zshrc")
	if err := os.WriteFile(rc, []byte("alias ll='ls -la'\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeBlockToRC(rc, "payload-A"); err != nil {
		t.Fatalf("writeBlockToRC: %v", err)
	}
	data, _ := os.ReadFile(rc)
	s := string(data)
	if !strings.HasPrefix(s, "alias ll='ls -la'\n") {
		t.Errorf("clobbered existing content: %q", s)
	}
	if !strings.Contains(s, "payload-A") {
		t.Errorf("missing payload: %q", s)
	}
}

func TestWriteBlockToRC_IdempotentReplace(t *testing.T) {
	dir := t.TempDir()
	rc := filepath.Join(dir, ".bashrc")
	if err := os.WriteFile(rc, []byte("# top\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := writeBlockToRC(rc, "first"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := writeBlockToRC(rc, "second"); err != nil {
		t.Fatalf("second install: %v", err)
	}
	data, _ := os.ReadFile(rc)
	s := string(data)
	if strings.Contains(s, "first") {
		t.Errorf("old block not stripped: %q", s)
	}
	if !strings.Contains(s, "second") {
		t.Errorf("new block missing: %q", s)
	}
	// Exactly one start marker after re-install.
	if c := strings.Count(s, completionInstallStart); c != 1 {
		t.Errorf("expected 1 start marker after re-install, got %d:\n%s", c, s)
	}
}

func TestWriteBlockToRC_NoBlankLineAccretion(t *testing.T) {
	// Re-installing 3x shouldn't grow the file by adding blank lines.
	dir := t.TempDir()
	rc := filepath.Join(dir, ".bashrc")
	if err := os.WriteFile(rc, []byte("# header\n"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := writeBlockToRC(rc, "payload"); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	data, _ := os.ReadFile(rc)
	// File should be header + one block. No leading blank lines from
	// repeated strip-then-append.
	if strings.Contains(string(data), "\n\n\n") {
		t.Errorf("accreted blank lines:\n%s", data)
	}
}
