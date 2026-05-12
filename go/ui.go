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

// uiState tracks dashboard interactivity: which job row is currently
// highlighted, whether we're in a full-screen detail view, and
// whether a "press Y to kill" prompt is up. Kept in one struct so
// the renderers can read it without each one growing a parameter.
type uiState struct {
	selectedJob int    // index into the rendered jobs list (post-filter); -1 = empty
	detailMode  bool   // showing the per-job detail panel
	killPrompt  bool   // armed for confirmation; next 'y' actually kills
	showAll     bool   // include completed (`.exit` marker present) jobs in the table
	statusMsg   string // transient line in the footer (kill result, etc.)
	lastFrame   string
	prevLines   int
	forceRedraw bool
	// liveness is the result of the last remote "which job ids have
	// an .exit marker" sweep: jobID -> alive. Missing entries mean
	// "unknown" (e.g. profile unreachable) and are treated as alive
	// so we never hide a job whose status we couldn't probe.
	liveness      map[string]bool
	livenessFresh time.Time
	hiddenJobs    int // filtered-out count for the footer hint
}

// cmdUI is `srv ui` -- a one-screen dashboard showing the bits of srv
// state that usually require five separate subcommands to inspect:
// active profile / cwd, daemon health, saved profile groups, saved
// tunnels (with live up/down state), MCP servers, detached jobs.
//
// Sessions are intentionally not surfaced: the active session is
// already implicit in the Active section, and the rest of `sessions.json`
// is "other shells" -- not what a dashboard rooted in *this* shell
// should foreground. `srv sessions list` is still there for the full
// view.
//
// Jobs are interactive: select with ↑/↓ (or j/k), Enter for the full
// detail panel, K to kill the selected job (one-key Y confirmation).
// Everything else is read-only; no inline tunnel toggle / group edit
// because those have clean subcommand surfaces and would invite the
// undo / confirmation rabbit hole.
//
// Refresh policy: ticks every 2 seconds, but only writes to the
// terminal when the rendered content actually changed. That keeps
// the screen perfectly still on an idle dashboard (no per-tick
// flicker) while still picking up changes from `srv group set` etc.
// in another shell within ~2s.
//
// Keys (dashboard mode):
//
//	q / Ctrl-C    exit
//	r             force a redraw
//	j / ↓         select next job
//	k / ↑         select previous job
//	Enter         open the detail panel for the selected job
//	K             kill the selected job (arms a Y/N confirm)
//
// Keys (detail mode):
//
//	q / any       back to dashboard
//	K             kill (still requires Y confirm)
func cmdUI(cfg *Config) error {
	if !isStdinTTY() {
		// Without a TTY there's no way to read keys; degrade to a
		// one-shot print of the snapshot so `srv ui | less` still
		// works (or piped into a script). Jobs are still listed --
		// just without the selection markers and key hints.
		fmt.Print(renderDashboard(cfg, currentJobs(), nil))
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
	st := &uiState{forceRedraw: true, selectedJob: 0, liveness: map[string]bool{}}
	const refreshEvery = 2 * time.Second
	const livenessTTL = 10 * time.Second

	for {
		// Reload config and jobs each loop so edits from another
		// terminal show up on the next refresh.
		fresh, _ := LoadConfig()
		if fresh == nil {
			fresh = cfg
		}
		allJobs := currentJobs()

		// Liveness: refresh from the remote whenever it's gone stale
		// (or whenever a forced redraw signals the user wants fresh
		// data). One SSH per profile, batched, so the cost is
		// bounded even with many jobs.
		if time.Since(st.livenessFresh) > livenessTTL || st.forceRedraw {
			st.liveness = checkJobLiveness(allJobs, fresh)
			st.livenessFresh = time.Now()
		}

		// Filter to active jobs unless the user asked for the full
		// list via 'a'. Active = no `.exit` marker yet (or liveness
		// unknown, e.g. profile down). This matches the semantics
		// `wait_job` uses to decide "completed".
		jobs := allJobs
		hidden := 0
		if !st.showAll {
			kept := allJobs[:0]
			for _, j := range allJobs {
				if alive, ok := st.liveness[j.ID]; ok && !alive {
					hidden++
					continue
				}
				kept = append(kept, j)
			}
			jobs = kept
		}
		clampJobSelection(st, len(jobs))

		var out string
		if st.detailMode && st.selectedJob >= 0 && st.selectedJob < len(jobs) {
			out = renderJobDetail(jobs[st.selectedJob], st)
		} else {
			st.hiddenJobs = hidden
			out = renderDashboard(fresh, jobs, st)
		}
		if st.forceRedraw || out != st.lastFrame {
			redrawDashboard(out, st.prevLines)
			st.lastFrame = out
			st.prevLines = strings.Count(out, "\n")
			st.forceRedraw = false
		}

		b, ok := kr.readWithTimeout(refreshEvery)
		if !ok {
			// 2s tick: clear any one-shot status message so the
			// footer doesn't carry "killed 20260510" forever.
			if st.statusMsg != "" {
				st.statusMsg = ""
				st.forceRedraw = true
			}
			continue
		}
		if !handleUIKey(b, st, jobs, fresh, kr) {
			clearPicker(st.prevLines)
			return nil
		}
	}
}

// currentJobs loads jobs.json and returns the slice (nil-safe).
// Pulled out so the main loop and the key handler share one source.
func currentJobs() []*JobRecord {
	jf := loadJobsFile()
	if jf == nil {
		return nil
	}
	return jf.Jobs
}

// clampJobSelection keeps st.selectedJob in [0, n) when n>0, or -1
// when there are no jobs. Called every tick because jobs can vanish
// out from under us (we killed one; another shell killed one).
func clampJobSelection(st *uiState, n int) {
	if n == 0 {
		st.selectedJob = -1
		return
	}
	if st.selectedJob < 0 {
		st.selectedJob = 0
	}
	if st.selectedJob >= n {
		st.selectedJob = n - 1
	}
}

// handleUIKey is the input dispatcher. Returns false when the user
// asked to exit (q / Ctrl-C in dashboard mode); true to keep the
// loop running. State mutations flow through st; side effects (the
// remote kill, status messages) are confined here.
func handleUIKey(b byte, st *uiState, jobs []*JobRecord, cfg *Config, kr *keyReader) bool {
	// Kill confirmation is the highest-precedence mode: any key
	// resolves it (Y/y = do it, anything else = cancel) before normal
	// dashboard / detail keys run.
	if st.killPrompt {
		st.killPrompt = false
		st.forceRedraw = true
		if (b == 'y' || b == 'Y') && st.selectedJob >= 0 && st.selectedJob < len(jobs) {
			j := jobs[st.selectedJob]
			if msg, err := uiKillJob(j, cfg); err != nil {
				st.statusMsg = ansiRed + "kill " + j.ID + " failed: " + err.Error() + ansiReset
			} else {
				st.statusMsg = ansiGreen + "kill " + j.ID + ": " + msg + ansiReset
			}
			st.detailMode = false
		} else {
			st.statusMsg = ansiDim + "kill cancelled" + ansiReset
		}
		return true
	}

	if st.detailMode {
		switch b {
		case 'K':
			if st.selectedJob >= 0 && st.selectedJob < len(jobs) {
				st.killPrompt = true
				st.forceRedraw = true
			}
		case 'q', '\x03', '\r', '\n', 0x1b:
			st.detailMode = false
			st.forceRedraw = true
		default:
			st.detailMode = false
			st.forceRedraw = true
		}
		return true
	}

	switch b {
	case 'q', '\x03': // q / Ctrl-C
		return false
	case 'r':
		st.forceRedraw = true
	case 'a':
		st.showAll = !st.showAll
		st.selectedJob = 0
		st.forceRedraw = true
	case 'j':
		if st.selectedJob >= 0 && st.selectedJob+1 < len(jobs) {
			st.selectedJob++
			st.forceRedraw = true
		}
	case 'k':
		if st.selectedJob > 0 {
			st.selectedJob--
			st.forceRedraw = true
		}
	case '\r', '\n':
		if st.selectedJob >= 0 && st.selectedJob < len(jobs) {
			st.detailMode = true
			st.forceRedraw = true
		}
	case 'K':
		if st.selectedJob >= 0 && st.selectedJob < len(jobs) {
			st.killPrompt = true
			st.forceRedraw = true
		}
	case 0x1b: // ESC -- possibly an arrow-key sequence
		b2, ok := kr.readWithTimeout(80 * time.Millisecond)
		if !ok {
			// bare ESC -- treat as quit so it's consistent with the picker
			return false
		}
		if b2 != '[' {
			return true
		}
		b3, ok := kr.readWithTimeout(20 * time.Millisecond)
		if !ok {
			return true
		}
		switch b3 {
		case 'A':
			if st.selectedJob > 0 {
				st.selectedJob--
				st.forceRedraw = true
			}
		case 'B':
			if st.selectedJob >= 0 && st.selectedJob+1 < len(jobs) {
				st.selectedJob++
				st.forceRedraw = true
			}
		}
	}
	return true
}

// uiKillJob is the dashboard-side kill: same wire shape as `srv kill
// <id>` (`kill -TERM <pid>` on the remote, then drop the local record
// on success). Returns the remote's response line ("killed" / "no
// such pid ...") so the caller can put it in the footer. Errors flow
// back to surface in the dashboard, NOT to stderr -- the screen is
// in raw mode and stray writes break the layout.
func uiKillJob(j *JobRecord, cfg *Config) (string, error) {
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return "", fmt.Errorf("profile %q not found", j.Profile)
	}
	cmd := fmt.Sprintf("kill -TERM %d 2>/dev/null && echo killed || echo 'no such pid'", j.Pid)
	res, err := runRemoteCapture(prof, "", cmd)
	if err != nil {
		return "", err
	}
	out := strings.TrimSpace(res.Stdout)
	if out == "" {
		out = strings.TrimSpace(res.Stderr)
	}
	// Drop the local record regardless of whether the remote pid
	// existed -- a "no such pid" usually means the job already exited
	// and we just hadn't cleaned up.
	jf := loadJobsFile()
	if jf != nil {
		kept := jf.Jobs[:0]
		for _, x := range jf.Jobs {
			if x.ID != j.ID {
				kept = append(kept, x)
			}
		}
		jf.Jobs = kept
		_ = saveJobsFile(jf)
	}
	return out, nil
}

