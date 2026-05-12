package main

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// cmdUI is `srv ui` -- a one-screen dashboard showing the bits of srv
// state that usually require five separate subcommands to inspect:
// active profile / cwd, daemon health, saved profile groups, saved
// tunnels (with live up/down state), detached jobs, MCP servers,
// recent sessions.
//
// Read-only by design. Adding inline edit (kill a job, toggle a
// tunnel) is tempting but invites a UX rabbit hole (confirmation
// dialogs, undo, error display) for actions the user can run as one
// subcommand in another terminal pane. The win here is "see
// everything at a glance" -- not "be a full TUI app".
//
// Refresh policy: ticks every 2 seconds, but only writes to the
// terminal when the rendered content actually changed. That keeps
// the screen perfectly still on an idle dashboard (no per-tick
// flicker) while still picking up changes from `srv group set` etc.
// in another shell within ~2s.
//
// Keys:
//
//	q / Ctrl-C    exit
//	r             force a redraw even if data is unchanged
func cmdUI(cfg *Config) error {
	if !isStdinTTY() {
		// Without a TTY there's no way to read keys; degrade to a
		// one-shot print of the snapshot so `srv ui | less` still
		// works (or piped into a script).
		fmt.Print(renderDashboard(cfg))
		return nil
	}
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return exitErr(1, "tty raw mode: %v", err)
	}
	defer term.Restore(fd, state)
	fmt.Fprint(os.Stderr, ansiHide)
	defer fmt.Fprint(os.Stderr, ansiShow)

	kr := newKeyReader()
	var lastFrame string
	prevLines := 0
	const refreshEvery = 2 * time.Second
	forceRedraw := true

	for {
		// Reload config each loop so profile / group / tunnel edits
		// from another terminal show up on the next refresh.
		fresh, _ := LoadConfig()
		if fresh == nil {
			fresh = cfg
		}

		out := renderDashboard(fresh)
		if forceRedraw || out != lastFrame {
			redrawDashboard(out, prevLines)
			lastFrame = out
			prevLines = strings.Count(out, "\n")
			forceRedraw = false
		}

		b, ok := kr.readWithTimeout(refreshEvery)
		if !ok {
			continue // timeout -> recheck data
		}
		switch b {
		case 'q', 0x03: // q / Ctrl-C
			clearPicker(prevLines)
			return nil
		case 'r':
			forceRedraw = true
		}
	}
}

// redrawDashboard moves the cursor up to the start of the previous
// frame and writes the new one over it. Crucially, the per-line erase
// (\x1b[K -- erase from cursor to end of line) happens AFTER each
// line's content lands, not before -- so the terminal only sees
// "rewrite this line and trim any stale tail," never "blank this line
// (visible flash) then refill." The whole frame is emitted in a single
// Fprint, so most terminals render it in one paint cycle.
//
// A final \x1b[J (erase to end of screen) handles the case where the
// new frame is shorter than the old (e.g. a job finished and dropped
// out of the table) -- leftover lines below get cleaned up.
func redrawDashboard(content string, prevLines int) {
	var sb strings.Builder
	if prevLines > 0 {
		fmt.Fprintf(&sb, "\x1b[%dA\r", prevLines)
	}
	// "<line>\x1b[K\r\n" -- write content first, then clear any stale
	// chars that extend past the new content on this line, then move
	// to the next line. \x1b[2K (clear whole line) would blank the
	// line briefly before refilling, which is what we want to avoid.
	sb.WriteString(strings.ReplaceAll(content, "\n", "\x1b[K\r\n"))
	sb.WriteString("\x1b[J")
	fmt.Fprint(os.Stderr, sb.String())
}

// renderDashboard collects every section into a single multi-line
// string. Pulled out so non-tty mode and the interactive loop share
// the same renderer. The output is deterministic from the inputs (no
// per-call timestamp embedded) so the refresh loop can hash-compare
// frames and skip redraws when nothing changed.
func renderDashboard(cfg *Config) string {
	var sb strings.Builder
	dashHeader(&sb)
	dashActive(&sb, cfg)
	dashDaemon(&sb)
	dashMCP(&sb)
	dashGroups(&sb, cfg)
	dashTunnels(&sb, cfg)
	dashJobs(&sb)
	dashSessions(&sb)
	dashFooter(&sb)
	return sb.String()
}

func dashHeader(sb *strings.Builder) {
	fmt.Fprintf(sb, "%ssrv ui%s\n\n", ansiBold, ansiReset)
}

