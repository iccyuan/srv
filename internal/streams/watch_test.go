package streams

import (
	"errors"
	"regexp"
	"srv/internal/srvutil"
	"srv/internal/sshx"
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
	res := &sshx.RunCaptureResult{Stdout: "hello\n", ExitCode: 0}
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
	res := &sshx.RunCaptureResult{
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
	if strings.Contains(out, srvutil.Reverse) {
		t.Errorf("identical inputs should not highlight: %q", out)
	}
}

func TestHighlightDiffLines_ChangedLineHighlighted(t *testing.T) {
	out := highlightDiffLines("a\nB\nc", "a\nb\nc")
	if !strings.Contains(out, srvutil.Reverse+"B"+srvutil.Reset) {
		t.Errorf("changed line not wrapped: %q", out)
	}
	// Unchanged lines stay bare.
	if strings.Contains(out, srvutil.Reverse+"a") {
		t.Errorf("unchanged 'a' was highlighted: %q", out)
	}
}

func TestHighlightDiffLines_NewTailLines(t *testing.T) {
	// Extra lines past prev's length should all be highlighted (no
	// previous baseline to compare against).
	out := highlightDiffLines("a\nb\nc\nd", "a\nb")
	if !strings.Contains(out, srvutil.Reverse+"c"+srvutil.Reset) {
		t.Errorf("new line 'c' not highlighted: %q", out)
	}
	if !strings.Contains(out, srvutil.Reverse+"d"+srvutil.Reset) {
		t.Errorf("new line 'd' not highlighted: %q", out)
	}
}

// untilCond covers the --until / --until-exit decision logic. These
// tests target the pure helper; the full Watch loop is exercised by
// the SSH-touching code path that we deliberately don't unit-test.

func TestUntilCond_MatchesStdoutRegex(t *testing.T) {
	re := mustRe(t, `Listening on :\d+`)
	u := untilCond{re: re}
	res := &sshx.RunCaptureResult{Stdout: "starting...\nListening on :8080\n", ExitCode: 0}
	if !u.matched(res, nil) {
		t.Errorf("expected match on 'Listening on :8080'")
	}
}

func TestUntilCond_MatchesStderrRegex(t *testing.T) {
	// stderr is concatenated with stdout (joined by \n) so a regex
	// against an error message in stderr still fires.
	re := mustRe(t, `Permission denied`)
	u := untilCond{re: re}
	res := &sshx.RunCaptureResult{Stdout: "", Stderr: "ssh: Permission denied"}
	if !u.matched(res, nil) {
		t.Errorf("expected match on stderr")
	}
}

func TestUntilCond_NoMatchYet(t *testing.T) {
	re := mustRe(t, `READY`)
	u := untilCond{re: re}
	res := &sshx.RunCaptureResult{Stdout: "starting...\n"}
	if u.matched(res, nil) {
		t.Errorf("did not expect match while output lacks 'READY'")
	}
}

func TestUntilCond_ExitCodeMatch(t *testing.T) {
	u := untilCond{exit: 0, hasExit: true}
	if !u.matched(&sshx.RunCaptureResult{ExitCode: 0}, nil) {
		t.Errorf("expected match on exit 0")
	}
	if u.matched(&sshx.RunCaptureResult{ExitCode: 1}, nil) {
		t.Errorf("did not expect match on exit 1")
	}
}

func TestUntilCond_BothConditionsOR(t *testing.T) {
	// Either condition matching exits -- not both required. Common
	// case: --until 'ready' --until-exit 0 means "leave when EITHER
	// the output says 'ready' OR the command returns success."
	u := untilCond{re: mustRe(t, `ready`), exit: 0, hasExit: true}
	if !u.matched(&sshx.RunCaptureResult{Stdout: "ready"}, nil) {
		t.Errorf("regex side should match")
	}
	if !u.matched(&sshx.RunCaptureResult{Stdout: "not yet", ExitCode: 0}, nil) {
		t.Errorf("exit side should match")
	}
	if u.matched(&sshx.RunCaptureResult{Stdout: "not yet", ExitCode: 1}, nil) {
		t.Errorf("neither side matches -- expected false")
	}
}

func TestUntilCond_CaptureErrorNeverMatches(t *testing.T) {
	// A transient SSH failure (runErr != nil) keeps the loop running.
	// Even with --until-exit 0 we must NOT confuse a missing result
	// for a successful exit.
	u := untilCond{exit: 0, hasExit: true}
	if u.matched(nil, errors.New("dial: timeout")) {
		t.Errorf("capture error must not match --until-exit 0")
	}
}

func TestUntilCond_ZeroValueNeverMatches(t *testing.T) {
	// Without --until or --until-exit, watch keeps polling forever
	// (Ctrl-C remains the only exit) -- matched() must return false
	// for any input under the zero value.
	var u untilCond
	if u.matched(&sshx.RunCaptureResult{Stdout: "anything", ExitCode: 0}, nil) {
		t.Errorf("zero-value untilCond must never match")
	}
}

func mustRe(t *testing.T, pat string) *regexp.Regexp {
	t.Helper()
	re, err := regexp.Compile(pat)
	if err != nil {
		t.Fatalf("regexp.Compile(%q): %v", pat, err)
	}
	return re
}
