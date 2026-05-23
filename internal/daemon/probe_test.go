package daemon

import (
	"errors"
	"testing"
	"time"
)

// fakeConn satisfies the narrow interface probePoolConn accepts. The
// SendRequest behavior is controllable per-test so we can exercise
// the three terminal states: fast success, fast failure, slow / never
// returns (used to verify the deadline fires).
type fakeConn struct {
	delay time.Duration
	err   error
}

func (f fakeConn) SendRequest(string, bool, []byte) (bool, []byte, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	return true, nil, f.err
}

// TestProbePoolConn_FastSuccess: a healthy conn returns nil within
// the deadline, the helper passes that through unchanged.
func TestProbePoolConn_FastSuccess(t *testing.T) {
	got := probePoolConn(fakeConn{}, 100*time.Millisecond)
	if got != nil {
		t.Errorf("healthy conn should probe nil, got %v", got)
	}
}

// TestProbePoolConn_FastFailure: a conn that returns an error
// promptly surfaces that error verbatim -- the caller treats both
// "request errored" and "request timed out" as stale, but the error
// text matters for daemon logs / debugging.
func TestProbePoolConn_FastFailure(t *testing.T) {
	want := errors.New("ssh: subsystem refused")
	got := probePoolConn(fakeConn{err: want}, 100*time.Millisecond)
	if got == nil {
		t.Fatal("expected probe error to be surfaced")
	}
	if !errors.Is(got, want) && got.Error() != want.Error() {
		t.Errorf("probe error = %q, want %q", got, want)
	}
}

// TestProbePoolConn_TimesOutOnHungConn is the core fix: a conn whose
// SendRequest never returns (NAT-broken TCP layer swallowing the
// probe) must NOT freeze the caller. The helper has to return a
// timeout error within ~the deadline, regardless of whether the
// background goroutine ever resolves. Without this bound, one stale
// conn would hang every subsequent acquireClient for minutes.
func TestProbePoolConn_TimesOutOnHungConn(t *testing.T) {
	start := time.Now()
	// Delay much longer than the probe deadline; the goroutine will
	// still be asleep when the deadline fires.
	got := probePoolConn(fakeConn{delay: 2 * time.Second}, 50*time.Millisecond)
	elapsed := time.Since(start)

	if got == nil {
		t.Fatal("hung conn should return a timeout error, got nil")
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("probe blocked %v but deadline was 50ms (deadline didn't fire)", elapsed)
	}
}
