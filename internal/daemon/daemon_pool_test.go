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

func TestDialIdentityTracksWhereAndWhoOnly(t *testing.T) {
	base := config.Profile{Host: "h", Port: 22, User: "root"}
	id := dialIdentity(&base)

	// Editing any of these re-targets the connection -> new identity.
	for name, mut := range map[string]func(p *config.Profile){
		"user":     func(p *config.Profile) { p.User = "yuan" },
		"host":     func(p *config.Profile) { p.Host = "other" },
		"port":     func(p *config.Profile) { p.Port = 3389 },
		"key":      func(p *config.Profile) { p.IdentityFile = "~/.ssh/k" },
		"jump":     func(p *config.Profile) { p.Jump = []config.JumpHop{{Spec: "bastion"}} },
		"ssh_opts": func(p *config.Profile) { p.SshOptions = []string{"User=x"} },
	} {
		p := base
		mut(&p)
		if dialIdentity(&p) == id {
			t.Errorf("changing %s must change the dial identity", name)
		}
	}

	// Tuning fields must NOT churn the pool.
	p := base
	p.KeepaliveInterval = 5
	p.DefaultCwd = "/srv"
	p.Env = map[string]string{"A": "1"}
	if dialIdentity(&p) != id {
		t.Error("keepalive/cwd/env edits should keep the same dial identity")
	}
}

func TestRetireStaleLockedDropsMismatchedConns(t *testing.T) {
	// Regression: profile user was edited root->yuan->root while the
	// daemon still pooled conns from each version; acquireClient kept
	// handing out the yuan conn, so wait_job's `kill -0 <root pid>`
	// hit EPERM and reported a live job as killed.
	s := &daemonState{
		pool:    map[string][]*pooledClient{},
		lsCache: map[string]*lsCacheEntry{},
	}
	cur := dialIdentity(&config.Profile{Host: "h", User: "root"})
	old := dialIdentity(&config.Profile{Host: "h", User: "yuan"})

	good := &pooledClient{ident: cur}
	staleIdle := &pooledClient{ident: old}
	staleBusy := &pooledClient{ident: old}
	staleBusy.inflight.Add(1)
	s.pool["hk"] = []*pooledClient{staleIdle, good, staleBusy}
	s.lsCache["hk\x00/root"] = &lsCacheEntry{}
	s.lsCache["other\x00/root"] = &lsCacheEntry{}

	s.mu.Lock()
	toClose := s.retireStaleLocked("hk", cur)
	s.mu.Unlock()

	if got := s.pool["hk"]; len(got) != 1 || got[0] != good {
		t.Fatalf("pool should keep only the matching conn, got %d entries", len(got))
	}
	if len(toClose) != 1 {
		t.Errorf("only the idle stale conn should be closed now, got %d", len(toClose))
	}
	if !staleBusy.retired.Load() {
		t.Error("busy stale conn must be flagged retired so its last release closes it")
	}
	if staleIdle.retired.Load() {
		t.Error("idle stale conn is closed directly, not retired")
	}
	if _, ok := s.lsCache["hk\x00/root"]; ok {
		t.Error("ls cache for the re-targeted profile should be dropped")
	}
	if _, ok := s.lsCache["other\x00/root"]; !ok {
		t.Error("other profiles' ls cache must be untouched")
	}

	// Nothing stale left: second pass is a no-op.
	s.mu.Lock()
	if again := s.retireStaleLocked("hk", cur); again != nil {
		t.Errorf("second retire pass should be a no-op, got %d", len(again))
	}
	s.mu.Unlock()
}

func TestRetireStaleLockedDeletesEmptyKey(t *testing.T) {
	s := &daemonState{pool: map[string][]*pooledClient{}, lsCache: map[string]*lsCacheEntry{}}
	s.pool["hk"] = []*pooledClient{{ident: "old"}}
	s.mu.Lock()
	s.retireStaleLocked("hk", "new")
	s.mu.Unlock()
	if _, present := s.pool["hk"]; present {
		t.Error("key should be deleted once every conn was retired")
	}
}

func TestReleaseClearsRetiredFlagOnLastSession(t *testing.T) {
	// The last release of a retired conn owns the Close (nil client
	// here, so we observe it via the CAS clearing the flag).
	s := &daemonState{pool: map[string][]*pooledClient{}}
	pc := &pooledClient{ident: "old"}
	pc.inflight.Add(2)
	pc.retired.Store(true)
	_, _, release1, _ := s.leaseRelease(pc, &config.Profile{})
	_, _, release2, _ := s.leaseRelease(pc, &config.Profile{})

	release1()
	if !pc.retired.Load() {
		t.Fatal("conn still has a session; must stay retired (not closed yet)")
	}
	release2()
	if pc.retired.Load() {
		t.Error("last release should have claimed the close")
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
