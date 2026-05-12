package main

import (
	"errors"
	"srv/internal/ansi"
	"strings"
	"testing"
	"time"
)

func TestFmtSecs(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{500 * time.Millisecond, "0.5s"},
		{time.Second, "1s"},
		{2 * time.Second, "2s"},
		{2500 * time.Millisecond, "2.5s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{2 * time.Minute, "2m"},
	}
	for _, c := range cases {
		if got := fmtSecs(c.d); got != c.want {
			t.Errorf("fmtSecs(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestBuildWatchFrame_HappyPath(t *testing.T) {
	res := &RunCaptureResult{Stdout: "hello\n", ExitCode: 0}
	frame := buildWatchFrame("uptime", "prod", 2*time.Second, 120*time.Millisecond, res, nil, "", false)
	if !strings.Contains(frame, "Every 2s on prod") {
		t.Errorf("header missing 'Every 2s on prod': %q", frame)
	}
	if !strings.Contains(frame, "$ uptime") {
		t.Errorf("command echo missing: %q", frame)
	}
	if !strings.Contains(frame, "hello") {
		t.Errorf("body missing: %q", frame)
	}
	if !strings.Contains(frame, "[exit 0  capture 0.12s]") {
		t.Errorf("footer missing or wrong: %q", frame)
	}
}

func TestBuildWatchFrame_CaptureError(t *testing.T) {
	frame := buildWatchFrame("uptime", "prod", time.Second, 0, nil, errors.New("dial: timeout"), "", false)
	if !strings.Contains(frame, "capture failed: dial: timeout") {
		t.Errorf("error not surfaced: %q", frame)
	}
}

func TestBuildWatchFrame_ExitCodeAndStderr(t *testing.T) {
	res := &RunCaptureResult{
		Stdout:   "ok\n",
		Stderr:   "warn: foo\n",
		ExitCode: 2,
	}
	frame := buildWatchFrame("foo", "p", time.Second, time.Second, res, nil, "", false)
	if !strings.Contains(frame, "--- stderr ---") {
		t.Errorf("stderr fence missing: %q", frame)
	}
	if !strings.Contains(frame, "warn: foo") {
		t.Errorf("stderr content missing: %q", frame)
	}
	if !strings.Contains(frame, "[exit 2") {
		t.Errorf("exit code missing: %q", frame)
	}
}

func TestHighlightDiffLines_SameLineNoHighlight(t *testing.T) {
	out := highlightDiffLines("a\nb\nc", "a\nb\nc")
	if strings.Contains(out, ansi.Reverse) {
		t.Errorf("identical inputs should not highlight: %q", out)
	}
}

func TestHighlightDiffLines_ChangedLineHighlighted(t *testing.T) {
	out := highlightDiffLines("a\nB\nc", "a\nb\nc")
	if !strings.Contains(out, ansi.Reverse+"B"+ansi.Reset) {
		t.Errorf("changed line not wrapped: %q", out)
	}
	// Unchanged lines stay bare.
	if strings.Contains(out, ansi.Reverse+"a") {
		t.Errorf("unchanged 'a' was highlighted: %q", out)
	}
}

func TestHighlightDiffLines_NewTailLines(t *testing.T) {
	// Extra lines past prev's length should all be highlighted (no
	// previous baseline to compare against).
	out := highlightDiffLines("a\nb\nc\nd", "a\nb")
	if !strings.Contains(out, ansi.Reverse+"c"+ansi.Reset) {
		t.Errorf("new line 'c' not highlighted: %q", out)
	}
	if !strings.Contains(out, ansi.Reverse+"d"+ansi.Reset) {
		t.Errorf("new line 'd' not highlighted: %q", out)
	}
}
