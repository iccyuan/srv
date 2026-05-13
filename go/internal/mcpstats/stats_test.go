package mcpstats

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// withTempPath redirects pathFn to a path under t.TempDir for the
// duration of the test. Restores the original on cleanup so other
// tests aren't affected.
func withTempPath(t *testing.T) (statsPath string) {
	t.Helper()
	dir := t.TempDir()
	statsPath = filepath.Join(dir, "stats.jsonl")
	prev := pathFn
	pathFn = func() string { return statsPath }
	t.Cleanup(func() { pathFn = prev })
	return statsPath
}

func TestAppendCallLoadCallsRoundTrip(t *testing.T) {
	withTempPath(t)
	t0 := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	want := []Call{
		{TS: t0, Tool: "run", DurMs: 100, InBytes: 30, OutBytes: 500, OK: true},
		{TS: t0.Add(time.Minute), Tool: "tail", DurMs: 5000, InBytes: 40, OutBytes: 2000, ProgressBytes: 10000, OK: true},
		{TS: t0.Add(2 * time.Minute), Tool: "run", DurMs: 200, InBytes: 35, OutBytes: 800, OK: false},
	}
	for _, c := range want {
		if err := AppendCall(c); err != nil {
			t.Fatalf("AppendCall: %v", err)
		}
	}
	got, err := LoadCalls(time.Time{})
	if err != nil {
		t.Fatalf("LoadCalls: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Tool != want[i].Tool || got[i].OutBytes != want[i].OutBytes || got[i].OK != want[i].OK {
			t.Errorf("record %d mismatch: got %+v, want %+v", i, got[i], want[i])
		}
		if !got[i].TS.Equal(want[i].TS) {
			t.Errorf("record %d TS mismatch: got %v, want %v", i, got[i].TS, want[i].TS)
		}
	}
}

func TestLoadCallsFiltersBySince(t *testing.T) {
	withTempPath(t)
	now := time.Now()
	old := Call{TS: now.Add(-2 * time.Hour), Tool: "x", OutBytes: 1, OK: true}
	new := Call{TS: now.Add(-10 * time.Minute), Tool: "x", OutBytes: 2, OK: true}
	if err := AppendCall(old); err != nil {
		t.Fatal(err)
	}
	if err := AppendCall(new); err != nil {
		t.Fatal(err)
	}
	got, err := LoadCalls(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].OutBytes != 2 {
		t.Errorf("since-filter wrong: got %+v", got)
	}
}

func TestClearWipesAllFiles(t *testing.T) {
	statsPath := withTempPath(t)
	if err := AppendCall(Call{Tool: "x", OK: true}); err != nil {
		t.Fatal(err)
	}
	if err := SaveCheckpoint(time.Now()); err != nil {
		t.Fatal(err)
	}
	// Manually create a rotation sibling so Clear has all three to chew on.
	if err := os.WriteFile(statsPath+".1", []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Clear(); err != nil {
		t.Fatalf("Clear: %v", err)
	}
	for _, p := range []string{statsPath, statsPath + ".1", checkpointPath()} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s should be gone after Clear, got err=%v", p, err)
		}
	}
}

