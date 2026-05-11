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
