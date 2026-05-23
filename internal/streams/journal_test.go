package streams

import (
	"strings"
	"testing"
)

func TestParseJournalArgs_AllFlags(t *testing.T) {
	jc, err := ParseJournalArgs([]string{
		"-u", "nginx.service",
		"--since", "10 min ago",
		"-p", "err",
		"-n", "200",
		"-g", "timeout",
		"-f",
	})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if jc.Unit != "nginx.service" {
		t.Errorf("unit=%q", jc.Unit)
	}
	if jc.Since != "10 min ago" {
		t.Errorf("since=%q", jc.Since)
	}
	if jc.Priority != "err" {
		t.Errorf("priority=%q", jc.Priority)
	}
	if jc.Lines != 200 {
		t.Errorf("lines=%d", jc.Lines)
	}
	if jc.Grep != "timeout" {
		t.Errorf("grep=%q", jc.Grep)
	}
	if !jc.Follow {
		t.Error("follow not set")
	}
}

func TestParseJournalArgs_EqualsForm(t *testing.T) {
	jc, err := ParseJournalArgs([]string{"--unit=nginx", "--since=1h"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if jc.Unit != "nginx" || jc.Since != "1h" {
		t.Errorf("got %+v", jc)
	}
}

func TestParseJournalArgs_Unknown(t *testing.T) {
	_, err := ParseJournalArgs([]string{"--bogus"})
	if err == nil {
		t.Error("expected error on unknown flag")
	}
}

func TestParseJournalArgs_MissingValue(t *testing.T) {
	_, err := ParseJournalArgs([]string{"-u"})
	if err == nil {
		t.Error("expected error on missing -u value")
	}
}

func TestParseJournalArgs_BadLines(t *testing.T) {
	_, err := ParseJournalArgs([]string{"-n", "not-a-number"})
	if err == nil {
		t.Error("expected error on non-numeric -n")
	}
}

func TestJournalCmd_ToRemoteCommand_Minimal(t *testing.T) {
	jc := JournalCmd{}
	got := jc.ToRemoteCommand()
	want := []string{"journalctl", "--no-pager", "-o", "short-iso"}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("expected %q in %q", w, got)
		}
	}
	if strings.Contains(got, "-u ") {
		t.Errorf("unset unit should not appear: %q", got)
	}
	if strings.Contains(got, "-f") {
		t.Errorf("non-follow shouldn't have -f: %q", got)
	}
}

func TestJournalCmd_ToRemoteCommand_FullShape(t *testing.T) {
	jc := JournalCmd{
		Unit: "nginx.service", Since: "1 hour ago", Priority: "warning",
		Lines: 50, Grep: "ERROR", Follow: true,
	}
	got := jc.ToRemoteCommand()
	// srvtty.ShQuote leaves alphanumerics + . / : etc. unquoted but always
	// quotes anything with whitespace. Assert against the shape we
	// expect from that contract rather than a literal one-true-string.
	for _, frag := range []string{
		"journalctl", "--no-pager",
		"-u nginx.service",
		"--since '1 hour ago'",
		"-p warning",
		"-n 50",
		"-g ERROR",
		"-f",
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("missing %q in %q", frag, got)
		}
	}
}

// TestParseJournalArgs_PreferLog confirms the opt-in dispatch flag
// parses in both spelled-out and `=`-equals forms, and that an
// unknown --prefer value is rejected loudly (so a typo doesn't
// silently fall back to journalctl).
func TestParseJournalArgs_PreferLog(t *testing.T) {
	for _, args := range [][]string{
		{"--prefer", "log"},
		{"--prefer=log"},
	} {
		jc, err := ParseJournalArgs(args)
		if err != nil {
			t.Fatalf("parse %v: %v", args, err)
		}
		if !jc.PreferLog {
			t.Errorf("parse %v: PreferLog not set", args)
		}
	}
	if _, err := ParseJournalArgs([]string{"--prefer", "bogus"}); err == nil {
		t.Error("expected error on unknown --prefer value")
	}
}

// TestToMacOSLogCommand_OneShot locks in the show-mode translation
// for the common shape: unit -> NSPredicate covering subsystem+process,
// since -> --last, lines -> piped tail, priority "err" -> messageType
// predicate, grep -> NSPredicate MATCHES.
func TestToMacOSLogCommand_OneShot(t *testing.T) {
	jc := JournalCmd{
		Unit: "nginx", Since: "10m", Priority: "err", Lines: 100, Grep: "timeout",
		PreferLog: true,
	}
	got := jc.ToRemoteCommand()
	for _, want := range []string{
		"/usr/bin/log show",
		"--style syslog",
		"--last 10m",
		"subsystem == \"nginx\"",
		"process == \"nginx\"",
		`eventMessage MATCHES "timeout"`,
		`messageType == "error"`,
		"| /usr/bin/tail -n 100",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n  %s", want, got)
		}
	}
	if strings.Contains(got, "journalctl") {
		t.Errorf("PreferLog should not emit journalctl: %s", got)
	}
	if strings.Contains(got, "log stream") {
		t.Errorf("non-follow should use `log show`, not `log stream`: %s", got)
	}
}

