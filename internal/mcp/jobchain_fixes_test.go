package mcp

import (
	"strings"
	"testing"
)

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

// TestRejectSync_NohupCommandPositionOnly verifies that the nohup
// gate fires only when `nohup` sits at a command position. The
// keyword shows up in legitimate read-only inspection (`cat
// /tmp/nohup.out`, `grep nohup` in logs) which must not be rejected.
func TestRejectSync_NohupCommandPositionOnly(t *testing.T) {
	blocking := []string{
		"nohup ./hub -config hub.json &",
		"  nohup ./bin/start &",
		"cd /opt/svc && nohup ./hub &",
		"echo starting; nohup ./hub -p 8080 &",
		"(nohup ./hub &)",
		"nohup\t./hub", // tab counts as \s
	}
	for _, c := range blocking {
		if rejectSync(c) == "" {
			t.Errorf("rejectSync(%q) = \"\"; want it rejected (real nohup)", c)
		}
	}
	notBlocking := []string{
		"cat /tmp/nohup.out",
		"head -n 50 /var/log/nohup.out",
		"grep nohup /var/log/syslog",
		`grep "nohup ./hub" /var/log/audit`,
		"echo nohup",
		"ls -la nohup.out",
		"pkill -f nohup",
		"ls -la", // baseline
	}
	for _, c := range notBlocking {
		if got := rejectSync(c); got != "" {
			t.Errorf("rejectSync(%q) = %q; want \"\" (must not false-positive)", c, got)
		}
	}
}

// TestRejectMessage_NohupGuidesToBackground locks in the message
// shape: the tailored nohup error must (a) point at background: true
// and (b) strip the `nohup` prefix + trailing `&` from the suggested
// command so the model copies a clean background invocation rather
// than mixing both detach mechanisms.
func TestRejectMessage_NohupGuidesToBackground(t *testing.T) {
	cmd := "cd /opt/svc && nohup ./hub -config hub.json &"
	why := rejectSync(cmd)
	if why == "" {
		t.Fatalf("rejectSync(%q) = \"\"; expected rejection", cmd)
	}
	msg := rejectMessage(cmd, why)
	if !strings.Contains(msg, "background: true") {
		t.Errorf("nohup rejection message must mention background: true, got %q", msg)
	}
	// The suggested command in the message should not still contain
	// `nohup` (we tell the model to drop it).
	if strings.Contains(msg, "nohup ./hub") {
		t.Errorf("suggested command should have nohup stripped, got %q", msg)
	}
	// And the trailing `&` should be gone too.
	if strings.Contains(msg, "hub.json &") {
		t.Errorf("suggested command should have trailing & stripped, got %q", msg)
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
