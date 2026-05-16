package daemon

import (
	"strings"
	"testing"

	"srv/internal/sshx"
)

// parseLsOutput must surface a non-zero `ls` exit as an error.
// Regression: it used to `return []string{}, nil` on any non-zero
// exit, so list_dir on a missing path looked exactly like an empty
// directory and the caller silently believed the dir existed.
func TestParseLsOutput_MissingDirIsError(t *testing.T) {
	res := &sshx.RunCaptureResult{
		ExitCode: 2,
		Stderr:   "ls: cannot access '/no/such/dir': No such file or directory",
	}
	entries, err := parseLsOutput(res)
	if err == nil {
		t.Fatalf("parseLsOutput(exit=2) = (%v, nil); want error", entries)
	}
	if !strings.Contains(err.Error(), "No such file or directory") {
		t.Errorf("error %q should carry the remote stderr", err)
	}
}

func TestParseLsOutput_NonZeroNoStderr(t *testing.T) {
	_, err := parseLsOutput(&sshx.RunCaptureResult{ExitCode: 1})
	if err == nil || !strings.Contains(err.Error(), "exit 1") {
		t.Fatalf("parseLsOutput(exit=1,no stderr) err = %v; want a generic exit error", err)
	}
}

func TestParseLsOutput_Success(t *testing.T) {
	res := &sshx.RunCaptureResult{Stdout: "a.txt\nsub/\r\n\nb.go\n"}
	entries, err := parseLsOutput(res)
	if err != nil {
		t.Fatalf("parseLsOutput(exit=0) err = %v; want nil", err)
	}
	want := []string{"a.txt", "sub/", "b.go"}
	if len(entries) != len(want) {
		t.Fatalf("entries = %v; want %v", entries, want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("entries[%d] = %q; want %q", i, entries[i], want[i])
		}
	}
}

// An existing-but-empty directory (exit 0, no stdout) stays a
// successful empty result -- that case must NOT regress into an error.
func TestParseLsOutput_EmptyDirIsNotError(t *testing.T) {
	entries, err := parseLsOutput(&sshx.RunCaptureResult{ExitCode: 0, Stdout: ""})
	if err != nil {
		t.Fatalf("empty dir err = %v; want nil", err)
	}
	if len(entries) != 0 {
		t.Fatalf("empty dir entries = %v; want []", entries)
	}
}