func dashActive(sb *strings.Builder, cfg *Config) {
	fmt.Fprintf(sb, "%sActive%s\n", ansiBold, ansiReset)
	name, prof, err := ResolveProfile(cfg, "")
	if err != nil {
		fmt.Fprintf(sb, "  %s(no active profile)%s\n\n", ansiDim, ansiReset)
		return
	}
	target := prof.Host
	if prof.User != "" {
		target = prof.User + "@" + prof.Host
	}
	if prof.GetPort() != 22 {
		target += ":" + strconv.Itoa(prof.GetPort())
	}
	fmt.Fprintf(sb, "  profile  %s%s%s  %s\n", ansiYellow, name, ansiReset, target)
	cwd := GetCwd(name, prof)
	fmt.Fprintf(sb, "  cwd      %s\n", cwd)
	if pf := resolveProjectFile(); pf != nil {
		fmt.Fprintf(sb, "  pinned by %s\n", pf.Path)
	}
	fmt.Fprintln(sb)
}

func dashDaemon(sb *strings.Builder) {
	fmt.Fprintf(sb, "%sDaemon%s\n", ansiBold, ansiReset)
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		fmt.Fprintf(sb, "  %sstopped%s\n\n", ansiDim, ansiReset)
		return
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "status"}, time.Second)
	if err != nil || resp == nil || !resp.OK {
		fmt.Fprintf(sb, "  %sunreachable%s\n\n", ansiDim, ansiReset)
		return
	}
	fmt.Fprintf(sb, "  running  uptime %s, %d pooled\n",
		fmtDuration(time.Duration(resp.Uptime)*time.Second), len(resp.Profiles))
	if len(resp.Profiles) > 0 {
		fmt.Fprintf(sb, "  pooled   %s\n", strings.Join(resp.Profiles, ", "))
	}
	fmt.Fprintln(sb)
}

func dashGroups(sb *strings.Builder, cfg *Config) {
	if len(cfg.Groups) == 0 {
		return
	}
	fmt.Fprintf(sb, "%sGroups (%d)%s\n", ansiBold, len(cfg.Groups), ansiReset)
	names := make([]string, 0, len(cfg.Groups))
	for n := range cfg.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		members := cfg.Groups[n]
		fmt.Fprintf(sb, "  %-12s  %d: %s\n", n, len(members), strings.Join(members, ", "))
	}
	fmt.Fprintln(sb)
}

func dashTunnels(sb *strings.Builder, cfg *Config) {
	if len(cfg.Tunnels) == 0 {
		return
	}
	active := loadActiveTunnels()
	fmt.Fprintf(sb, "%sTunnels (%d)%s\n", ansiBold, len(cfg.Tunnels), ansiReset)
	names := make([]string, 0, len(cfg.Tunnels))
	for n := range cfg.Tunnels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		def := cfg.Tunnels[n]
		status := ansiDim + "stopped" + ansiReset
		extra := ""
		if a, ok := active[n]; ok {
			status = ansiYellow + "running" + ansiReset
			extra = "  listen=" + a.Listen
		}
		flag := ""
		if def.Autostart {
			flag = " " + ansiCyan + "[autostart]" + ansiReset
		}
		fmt.Fprintf(sb, "  %-12s  %-7s %s  %s%s%s\n",
			n, def.Type, def.Spec, status, extra, flag)
	}
	fmt.Fprintln(sb)
}

func dashJobs(sb *strings.Builder) {
	jf := loadJobsFile()
	if jf == nil || len(jf.Jobs) == 0 {
		return
	}
	fmt.Fprintf(sb, "%sJobs (%d)%s\n", ansiBold, len(jf.Jobs), ansiReset)
	for _, j := range jf.Jobs {
		cmd := j.Cmd
		if len(cmd) > 60 {
			cmd = cmd[:57] + "..."
		}
		started := j.Started
		if t, ok := parseISOLike(j.Started); ok {
			started = fmtDuration(time.Since(t)) + " ago"
		}
		fmt.Fprintf(sb, "  %-12s  %-10s  pid=%-6d  %s  %s\n",
			truncID(j.ID), j.Profile, j.Pid, ansiDim+started+ansiReset, cmd)
	}
	fmt.Fprintln(sb)
}

