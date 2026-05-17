package runwrap

import (
	"strings"
	"testing"
	"time"
)

func TestWrapNoOptsPassthrough(t *testing.T) {
	got := Wrap("ls -la", Opts{})
	if got != "ls -la" {
		t.Errorf("expected passthrough, got %q", got)
	}
}

func TestWrapResourceLimitsBoth(t *testing.T) {
	got := Wrap("python app.py", Opts{CPULimit: "75", MemLimit: "512M"})
	if !strings.Contains(got, "CPUQuota=75%") {
		t.Errorf("CPU quota missing: %s", got)
	}
	if !strings.Contains(got, "MemoryMax=512M") {
		t.Errorf("memory max missing: %s", got)
	}
	if !strings.Contains(got, "systemd-run") {
		t.Errorf("systemd-run missing: %s", got)
	}
	// Fallback branch should also exist so the run still happens when
	// systemd-run is missing.
	if !strings.Contains(got, "without resource limits") {
		t.Errorf("missing-systemd-run fallback missing: %s", got)
	}
}

func TestWrapResourceCPUOnly(t *testing.T) {
	got := Wrap("./svc", Opts{CPULimit: "200%"})
	if !strings.Contains(got, "CPUQuota=200%") {
		t.Errorf("CPU quota missing: %s", got)
	}
	if strings.Contains(got, "MemoryMax") {
		t.Errorf("memory should not be set: %s", got)
	}
}

func TestWrapRestartUnlimited(t *testing.T) {
	got := Wrap("flaky", Opts{RestartOnFail: -1, RestartDelay: 2 * time.Second})
	if !strings.Contains(got, "__srv_max=;") {
		t.Errorf("unlimited should keep max empty: %s", got)
	}
	if !strings.Contains(got, "__srv_delay=2;") {
		t.Errorf("delay missing or wrong: %s", got)
	}
}

func TestWrapRestartBounded(t *testing.T) {
	got := Wrap("flaky", Opts{RestartOnFail: 3})
	if !strings.Contains(got, "__srv_max=3;") {
		t.Errorf("max not bounded to 3: %s", got)
	}
	if !strings.Contains(got, "__srv_delay=5;") {
		t.Errorf("default delay (5s) missing: %s", got)
	}
}

func TestWrapRestartHonorsSignal(t *testing.T) {
	got := Wrap("svc", Opts{RestartOnFail: -1})
	// Restart loop must break on SIGINT (130) / SIGTERM (143) so the
	// user can Ctrl-C out of a runaway loop.
	if !strings.Contains(got, "130|143") {
		t.Errorf("signal break missing: %s", got)
	}
}

func TestWrapRestartPlusResource(t *testing.T) {
	// Both wrappers active: restart loop must be OUTERMOST so the
	// retry covers systemd-run setup failures too.
	got := Wrap("svc", Opts{RestartOnFail: 2, CPULimit: "50%"})
	systemdIdx := strings.Index(got, "systemd-run")
	loopIdx := strings.Index(got, "__srv_max=")
	if loopIdx < 0 || systemdIdx < 0 {
		t.Fatalf("both wrappers expected: %s", got)
	}
	if loopIdx > systemdIdx {
		t.Errorf("restart loop should be outside systemd-run; got loop at %d, sd-run at %d", loopIdx, systemdIdx)
	}
}

func TestNormalizeCPU(t *testing.T) {
	if got := normalizeCPU("50%"); got != "50%" {
		t.Errorf("preserve %%: got %q", got)
	}
	if got := normalizeCPU("75"); got != "75%" {
		t.Errorf("append %%: got %q", got)
	}
	if got := normalizeCPU(" 200 "); got != "200%" {
		t.Errorf("trim+%%: got %q", got)
	}
}

func TestShQuoteEscapesSingleQuotes(t *testing.T) {
	got := shQuote(`it's a test`)
	want := `'it'\''s a test'`
	if got != want {
		t.Errorf("shQuote: got %s want %s", got, want)
	}
}
