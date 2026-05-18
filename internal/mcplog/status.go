package mcplog

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status reader for the dashboard. Tails the last 64 KiB of mcp.log
// (cheap, bounded), parses line-by-line, and builds a per-pid view:
// started? exited? last activity? plus the most recent `tool=` lines
// across the whole tail for "what did MCP do last?". Anything older
// than staleAfter is considered dead even without an exit line --
// a hard crash never wrote `exit`.

const (
	tailBytes  = 64 << 10
	staleAfter = 5 * time.Minute
	recentMax  = 5
)

// Status is the snapshot the dashboard renders. Returned by Read.
type Status struct {
	LogPath   string
	LogExists bool
	// ActivePIDs are pids whose last event was a recent activity and
	// not an `exit` line.
	ActivePIDs []int
	// LastActive is the timestamp of the most recent line in the log
	// (any pid). Zero if log empty / unreadable.
	LastActive time.Time
	// RecentTools is the trailing slice of tool calls (most-recent
	// last) parsed from the tail window. Bounded by recentMax so the
	// dashboard never grows unbounded. Empty when no `tool=...` line
	// is present.
	RecentTools []ToolCall
}

// ToolCall summarises one `tool=NAME dur=Ds <ok|err>` log line.
// All fields are derived from a single line. PID is the bracketed
// process id from the log prefix -- lets the UI cross-reference
// against ActivePIDs to flag "still alive" vs "previous session".
type ToolCall struct {
	When time.Time
	Name string
	Dur  string
	OK   bool
	PID  int
}

// Read tails mcp.log and condenses it into the dashboard view.
// Robust to a missing / truncated / empty log -- in every failure
// case we return a Status with LogExists=false rather than an error;
// the caller renders a one-line "stopped".
func Read() Status {
	st := Status{LogPath: Path()}
	// Stat first so "file exists but unreadable" still renders
	// LogExists=true (a dead session, not "never started"). tailText
	// re-stats -- a microsecond, well inside doctor's ~10 ms budget.
	if _, err := os.Stat(st.LogPath); err != nil {
		return st
	}
	st.LogExists = true

	text, ok := tailText()
	if !ok {
		return st
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
		ts, pid, payload, ok := ParseLine(line)
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
			name, dur, ok2 := ParseToolLine(payload)
			st.RecentTools = append(st.RecentTools, ToolCall{
				When: ts, Name: name, Dur: dur, OK: ok2, PID: pid,
			})
		}
	}

	// Keep only the trailing window. Log is read forward so the slice
	// is already chronological -- last element is most recent.
	if n := len(st.RecentTools); n > recentMax {
		st.RecentTools = st.RecentTools[n-recentMax:]
	}

	now := time.Now()
	for pid, ps := range pids {
		if ps.exited {
			continue
		}
		if now.Sub(ps.lastSeen) > staleAfter {
			continue
		}
		st.ActivePIDs = append(st.ActivePIDs, pid)
	}
	sort.Ints(st.ActivePIDs)
	return st
}

// tailText reads at most the last tailBytes of mcp.log and returns it
// as a string, dropping a partial leading line when the window starts
// mid-file (so callers never parse a half record). Returns ("", false)
// when the log is missing or unreadable. Shared by Read (dashboard)
// and LastElicitation (doctor) so the bounded-tail logic lives once.
func tailText() (string, bool) {
	path := Path()
	info, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()

	// Anything beyond tailBytes is older than any "current state" view
	// cares about.
	var startAt int64
	if info.Size() > tailBytes {
		startAt = info.Size() - tailBytes
	}
	if _, err := f.Seek(startAt, 0); err != nil {
		return "", false
	}
	buf := make([]byte, info.Size()-startAt)
	if _, err := f.Read(buf); err != nil {
		return "", false
	}
	text := string(buf)
	if startAt > 0 {
		if i := strings.IndexByte(text, '\n'); i >= 0 {
			text = text[i+1:]
		}
	}
	return text, true
}

// LastElicitation reports the most recent client/server elicitation
// negotiation recorded in mcp.log. Each `srv mcp` process logs one
// `initialize elicitation=<bool>` line at handshake (loop.go); the
// last such line in the tail window is the current answer.
//
// Returns:
//
//	known -> an initialize line was found (an MCP session has run)
//	on    -> the negotiated value (client advertised the capability)
//	when  -> timestamp of that handshake
//	pid   -> which `srv mcp` process negotiated it
//
// known=false means no MCP session has handshaked recently -- the
// caller should say "unknown", not "off".
func LastElicitation() (known, on bool, when time.Time, pid int) {
	text, ok := tailText()
	if !ok {
		return false, false, time.Time{}, 0
	}
	const prefix = "initialize elicitation="
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		ts, p, payload, ok := ParseLine(line)
		if !ok || !strings.HasPrefix(payload, prefix) {
			continue
		}
		// Read forward, keep overwriting -> last match wins (most
		// recent handshake, possibly a different pid than earlier ones).
		known = true
		on = strings.TrimSpace(payload[len(prefix):]) == "true"
		when = ts
		pid = p
	}
	return known, on, when, pid
}

// ParseLine splits one log line into its three pieces. Format:
//
//	<RFC3339> [<pid>] <payload>
//
// Returns ok=false on any malformation so a truncated / future-format
// line doesn't poison the rest of the tail.
func ParseLine(line string) (time.Time, int, string, bool) {
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

// ParseToolLine pulls (name, dur, ok) out of a `tool=NAME dur=Ds ok`
// or `tool=NAME dur=Ds err` payload. Returns (name, dur, true-on-ok).
// Best-effort on malformed entries (returns zero values).
func ParseToolLine(payload string) (string, string, bool) {
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

// PidActive reports whether pid appears in active. Returns false when
// active is nil or empty. The dashboard's detail panel calls this on
// each rendered ToolCall to flag "still alive" vs "from a previous
// session".
func PidActive(pid int, active []int) bool {
	for _, p := range active {
		if p == pid {
			return true
		}
	}
	return false
}
