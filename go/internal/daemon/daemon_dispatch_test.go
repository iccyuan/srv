package daemon

import "testing"

// TestOpRegistryHasEntryForEveryDocumentedOp pins the registry's
// coverage against the protocol comment at the top of daemon.go. If
// a new op gets documented but not registered, this fails. Tests in
// other packages that call daemon ops indirectly (via daemon.Call /
// TryRunCapture etc.) wouldn't catch a missing entry until the
// runtime dispatch returned "unknown op"; this lifts the contract
// out of code paths into explicit data.
func TestOpRegistryHasEntryForEveryDocumentedOp(t *testing.T) {
	want := []string{
		"ping",
		"status",
		"shutdown",
		"ls",
		"cd",
		"pwd",
		"run",
		"tunnel_up",
		"tunnel_down",
		"tunnel_list",
		"sudo_cache_get",
		"sudo_cache_set",
		"sudo_cache_clear",
		"disconnect",
		"disconnect_all",
	}
	for _, op := range want {
		if _, ok := opRegistry[op]; !ok {
			t.Errorf("opRegistry missing entry for documented op %q", op)
		}
	}
	// Reverse: every registry entry should match a known op so
	// stray entries (e.g. left over after a rename) get caught.
	known := map[string]bool{}
	for _, op := range want {
		known[op] = true
	}
	for op := range opRegistry {
		if !known[op] {
			t.Errorf("opRegistry has unknown op %q; if intentional, add to want list", op)
		}
	}
}

// TestDispatchUnknownOpReturnsError checks the fallback path the
// registry lookup falls through to. Caller-visible behaviour for an
// unrecognised op must be a clean "unknown op: X" response rather
// than a panic or hang.
func TestDispatchUnknownOpReturnsError(t *testing.T) {
	s := &daemonState{}
	resp := s.dispatch(Request{Op: "no_such_op"})
	if resp.OK {
		t.Error("unknown op should not return OK")
	}
	if resp.Err == "" {
		t.Error("unknown op should set an error message")
	}
}

// TestDispatchRecoversFromPanic asserts the defer in dispatch
// catches a handler panic and turns it into an Error response so
// one buggy op doesn't take the whole daemon goroutine with it.
func TestDispatchRecoversFromPanic(t *testing.T) {
	// Inject a panicking handler under a key that doesn't collide
	// with real ops. We don't restore via defer because tests
	// shouldn't mutate package state, but this entry is harmless
	// after the test (no real client ever sends this op).
	const op = "__test_panic"
	if _, exists := opRegistry[op]; exists {
		t.Fatalf("test op %q already registered; pick a different name", op)
	}
	opRegistry[op] = func(*daemonState, Request) Response {
		panic("boom")
	}
	defer delete(opRegistry, op)

	s := &daemonState{}
	resp := s.dispatch(Request{Op: op})
	if resp.OK {
		t.Error("panicking handler must not return OK")
	}
	if resp.Err == "" {
		t.Error("panic recovery should populate Err")
	}
}
