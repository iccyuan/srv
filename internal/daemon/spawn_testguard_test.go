package daemon

import "testing"

// TestUnderGoTestIsTrueHere is the regression guard for the daemon
// auto-spawn fork bomb (see underGoTest's doc comment). This code runs
// inside a `go test` binary, so underGoTest MUST report true -- if it
// ever returns false here, Ensure() would exec the test binary with a
// `daemon` arg and recursively re-run the suite, freezing the machine.
// Bounded, no SSH, no spawn: safe to run in any loop / -count=N.
func TestUnderGoTestIsTrueHere(t *testing.T) {
	if !underGoTest() {
		t.Fatal("underGoTest() == false inside a go test binary: " +
			"daemon auto-spawn is no longer gated -- RunCapture from a " +
			"test would fork-bomb the machine. Do not relax this guard.")
	}
}
