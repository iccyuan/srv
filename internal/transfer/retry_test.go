package transfer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"testing"
	"time"
)

// fakeNetTimeoutErr satisfies net.Error with Timeout()=true so we
// can test the typed-check branch of isConnLevelError without
// needing a real network op to time out.
type fakeNetTimeoutErr struct{}

func (fakeNetTimeoutErr) Error() string   { return "fake i/o timeout" }
func (fakeNetTimeoutErr) Timeout() bool   { return true }
func (fakeNetTimeoutErr) Temporary() bool { return true }

// TestIsConnLevelError pins the classifier so the retry layer
// neither misses real conn deaths NOR swallows business failures
// as "transient." Both directions matter equally -- a false
// negative reverts to the user-retries-by-hand status quo, but a
// false positive masks a permanent failure as a slow retry that
// also fails, eroding signal in the error.
func TestIsConnLevelError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		// Conn-level (must retry):
		{"nil is not an error", nil, false},
		{"io.EOF", io.EOF, true},
		{"io.ErrUnexpectedEOF", io.ErrUnexpectedEOF, true},
		{"os.ErrClosed", os.ErrClosed, true},
		{"wrapped io.EOF", fmt.Errorf("sftp: %w", io.EOF), true},
		{"net timeout err type", fakeNetTimeoutErr{}, true},
		{"connection closed text", errors.New("ssh: connection closed"), true},
		{"connection reset text", errors.New("read tcp: connection reset by peer"), true},
		{"broken pipe text", errors.New("write tcp: broken pipe"), true},
		{"channel closed text", errors.New("ssh: channel closed"), true},
		{"session closed text", errors.New("session closed"), true},
		{"use of closed network connection", errors.New("use of closed network connection"), true},
		{"i/o timeout text (the original symptom)", errors.New("jump \"x\": ssh: handshake failed: read tcp: i/o timeout"), true},
		{"case-insensitive EOF substring", errors.New("got unexpected EOF mid-frame"), true},

		// Business / file-level (must NOT retry):
		{"no such file", errors.New("file does not exist"), false},
		{"permission denied", errors.New("sftp: \"permission denied\" (SSH_FX_PERMISSION_DENIED)"), false},
		{"sftp generic failure", errors.New("sftp: \"failure\" (SSH_FX_FAILURE)"), false},
		{"local stat error", errors.New("stat /tmp/foo: no such file or directory"), false},
		{"disk full", errors.New("write quota exceeded"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isConnLevelError(tc.err); got != tc.want {
				t.Errorf("isConnLevelError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestWithRetryOnConnDeath_NoErrorRunsOnce: success path stays
// single-attempt; no eviction, no second call. Guards against a
// regression that retries on every call.
func TestWithRetryOnConnDeath_NoErrorRunsOnce(t *testing.T) {
	calls := 0
	err := withRetryOnConnDeath("p", func() error {
		calls++
		return nil
	})
	if err != nil {
		t.Fatalf("err=%v, want nil", err)
	}
	if calls != 1 {
		t.Errorf("ran %d times, want 1", calls)
	}
}

// TestWithRetryOnConnDeath_BusinessErrorNoRetry: a SFTP "no such
// file" must NOT trigger a retry; the second call would just
// produce the same error AND waste a dial. Surface immediately.
func TestWithRetryOnConnDeath_BusinessErrorNoRetry(t *testing.T) {
	calls := 0
	want := errors.New("sftp: \"no such file\"")
	got := withRetryOnConnDeath("p", func() error {
		calls++
		return want
	})
	if got != want {
		t.Errorf("err = %v, want %v", got, want)
	}
	if calls != 1 {
		t.Errorf("business error ran %d times, want 1 (no retry)", calls)
	}
}

// TestWithRetryOnConnDeath_ConnErrorRetriesOnceThenSucceeds: this
// is the value-add scenario -- first attempt hits a dead cached
// conn, second attempt (post-eviction) succeeds. Verifies the
// op is called exactly twice and the final nil error is returned.
func TestWithRetryOnConnDeath_ConnErrorRetriesOnceThenSucceeds(t *testing.T) {
	resetClientCache(t)
	calls := 0
	err := withRetryOnConnDeath("p", func() error {
		calls++
		if calls == 1 {
			return io.EOF // classic dead-conn signature
		}
		return nil
	})
	if err != nil {
		t.Errorf("expected nil after retry success, got %v", err)
	}
	if calls != 2 {
		t.Errorf("ran %d times, want exactly 2 (try + retry)", calls)
	}
}

// TestWithRetryOnConnDeath_ConnErrorRetriesOnceThenPersists:
// when the second attempt also fails, surface that error and stop
// (no third try). Bounded retry is what keeps a genuinely-down
// remote from spinning forever.
func TestWithRetryOnConnDeath_ConnErrorRetriesOnceThenPersists(t *testing.T) {
	resetClientCache(t)
	calls := 0
	persistent := errors.New("ssh: connection closed")
	got := withRetryOnConnDeath("p", func() error {
		calls++
		return persistent
	})
	if got != persistent {
		t.Errorf("err = %v, want %v", got, persistent)
	}
	if calls != 2 {
		t.Errorf("ran %d times, want exactly 2 (bounded retry)", calls)
	}
}

// TestWithRetryOnConnDeath_EvictsBetweenAttempts: between try 1
// and try 2, EvictSharedClient must run so the second attempt's
// AcquireSharedClient hits an empty cache and dials fresh. The
// test seeds a cache entry, fires a conn error from try 1, and
// asserts the cache is empty when try 2 starts.
func TestWithRetryOnConnDeath_EvictsBetweenAttempts(t *testing.T) {
	resetClientCache(t)
	// Seed a fake cache entry. The retry layer should evict it on
	// the first conn error; the second attempt's op observes the
	// cache state.
	clientCacheMu.Lock()
	clientCache["p"] = &cachedClient{lastUsed: time.Now()} // nil client; only the entry's presence matters here
	clientCacheMu.Unlock()

	calls := 0
	cacheHadEntryOnSecondTry := true
	_ = withRetryOnConnDeath("p", func() error {
		calls++
		if calls == 1 {
			return io.EOF
		}
		// Check cache state at start of attempt #2
		clientCacheMu.Lock()
		_, present := clientCache["p"]
		clientCacheMu.Unlock()
		cacheHadEntryOnSecondTry = present
		return nil
	})
	if cacheHadEntryOnSecondTry {
		t.Error("cache entry NOT evicted between attempts -- retry would reuse the dead client")
	}
	if calls != 2 {
		t.Errorf("ran %d times, want 2", calls)
	}
}
