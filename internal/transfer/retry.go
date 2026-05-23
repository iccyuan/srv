package transfer

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
)

// Auto-retry layer for the shared-client cache.
//
// Without this, the 30s skip-probe window introduced for the cache
// can hand out a connection that's been killed inside that window
// (network blip, jump host restart, mid-path NAT entry forgotten
// faster than usual). The next SFTP op then fails with a generic
// "connection closed" or "i/o timeout" and the user has to retry
// by hand.
//
// The retry is bounded to ONE additional attempt and gated on the
// error looking like a connection-level failure -- business errors
// (no such file, permission denied, disk full) MUST NOT trigger a
// retry, both because the retry can't fix them AND because masking
// them as transient noise would erode users' trust in the error.

// isConnLevelError tries to distinguish "the SSH conn is dead" from
// "the SFTP server told us no" using a mix of typed checks (io.EOF,
// net.Error.Timeout) and a string substring fallback for the
// transport-layer errors that pkg/ssh wraps without a sentinel type
// we can match. False negatives (treating a conn error as business)
// just mean the user retries by hand once -- the old behaviour.
// False positives (treating a business error as conn) would cause a
// useless redial and then the same error again, also bounded to one
// extra round-trip so it's not catastrophic.
func isConnLevelError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, os.ErrClosed) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	// pkg/sftp and pkg/ssh wrap transport failures in plain errors;
	// the substring set below comes from the actual error texts they
	// produce when a session/channel/connection is torn down. We
	// match on lowercase so casing variants don't slip past.
	s := strings.ToLower(err.Error())
	for _, frag := range []string{
		"connection closed",
		"connection reset",
		"broken pipe",
		"channel closed",
		"session closed",
		"use of closed network connection",
		"unexpected packet",
		"i/o timeout",
		"eof", // catches "EOF" and "unexpected EOF" wrappings
	} {
		if strings.Contains(s, frag) {
			return true
		}
	}
	return false
}

// withRetryOnConnDeath runs `op` once. If it returns a conn-level
// error, evicts the cached client for `profileName` (so the next
// AcquireSharedClient dials fresh) and runs `op` exactly once more.
// On any non-conn error or success, returns that result verbatim.
//
// The retry is intentionally NOT recursive / NOT capped at higher
// numbers. One retry resolves the realistic case (cached conn
// silently died within the skip-probe window); a second one would
// just spin against a genuinely-down remote and burn time the user
// is already waiting on.
func withRetryOnConnDeath(profileName string, op func() error) error {
	err := op()
	if err == nil || !isConnLevelError(err) {
		return err
	}
	EvictSharedClient(profileName)
	return op()
}