func TestRotationOnLargeFile(t *testing.T) {
	statsPath := withTempPath(t)
	// Pre-seed the stats file past the rotation threshold so the
	// next AppendCall trips rotation. Padding lines are valid JSONL
	// (won't break LoadCalls if we ever inspect them).
	padding := strings.Repeat(`{"ts":"2026-01-01T00:00:00Z","tool":"pad","ok":true}`+"\n", 1)
	chunks := int(maxFileBytes / int64(len(padding)))
	f, err := os.Create(statsPath)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < chunks+10; i++ {
		f.WriteString(padding)
	}
	f.Close()

	// Append should trigger rotation: file becomes .1, new file
	// starts with just the new record.
	if err := AppendCall(Call{Tool: "fresh", OK: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(statsPath + ".1"); err != nil {
		t.Errorf("expected rotated file at %s.1, got err=%v", statsPath, err)
	}
	got, err := LoadCalls(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// LoadCalls reads only the live file, so we should see just our
	// one fresh record.
	if len(got) != 1 || got[0].Tool != "fresh" {
		t.Errorf("after rotation, live file should hold only fresh record; got %+v", got)
	}
}

func TestCheckpointRoundTrip(t *testing.T) {
	withTempPath(t)
	if cp := LoadCheckpoint(); !cp.IsZero() {
		t.Errorf("no checkpoint yet, got %v", cp)
	}
	want := time.Now().Truncate(time.Microsecond)
	if err := SaveCheckpoint(want); err != nil {
		t.Fatal(err)
	}
	got := LoadCheckpoint()
	if !got.Equal(want) {
		t.Errorf("checkpoint mismatch: got %v, want %v", got, want)
	}
}

func TestAggregateByTool(t *testing.T) {
	now := time.Now()
	calls := []Call{
		{TS: now, Tool: "run", DurMs: 100, InBytes: 50, OutBytes: 1000, OK: true},
		{TS: now, Tool: "run", DurMs: 200, InBytes: 60, OutBytes: 4000, OK: true},
		{TS: now, Tool: "run", DurMs: 300, InBytes: 70, OutBytes: 2000, OK: false},
		{TS: now, Tool: "journal", DurMs: 500, InBytes: 80, OutBytes: 10000, ProgressBytes: 50000, OK: true},
	}
	aggs := AggregateByTool(calls)
	if len(aggs) != 2 {
		t.Fatalf("expected 2 aggregates, got %d", len(aggs))
	}
	byName := map[string]Aggregate{}
	for _, a := range aggs {
		byName[a.Tool] = a
	}
	run := byName["run"]
	if run.Calls != 3 {
		t.Errorf("run.Calls = %d, want 3", run.Calls)
	}
	if run.TotalOutBytes != 7000 {
		t.Errorf("run.TotalOutBytes = %d, want 7000", run.TotalOutBytes)
	}
	if run.MaxOutBytes != 4000 {
		t.Errorf("run.MaxOutBytes = %d, want 4000", run.MaxOutBytes)
	}
	if run.Errors != 1 {
		t.Errorf("run.Errors = %d, want 1", run.Errors)
	}
	if run.AvgOutBytes != 2333 {
		t.Errorf("run.AvgOutBytes = %d, want 2333", run.AvgOutBytes)
	}
	// EstTotalTokens = (50+60+70 + 1000+4000+2000 + 0) / 4 = 7180/4 = 1795
	if run.EstTotalTokens != 1795 {
		t.Errorf("run.EstTotalTokens = %d, want 1795", run.EstTotalTokens)
	}

	j := byName["journal"]
	if j.TotalProgress != 50000 {
		t.Errorf("journal.TotalProgress = %d, want 50000", j.TotalProgress)
	}
}

func TestPercentile(t *testing.T) {
	cases := []struct {
		xs   []int
		p    int
		want int
	}{
		{nil, 50, 0},
		{[]int{42}, 50, 42},
		{[]int{1, 2, 3, 4, 5}, 0, 1},
		{[]int{1, 2, 3, 4, 5}, 50, 3},
		{[]int{1, 2, 3, 4, 5}, 100, 5},
		// 10 elements, p50 → idx=4 (the 5th element).
		{[]int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 50, 50},
		// p95 → idx=8 (the 9th element).
		{[]int{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}, 95, 90},
		// Unsorted input -- percentile sorts internally.
		{[]int{5, 1, 4, 2, 3}, 50, 3},
	}
	for _, c := range cases {
		// Pass a copy because percentile mutates.
		xs := append([]int(nil), c.xs...)
		got := percentile(xs, c.p)
		if got != c.want {
			t.Errorf("percentile(%v, %d) = %d, want %d", c.xs, c.p, got, c.want)
		}
	}
}

func TestCallEstTokens(t *testing.T) {
	c := Call{InBytes: 100, OutBytes: 4000, ProgressBytes: 0}
	if c.EstTokens() != 1025 {
		t.Errorf("EstTokens = %d, want 1025", c.EstTokens())
	}
	c2 := Call{InBytes: 100, OutBytes: 4000, ProgressBytes: 8000}
	if c2.EstTokens() != 3025 {
		t.Errorf("EstTokens (with progress) = %d, want 3025", c2.EstTokens())
	}
}
