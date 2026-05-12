package daemon

import (
	"srv/internal/config"
	"testing"
	"time"
)

// daemonState.gc()'s idle-shutdown decision was previously based
// solely on time.Since(lastReq). That killed any tunnels running
// inside the daemon as a side effect, since tunnel forwarders don't
// pump requests through s.lastReq. These tests pin the new
// behaviour: idle => shutdown only when no tunnels are active.

func newGCTestState() *daemonState {
	return &daemonState{
		pool:      map[string]*pooledClient{},
		lsCache:   map[string]*lsCacheEntry{},
		stopCh:    make(chan struct{}),
		tunnels:   map[string]*activeTunnel{},
		sudoCache: map[string]sudoCacheEntry{},
	}
}

func TestGC_IdleWithNoTunnelsTriggersShutdown(t *testing.T) {
	s := newGCTestState()
	s.lastReq = time.Now().Add(-2 * daemonIdleTTL)
	s.gc()
	select {
	case <-s.stopCh:
		// expected: shutdown requested
	default:
		t.Error("expected idle daemon with no tunnels to request shutdown")
	}
}

func TestGC_IdleWithActiveTunnelStaysAlive(t *testing.T) {
	s := newGCTestState()
	s.lastReq = time.Now().Add(-2 * daemonIdleTTL)
	s.tunnels["db"] = &activeTunnel{
		name:      "db",
		def:       &config.TunnelDef{Type: "local", Spec: "5432"},
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
		startedAt: time.Now(),
	}
	s.gc()
	select {
	case <-s.stopCh:
		t.Error("daemon should NOT shut down while a tunnel is active")
	default:
		// expected: stays alive
	}
}

func TestGC_BusyDoesntShutDownRegardlessOfTunnels(t *testing.T) {
	s := newGCTestState()
	s.lastReq = time.Now() // fresh activity
	s.gc()
	select {
	case <-s.stopCh:
		t.Error("active daemon should not shut down")
	default:
	}
}
