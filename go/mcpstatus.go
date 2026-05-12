package main

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MCP log scraper used by `srv ui`'s MCP section.
//
// The mcp.log file (~/.srv/mcp.log) is human-readable and append-only:
//
//	2026-05-10T15:30:00+08:00 [1234] start v=2.6.6
//	2026-05-10T15:30:01+08:00 [1234] tool=run dur=0.5s ok
//	2026-05-10T15:32:15+08:00 [1234] exit reason=stdin-eof
//
// We tail the last 64 KiB (cheap, bounded), parse line-by-line, and
// build a per-pid view: started? exited? last activity? plus the most
// recent `tool=` line across the whole tail for "what did MCP do
// last?". Anything older than mcpStaleAfter is considered dead even
// without an exit line -- a hard crash never wrote `exit`.

const (
	mcpLogTailBytes = 64 << 10
	mcpStaleAfter   = 5 * time.Minute
)

// mcpStatus is the snapshot rendered into the dashboard.
type mcpStatus struct {
	LogPath   string
	LogExists bool
	// ActivePIDs are pids whose last event was a recent activity and
	// not an `exit` line.
	ActivePIDs []int
	// LastActive is the timestamp of the most recent line in the log
	// (any pid). Zero if log empty / unreadable.
	LastActive time.Time
	// RecentTools is the trailing slice of tool calls (most-recent
	// last) parsed from the tail window. Bounded by mcpRecentToolsMax
	// so the dashboard never grows unbounded. Empty when no
	// `tool=...` line is present.
	RecentTools []mcpToolCall
}

// mcpToolCall summarises one `tool=NAME dur=Ds <ok|err>` log line.
// All fields are derived from a single line. PID is the bracketed
// process id from the log prefix -- lets the UI cross-reference
// against ActivePIDs to flag "still alive" vs "previous session".
type mcpToolCall struct {
	When time.Time
	Name string
	Dur  string
	OK   bool
	PID  int
}

// mcpRecentToolsMax bounds the rolling history the dashboard keeps.
// Five is enough to see "what the model has been doing lately"
// without making the section dominate the screen.
const mcpRecentToolsMax = 5

// readMCPStatus reads the tail of mcp.log and condenses it into the
// dashboard view. Robust to a missing / truncated / empty log -- in
// every failure case we return an mcpStatus with LogExists=false
// rather than an error; the caller renders a one-line "stopped".
func readMCPStatus() mcpStatus {
	st := mcpStatus{LogPath: mcpLogPath()}
	info, err := os.Stat(st.LogPath)
	if err != nil {
		return st
	}
	st.LogExists = true

	f, err := os.Open(st.LogPath)
	if err != nil {
		return st
	}
	defer f.Close()

	// Read just the tail. Anything beyond 64 KiB is older than we
	// care about for a "current state" view.
	var startAt int64
	if info.Size() > mcpLogTailBytes {
		startAt = info.Size() - mcpLogTailBytes
	}
	if _, err := f.Seek(startAt, 0); err != nil {
		return st
	}
	buf := make([]byte, info.Size()-startAt)
	if _, err := f.Read(buf); err != nil {
		return st
	}
	text := string(buf)
	// If we jumped into the middle of a line (likely on the first
	// tail), drop the partial leading line.
	if startAt > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}

	type pidState struct {
		started  bool
		exited   bool
		lastSeen time.Time
	}
	pids := map[int]*pidState{}

	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		ts, pid, payload, ok := parseMCPLogLine(line)
		if !ok {
			continue
		}
		if ts.After(st.LastActive) {
			st.LastActive = ts
		}
		ps := pids[pid]
		if ps == nil {
			ps = &pidState{}
			pids[pid] = ps
		}
		if ts.After(ps.lastSeen) {
			ps.lastSeen = ts
		}
		switch {
		case strings.HasPrefix(payload, "start"):
			ps.started = true
			ps.exited = false
		case strings.HasPrefix(payload, "exit"):
			ps.exited = true
		case strings.HasPrefix(payload, "tool="):
			name, dur, ok2 := parseToolLine(payload)
			st.RecentTools = append(st.RecentTools, mcpToolCall{
				When: ts, Name: name, Dur: dur, OK: ok2, PID: pid,
			})
		}
	}

	// Keep only the trailing window. Log is read forward so the slice
	// is already chronological -- last element is most recent.
	if n := len(st.RecentTools); n > mcpRecentToolsMax {
		st.RecentTools = st.RecentTools[n-mcpRecentToolsMax:]
	}

	now := time.Now()
	for pid, ps := range pids {
		if ps.exited {
			continue
		}
		if now.Sub(ps.lastSeen) > mcpStaleAfter {
			continue
		}
		st.ActivePIDs = append(st.ActivePIDs, pid)
	}
	sort.Ints(st.ActivePIDs)
	return st
}

// parseMCPLogLine splits one log line into its three pieces. Format:
//
//	<RFC3339> [<pid>] <payload>
//
// Returns ok=false on any malformation so a truncated / future-format
// line doesn't poison the rest of the tail.
func parseMCPLogLine(line string) (time.Time, int, string, bool) {
	// Find the first space (between timestamp and `[pid]`).
	sp := strings.IndexByte(line, ' ')
	if sp <= 0 {
		return time.Time{}, 0, "", false
	}
	tsStr := line[:sp]
	rest := line[sp+1:]
	if !strings.HasPrefix(rest, "[") {
		return time.Time{}, 0, "", false
	}
	end := strings.IndexByte(rest, ']')
	if end <= 1 {
		return time.Time{}, 0, "", false
	}
	pid, err := strconv.Atoi(rest[1:end])
	if err != nil {
		return time.Time{}, 0, "", false
	}
	payload := strings.TrimSpace(rest[end+1:])
	ts, err := time.Parse(time.RFC3339, tsStr)
	if err != nil {
		return time.Time{}, 0, "", false
	}
	return ts, pid, payload, true
}

// parseToolLine pulls (name, dur, ok) out of a `tool=NAME dur=Ds ok`
// or `tool=NAME dur=Ds err` payload. Returns (name, dur, true-on-ok).
// Best-effort on malformed entries (returns zero values).
func parseToolLine(payload string) (string, string, bool) {
	// payload starts with "tool=..." -- split on spaces.
	parts := strings.Fields(payload)
	name, dur := "", ""
	ok := false
	for _, p := range parts {
		switch {
		case strings.HasPrefix(p, "tool="):
			name = p[len("tool="):]
		case strings.HasPrefix(p, "dur="):
			dur = p[len("dur="):]
		case p == "ok":
			ok = true
		}
	}
	return name, dur, ok
}
