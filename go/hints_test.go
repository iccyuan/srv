package main

import "testing"

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "abc", 0},
		{"", "abc", 3},
		{"abc", "", 3},
		{"staus", "status", 1},
		{"pwd", "pwd2", 1},
		{"chk", "check", 2},
		{"completely", "different", 8},
		{"a", "b", 1},
	}
	for _, c := range cases {
		got := levenshtein(c.a, c.b)
		if got != c.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSuggestSubcommand(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Typical typos that should match
		{"staus", "status"},     // distance 1, len >= 5
		{"chec", "check"},       // distance 1, len < 5 OK
		{"sttus", "status"},     // distance 1, len >= 5
		{"helo", "help"},        // distance 1, len < 5 OK
		{"sesions", "sessions"}, // distance 1
		// Short tokens with distance 2 -- threshold tightens to 1, no match
		{"chk", ""},  // dist 2 from "check"; len < 5
		{"snyc", ""}, // dist 2 from "sync"; len < 5
		// Exact match: don't suggest (caller would've dispatched directly)
		{"status", ""},
		{"check", ""},
		// Way off: no suggestion
		{"xyzzy", ""},
		{"", ""},
		// First-letter mismatch: no suggestion
		{"ttatus", ""}, // doesn't start with 's'
		// Long distance from any candidate
		{"completelydifferent", ""},
	}
	for _, c := range cases {
		got := suggestSubcommand(c.in)
		if got != c.want {
			t.Errorf("suggestSubcommand(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSuggestSubcommandShortTokenStrict(t *testing.T) {
	// 4-char tokens get threshold 1 (so "chk"->"check" with dist 2 is NOT a hit).
	// This guards against noisy 3-letter false positives.
	if got := suggestSubcommand("chk"); got != "" {
		t.Errorf("suggestSubcommand(\"chk\") = %q, want \"\" (distance 2 over short threshold)", got)
	}
	// 5+ chars get threshold 2 (so "stausx"->"status" works).
	if got := suggestSubcommand("stausx"); got != "status" {
		t.Errorf("suggestSubcommand(\"stausx\") = %q, want \"status\"", got)
	}
}
