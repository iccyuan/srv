package main

import (
	"strings"
	"testing"
)

func TestRunRejectUnfiltered_RejectsBareCat(t *testing.T) {
	label, msg := runRejectUnfiltered("cat /var/log/syslog")
	if label != "cat" {
		t.Errorf("label=%q, want cat", label)
	}
	if !strings.Contains(msg, "head -n") {
		t.Errorf("message should suggest head -n: %q", msg)
	}
}

func TestRunRejectUnfiltered_CatWithPipeAllowed(t *testing.T) {
	cases := []string{
		"cat /var/log/syslog | head -n 100",
		"cat foo | grep ERROR",
		"cat foo | wc -l",
		"cat foo | tail",
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label != "" {
			t.Errorf("expected pass: %q got reject %q", c, label)
		}
	}
}

func TestRunRejectUnfiltered_HeadAlwaysOK(t *testing.T) {
	// head defaults to 10 lines; we trust that the model picked
	// head over cat deliberately.
	cases := []string{
		"head /var/log/syslog",
		"head -n 50 file",
		"head -c 1024 file",
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label != "" {
			t.Errorf("head should pass: %q got reject %q", c, label)
		}
	}
}

func TestRunRejectUnfiltered_RejectsDmesg(t *testing.T) {
	label, _ := runRejectUnfiltered("dmesg")
	if label != "dmesg" {
		t.Errorf("dmesg should reject, got label=%q", label)
	}
}

func TestRunRejectUnfiltered_DmesgWithFilterOK(t *testing.T) {
	cases := []string{
		"dmesg | tail -n 100",
		"dmesg | grep error",
		"dmesg | wc -l",
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label != "" {
			t.Errorf("piped dmesg should pass: %q got %q", c, label)
		}
	}
}

func TestRunRejectUnfiltered_BareJournalctlRejected(t *testing.T) {
	label, msg := runRejectUnfiltered("journalctl")
	if label != "journalctl" {
		t.Errorf("bare journalctl should reject, got %q", label)
	}
	if !strings.Contains(msg, "journal { unit:") {
		t.Errorf("should suggest the MCP tool: %q", msg)
	}
}

func TestRunRejectUnfiltered_JournalctlWithFilterOK(t *testing.T) {
	cases := []string{
		"journalctl -u nginx",
		"journalctl --since '10 min ago'",
		"journalctl -p err",
		"journalctl -n 100",
		"journalctl -g ERROR",
		"journalctl -k",
		"journalctl -f -u nginx",
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label != "" {
			t.Errorf("filtered journalctl should pass: %q got %q", c, label)
		}
	}
}

func TestRunRejectUnfiltered_BareFindRejected(t *testing.T) {
	label, _ := runRejectUnfiltered("find /")
	if label != "find" {
		t.Errorf("bare find / should reject, got %q", label)
	}
}

func TestRunRejectUnfiltered_FindWithFilterOK(t *testing.T) {
	cases := []string{
		"find /var/log -name '*.log'",
		"find / -maxdepth 2",
		"find /etc -type f",
		"find /home -newer /tmp/marker",
		"find / -mtime -1",
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label != "" {
			t.Errorf("filtered find should pass: %q got %q", c, label)
		}
	}
}

func TestRunRejectUnfiltered_FindRelativePathNotChecked(t *testing.T) {
	// `find` with no leading slash (e.g. `find .` or `find foo`) is
	// scoped by user intent to "right here". Don't fire on those.
	if label, _ := runRejectUnfiltered("find ."); label != "" {
		t.Errorf("find . should pass, got reject %q", label)
	}
}

func TestRunRejectUnfiltered_QuotedContentDoesntFalsePositive(t *testing.T) {
	cases := []string{
		`echo "cat /etc/passwd"`,
		`echo 'dmesg'`,
		`grep "journalctl" /tmp/foo`,
		`logger "cat is in this quoted string"`,
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label != "" {
			t.Errorf("quoted content should not reject: %q got %q", c, label)
		}
	}
}

func TestRunRejectUnfiltered_PipeChainsCount(t *testing.T) {
	// Even after a pipe, a downstream limiter satisfies the gate.
	cases := []string{
		"cat foo | grep -i error | tail -n 50",
		"dmesg | grep -i error | head -n 20",
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label != "" {
			t.Errorf("pipe chain with limiter should pass: %q got %q", c, label)
		}
	}
}

func TestRunRejectUnfiltered_AfterSemicolonStillCaught(t *testing.T) {
	// `cd /tmp && cat foo` -- the cat is at command position after &&.
	cases := []string{
		"cd /tmp && cat foo",
		"true; cat /etc/hosts",
		"echo hi || dmesg",
	}
	for _, c := range cases {
		if label, _ := runRejectUnfiltered(c); label == "" {
			t.Errorf("post-separator unbounded cmd should reject: %q", c)
		}
	}
}

func TestStripShellQuotedContent(t *testing.T) {
	// Inside quotes every byte becomes a space placeholder, except an
	// escaped pair (`\<x>` inside `"..."`) where both bytes are
	// dropped entirely (matching shell semantics: the backslash isn't
	// a literal). Whitespace count therefore matches input length
	// minus 2 per escape pair.
	cases := []struct {
		in, want string
	}{
		{`cat /etc/passwd`, `cat /etc/passwd`},
		// "cat /etc/passwd" is 15 chars inside the quotes -> 15 spaces.
		{`echo "cat /etc/passwd"`, `echo "               "`},
		// "cat foo" is 7 chars -> 7 spaces.
		{`echo 'cat foo'`, `echo '       '`},
		{`grep "x" file`, `grep " " file`},
		// `\"` is an escaped pair -- both bytes dropped, so `a\"b`
		// inside double quotes becomes 2 spaces (a and b only).
		{`echo "a\"b"`, `echo "  "`},
	}
	for _, c := range cases {
		got := stripShellQuotedContent(c.in)
		if got != c.want {
			t.Errorf("strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRunRejectUnfilteredMessage_StructuredShape(t *testing.T) {
	r := runRejectUnfilteredMessage("cat", "use head -n N instead")
	if !r.IsError {
		t.Error("rejection should be IsError=true")
	}
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent not a map: %T", r.StructuredContent)
	}
	if sc["rejected_reason"] != "unbounded_output" {
		t.Errorf("rejected_reason=%v", sc["rejected_reason"])
	}
	if sc["pattern"] != "cat" {
		t.Errorf("pattern=%v", sc["pattern"])
	}
}
