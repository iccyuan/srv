package srvtty

import (
	"sync/atomic"
	"testing"
	"time"
)

// TestWatchWindowResizeStopIsIdempotent makes sure the returned
// stop function tolerates being called twice (once via defer, again
// via explicit cleanup) without panicking on a double-close. We
// implemented this by guarding the close in a select-default; this
// test pins the contract so a future "cleaner" rewrite that drops
// the guard fails CI immediately.
func TestWatchWindowResizeStopIsIdempotent(t *testing.T) {
	stop := WatchWindowResize(func(int, int) {})
	stop()
	stop() // must not panic
	// Give the goroutine a brief chance to exit cleanly. We can't
	// observe its exit directly but a leaked goroutine would only
	// show as a slow-down in subsequent tests, not a failure here.
	time.Sleep(10 * time.Millisecond)
}

// TestWatchWindowResizeNoCallbackOnSameSize is best-effort: when the
// terminal hasn't changed dimensions, the callback must not fire.
// On the Unix path SIGWINCH never fires either, so this is more
// meaningful on Windows where we explicitly poll-and-compare. The
// test runs everywhere because the contract should hold uniformly.
func TestWatchWindowResizeNoCallbackOnSameSize(t *testing.T) {
	var fires atomic.Int32
	stop := WatchWindowResize(func(int, int) { fires.Add(1) })
	defer stop()
	// 300ms covers one poll cycle on Windows (250ms) plus slack.
	time.Sleep(300 * time.Millisecond)
	if n := fires.Load(); n != 0 {
		t.Errorf("expected no callback when size unchanged, got %d", n)
	}
}

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		"":             "''",
		"foo":          "foo",
		"hello world":  "'hello world'",
		"a'b":          `'a'\''b'`,
		"$HOME":        "'$HOME'",
		"already-safe": "already-safe",
	}
	for in, want := range cases {
		if got := ShQuote(in); got != want {
			t.Errorf("ShQuote(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestShQuotePath(t *testing.T) {
	// ShQuotePath must preserve a leading ~ or ~/ so the remote shell
	// expands it. This is the bug we hit in v2.0.1: cwd="~" got quoted
	// as 'srv', breaking every subsequent run.
	cases := map[string]string{
		"~":            "~",
		"~/foo":        "~/foo",
		"~/some path":  "~/'some path'",
		"/opt/app":     "/opt/app",
		"/opt/with sp": "'/opt/with sp'",
		"plain":        "plain",
	}
	for in, want := range cases {
		if got := ShQuotePath(in); got != want {
			t.Errorf("ShQuotePath(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestBase64Encode(t *testing.T) {
	if got := Base64Encode("hi"); got != "aGk=" {
		t.Errorf("Base64Encode(hi) = %q; want aGk=", got)
	}
}
