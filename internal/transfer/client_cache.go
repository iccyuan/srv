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

// cacheProbeSkipWindow: when the cached client was used within this
// window, skip the probe entirely and hand it back. A keepalive
// round-trip through a high-latency jump path costs 500-800ms;
// skipping it for back-to-back transfers (the most common pattern)
// is the dominant residual saving after caching the dial itself.
// 30s is comfortably below typical NAT idle (60-120s) so we still
// probe before a conn realistically could have gone stale.
const cacheProbeSkipWindow = 30 * time.Second

type cachedClient struct {
	client   *sshx.Client
	lastUsed time.Time
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
//
// Returns (client, note, err) where `note` is a short tag suitable
// for timing logs: "cache-hit", "cache-stale-redial", "fresh-dial",
// "lost-race". Empty when the timing knob is off (zero overhead).
func AcquireSharedClient(profile *config.Profile) (*sshx.Client, error) {
	c, _, err := acquireSharedClientWithNote(profile)
	return c, err
}

// acquireSharedClientWithNote is the internal entry that also
// returns a one-word note describing how the client was obtained,
// for the timing breakdown.
func acquireSharedClientWithNote(profile *config.Profile) (*sshx.Client, string, error) {
	if profile == nil || profile.Name == "" {
		c, err := sshx.Dial(profile)
		return c, "fresh-dial-unkeyed", err
	}

	clientCacheMu.Lock()
	entry := clientCache[profile.Name]
	clientCacheMu.Unlock()

	stale := false
	if entry != nil && entry.client != nil && entry.client.Conn != nil {
		// Skip the probe when the conn was used very recently:
		// keepalive RTT through a jump path costs ~0.5-0.8s, and
		// the conn can't realistically have NAT-timed-out in 30s.
		if time.Since(entry.lastUsed) < cacheProbeSkipWindow {
			clientCacheMu.Lock()
			if cur, ok := clientCache[profile.Name]; ok && cur == entry {
				cur.lastUsed = time.Now()
			}
			clientCacheMu.Unlock()
			return entry.client, "cache-hit-fresh", nil
		}
		if probeOK(entry.client) {
			clientCacheMu.Lock()
			if cur, ok := clientCache[profile.Name]; ok && cur == entry {
				cur.lastUsed = time.Now()
			}
			clientCacheMu.Unlock()
			return entry.client, "cache-hit", nil
		}
		stale = true
		clientCacheMu.Lock()
		if clientCache[profile.Name] == entry {
			delete(clientCache, profile.Name)
		}
		clientCacheMu.Unlock()
		_ = entry.client.Close()
	}

	c, err := sshx.Dial(profile)
	if err != nil {
		return nil, "fresh-dial", err
	}
	clientCacheMu.Lock()
	if existing, ok := clientCache[profile.Name]; ok && existing.client != nil && existing.client.Conn != nil {
		existing.lastUsed = time.Now()
		clientCacheMu.Unlock()
		_ = c.Close()
		return existing.client, "lost-race", nil
	}
	clientCache[profile.Name] = &cachedClient{client: c, lastUsed: time.Now()}
	clientCacheMu.Unlock()
	if stale {
		return c, "cache-stale-redial", nil
	}
	return c, "fresh-dial", nil
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