// redrawDashboard is a thin alias over the shared redrawInPlace
// helper. Kept as a named entry point so the dashboard's call site
// reads at the right level of abstraction (we're repainting a
// dashboard, not invoking a generic terminal helper).
func redrawDashboard(content string, prevLines int) {
	redrawInPlace(content, prevLines)
}

// renderDashboard collects every section into a single multi-line
// string. Pulled out so non-tty mode and the interactive loop share
// the same renderer. The output is deterministic from the inputs (no
// per-call timestamp embedded) so the refresh loop can hash-compare
// frames and skip redraws when nothing changed.
//
// `jobs` and `st` are nil for the non-TTY one-shot path; in that
// mode the Jobs section renders without selection markers and the
// footer drops the interactive-key hints.
func renderDashboard(cfg *Config, jobs []*JobRecord, st *uiState) string {
	var sb strings.Builder
	dashHeader(&sb, st)
	dashActive(&sb, cfg)
	dashDaemon(&sb)
	dashMCP(&sb)
	dashGroups(&sb, cfg)
	dashTunnels(&sb, cfg)
	dashJobs(&sb, jobs, st)
	dashFooter(&sb, st)
	return sb.String()
}

// renderJobDetail is the full-screen view that replaces the
// dashboard when the user hits Enter on a job row. Shows every
// field of the JobRecord plus the local references the user might
// need to act on it (`srv logs`, `srv kill`).
func renderJobDetail(j *JobRecord, st *uiState) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%sJOB DETAIL%s  %s%s%s\n",
		ansiBold+ansiMagenta, ansiReset, ansiDim, j.ID, ansiReset)
	fmt.Fprintf(&sb, "%s%s%s\n\n", ansiDim, dashboardRule, ansiReset)

	dashField(&sb, "id", dashName(j.ID))
	dashField(&sb, "profile", ansiCyan+j.Profile+ansiReset)
	dashField(&sb, "pid", strconv.Itoa(j.Pid))
	started := j.Started
	if t, ok := parseISOLike(j.Started); ok {
		started = j.Started + dashMeta(" ("+fmtDuration(time.Since(t))+" ago)")
	}
	dashField(&sb, "started", started)
	if j.Cwd != "" {
		dashField(&sb, "cwd", dashPath(j.Cwd))
	}
	if j.Log != "" {
		dashField(&sb, "log", dashPath(j.Log))
	}
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "  %sCOMMAND:%s\n", ansiDim, ansiReset)
	// Wrap the command across multiple lines so a long pipeline
	// stays visible without horizontal scrolling.
	for _, line := range wrapText(j.Cmd, 76) {
		fmt.Fprintf(&sb, "    %s\n", line)
	}
	fmt.Fprintln(&sb)

	fmt.Fprintf(&sb, "%s%s%s\n", ansiDim, dashboardRule, ansiReset)
	if st != nil && st.killPrompt {
		fmt.Fprintf(&sb, "%skill %s? press %sY%s to confirm, any other key cancels%s\n",
			ansiRed+ansiBold, j.ID, ansiYellow+ansiBold, ansiRed+ansiBold, ansiReset)
		return sb.String()
	}
	fmt.Fprintf(&sb, "Keys: %sq%s back   %sK%s kill   %ssrv logs %s -f%s tails remotely\n",
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiDim, j.ID, ansiReset)
	if st != nil && st.statusMsg != "" {
		fmt.Fprintf(&sb, "%s\n", st.statusMsg)
	}
	return sb.String()
}

