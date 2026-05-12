package main

import (
	"testing"
	"time"
)

func TestNextBackoff(t *testing.T) {
	cases := []struct {
		in, want time.Duration
	}{
		{time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{8 * time.Second, 16 * time.Second},
		{16 * time.Second, 30 * time.Second}, // hits cap
		{30 * time.Second, 30 * time.Second}, // stays at cap
		{45 * time.Second, 30 * time.Second}, // never above cap
	}
	for _, c := range cases {
		if got := nextBackoff(c.in, 30*time.Second); got != c.want {
			t.Errorf("nextBackoff(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestWaitOrStop_TimerFires(t *testing.T) {
	stop := make(chan struct{})
	got := waitOrStop(20*time.Millisecond, stop)
	if !got {
		t.Error("expected true (timer fired), got false")
	}
}

func TestWaitOrStop_StopFires(t *testing.T) {
	stop := make(chan struct{})
	close(stop)
	got := waitOrStop(time.Second, stop)
	if got {
		t.Error("expected false (stopped), got true")
	}
}

func TestParseMCPLogLine(t *testing.T) {
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
		_, pid, _, ok := parseMCPLogLine(c.line)
		if ok != c.wantOK || pid != c.wantPid {
			t.Errorf("parseMCPLogLine(%q): pid=%d ok=%v, want pid=%d ok=%v",
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
		{"tool=run_stream dur=45.0s ok", "run_stream", "45.0s", true},
		// Out-of-order fields shouldn't break parsing.
		{"dur=1s tool=tail ok", "tail", "1s", true},
		// Missing fields produce zero values.
		{"tool=foo", "foo", "", false},
		// Empty payload.
		{"", "", "", false},
	}
	for _, c := range cases {
		n, d, ok := parseToolLine(c.in)
		if n != c.name || d != c.dur || ok != c.isOK {
			t.Errorf("parseToolLine(%q) = (%q, %q, %v), want (%q, %q, %v)",
				c.in, n, d, ok, c.name, c.dur, c.isOK)
		}
	}
}
