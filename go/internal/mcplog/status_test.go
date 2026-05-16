package mcplog

import (
	"os"
	"path/filepath"
	"testing"
)

// Read must thread the bracketed log PID through to each ToolCall.PID
// -- the UI's "alive vs previous session" detail rendering relies on
// it.
func TestRead_ToolCallsCarryPID(t *testing.T) {
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

	st := Read()
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

func TestPidActive(t *testing.T) {
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
		if got := PidActive(c.pid, c.active); got != c.want {
			t.Errorf("PidActive(%d, %v) = %v, want %v", c.pid, c.active, got, c.want)
		}
	}
}

func TestParseLine(t *testing.T) {
	cases := []struct {
		line    string
		wantPid int
		wantOK  bool
	}{
		{"2026-05-12T01:00:00+08:00 [1234] start v=2.7.0", 1234, true},
		{"2026-05-12T01:00:01+08:00 [1234] tool=run dur=0.5s ok", 1234, true},
		{"2026-05-12T01:00:02+08:00 [1234] exit reason=stdin-eof", 1234, true},
		{"malformed line without timestamp", 0, false},
		{"2026-05-12T01:00:00+08:00 missing-bracket pid stuff", 0, false},
		{"2026-05-12T01:00:00+08:00 [notanint] start", 0, false},
		{"not-a-time [1234] start", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		_, pid, _, ok := ParseLine(c.line)
		if ok != c.wantOK || pid != c.wantPid {
			t.Errorf("ParseLine(%q): pid=%d ok=%v, want pid=%d ok=%v",
				c.line, pid, ok, c.wantPid, c.wantOK)
		}
	}
}

func TestParseToolLine(t *testing.T) {
	cases := []struct {
		in   string
		name string
		dur  string
		isOK bool
	}{
		{"tool=run dur=0.5s ok", "run", "0.5s", true},
		{"tool=push dur=12.3s err", "push", "12.3s", false},
		{"tool=run dur=45.0s ok", "run", "45.0s", true},
		// Out-of-order fields shouldn't break parsing.
		{"dur=1s tool=tail ok", "tail", "1s", true},
		// Missing fields produce zero values.
		{"tool=foo", "foo", "", false},
	}
	for _, c := range cases {
		n, d, ok := ParseToolLine(c.in)
		if n != c.name || d != c.dur || ok != c.isOK {
			t.Errorf("ParseToolLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, n, d, ok, c.name, c.dur, c.isOK)
		}
	}
}