// wrapText breaks `s` into lines of at most `width` bytes, splitting
// on whitespace when possible. Single tokens longer than width are
// emitted unbroken (we don't try to hyphenate / split a 200-byte
// path mid-token).
func wrapText(s string, width int) []string {
	if width <= 0 || len(s) <= width {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{s}
	}
	var lines []string
	cur := words[0]
	for _, w := range words[1:] {
		if len(cur)+1+len(w) > width {
			lines = append(lines, cur)
			cur = w
		} else {
			cur += " " + w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

const dashboardRule = "================================================================"
const dashboardSubRule = "----------------------------------------------------------------"

func dashHeader(sb *strings.Builder, st *uiState) {
	fmt.Fprintf(sb, "%sSRV UI%s  %scurrent-shell view, jobs are global%s\n",
		ansiBold+ansiMagenta, ansiReset, ansiDim, ansiReset)
	fmt.Fprintf(sb, "%s%s%s\n", ansiDim, dashboardRule, ansiReset)
	if st == nil {
		// Non-TTY snapshot mode: no interactive keys to advertise.
		fmt.Fprintf(sb, "%ssnapshot mode (no tty)%s\n\n", ansiDim, ansiReset)
		return
	}
	mode := "active only"
	if st.showAll {
		mode = "all jobs"
	}
	fmt.Fprintf(sb,
		"Keys: %sq%s quit  %sr%s redraw  %sjk%s/%s↑↓%s select  %s⏎%s detail  %sK%s kill  %sa%s %s\n\n",
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiDim+"("+mode+")"+ansiReset,
	)
}

func dashSection(sb *strings.Builder, title string) {
	fmt.Fprintf(sb, "%s== %s ==%s\n", ansiBold+ansiCyan, strings.ToUpper(title), ansiReset)
}

func dashSectionCount(sb *strings.Builder, title string, count int) {
	fmt.Fprintf(sb, "%s== %s %s(%d)%s ==%s\n",
		ansiBold+ansiCyan, strings.ToUpper(title), ansiDim, count, ansiReset+ansiBold+ansiCyan, ansiReset)
}

func dashField(sb *strings.Builder, key, value string) {
	fmt.Fprintf(sb, "  %-10s %s\n", strings.ToUpper(key)+":", value)
}

func dashStatus(label, color string) string {
	return color + ansiBold + "[" + strings.ToUpper(label) + "]" + ansiReset
}

func dashName(s string) string {
	return ansiYellow + ansiBold + s + ansiReset
}

func dashMeta(s string) string {
	if s == "" {
		return ""
	}
	return ansiDim + s + ansiReset
}

func dashPath(s string) string {
	return ansiGreen + s + ansiReset
}

func dashTableHeader(sb *strings.Builder, cols ...string) {
	fmt.Fprint(sb, "  ")
	for i, col := range cols {
		if i > 0 {
			fmt.Fprint(sb, "  ")
		}
		fmt.Fprintf(sb, "%s%s%s", ansiDim, col, ansiReset)
	}
	fmt.Fprintln(sb)
	fmt.Fprintf(sb, "  %s%s%s\n", ansiDim, dashboardSubRule, ansiReset)
}

func dashActive(sb *strings.Builder, cfg *Config) {
	dashSection(sb, "Active")
	name, prof, err := ResolveProfile(cfg, "")
	if err != nil {
		dashField(sb, "state", dashStatus("no profile", ansiDim))
		fmt.Fprintln(sb)
		return
	}
	target := prof.Host
	if prof.User != "" {
		target = prof.User + "@" + prof.Host
	}
	if prof.GetPort() != 22 {
		target += ":" + strconv.Itoa(prof.GetPort())
	}
	dashField(sb, "profile", dashName(name))
	dashField(sb, "target", ansiCyan+target+ansiReset)
	cwd := GetCwd(name, prof)
	dashField(sb, "cwd", dashPath(cwd))
	if pf := resolveProjectFile(); pf != nil {
		dashField(sb, "pinned", dashPath(pf.Path))
	}
	fmt.Fprintln(sb)
}

func dashDaemon(sb *strings.Builder) {
	dashSection(sb, "Daemon")
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		dashField(sb, "state", dashStatus("stopped", ansiDim))
		fmt.Fprintln(sb)
		return
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "status"}, time.Second)
	if err != nil || resp == nil || !resp.OK {
		dashField(sb, "state", dashStatus("unreachable", ansiRed))
		fmt.Fprintln(sb)
		return
	}
	dashField(sb, "state", dashStatus("running", ansiGreen))
	dashField(sb, "uptime", fmtDuration(time.Duration(resp.Uptime)*time.Second))
	dashField(sb, "pooled", strconv.Itoa(len(resp.Profiles)))
	if len(resp.Profiles) > 0 {
		dashField(sb, "profiles", ansiCyan+strings.Join(resp.Profiles, ", ")+ansiReset)
	}
	fmt.Fprintln(sb)
}

func dashGroups(sb *strings.Builder, cfg *Config) {
	if len(cfg.Groups) == 0 {
		return
	}
	dashSectionCount(sb, "Groups", len(cfg.Groups))
	names := make([]string, 0, len(cfg.Groups))
	for n := range cfg.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	dashTableHeader(sb, "NAME          SIZE  MEMBERS")
	for _, n := range names {
		members := cfg.Groups[n]
		fmt.Fprintf(sb, "  %-12s  %s%2d%s  %s\n",
			dashName(n), ansiMagenta+ansiBold, len(members), ansiReset, ansiCyan+strings.Join(members, ", ")+ansiReset)
	}
	fmt.Fprintln(sb)
}

func dashTunnels(sb *strings.Builder, cfg *Config) {
	if len(cfg.Tunnels) == 0 {
		return
	}
	active := loadActiveTunnels()
	dashSectionCount(sb, "Tunnels", len(cfg.Tunnels))
	names := make([]string, 0, len(cfg.Tunnels))
	for n := range cfg.Tunnels {
		names = append(names, n)
	}
	sort.Strings(names)
	dashTableHeader(sb, "NAME          TYPE     SPEC / STATE")
	for _, n := range names {
		def := cfg.Tunnels[n]
		status := dashStatus("stopped", ansiDim)
		extra := ""
		if a, ok := active[n]; ok {
			status = dashStatus("running", ansiGreen)
			extra = "  listen=" + a.Listen
		}
		flag := ""
		if def.Autostart {
			flag = " " + dashStatus("autostart", ansiCyan)
		}
		if extra != "" {
			extra = ansiDim + extra + ansiReset
		}
		fmt.Fprintf(sb, "  %-12s  %-7s  %s  %s%s%s\n",
			dashName(n), ansiMagenta+def.Type+ansiReset, dashPath(def.Spec), status, extra, flag)
	}
	fmt.Fprintln(sb)
}

// dashJobs renders the jobs table. When `st` is non-nil and the
// caller has a valid selection (st.selectedJob in range), the
// matching row gets a `>` marker + reverse video so the user can
// see what their next Enter / K will target.
//
// If the active filter (default) hid completed jobs, the section
// header gets a "(N hidden)" tail so the user knows to press 'a'
// when they want the full list.
func dashJobs(sb *strings.Builder, jobs []*JobRecord, st *uiState) {
	hidden := 0
	if st != nil && !st.showAll {
		hidden = st.hiddenJobs
	}
	if len(jobs) == 0 && hidden == 0 {
		return
	}
	if hidden > 0 {
		fmt.Fprintf(sb, "%s== JOBS %s(%d, %d completed hidden -- press %sa%s%s%s)%s ==%s\n",
			ansiBold+ansiCyan, ansiDim, len(jobs), hidden,
			ansiYellow+ansiBold, ansiReset+ansiDim, "", "",
			ansiReset+ansiBold+ansiCyan, ansiReset)
	} else {
		dashSectionCount(sb, "Jobs", len(jobs))
	}
	if len(jobs) == 0 {
		fmt.Fprintf(sb, "  %s(nothing running)%s\n\n", ansiDim, ansiReset)
		return
	}
	dashTableHeader(sb, "  ID            PROFILE     PID       AGE       COMMAND")
	for i, j := range jobs {
		cmd := j.Cmd
		if len(cmd) > 60 {
			cmd = cmd[:57] + "..."
		}
		started := j.Started
		if t, ok := parseISOLike(j.Started); ok {
			started = fmtDuration(time.Since(t)) + " ago"
		}
		marker := "   "
		selected := st != nil && st.selectedJob == i
		if selected {
			marker = ansiBold + ansiYellow + " > " + ansiReset
		}
		row := fmt.Sprintf("%-12s  %-10s  %-8d  %-8s  %s",
			dashName(truncID(j.ID)), ansiCyan+j.Profile+ansiReset, j.Pid, dashMeta(started), cmd)
		if selected {
			// Reverse-video the row content so the selection is
			// readable on terminals that drop the cursor marker.
			fmt.Fprintf(sb, "%s%s%s%s\n", marker, ansiReverse, row, ansiReset)
		} else {
			fmt.Fprintf(sb, "%s%s\n", marker, row)
		}
	}
	fmt.Fprintln(sb)
}

// dashSessions intentionally removed: the active session's profile +
// cwd is already in the Active section, and a "top N sessions"
// listing pulled in records owned by *other* shells, which isn't
// what a current-shell dashboard should foreground. The full view
// still lives in `srv sessions list`.

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
	dashSection(sb, "MCP")
	if len(st.ActivePIDs) == 0 {
		// Log exists but no recent activity / no live pid. Show that
		// MCP is idle plus the most recent activity for context.
		if !st.LastActive.IsZero() {
			dashField(sb, "state", dashStatus("idle", ansiDim))
			dashField(sb, "last", fmtDuration(time.Since(st.LastActive))+" ago")
		} else {
			dashField(sb, "state", dashStatus("idle", ansiDim))
		}
	} else {
		pids := make([]string, 0, len(st.ActivePIDs))
		for _, p := range st.ActivePIDs {
			pids = append(pids, strconv.Itoa(p))
		}
		dashField(sb, "state", dashStatus("running", ansiGreen))
		dashField(sb, "pids", strings.Join(pids, ", "))
	}
	if len(st.RecentTools) > 0 {
		fmt.Fprintln(sb)
		dashTableHeader(sb, "TOOL                  DUR      STATE    AGE")
		for _, tc := range st.RecentTools {
			status := dashStatus("ok", ansiGreen)
			if !tc.OK {
				status = dashStatus("err", ansiRed)
			}
			age := dashMeta(fmtDuration(time.Since(tc.When)) + " ago")
			fmt.Fprintf(sb, "  %-20s  %-7s  %-7s  %s\n", ansiYellow+tc.Name+ansiReset, ansiMagenta+tc.Dur+ansiReset, status, age)
		}
	}
	fmt.Fprintln(sb)
}

func dashFooter(sb *strings.Builder, st *uiState) {
	fmt.Fprintf(sb, "%s%s%s\n", ansiDim, dashboardRule, ansiReset)
	if st == nil {
		fmt.Fprintf(sb, "%ssnapshot complete%s\n", ansiDim, ansiReset)
		return
	}
	// Kill prompt takes over the bottom line so the model / user
	// can see exactly what's about to happen.
	if st.killPrompt {
		fmt.Fprintf(sb, "%skill selected job? press %sY%s to confirm, any other key cancels%s\n",
			ansiRed+ansiBold, ansiYellow+ansiBold, ansiRed+ansiBold, ansiReset)
		return
	}
	mode := "active only"
	if st.showAll {
		mode = "all jobs"
	}
	fmt.Fprintf(sb, "Keys: %sq%s quit  %sr%s redraw  %sjk%s/%s↑↓%s move  %s⏎%s detail  %sK%s kill  %sa%s %s\n",
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiDim+"("+mode+")"+ansiReset,
	)
	if st.statusMsg != "" {
		fmt.Fprintf(sb, "%s\n", st.statusMsg)
	}
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
