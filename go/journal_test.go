package main

import (
	"strings"
	"testing"
)

func TestParseJournalArgs_AllFlags(t *testing.T) {
	jc, err := parseJournalArgs([]string{
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
	if jc.unit != "nginx.service" {
		t.Errorf("unit=%q", jc.unit)
	}
	if jc.since != "10 min ago" {
		t.Errorf("since=%q", jc.since)
	}
	if jc.priority != "err" {
		t.Errorf("priority=%q", jc.priority)
	}
	if jc.lines != 200 {
		t.Errorf("lines=%d", jc.lines)
	}
	if jc.grep != "timeout" {
		t.Errorf("grep=%q", jc.grep)
	}
	if !jc.follow {
		t.Error("follow not set")
	}
}

func TestParseJournalArgs_EqualsForm(t *testing.T) {
	jc, err := parseJournalArgs([]string{"--unit=nginx", "--since=1h"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if jc.unit != "nginx" || jc.since != "1h" {
		t.Errorf("got %+v", jc)
	}
}

func TestParseJournalArgs_Unknown(t *testing.T) {
	_, err := parseJournalArgs([]string{"--bogus"})
	if err == nil {
		t.Error("expected error on unknown flag")
	}
}

func TestParseJournalArgs_MissingValue(t *testing.T) {
	_, err := parseJournalArgs([]string{"-u"})
	if err == nil {
		t.Error("expected error on missing -u value")
	}
}

func TestParseJournalArgs_BadLines(t *testing.T) {
	_, err := parseJournalArgs([]string{"-n", "not-a-number"})
	if err == nil {
		t.Error("expected error on non-numeric -n")
	}
}

func TestJournalCmd_ToRemoteCommand_Minimal(t *testing.T) {
	jc := journalCmd{}
	got := jc.toRemoteCommand()
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
	jc := journalCmd{
		unit: "nginx.service", since: "1 hour ago", priority: "warning",
		lines: 50, grep: "ERROR", follow: true,
	}
	got := jc.toRemoteCommand()
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
