package main

import (
	"os"
	"path/filepath"
	"testing"
)

// readMCPStatus must thread the bracketed log PID through to each
// mcpToolCall.PID -- the UI's "alive vs previous session" detail
// rendering relies on it.
func TestReadMCPStatus_ToolCallsCarryPID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SRV_HOME", home)

	logBody := `2026-05-12T10:00:00+08:00 [1234] start v=2.7.0
2026-05-12T10:00:01+08:00 [1234] tool=run dur=0.5s ok
2026-05-12T10:00:02+08:00 [1234] tool=push dur=1.2s err
2026-05-12T10:01:00+08:00 [5678] start v=2.7.0
2026-05-12T10:01:01+08:00 [5678] tool=sync dur=3.4s ok
`
	if err := os.WriteFile(filepath.Join(home, "mcp.log"), []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}

	st := readMCPStatus()
	if len(st.RecentTools) != 3 {
		t.Fatalf("got %d recent tools, want 3", len(st.RecentTools))
	}
	want := []struct {
		name string
		pid  int
		ok   bool
	}{
		{"run", 1234, true},
		{"push", 1234, false},
		{"sync", 5678, true},
	}
	for i, w := range want {
		got := st.RecentTools[i]
		if got.Name != w.name {
			t.Errorf("tool[%d].Name = %q, want %q", i, got.Name, w.name)
		}
		if got.PID != w.pid {
			t.Errorf("tool[%d].PID = %d, want %d", i, got.PID, w.pid)
		}
		if got.OK != w.ok {
			t.Errorf("tool[%d].OK = %v, want %v", i, got.OK, w.ok)
		}
	}
}

func TestPidIsActive(t *testing.T) {
	cases := []struct {
		pid    int
		active []int
		want   bool
	}{
		{1234, []int{1234, 5678}, true},
		{9999, []int{1234, 5678}, false},
		{1234, nil, false},
		{0, []int{0}, true}, // edge: 0 still matches if literally in set
	}
	for _, c := range cases {
		if got := pidIsActive(c.pid, c.active); got != c.want {
			t.Errorf("pidIsActive(%d, %v) = %v, want %v", c.pid, c.active, got, c.want)
		}
	}
}
