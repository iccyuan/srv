package mcp

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

// TestReplayDiff_HappyPath writes two replay rows that differ in args
// AND result text, then drives ReplayCmd("diff", "0", "1") through
// stdout capture. We don't try to assert the exact diff body (git's
// output formatting drifts version-to-version) -- the contract is:
//
//   - both header lines render with the right indices and tool names
//   - both an "--- args ---" and "--- result ---" section appear
//   - identical sections collapse to "(no change)"; differing ones don't
func TestReplayDiff_HappyPath(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir())

	now := time.Now()
	a := replayEntry{
		TS:   now,
		Tool: "run",
		Args: map[string]any{"cmd": "uptime", "profile": "prod"},
		Result: toolResult{Content: []toolContent{
			{Type: "text", Text: "load: 0.42\n[ok cwd ~]"},
		}},
		DurMs: 120,
	}
	b := replayEntry{
		TS:   now.Add(time.Minute),
		Tool: "run",
		Args: map[string]any{"cmd": "uptime", "profile": "staging"},
		Result: toolResult{Content: []toolContent{
			{Type: "text", Text: "load: 9.99\n[ok cwd ~]"},
		}},
		DurMs: 140,
	}
	if err := appendReplay(a); err != nil {
		t.Fatalf("appendReplay a: %v", err)
	}
	if err := appendReplay(b); err != nil {
		t.Fatalf("appendReplay b: %v", err)
	}

	out := captureStdout(t, func() {
		if err := ReplayCmd([]string{"diff", "0", "1"}); err != nil {
			t.Fatalf("ReplayCmd diff: %v", err)
		}
	})
	mustContain(t, out, "[0]")
	mustContain(t, out, "[1]")
	mustContain(t, out, "--- args ---")
	mustContain(t, out, "--- result ---")
	// Neither section is identical -- both should NOT collapse.
	if strings.Count(out, "(no change)") != 0 {
		t.Errorf("expected diffing sections, got (no change) somewhere:\n%s", out)
	}
}

// TestReplayDiff_IdenticalSectionsCollapse exercises the equal-payload
// short-circuit: when args (or result) are byte-identical we emit
// "(no change)" rather than shelling out to git for a guaranteed-empty
// diff. This matters because a same-tool same-args reissue (model
// retry on a flake) should read at a glance as "args unchanged, only
// the result differs."
func TestReplayDiff_IdenticalSectionsCollapse(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir())

	args := map[string]any{"cmd": "uptime", "profile": "prod"}
	a := replayEntry{TS: time.Now(), Tool: "run", Args: args,
		Result: toolResult{Content: []toolContent{{Type: "text", Text: "first"}}}}
	b := replayEntry{TS: time.Now().Add(time.Second), Tool: "run", Args: args,
		Result: toolResult{Content: []toolContent{{Type: "text", Text: "second"}}}}
	if err := appendReplay(a); err != nil {
		t.Fatalf("appendReplay a: %v", err)
	}
	if err := appendReplay(b); err != nil {
		t.Fatalf("appendReplay b: %v", err)
	}

	out := captureStdout(t, func() {
		if err := ReplayCmd([]string{"diff", "0", "1"}); err != nil {
			t.Fatalf("ReplayCmd diff: %v", err)
		}
	})
	// args block must collapse; result block must not.
	if !strings.Contains(out, "--- args ---\n(no change)") {
		t.Errorf("expected '(no change)' under --- args ---:\n%s", out)
	}
	if strings.Contains(out, "--- result ---\n(no change)") {
		t.Errorf("did not expect '(no change)' under --- result ---:\n%s", out)
	}
}

// TestReplayDiff_BadIndex makes sure index validation surfaces a
// helpful error instead of crashing on out-of-range slice access.
func TestReplayDiff_BadIndex(t *testing.T) {
	t.Setenv("SRV_HOME", t.TempDir())
	// no entries written
	err := ReplayCmd([]string{"diff", "0", "1"})
	if err == nil {
		t.Fatal("expected error on empty replay log, got nil")
	}
	if !strings.Contains(err.Error(), "out of range") {
		t.Errorf("expected 'out of range' message, got: %v", err)
	}
}

// TestReplayDiff_UsageOnMissingIndex catches the user typing
// `srv mcp replay diff` (or with only one index) -- we want a usage
// hint, not a panic.
func TestReplayDiff_UsageOnMissingIndex(t *testing.T) {
	err := ReplayCmd([]string{"diff"})
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("want usage error, got %v", err)
	}
	err = ReplayCmd([]string{"diff", "0"})
	if err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Errorf("want usage error, got %v", err)
	}
}

// captureStdout redirects os.Stdout for the duration of `fn` and
// returns whatever was written. Used by the diff tests because
// ReplayCmd writes directly to fmt.Println / Printf rather than an
// injectable Writer.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan struct{})
	var buf bytes.Buffer
	go func() {
		_, _ = io.Copy(&buf, r)
		close(done)
	}()
	fn()
	_ = w.Close()
	<-done
	os.Stdout = old
	return buf.String()
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Errorf("output missing %q\n--- got ---\n%s", needle, haystack)
	}
}
