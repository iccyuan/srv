package daemon

import (
	"sync"
	"sync/atomic"
	"testing"
)

// TestRunInflightSingleFlight proves the key tuple uniquely identifies
// a single-flight slot. We don't drive a real handleRun (that would
// require an SSH stub); we just exercise the inflight map directly
// the way handleRun does -- but with a hold-the-line barrier inside
// the "owner" path so peers actually arrive while the entry is still
// in the map. Without the barrier the owner's work was so fast that
// every goroutine missed the cache and became its own owner, which
// trivially proves nothing about single-flight behaviour.
//
// The assertion is "N concurrent waiters for the same key see exactly
// one inflight entry pass through".
func TestRunInflightSingleFlight(t *testing.T) {
	s := &daemonState{runInflight: map[string]*runInflightEntry{}}
	const workers = 16
	key := s.runInflightKey("prof", "/tmp", "ls")

	var ownerCount int32
	var peersJoined int32
	// release unblocks the owner once all peers have entered (or
	// reached `wg.Done` via the join path). Sized > workers so even
	// if peers send into it without anyone reading, no deadlock.
	peerArrived := make(chan struct{}, workers)
	var wg sync.WaitGroup
	gate := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-gate
			s.runMu.Lock()
			if existing, ok := s.runInflight[key]; ok {
				s.runMu.Unlock()
				atomic.AddInt32(&peersJoined, 1)
				peerArrived <- struct{}{}
				<-existing.done
				return
			}
			entry := &runInflightEntry{done: make(chan struct{})}
			s.runInflight[key] = entry
			s.runMu.Unlock()
			atomic.AddInt32(&ownerCount, 1)
			// Wait for every peer to either join or fail to start
			// (whichever comes first) before completing. We expect
			// workers-1 peers; if some are still scheduling, we'd
			// time out and the assertion that follows would catch
			// the under-count.
			for waited := 0; waited < workers-1; waited++ {
				<-peerArrived
			}
			entry.resp = Response{OK: true, Stdout: "hello\n"}
			s.runMu.Lock()
			delete(s.runInflight, key)
			s.runMu.Unlock()
			close(entry.done)
		}()
	}
	close(gate)
	wg.Wait()

	if got := atomic.LoadInt32(&ownerCount); got != 1 {
		t.Fatalf("expected exactly 1 inflight owner across %d racing goroutines, got %d (peers joined: %d)",
			workers, got, atomic.LoadInt32(&peersJoined))
	}
	if got := atomic.LoadInt32(&peersJoined); got != workers-1 {
		t.Fatalf("expected %d peers to join the inflight entry, got %d", workers-1, got)
	}
	if n := len(s.runInflight); n != 0 {
		t.Fatalf("inflight map should be empty after wait, has %d entries", n)
	}
}

// TestRunInflightDistinctKeysDoNotCollide ensures the NUL-separator
// key construction doesn't accidentally collide two distinct triples
// just because the concatenations happen to align. "a" + "b" + "cd"
// vs "ab" + "" + "cd" are different requests; they must land in
// distinct slots.
func TestRunInflightDistinctKeysDoNotCollide(t *testing.T) {
	s := &daemonState{runInflight: map[string]*runInflightEntry{}}
	k1 := s.runInflightKey("a", "b", "cd")
	k2 := s.runInflightKey("ab", "", "cd")
	if k1 == k2 {
		t.Fatalf("expected distinct keys, both became %q", k1)
	}
}
