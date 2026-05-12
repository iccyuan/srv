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
