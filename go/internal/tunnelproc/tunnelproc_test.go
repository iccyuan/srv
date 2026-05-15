package tunnelproc

import (
	"os"
	"path/filepath"
	"srv/internal/platform"
	"strings"
	"testing"
	"time"
)

// withSrvHome rewires SRV_HOME so each test gets its own tunnels/
// directory. Without the override, tests would race against the
// real ~/.srv/tunnels of whoever's running them.
func withSrvHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SRV_HOME", dir)
	return dir
}

func TestWriteAndReadStatusRoundtrip(t *testing.T) {
	dir := withSrvHome(t)
	st := Status{
		Name:    "db",
		Type:    "local",
		Spec:    "5432:db:5432",
		Profile: "prod",
		Listen:  "127.0.0.1:5432",
		Started: time.Now().Unix(),
		PID:     os.Getpid(),
	}
	if err := WriteStatus(st); err != nil {
		t.Fatalf("write: %v", err)
	}
	// File should land at <SRV_HOME>/tunnels/db.json.
	if _, err := os.Stat(filepath.Join(dir, "tunnels", "db.json")); err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	got, err := ReadStatus("db")
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got == nil {
		t.Fatalf("read returned nil for present status")
	}
	if got.Listen != st.Listen || got.PID != st.PID {
		t.Errorf("roundtrip mismatch: got %+v want %+v", got, st)
	}
}

func TestReadStatusMissingReturnsNil(t *testing.T) {
	withSrvHome(t)
	// Don't write -- ReadStatus must return (nil, nil) for not-found
	// so callers can treat absence as "tunnel not independent" without
	// having to special-case the error.
	got, err := ReadStatus("ghost")
	if err != nil {
		t.Errorf("expected no error for missing status, got %v", err)
	}
	if got != nil {
		t.Errorf("expected nil for missing status, got %+v", got)
	}
}

func TestListStatusesSkipsDeadPIDs(t *testing.T) {
	withSrvHome(t)
	// Live PID: our own.
	live := Status{Name: "live", Spec: "9000", PID: os.Getpid(), Started: time.Now().Unix()}
	if err := WriteStatus(live); err != nil {
		t.Fatal(err)
	}
	// Dead PID: a value extremely unlikely to be in use right now.
	// Stay under 2^16 so it's at least plausible on every platform,
	// but high enough that any reasonable initial PID is past it.
	dead := Status{Name: "dead", Spec: "9001", PID: 1, Started: time.Now().Unix()}
	if dead.PID == os.Getpid() {
		// Extremely unlikely (we'd have to be PID 1 ourselves), but
		// guard so the test isn't flaky in container init.
		t.Skip("test pid happens to be 1; can't construct a dead-pid case")
	}
	// Force-write the dead-pid status without ListStatuses cleaning
	// it up first.
	if err := WriteStatus(dead); err != nil {
		t.Fatal(err)
	}

	got, err := ListStatuses()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, ok := got["live"]; !ok {
		t.Error("live entry missing from ListStatuses output")
	}
	if _, ok := got["dead"]; ok {
		t.Error("dead-pid entry should have been filtered out")
	}
	// The dead status file should have been auto-removed as a side
	// effect, so a future call doesn't have to re-walk it.
	if _, err := os.Stat(StatusPath("dead")); !os.IsNotExist(err) {
		t.Errorf("dead status file should be auto-removed; got err=%v", err)
	}
}

func TestRemoveStatusIsIdempotent(t *testing.T) {
	withSrvHome(t)
	RemoveStatus("never-existed") // must not panic / error
}

func TestStatusFilePathHonoursSrvHome(t *testing.T) {
	dir := withSrvHome(t)
	got := StatusPath("xx")
	want := filepath.Join(dir, "tunnels", "xx.json")
	if got != want {
		t.Errorf("StatusPath = %q, want %q", got, want)
	}
	// Sanity: filenames stay under tunnelsDir() (no path traversal
	// via funny names).
	if !strings.HasPrefix(got, filepath.Join(dir, "tunnels")) {
		t.Errorf("path %q escapes tunnels dir", got)
	}
}

func TestPIDStartRoundtripForOurOwnPID(t *testing.T) {
	// Querying our own PID must succeed on Linux + Windows (the
	// platforms we implemented). macOS/BSD return (0, false) which
	// is also a valid answer -- the test just asserts whatever we
	// got is internally consistent.
	got, ok := platform.Proc.PIDStartTime(os.Getpid())
	if !ok {
		t.Skip("platform doesn't expose process start time (fallback path)")
	}
	// A live process's start time must be in the past (nanos < now).
	if got >= time.Now().UnixNano() {
		t.Errorf("pid_start %d is in the future vs now %d", got, time.Now().UnixNano())
	}
	// And not absurdly far in the past (an hour is generous).
	if time.Now().UnixNano()-got > int64(time.Hour) {
		t.Errorf("pid_start %d is more than an hour ago; we just started", got)
	}
}

func TestPIDAliveMatchAcceptsCorrectStart(t *testing.T) {
	start, ok := platform.Proc.PIDStartTime(os.Getpid())
	if !ok {
		t.Skip("no pidStartTime support")
	}
	if !pidAliveMatch(os.Getpid(), start) {
		t.Error("our own pid + start time should match")
	}
}

func TestPIDAliveMatchRejectsWrongStart(t *testing.T) {
	start, ok := platform.Proc.PIDStartTime(os.Getpid())
	if !ok {
		t.Skip("no pidStartTime support")
	}
	// Pretend the recorded start was way in the past -- PID reuse
	// case. Liveness must report dead because the PID survived but
	// the recorded process didn't.
	off := start - int64(time.Hour)
	if pidAliveMatch(os.Getpid(), off) {
		t.Error("mismatched start time should report dead")
	}
}

func TestPIDAliveMatchFallsBackOnZeroStart(t *testing.T) {
	// Status files written before this feature have PIDStart=0;
	// pidAliveMatch must degrade to PID-only liveness instead of
	// rejecting them outright.
	if !pidAliveMatch(os.Getpid(), 0) {
		t.Error("legacy status with PIDStart=0 should keep matching by PID alone")
	}
}

func TestFormatStartedAgo(t *testing.T) {
	now := time.Now().Unix()
	cases := []struct {
		dur  time.Duration
		want string
	}{
		{5 * time.Second, "5s"},
		{4 * time.Minute, "4m"},
		{1 * time.Hour, "1h"},
		{1*time.Hour + 6*time.Minute, "1h6m"},
		{26 * time.Hour, "1d2h"},
		{72 * time.Hour, "3d"},
	}
	for _, c := range cases {
		got := formatStartedAgo(now - int64(c.dur/time.Second))
		if got != c.want {
			t.Errorf("dur=%v: got %q want %q", c.dur, got, c.want)
		}
	}
}