func dashSessions(sb *strings.Builder) {
	sf := loadSessionsFile()
	if sf == nil || len(sf.Sessions) == 0 {
		return
	}
	// Only sessions that have actually pinned a profile are
	// dashboard-worthy. Unpinned sessions are created on every srv
	// invocation by TouchSession() -- they are noise, not state.
	// Users who want the full list still have `srv sessions list`.
	type row struct {
		sid     string
		rec     *SessionRecord
		seen    time.Time
		hasSeen bool
	}
	rows := make([]row, 0, len(sf.Sessions))
	for sid, rec := range sf.Sessions {
		if rec.Profile == nil || *rec.Profile == "" {
			continue
		}
		r := row{sid: sid, rec: rec}
		if t, ok := parseISOLike(rec.LastSeen); ok {
			r.seen = t
			r.hasSeen = true
		}
		rows = append(rows, r)
	}
	if len(rows) == 0 {
		return
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].seen.After(rows[j].seen) })
	if len(rows) > 5 {
		rows = rows[:5]
	}
	fmt.Fprintf(sb, "%sSessions (top %d)%s\n", ansiBold, len(rows), ansiReset)
	for _, r := range rows {
		prof := *r.rec.Profile
		age := "?"
		if r.hasSeen {
			age = fmtDuration(time.Since(r.seen)) + " ago"
		}
		fmt.Fprintf(sb, "  %-20s  pin=%-12s  %s\n", truncID(r.sid), prof, ansiDim+age+ansiReset)
	}
	fmt.Fprintln(sb)
}

// dashMCP shows whether any MCP server processes are alive (parsed
// from the tail of mcp.log) plus the trailing N tool calls. Useful
// when the dashboard sits in one terminal pane while a Claude Code
// session in another runs MCP tools -- you can watch tool=... lines
// arrive in near real time, and a small history (default 5) gives
// the "what's been happening" context that a single last-line view
// missed.
func dashMCP(sb *strings.Builder) {
	st := readMCPStatus()
	if !st.LogExists {
		// Never started an MCP server on this machine; no signal to
		// show. Don't print an empty section.
		return
	}
	fmt.Fprintf(sb, "%sMCP%s\n", ansiBold, ansiReset)
	if len(st.ActivePIDs) == 0 {
		// Log exists but no recent activity / no live pid. Show that
		// MCP is idle plus the most recent activity for context.
		if !st.LastActive.IsZero() {
			fmt.Fprintf(sb, "  %sidle%s     last activity %s ago\n",
				ansiDim, ansiReset, fmtDuration(time.Since(st.LastActive)))
		} else {
			fmt.Fprintf(sb, "  %sidle%s\n", ansiDim, ansiReset)
		}
	} else {
		pids := make([]string, 0, len(st.ActivePIDs))
		for _, p := range st.ActivePIDs {
			pids = append(pids, strconv.Itoa(p))
		}
		fmt.Fprintf(sb, "  %srunning%s  pids %s\n",
			ansiYellow, ansiReset, strings.Join(pids, ", "))
	}
	if len(st.RecentTools) > 0 {
		fmt.Fprintf(sb, "  %srecent%s\n", ansiDim, ansiReset)
		for _, tc := range st.RecentTools {
			status := ansiYellow + "ok" + ansiReset
			if !tc.OK {
				status = "\x1b[31m" + "err" + ansiReset
			}
			age := ansiDim + fmtDuration(time.Since(tc.When)) + " ago" + ansiReset
			fmt.Fprintf(sb, "    %-18s %-7s %s  %s\n", tc.Name, tc.Dur, status, age)
		}
	}
	fmt.Fprintln(sb)
}

func dashFooter(sb *strings.Builder) {
	fmt.Fprintf(sb, "%sq quit  r force-redraw  (auto-redraws on data change, ~2s)%s\n",
		ansiDim, ansiReset)
}

// parseISOLike accepts the timestamp formats srv writes -- nowISO()
// emits "2006-01-02T15:04:05" in local time (no timezone), while the
// mcp log uses time.RFC3339. We try both rather than RFC3339-only so
// stale job records / sessions from before the dashboard existed
// still render relative times.
func parseISOLike(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	// nowISO() writes local wall-clock without a tz suffix. Parse in
	// time.Local so time.Since(t) compares apples to apples.
	if t, err := time.ParseInLocation("2006-01-02T15:04:05", s, time.Local); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// fmtDuration renders a duration as the largest single unit that fits
// (e.g. "2h 15m" -> "2h", "8s" -> "8s"). Coarse on purpose -- the
// dashboard isn't a stopwatch.
func fmtDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		h := int(d / time.Hour)
		m := int(d/time.Minute) % 60
		if m == 0 {
			return strconv.Itoa(h) + "h"
		}
		return strconv.Itoa(h) + "h " + strconv.Itoa(m) + "m"
	default:
		return strconv.Itoa(int(d/(24*time.Hour))) + "d"
	}
}

// truncID shortens long IDs (job IDs, session IDs) to the first 8 chars
// so they fit table columns; full IDs stay visible via the subcommand
// list views.
func truncID(s string) string {
	if len(s) <= 8 {
		return s
	}
	return s[:8]
}
