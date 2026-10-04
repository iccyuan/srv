package syncx

import (
	"srv/internal/config"
	"srv/internal/sshx"
	"srv/internal/transfer"
)

// acquire returns the process-shared SSH client for `profile`.
//
// Every remote step of a sync used to sshx.Dial on its own: collect
// remote stats, upload the tar, delete, pull-side git/glob listing --
// a `sync --delete --diff` paid three or four full handshakes (plus
// the jump chain each time) and `sync --watch` re-dialled on every
// debounced save. transfer already keeps one healthy *sshx.Client per
// profile for the life of the process; syncx now borrows it. Callers
// MUST NOT Close the returned client -- the cache owns it.
func acquire(profile *config.Profile) (*sshx.Client, error) {
	return transfer.AcquireSharedClient(profile)
}

// withConn runs `op` with a shared client and retries exactly once
// on a connection-level failure (the cached conn died silently inside
// the probe-skip window). Every syncx remote operation is idempotent --
// tar extraction, rm -f, read-only listings -- so a blind second
// attempt is safe. Non-connection errors return verbatim.
func withConn(profile *config.Profile, op func(c *sshx.Client) error) error {
	name := ""
	if profile != nil {
		name = profile.Name
	}
	return transfer.RetryOnConnDeath(name, func() error {
		c, err := acquire(profile)
		if err != nil {
			return err
		}
		return op(c)
	})
}
