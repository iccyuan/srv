package main

import (
	"testing"
	"time"
)

func TestFmtDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"},
		{8 * time.Second, "8s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{15 * time.Minute, "15m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := fmtDuration(c.d); got != c.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestParseISOLike(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"not a time", false},
		// nowISO() output -- no timezone.
		{"2026-05-10T23:25:16", true},
		// RFC3339 with Z.
		{"2026-05-10T23:25:16Z", true},
		// RFC3339 with explicit offset.
		{"2026-05-10T23:25:16+08:00", true},
	}
	for _, c := range cases {
		_, ok := parseISOLike(c.in)
		if ok != c.want {
			t.Errorf("parseISOLike(%q) ok=%v, want %v", c.in, ok, c.want)
		}
	}
	// Sanity: parsing nowISO() and computing time.Since should give a
	// near-zero duration, not "57 years ago" (would happen if we
	// parsed local time as UTC).
	if t1, ok := parseISOLike("2026-05-11T00:00:00"); ok {
		if t1.Year() != 2026 {
			t.Errorf("parsed year=%d, want 2026", t1.Year())
		}
	}
}

func TestTruncID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"short", "short"},
		{"12345678", "12345678"},
		{"123456789", "12345678"},
		{"abcdef-fedcba-aaaa", "abcdef-f"},
	}
	for _, c := range cases {
		if got := truncID(c.in); got != c.want {
			t.Errorf("truncID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
