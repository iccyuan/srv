package mcpstats

import (
	"testing"
	"time"
)

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
