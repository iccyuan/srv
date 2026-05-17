package daemon

import (
	"srv/internal/config"
	"testing"
	"time"
)

// These tests exercise the multi-conn pool's selection + GC logic
// without driving a real SSH dial -- we construct pooledClient values
// with nil `client` fields and pre-populate s.pool. acquireClient
// itself can't run without config/SSH, but the slice-walking and
// eviction logic is fair game.

func TestEvictFromSlotRemovesMatchingEntry(t *testing.T) {
	s := &daemonState{pool: map[string][]*pooledClient{}}
	pc1 := &pooledClient{lastUsed: time.Now()}
	pc2 := &pooledClient{lastUsed: time.Now()}
	pc3 := &pooledClient{lastUsed: time.Now()}
	s.pool["prod"] = []*pooledClient{pc1, pc2, pc3}

	s.mu.Lock()
	s.evictFromSlot("prod", pc2)
	s.mu.Unlock()

	got := s.pool["prod"]
	if len(got) != 2 {
		t.Fatalf("after eviction expected 2 entries, got %d", len(got))
	}
	for _, pc := range got {
		if pc == pc2 {
			t.Error("evicted pc2 still present")
		}
	}
}

func TestEvictFromSlotDeletesEmptyKey(t *testing.T) {
	// Last entry eviction should drop the whole map key so an
	// "is this profile pooled" check reads false (it would otherwise
	// see an empty slice and misreport).
	s := &daemonState{pool: map[string][]*pooledClient{}}
	pc := &pooledClient{lastUsed: time.Now()}
	s.pool["solo"] = []*pooledClient{pc}

	s.mu.Lock()
	s.evictFromSlot("solo", pc)
	s.mu.Unlock()

	if _, present := s.pool["solo"]; present {
		t.Error("solo key should be deleted after evicting its last entry")
	}
}

func TestGCClosesIdleSlotPreservesBusyOnes(t *testing.T) {
	// Multi-conn invariant: GC must NOT take down a whole pool just
	// because one slot is idle. The busy slot stays.
	s := &daemonState{
		pool:      map[string][]*pooledClient{},
		lsCache:   map[string]*lsCacheEntry{},
		stopCh:    make(chan struct{}),
		tunnels:   map[string]*activeTunnel{},
		sudoCache: map[string]sudoCacheEntry{},
	}
	s.lastReq = time.Now() // not idle, so daemon shutdown isn't triggered

	idle := &pooledClient{lastUsed: time.Now().Add(-2 * connIdleTTL)}
	busy := &pooledClient{lastUsed: time.Now().Add(-2 * connIdleTTL)}
	busy.inflight.Add(1) // pretend a session is in flight
	s.pool["prod"] = []*pooledClient{idle, busy}

	s.gc()

	got := s.pool["prod"]
	if len(got) != 1 {
		t.Fatalf("after gc expected 1 surviving entry, got %d", len(got))
	}
	if got[0] != busy {
		t.Error("busy slot was wrongly evicted; idle slot survived")
	}
}

func TestPoolSizeClamps(t *testing.T) {
	// Profile.GetPoolSize is the cap acquireClient consults. Invalid
	// values should clamp to a sane range instead of crashing or
	// uncapped growth.
	cases := []struct {
		raw, want int
	}{
		{0, 4},
		{-1, 4},
		{1, 1},
		{4, 4},
		{16, 16},
		{17, 16},
		{1000, 16},
	}
	for _, c := range cases {
		p := &config.Profile{PoolSize: c.raw}
		if got := p.GetPoolSize(); got != c.want {
			t.Errorf("PoolSize=%d: got %d, want %d", c.raw, got, c.want)
		}
	}
}
