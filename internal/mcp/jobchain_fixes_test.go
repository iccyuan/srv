package mcp

import "testing"

// P-CHAIN-5: the sync-blocking sleep gate must fire only when `sleep`
// is at a command position, not when the text merely appears inside
// an argument of a fast command.
func TestRejectSync_SleepCommandPositionOnly(t *testing.T) {
	blocking := []string{
		"sleep 30",
		"  sleep 8",
		"echo hi; sleep 9",
		"a=1 && sleep 10",
		"(sleep 7)",
		"for i in 1 2 3; do sleep 8; done",
	}
	for _, c := range blocking {
		if rejectSync(c) == "" {
			t.Errorf("rejectSync(%q) = \"\"; want it rejected (real blocking sleep)", c)
		}
	}

	notBlocking := []string{
		"pgrep -af 'sleep 30'",
		`grep "sleep 60" /var/log/x`,
		"echo sleep 90",
		"pkill -f sleep",
		"sleep 3", // matched but N<=5: caller's >5 check keeps it allowed
		"ls -la /tmp",
	}
	for _, c := range notBlocking {
		if got := rejectSync(c); got != "" {
			t.Errorf("rejectSync(%q) = %q; want \"\" (must not false-positive)", c, got)
		}
	}
}

// P-CHAIN-6 hardening: only a bare signal name/number reaches the
// remote `kill -%s`; anything that could inject shell is rejected.
func TestIsSafeSignal(t *testing.T) {
	for _, ok := range []string{"TERM", "KILL", "SIGUSR1", "9", "15", "usr1"} {
		if !isSafeSignal(ok) {
			t.Errorf("isSafeSignal(%q) = false; want true", ok)
		}
	}
	for _, bad := range []string{"", "TERM; rm -rf /", "9 || x", "$(id)", "-9 -1", "a b", "TOOLONGSIGNALNAME"} {
		if isSafeSignal(bad) {
			t.Errorf("isSafeSignal(%q) = true; want false", bad)
		}
	}
}
