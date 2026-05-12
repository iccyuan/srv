package main

import (
	"strings"
	"testing"
)

func TestIsMeaningfulFilter(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		// Empty / whitespace -- no filter.
		{"", false},
		{"   ", false},
		{"\t\n", false},
		// "Matches everything" patterns -- bypass attempts.
		{".*", false},
		{".+", false},
		{".", false},
		{"[\\s\\S]*", false},
		// Real filters -- pass.
		{"ERROR", true},
		{"foo.bar", true},
		{"^WARN", true},
		{"a", true}, // single-char literal is still a filter
		// Pattern with leading/trailing whitespace + content -- pass.
		{"  ERROR  ", true},
		// A regex that does filter, even if it has dots: ".*ERROR.*" has
		// surrounding content so doesn't match the bypass regex.
		{".*ERROR.*", true},
	}
	for _, c := range cases {
		if got := isMeaningfulFilter(c.in); got != c.want {
			t.Errorf("isMeaningfulFilter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRequireStreamFilter_NoFollow(t *testing.T) {
	// follow=0 means one-shot, gate doesn't apply.
	r := requireStreamFilter("tail", 0, []string{""}, "(example)")
	if r != nil {
		t.Errorf("one-shot call shouldn't gate; got rejection")
	}
}

func TestRequireStreamFilter_ShortFollowAllowed(t *testing.T) {
	// follow <= streamShortFollowSec is OK without filter.
	for _, follow := range []int{1, 3, streamShortFollowSec} {
		r := requireStreamFilter("tail", follow, []string{""}, "(example)")
		if r != nil {
			t.Errorf("follow=%d should pass (under threshold), got reject", follow)
		}
	}
}

func TestRequireStreamFilter_LongFollowNoFilterRejected(t *testing.T) {
	r := requireStreamFilter("tail", 30, []string{""}, `{ path: "x", grep: "ERROR" }`)
	if r == nil {
		t.Fatal("expected rejection for long follow without filter")
	}
	if !r.IsError {
		t.Error("rejection should be IsError=true")
	}
	if len(r.Content) == 0 ||
		!strings.Contains(r.Content[0].Text, "requires at least one output filter") {
		t.Errorf("missing standard rejection message: %+v", r.Content)
	}
	if !strings.Contains(r.Content[0].Text, `grep: "ERROR"`) {
		t.Error("rejection should include the caller-supplied hint example")
	}
	// Structured content should carry diagnostic fields the model
	// can branch on.
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent not a map: %T", r.StructuredContent)
	}
	if sc["rejected_reason"] != "unbounded_streaming" {
		t.Errorf("rejected_reason=%v", sc["rejected_reason"])
	}
}

func TestRequireStreamFilter_LongFollowWithFilterAllowed(t *testing.T) {
	// A meaningful grep -- pass.
	r := requireStreamFilter("tail", 30, []string{"ERROR"}, "(example)")
	if r != nil {
		t.Errorf("real filter should pass; got rejection: %+v", r)
	}
}

func TestRequireStreamFilter_BypassPatternRejected(t *testing.T) {
	// grep=".*" should be treated as "no filter" even though non-empty.
	r := requireStreamFilter("tail", 30, []string{".*"}, "(example)")
	if r == nil {
		t.Error("bypass pattern '.*' should still trigger rejection")
	}
}

func TestRequireStreamFilter_AnyOneFilterPasses(t *testing.T) {
	// journal-style: multiple slots, any one meaningful is enough.
	r := requireStreamFilter("journal", 30,
		[]string{"", "10 min ago", "", ""}, "(example)")
	if r != nil {
		t.Errorf("any non-empty filter should pass; got reject")
	}
}

func TestRequireStreamFilter_AllEmptyRejected(t *testing.T) {
	r := requireStreamFilter("journal", 30,
		[]string{"", "", "", ""}, "(example)")
	if r == nil {
		t.Error("all-empty filters with long follow should reject")
	}
}

func TestClampLines(t *testing.T) {
	cases := []struct {
		asked, max, want int
		clamped          bool
	}{
		{0, 1000, 0, false},
		{50, 1000, 50, false},
		{1000, 1000, 1000, false},
		{1001, 1000, 1000, true},
		{1_000_000, 2000, 2000, true},
	}
	for _, c := range cases {
		got, cl := clampLines(c.asked, c.max)
		if got != c.want || cl != c.clamped {
			t.Errorf("clampLines(%d, %d) = (%d, %v), want (%d, %v)",
				c.asked, c.max, got, cl, c.want, c.clamped)
		}
	}
}