// TestToMacOSLogCommand_Stream confirms follow mode picks the
// `log stream` subcommand AND drops --last (which only applies to
// `log show`) AND skips the `| tail -n N` pipeline (stream is
// unbounded; the line cap is moot in stream mode).
func TestToMacOSLogCommand_Stream(t *testing.T) {
	jc := JournalCmd{
		Unit: "myapp", Since: "10m", Lines: 100, Follow: true, PreferLog: true,
	}
	got := jc.ToRemoteCommand()
	if !strings.Contains(got, "/usr/bin/log stream") {
		t.Errorf("follow mode should use `/usr/bin/log stream`: %s", got)
	}
	if strings.Contains(got, "--last") {
		t.Errorf("--last has no meaning for `log stream`: %s", got)
	}
	if strings.Contains(got, "tail -n") {
		t.Errorf("line cap should not pipe through tail in stream mode: %s", got)
	}
	if !strings.Contains(got, `subsystem == "myapp"`) {
		t.Errorf("predicate should still apply in stream mode: %s", got)
	}
}

// TestToMacOSLogCommand_AbsolutePathRequired guards a real bug we hit
// on live macOS: zsh ships `log` as a builtin that prints login
// history and rejects subcmds with "too many arguments". The system
// log tool we want is /usr/bin/log; without the absolute path the
// builtin shadows it on the default macOS shell. Lock this in so a
// future cleanup doesn't quietly revert to bare `log` and reintroduce
// the failure.
func TestToMacOSLogCommand_AbsolutePathRequired(t *testing.T) {
	for _, follow := range []bool{false, true} {
		got := JournalCmd{PreferLog: true, Follow: follow}.ToRemoteCommand()
		if !strings.HasPrefix(got, "/usr/bin/log ") {
			t.Errorf("follow=%v: must invoke /usr/bin/log to bypass zsh builtin, got:\n  %s", follow, got)
		}
	}
}

// TestToMacOSLogCommand_PriorityMapping documents the priority->log
// translation so a future refactor doesn't quietly change it. info
// becomes a --info flag (not a predicate); debug pulls in both
// --info and --debug; an unrecognized value passes through as a
// literal predicate so `log` itself reports the parse error rather
// than silently dropping the filter.
func TestToMacOSLogCommand_PriorityMapping(t *testing.T) {
	cases := []struct {
		priority string
		want     string
	}{
		{"info", "--info"},
		{"debug", "--debug"},
		{"warning", "--info"},                           // closest match
		{"fault", `messageType == "error"`},             // grouped with err
		{"weirdcustom", `messageType == "weirdcustom"`}, // passthrough
	}
	for _, tc := range cases {
		jc := JournalCmd{PreferLog: true, Priority: tc.priority}
		got := jc.ToRemoteCommand()
		if !strings.Contains(got, tc.want) {
			t.Errorf("priority %q: want %q in %q", tc.priority, tc.want, got)
		}
	}
}

// TestMissingJournalctlHint_BashAndZsh covers both common shells'
// not-found wording. The hint must fire on a bash-style
// `journalctl: command not found` AND a zsh-style
// `command not found: journalctl` -- modern macOS defaults to zsh,
// which is the whole point of having this hint.
func TestMissingJournalctlHint_BashAndZsh(t *testing.T) {
	for _, stderr := range []string{
		"bash: journalctl: command not found\n",
		"zsh:2: command not found: journalctl\n",
		"-bash: journalctl: command not found\n",
	} {
		got := MissingJournalctlHint(127, stderr, false)
		if got == "" {
			t.Errorf("expected hint for stderr %q", stderr)
		}
		if !strings.Contains(got, "--prefer log") {
			t.Errorf("hint should mention --prefer log: %q", got)
		}
	}
}

// TestMissingJournalctlHint_SuppressedWhenPreferLog: when the user
// already opted into the macOS path, a follow-up failure has a
// different cause (e.g. `log` itself missing on a stripped-down
// container) and the journalctl hint would be misleading. Verify
// it's suppressed.
func TestMissingJournalctlHint_SuppressedWhenPreferLog(t *testing.T) {
	if got := MissingJournalctlHint(127, "zsh: command not found: journalctl", true); got != "" {
		t.Errorf("expected no hint when PreferLog=true, got %q", got)
	}
}

// TestMissingJournalctlHint_NoFireOnSuccessOrOtherErrors: the hint
// must NOT fire on (a) successful runs (exit 0) or (b) unrelated
// stderr text -- a generic "permission denied" or empty stderr
// shouldn't suggest the macOS path.
func TestMissingJournalctlHint_NoFireOnSuccessOrOtherErrors(t *testing.T) {
	if got := MissingJournalctlHint(0, "journalctl: command not found", false); got != "" {
		t.Errorf("exit 0 should suppress hint, got %q", got)
	}
	if got := MissingJournalctlHint(1, "permission denied\n", false); got != "" {
		t.Errorf("unrelated stderr should suppress hint, got %q", got)
	}
}
