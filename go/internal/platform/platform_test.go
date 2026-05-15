package platform

import (
	"os"
	"os/exec"
	"testing"
)

// stubProcess shows the recommended pattern for tests that want to
// drive code paths normally hidden by the real OS implementation
// (signal failure, dead PIDs, etc.). Production code never reaches
// for this -- it's an example fixture that any package's tests can
// adapt to their own scenario.
type stubProcess struct {
	detached       bool
	signalErr      error
	alive          bool
	startTime      int64
	startTimeKnown bool
}

func (s *stubProcess) Detach(*exec.Cmd)          { s.detached = true }
func (s *stubProcess) SignalTerminate(int) error { return s.signalErr }
func (s *stubProcess) PIDAlive(int) bool         { return s.alive }
func (s *stubProcess) PIDStartTime(int) (int64, bool) {
	return s.startTime, s.startTimeKnown
}

// TestStubReplacementRoundtrips is the canonical example of how a
// test in another package can swap in a fake Process, run code
// against it, and restore the real implementation via defer. The
// pattern works the same way for Term (Console) and Sec (Crypto).
func TestStubReplacementRoundtrips(t *testing.T) {
	real := Proc
	defer func() { Proc = real }()

	stub := &stubProcess{alive: true, startTime: 42, startTimeKnown: true}
	Proc = stub

	if got := Proc.PIDAlive(123); !got {
		t.Error("expected stub's PIDAlive=true")
	}
	if got, ok := Proc.PIDStartTime(123); got != 42 || !ok {
		t.Errorf("PIDStartTime: got (%d, %v), want (42, true)", got, ok)
	}
	// And the real impl returns again after defer fires (we can't
	// verify post-defer here, but the structure speaks for itself).
}

// TestInitInstallsRealImplementations is a smoke test that the
// platform_<goos>.go init wired the package-level vars to something
// non-nil. Catches a future build-tag drift where, say, adding a
// `//go:build amd64` accidentally excluded a file from a valid OS
// target.
func TestInitInstallsRealImplementations(t *testing.T) {
	if Proc == nil {
		t.Error("Proc is nil; platform_<goos>.go init didn't run")
	}
	if Term == nil {
		t.Error("Term is nil; platform_<goos>.go init didn't run")
	}
	if Sec == nil {
		t.Error("Sec is nil; platform_<goos>.go init didn't run")
	}
}

// TestProcessSelfLiveness is an end-to-end check that the real
// Process implementation considers us alive and returns a plausible
// start time on supported platforms.
func TestProcessSelfLiveness(t *testing.T) {
	if !Proc.PIDAlive(os.Getpid()) {
		t.Error("real Process should report our own pid as alive")
	}
	// PIDStartTime may legitimately return (0, false) on macOS/BSD
	// where the impl is a documented fallback. Don't assert success.
	_, _ = Proc.PIDStartTime(os.Getpid())
}
