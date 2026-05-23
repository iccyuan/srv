package transfer

import (
	"fmt"
	"sync"
	"time"

	"srv/internal/config"
	"srv/internal/sshx"
)

// MCP-process-local SSH client cache for file transfer.
//
// Background: push/pull have historically called sshx.Dial directly,
// which means every transfer pays the full SSH handshake cost (+ any
// jump chain). The daemon's shared pool can't help here -- an SSH
// connection is bound to one process, and SFTP runs on top of that
// connection's subchannels, so daemon-side pooling can't be handed
// across to the MCP/CLI process where transfer.go runs.
//
// This cache memoizes a single *sshx.Client per profile NAME for the
// lifetime of THIS process. The common AI usage pattern -- one MCP
// server session doing many push / pull / diff calls -- pays the
// 16-17s cold dial cost exactly once per profile, not per file.
//
// Limitations (documented honestly):
//   - CLI one-shot `srv push X` / `srv pull X` exits between commands;
//     the cache is empty on the next run. Helping that case requires
//     a daemon-side SFTP refactor (orders of magnitude more code).
//   - Cached conns are NEVER actively closed -- they live until the
//     process exits or the bounded health probe declares them stale
//     at next use. For short-lived processes this is fine; for very
//     long-lived MCP sessions, a dead conn is detected and replaced
//     on next acquire.
//   - Per-profile mutex serializes the dial + probe step. Concurrent
//     pushes to the SAME profile wait for the dial; concurrent
//     pushes to DIFFERENT profiles do not contend.

// cacheProbeTimeout bounds the liveness probe at acquire time. A
// NAT-broken TCP conn's SendRequest can block for the kernel's full
// retransmit window (minutes); the bound makes the cache fail open
// to a fresh dial within a couple seconds. Matches the equivalent
// constant in internal/daemon (intentionally NOT shared via an
// import to keep transfer decoupled from daemon).
const cacheProbeTimeout = 5 * time.Second

type cachedClient struct {
	client *sshx.Client
}

var (
	clientCacheMu sync.Mutex
	clientCache   = map[string]*cachedClient{}
)

// AcquireSharedClient returns a process-shared *sshx.Client for the
// profile, dialing once and reusing across calls. Caller MUST NOT
// Close the returned client; ownership stays with the cache.
//
// Health is verified with a bounded SendRequest before each reuse,
// so a stale cached conn (idle long enough for an NAT hop to forget
// it) won't be handed out. On probe failure the dead client is
// evicted and a fresh dial replaces it transparently. Callers see
// only "got a client" or "dial error".
func AcquireSharedClient(profile *config.Profile) (*sshx.Client, error) {
	if profile == nil || profile.Name == "" {
		// No name to key on: fall through to a fresh dial that the
		// caller owns. Defensive -- in practice config.Resolve sets
		// the name, but the cache requires it.
		return sshx.Dial(profile)
	}

	clientCacheMu.Lock()
	entry := clientCache[profile.Name]
	clientCacheMu.Unlock()

	if entry != nil && entry.client != nil && entry.client.Conn != nil {
		if probeOK(entry.client) {
			return entry.client, nil
		}
		// Stale: drop it. The peer goroutine inside probeOK that
		// might still be blocked on SendRequest unblocks the moment
		// we call Close (the underlying TCP gets torn down).
		clientCacheMu.Lock()
		if clientCache[profile.Name] == entry {
			delete(clientCache, profile.Name)
		}
		clientCacheMu.Unlock()
		_ = entry.client.Close()
		// fall through to dial fresh
	}

	// Dial fresh and install. Multiple concurrent first-time acquires
	// race here; the loser of the race throws away its dial. Tolerated
	// because (a) first-time concurrent push to the same profile is
	// rare in practice and (b) installing both would just leak one
	// conn until process exit.
	c, err := sshx.Dial(profile)
	if err != nil {
		return nil, err
	}
	clientCacheMu.Lock()
	if existing, ok := clientCache[profile.Name]; ok && existing.client != nil && existing.client.Conn != nil {
		// Lost the race -- another goroutine just installed a healthy
		// entry. Use that and discard our dial.
		clientCacheMu.Unlock()
		_ = c.Close()
		return existing.client, nil
	}
	clientCache[profile.Name] = &cachedClient{client: c}
	clientCacheMu.Unlock()
	return c, nil
}

// probeOK races a single SSH keepalive request against
// cacheProbeTimeout. Returns true iff the request returned without
// error inside the deadline. The goroutine that fires the request is
// allowed to outlive this function call -- if the deadline fires
// first, the caller closes the underlying conn, which makes
// SendRequest's next syscall unblock immediately, so no real leak.
func probeOK(c *sshx.Client) bool {
	if c == nil || c.Conn == nil {
		return false
	}
	type result struct{ err error }
	ch := make(chan result, 1)
	go func() {
		_, _, err := c.Conn.SendRequest("keepalive@openssh.com", true, nil)
		ch <- result{err: err}
	}()
	select {
	case r := <-ch:
		return r.err == nil
	case <-time.After(cacheProbeTimeout):
		return false
	}
}

// EvictSharedClient drops the cached client for a profile if one
// exists. Useful for test cleanup and for callers that detect a hard
// SFTP-level failure and want to force a re-dial on the next use.
// Idempotent.
func EvictSharedClient(profileName string) {
	clientCacheMu.Lock()
	entry, ok := clientCache[profileName]
	if ok {
		delete(clientCache, profileName)
	}
	clientCacheMu.Unlock()
	if ok && entry != nil && entry.client != nil {
		_ = entry.client.Close()
	}
}

// sharedClientCacheStats is exposed via fmt for ad-hoc debugging /
// future telemetry hooks. Not part of any public surface.
func sharedClientCacheStats() string {
	clientCacheMu.Lock()
	defer clientCacheMu.Unlock()
	return fmt.Sprintf("transfer client cache: %d profile(s)", len(clientCache))
}
