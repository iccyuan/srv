package transfer

import (
	"sync"
	"testing"
	"time"
)

// resetClientCache wipes the package-global cache so tests don't
// leak state into each other. Caller takes the mu directly because
// EvictSharedClient needs a profile name and we want a wholesale
// reset between tests.
func resetClientCache(t *testing.T) {
	t.Helper()
	clientCacheMu.Lock()
	for name := range clientCache {
		delete(clientCache, name)
	}
	clientCacheMu.Unlock()
}

// TestProbeOK_NilClientFalse: an entry whose underlying client is
// nil (defensive case -- should never happen in practice but the
// caller might call probeOK on a half-constructed value) must
// return false rather than crashing on the nil deref.
func TestProbeOK_NilClientFalse(t *testing.T) {
	if probeOK(nil) {
		t.Error("nil client should probe false")
	}
}

// TestEvictSharedClient_Idempotent: evicting a profile that isn't
// in the cache is a no-op (no error, no panic). The cache is shared
// across the process and EvictSharedClient may be called from
// recovery paths that don't know whether a dial happened, so this
// has to be safe to call blindly.
func TestEvictSharedClient_Idempotent(t *testing.T) {
	resetClientCache(t)
	// Must not panic; nothing observable to assert beyond "no crash".
	EvictSharedClient("does-not-exist")
	EvictSharedClient("")
}

// TestSharedClientCacheStats_ReportsCount confirms the cache counter
// reflects what we put in. Not part of the public API but used as a
// proxy assertion for "the cache map is actually being touched"
// across the other tests.
func TestSharedClientCacheStats_ReportsCount(t *testing.T) {
	resetClientCache(t)
	if got := sharedClientCacheStats(); got != "transfer client cache: 0 profile(s)" {
		t.Errorf("empty cache stats = %q", got)
	}
	clientCacheMu.Lock()
	clientCache["p1"] = &cachedClient{}
	clientCache["p2"] = &cachedClient{}
	clientCacheMu.Unlock()
	if got := sharedClientCacheStats(); got != "transfer client cache: 2 profile(s)" {
		t.Errorf("2-entry cache stats = %q", got)
	}
	resetClientCache(t)
}

// TestClientCache_ConcurrentSafeReads exercises the mutex under
// concurrent reads of the same key. The dial path itself can't be
// exercised without a live SSH endpoint, but the cache lookup +
// stats are heavily hit by parallel goroutines here -- a missing
// lock would surface as a race when run with `-race`.
func TestClientCache_ConcurrentSafeReads(t *testing.T) {
	resetClientCache(t)
	clientCacheMu.Lock()
	clientCache["prof"] = &cachedClient{}
	clientCacheMu.Unlock()

	const goroutines = 32
	const ops = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < ops; j++ {
				clientCacheMu.Lock()
				_ = clientCache["prof"]
				clientCacheMu.Unlock()
				_ = sharedClientCacheStats()
			}
		}()
	}
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
		// fine
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent reads deadlocked or starved")
	}
	resetClientCache(t)
}
