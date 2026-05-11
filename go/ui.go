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
// tunnels (with live up/down state), detached jobs, recent sessions.
//
// Read-only by design. Adding inline edit (kill a job, toggle a
// tunnel) is tempting but invites a UX rabbit hole (confirmation
// dialogs, undo, error display) for actions the user can run as one
// subcommand in another terminal pane. The win here is "see
// everything at a glance" -- not "be a full TUI app".
//
// Auto-refreshes every 2 seconds. Keys:
//
//	q / Ctrl-C    exit
//	r             refresh immediately
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
	prevLines := 0
	const refreshEvery = 2 * time.Second

	for {
		// Reload config each loop so profile / group / tunnel edits
		// from another terminal show up on the next refresh -- the
		// dashboard becomes a passive monitor for the whole srv setup.
		fresh, _ := LoadConfig()
		if fresh == nil {
			fresh = cfg
		}

		if prevLines > 0 {
			fmt.Fprintf(os.Stderr, "\x1b[%dA\x1b[J", prevLines)
		}
		out := renderDashboard(fresh)
		fmt.Fprint(os.Stderr, strings.ReplaceAll(out, "\n", "\r\n"))
		prevLines = strings.Count(out, "\n")

		b, ok := kr.readWithTimeout(refreshEvery)
		if !ok {
			continue // timeout -> refresh
		}
		switch b {
		case 'q', 0x03: // q / Ctrl-C
			clearPicker(prevLines)
			return nil
		case 'r':
			continue
		}
	}
}

// renderDashboard collects every section into a single multi-line
// string. Pulled out so non-tty mode and the interactive loop share
// the same renderer. Returns content ending with a newline.
func renderDashboard(cfg *Config) string {
	var sb strings.Builder
	dashHeader(&sb)
	dashActive(&sb, cfg)
	dashDaemon(&sb)
	dashGroups(&sb, cfg)
	dashTunnels(&sb, cfg)
	dashJobs(&sb)
	dashSessions(&sb)
	dashFooter(&sb)
	return sb.String()
}

func dashHeader(sb *strings.Builder) {
	now := time.Now().Format("15:04:05")
	fmt.Fprintf(sb, "%ssrv ui%s   %s\n\n",
		ansiBold, ansiReset, ansiDim+now+ansiReset)
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
		if t, err := time.Parse(time.RFC3339, j.Started); err == nil {
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
	// Only show recently-seen sessions to keep the dashboard tight --
	// the rest still live in sessions.json and `srv sessions list`.
	type row struct {
		sid     string
		rec     *SessionRecord
		seen    time.Time
		hasSeen bool
	}
	rows := make([]row, 0, len(sf.Sessions))
	for sid, rec := range sf.Sessions {
		r := row{sid: sid, rec: rec}
		if t, err := time.Parse(time.RFC3339, rec.LastSeen); err == nil {
			r.seen = t
			r.hasSeen = true
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].seen.After(rows[j].seen) })
	if len(rows) > 5 {
		rows = rows[:5]
	}
	fmt.Fprintf(sb, "%sSessions (top %d)%s\n", ansiBold, len(rows), ansiReset)
	for _, r := range rows {
		prof := "-"
		if r.rec.Profile != nil {
			prof = *r.rec.Profile
		}
		age := "?"
		if r.hasSeen {
			age = fmtDuration(time.Since(r.seen)) + " ago"
		}
		fmt.Fprintf(sb, "  %-20s  pin=%-12s  %s\n", truncID(r.sid), prof, ansiDim+age+ansiReset)
	}
	fmt.Fprintln(sb)
}

func dashFooter(sb *strings.Builder) {
	fmt.Fprintf(sb, "%sq quit  r refresh  (auto-refresh every 2s)%s\n",
		ansiDim, ansiReset)
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
