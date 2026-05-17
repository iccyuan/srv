package hints

import (
	"testing"
)

// Tests assume Suggest sees the same canonical candidate set the
// dispatcher loads via SetCandidates in main's init(). Mirror that
// here so the tests run hermetic.
func init() {
	SetCandidates([]string{
		"help", "version", "config", "use", "cd", "pwd", "status",
		"check", "doctor", "shell", "env", "push", "pull", "sync",
		"edit", "open", "code", "diff", "tunnel", "jobs", "logs",
		"kill", "sessions", "mcp", "guard", "color", "daemon",
		"project", "group", "sudo", "ui", "tail", "watch", "journal",
		"top", "run", "exec",
	})
}

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

func TestSuggest(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// Typical typos that should match.
		{"staus", "status"},
		{"chec", "check"},
		{"sttus", "status"},
		{"helo", "help"},
		{"sesions", "sessions"},
		// Short tokens with distance 2 -- threshold tightens to 1, no match.
		{"chk", ""},
		{"snyc", ""},
		// Exact match: don't suggest.
		{"status", ""},
		{"check", ""},
		// Way off: no suggestion.
		{"xyzzy", ""},
		{"", ""},
		// First-letter mismatch: no suggestion.
		{"ttatus", ""},
		// Long distance from any candidate.
		{"completelydifferent", ""},
	}
	for _, c := range cases {
		got := Suggest(c.in)
		if got != c.want {
			t.Errorf("Suggest(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSuggestShortTokenStrict(t *testing.T) {
	// 4-char tokens get threshold 1 (so "chk"→"check" with dist 2 is NOT a hit).
	if got := Suggest("chk"); got != "" {
		t.Errorf("Suggest(%q) = %q, want \"\" (distance 2 over short threshold)", "chk", got)
	}
	// 5+ chars get threshold 2 (so "stausx"→"status" works).
	if got := Suggest("stausx"); got != "status" {
		t.Errorf("Suggest(%q) = %q, want %q", "stausx", got, "status")
	}
}
