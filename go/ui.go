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

// uiRow names a selectable row in the dashboard. The cursor is a
// single index into a flat list of uiRows assembled in display order
// (Tunnels first, then Jobs) so j/k walks visually top-down across
// the two interactive sections without the user having to switch
// "focused pane" themselves.
type uiRow struct {
	kind string // "tunnel" or "job"
	id   string // tunnel name or job ID -- stable across re-renders
	idx  int    // index within the section's slice
}

// uiConfirm is a popup confirmation request. Set when a destructive
// action (kill, tunnel down, tunnel remove) needs Y to proceed.
// title is the heading; body is the explanatory lines.
type uiConfirm struct {
	title  string
	body   []string
	action func() (msg string, err error) // called on Y press
}

// uiState tracks dashboard interactivity: which row is highlighted,
// whether we're in a full-screen detail view, and whether a
// confirmation popup is up. Kept in one struct so the renderers can
// read it without each one growing a parameter.
type uiState struct {
	cursor      int        // index into rows; -1 when rows is empty
	rows        []uiRow    // selectable rows in display order
	detailMode  bool       // showing the per-row detail panel
	confirm     *uiConfirm // non-nil = popup is up, awaiting Y/N
	showAll     bool       // include completed (`.exit` marker present) jobs
	statusMsg   string     // transient line in the footer (kill result, etc.)
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

// currentRow returns the uiRow under the cursor, or a zero uiRow
// when there's no selection. Centralised so renderers / handlers
// don't each duplicate the bounds check.
func (s *uiState) currentRow() uiRow {
	if s == nil || s.cursor < 0 || s.cursor >= len(s.rows) {
		return uiRow{}
	}
	return s.rows[s.cursor]
}

// isSelected returns true when the given (kind, idx) matches the
// currently-focused row. Used by table renderers to decide whether
// to draw a `>` marker and reverse video on a row.
func (s *uiState) isSelected(kind string, idx int) bool {
	r := s.currentRow()
	return r.kind == kind && r.idx == idx
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
		fmt.Print(renderDashboard(cfg, currentJobs(), sortedTunnelNames(cfg), nil))
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
	st := &uiState{forceRedraw: true, cursor: 0, liveness: map[string]bool{}}
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
		tunnelNames := sortedTunnelNames(fresh)
		st.hiddenJobs = hidden
		st.rows = buildSelectableRows(tunnelNames, jobs)
		clampCursor(st)

		var out string
		row := st.currentRow()
		if st.detailMode {
			switch row.kind {
			case "job":
				if row.idx >= 0 && row.idx < len(jobs) {
					out = renderJobDetail(jobs[row.idx], st)
				}
			case "tunnel":
				if row.idx >= 0 && row.idx < len(tunnelNames) {
					out = renderTunnelDetail(tunnelNames[row.idx], fresh, st)
				}
			}
		}
		if out == "" {
			out = renderDashboard(fresh, jobs, tunnelNames, st)
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
		if !handleUIKey(b, st, jobs, tunnelNames, fresh, kr) {
			clearPicker(st.prevLines)
			return nil
		}
	}
}

// sortedTunnelNames returns the names of all defined tunnels in
// stable alphabetical order so the cursor sees a deterministic row
// layout across re-renders.
func sortedTunnelNames(cfg *Config) []string {
	if cfg == nil || len(cfg.Tunnels) == 0 {
		return nil
	}
	out := make([]string, 0, len(cfg.Tunnels))
	for n := range cfg.Tunnels {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// buildSelectableRows assembles the flat row list used by the
// shared cursor. Tunnels come first because they appear first in
// the dashboard; jobs follow. Keeping the order display-aligned
// means a press of `j` moves the cursor visually downward without
// jumping between sections.
func buildSelectableRows(tunnels []string, jobs []*JobRecord) []uiRow {
	rows := make([]uiRow, 0, len(tunnels)+len(jobs))
	for i, n := range tunnels {
		rows = append(rows, uiRow{kind: "tunnel", id: n, idx: i})
	}
	for i, j := range jobs {
		rows = append(rows, uiRow{kind: "job", id: j.ID, idx: i})
	}
	return rows
}

// clampCursor keeps st.cursor in [0, len(rows)) when rows is non-
// empty, -1 otherwise. Called every tick because rows can shrink
// out from under the cursor (we killed a job; another shell did
// `srv tunnel remove`).
func clampCursor(st *uiState) {
	n := len(st.rows)
	if n == 0 {
		st.cursor = -1
		return
	}
	if st.cursor < 0 {
		st.cursor = 0
	}
	if st.cursor >= n {
		st.cursor = n - 1
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

// handleUIKey is the input dispatcher. Returns false when the user
// asked to exit (q / Ctrl-C in dashboard mode); true to keep the
// loop running. State mutations flow through st; side effects (the
// remote kill, tunnel up/down, status messages) are confined here.
//
// Precedence: an active confirmation popup eats every key until it
// resolves (Y = run, anything else = cancel), so a stray j/k can't
// accidentally trigger a different action while a "kill?" is up.
func handleUIKey(b byte, st *uiState, jobs []*JobRecord, tunnelNames []string, cfg *Config, kr *keyReader) bool {
	if st.confirm != nil {
		yes := b == 'y' || b == 'Y'
		action := st.confirm.action
		title := st.confirm.title
		st.confirm = nil
		st.forceRedraw = true
		if yes && action != nil {
			msg, err := action()
			if err != nil {
				st.statusMsg = ansiRed + title + " failed: " + err.Error() + ansiReset
			} else {
				st.statusMsg = ansiGreen + title + ": " + msg + ansiReset
			}
			st.detailMode = false
		} else {
			st.statusMsg = ansiDim + title + " cancelled" + ansiReset
		}
		return true
	}

	row := st.currentRow()

	if st.detailMode {
		switch b {
		case 'K':
			armConfirmFor(st, row, jobs, tunnelNames, cfg, 'K')
		case ' ':
			armConfirmFor(st, row, jobs, tunnelNames, cfg, ' ')
		case 'x':
			armConfirmFor(st, row, jobs, tunnelNames, cfg, 'x')
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
		st.cursor = 0
		st.forceRedraw = true
	case 'j':
		if st.cursor >= 0 && st.cursor+1 < len(st.rows) {
			st.cursor++
			st.forceRedraw = true
		}
	case 'k':
		if st.cursor > 0 {
			st.cursor--
			st.forceRedraw = true
		}
	case '\r', '\n':
		if row.kind != "" {
			st.detailMode = true
			st.forceRedraw = true
		}
	case 'K', ' ', 'x':
		armConfirmFor(st, row, jobs, tunnelNames, cfg, b)
	case 0x1b: // ESC -- possibly an arrow-key sequence
		b2, ok := kr.readWithTimeout(80 * time.Millisecond)
		if !ok {
			return false // bare ESC = quit
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
			if st.cursor > 0 {
				st.cursor--
				st.forceRedraw = true
			}
		case 'B':
			if st.cursor >= 0 && st.cursor+1 < len(st.rows) {
				st.cursor++
				st.forceRedraw = true
			}
		}
	}
	return true
}

// armConfirmFor sets up a popup confirmation tailored to the current
// row + the pressed key. Mapping:
//
//	on a job row:    K = kill (TERM)
//	on a tunnel row: Space = toggle up/down,  x = remove
//
// Non-applicable combinations (Space on a job, K on a tunnel) are
// silently ignored -- the key hint in the footer already advertises
// which keys apply to which kind of row.
func armConfirmFor(st *uiState, row uiRow, jobs []*JobRecord, tunnelNames []string, cfg *Config, key byte) {
	switch row.kind {
	case "job":
		if key != 'K' || row.idx < 0 || row.idx >= len(jobs) {
			return
		}
		j := jobs[row.idx]
		st.confirm = &uiConfirm{
			title: "kill " + j.ID,
			body: []string{
				j.ID + "  (" + j.Profile + ", pid " + strconv.Itoa(j.Pid) + ")",
				truncOneLine(j.Cmd, 60),
				"",
				"Send SIGTERM to the remote pid and drop the local jobs.json entry.",
			},
			action: func() (string, error) { return uiKillJob(j, cfg) },
		}
		st.forceRedraw = true
	case "tunnel":
		if row.idx < 0 || row.idx >= len(tunnelNames) {
			return
		}
		name := tunnelNames[row.idx]
		def := cfg.Tunnels[name]
		if def == nil {
			return
		}
		active := loadActiveTunnels()
		_, isUp := active[name]
		switch key {
		case ' ':
			if isUp {
				st.confirm = &uiConfirm{
					title: "tunnel down " + name,
					body: []string{
						name + "  (" + def.Type + " " + def.Spec + ")",
						"",
						"Stop the daemon-hosted listener. Existing connections drop.",
					},
					action: func() (string, error) { return uiTunnelDown(name) },
				}
			} else {
				st.confirm = &uiConfirm{
					title: "tunnel up " + name,
					body: []string{
						name + "  (" + def.Type + " " + def.Spec + ", profile " + tunnelProfileLabel(def) + ")",
						"",
						"Bring the tunnel up via the daemon.",
					},
					action: func() (string, error) { return uiTunnelUp(name) },
				}
			}
			st.forceRedraw = true
		case 'x':
			extra := ""
			if isUp {
				extra = " The currently-running tunnel will be stopped first."
			}
			st.confirm = &uiConfirm{
				title: "remove tunnel " + name,
				body: []string{
					name + "  (" + def.Type + " " + def.Spec + ")",
					"",
					"Delete the saved definition from config." + extra,
				},
				action: func() (string, error) { return uiTunnelRemove(name, cfg) },
			}
			st.forceRedraw = true
		}
	}
}

// truncOneLine clips a string to width chars, appending an ellipsis
// when shortened. Used for popup body lines where we want exactly
// one row of context.
func truncOneLine(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 3 {
		return s[:width]
	}
	return s[:width-3] + "..."
}

// tunnelProfileLabel renders the profile chosen for a tunnel, falling
// back to "(default at up-time)" when the def left it empty.
func tunnelProfileLabel(def *TunnelDef) string {
	if def.Profile == "" {
		return "(default)"
	}
	return def.Profile
}

// uiTunnelUp / uiTunnelDown / uiTunnelRemove are the dashboard-side
// equivalents of `srv tunnel up <name>` etc. They go through the
// same daemon protocol as the CLI subcommands so behaviour stays
// identical; result strings flow back to the footer.
func uiTunnelUp(name string) (string, error) {
	if !ensureDaemon() {
		return "", fmt.Errorf("daemon unavailable")
	}
	conn := daemonDial(2 * time.Second)
	if conn == nil {
		return "", fmt.Errorf("daemon unreachable")
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "tunnel_up", Name: name}, 10*time.Second)
	if err != nil {
		return "", err
	}
	if resp == nil || !resp.OK {
		msg := "daemon refused"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return "", fmt.Errorf("%s", msg)
	}
	if resp.Listen != "" {
		return "listening on " + resp.Listen, nil
	}
	return "up", nil
}

func uiTunnelDown(name string) (string, error) {
	conn := daemonDial(2 * time.Second)
	if conn == nil {
		return "", fmt.Errorf("daemon not running")
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "tunnel_down", Name: name}, 5*time.Second)
	if err != nil {
		return "", err
	}
	if resp == nil || !resp.OK {
		msg := "daemon refused"
		if resp != nil && resp.Err != "" {
			msg = resp.Err
		}
		return "", fmt.Errorf("%s", msg)
	}
	return "stopped", nil
}

// uiTunnelRemove stops any running instance first (best-effort), then
// deletes the saved definition from config. Saving the config goes
// through the regular write-back path so other shells see the
// removal on their next LoadConfig.
func uiTunnelRemove(name string, cfg *Config) (string, error) {
	// Best-effort stop. Ignoring errors here is intentional: the
	// user said "remove", not "remove iff running"; if the down
	// fails we still drop the saved entry.
	_, _ = uiTunnelDown(name)
	if cfg == nil || cfg.Tunnels == nil {
		return "", fmt.Errorf("no tunnels configured")
	}
	if _, ok := cfg.Tunnels[name]; !ok {
		return "", fmt.Errorf("tunnel %q not found", name)
	}
	delete(cfg.Tunnels, name)
	if err := SaveConfig(cfg); err != nil {
		return "", err
	}
	return "removed", nil
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
func renderDashboard(cfg *Config, jobs []*JobRecord, tunnelNames []string, st *uiState) string {
	var sb strings.Builder
	dashHeader(&sb, st)
	dashActive(&sb, cfg)
	dashDaemon(&sb)
	dashMCP(&sb)
	dashGroups(&sb, cfg)
	dashTunnels(&sb, cfg, tunnelNames, st)
	dashJobs(&sb, jobs, st)
	dashFooter(&sb, st)
	if st != nil && st.confirm != nil {
		renderConfirmPopup(&sb, st.confirm)
	}
	return sb.String()
}

// renderConfirmPopup draws a centered box at the bottom of the
// dashboard with the action title and explanatory body lines. The
// Y/N choice is anchored at the box's last row so the user's eye
// lands on it after reading the body.
func renderConfirmPopup(sb *strings.Builder, c *uiConfirm) {
	width := 64
	for _, line := range c.body {
		if w := visualWidth(line) + 4; w > width {
			width = w
		}
	}
	if w := visualWidth(c.title) + 6; w > width {
		width = w
	}
	if width > 78 {
		width = 78
	}
	indent := "  "
	top := indent + "┌" + strings.Repeat("─", width-2) + "┐"
	bot := indent + "└" + strings.Repeat("─", width-2) + "┘"
	fmt.Fprintln(sb)
	fmt.Fprintf(sb, "%s%s%s\n", ansiBold+ansiRed, top, ansiReset)
	fmt.Fprintf(sb, "%s│%s %s%s%s%s%s│%s\n",
		ansiBold+ansiRed, ansiReset,
		ansiBold+ansiRed, c.title, ansiReset,
		strings.Repeat(" ", max(0, width-3-visualWidth(c.title))),
		ansiBold+ansiRed, ansiReset)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		ansiBold+ansiRed, ansiReset,
		strings.Repeat(" ", width-2),
		ansiBold+ansiRed, ansiReset)
	for _, line := range c.body {
		pad := max(0, width-3-visualWidth(line))
		fmt.Fprintf(sb, "%s│%s %s%s%s│%s\n",
			ansiBold+ansiRed, ansiReset,
			line, strings.Repeat(" ", pad),
			ansiBold+ansiRed, ansiReset)
	}
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		ansiBold+ansiRed, ansiReset,
		strings.Repeat(" ", width-2),
		ansiBold+ansiRed, ansiReset)
	choice := ansiYellow + ansiBold + "[Y]" + ansiReset + " confirm    " +
		ansiYellow + ansiBold + "[N/Esc]" + ansiReset + " cancel"
	pad := max(0, width-3-visualWidth("[Y] confirm    [N/Esc] cancel"))
	fmt.Fprintf(sb, "%s│%s %s%s%s│%s\n",
		ansiBold+ansiRed, ansiReset,
		choice, strings.Repeat(" ", pad),
		ansiBold+ansiRed, ansiReset)
	fmt.Fprintf(sb, "%s%s%s\n", ansiBold+ansiRed, bot, ansiReset)
}

// visualWidth returns the *visible* column count of s with ANSI
// escape sequences stripped. CJK width is not handled (each rune
// counts as one column) -- good enough for our short popup labels.
func visualWidth(s string) int {
	w := 0
	inEsc := false
	for _, r := range s {
		if r == 0x1b {
			inEsc = true
			continue
		}
		if inEsc {
			if r == 'm' {
				inEsc = false
			}
			continue
		}
		w++
	}
	return w
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
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
	if st != nil && st.confirm != nil {
		renderConfirmPopup(&sb, st.confirm)
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

// renderTunnelDetail is the per-tunnel detail panel triggered by
// Enter on a tunnel row. Mirrors renderJobDetail's shape.
func renderTunnelDetail(name string, cfg *Config, st *uiState) string {
	def := cfg.Tunnels[name]
	if def == nil {
		return "tunnel " + name + " not found\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%sTUNNEL DETAIL%s  %s%s%s\n",
		ansiBold+ansiMagenta, ansiReset, ansiDim, name, ansiReset)
	fmt.Fprintf(&sb, "%s%s%s\n\n", ansiDim, dashboardRule, ansiReset)
	dashField(&sb, "name", dashName(name))
	dashField(&sb, "type", ansiMagenta+def.Type+ansiReset)
	dashField(&sb, "spec", dashPath(def.Spec))
	dashField(&sb, "profile", ansiCyan+tunnelProfileLabel(def)+ansiReset)
	dashField(&sb, "autostart", boolLabel(def.Autostart))
	active, errs := loadTunnelStatuses()
	if a, ok := active[name]; ok {
		dashField(&sb, "state", dashStatus("running", ansiGreen))
		dashField(&sb, "listen", dashPath(a.Listen))
	} else if msg, ok := errs[name]; ok {
		dashField(&sb, "state", dashStatus("failed", ansiRed))
		// Errors can be wordy ("dial profile X: ssh: handshake
		// failed: connect to ... timeout"). Show on its own block
		// rather than squeezing into one field row.
		fmt.Fprintln(&sb)
		fmt.Fprintf(&sb, "  %sERROR:%s\n", ansiRed+ansiBold, ansiReset)
		for _, line := range wrapText(msg, 72) {
			fmt.Fprintf(&sb, "    %s%s%s\n", ansiRed, line, ansiReset)
		}
	} else {
		dashField(&sb, "state", dashStatus("stopped", ansiDim))
	}
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "%s%s%s\n", ansiDim, dashboardRule, ansiReset)
	if st != nil && st.confirm != nil {
		renderConfirmPopup(&sb, st.confirm)
		return sb.String()
	}
	fmt.Fprintf(&sb, "Keys: %sq%s back   %sSpace%s toggle up/down   %sx%s remove\n",
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset)
	if st != nil && st.statusMsg != "" {
		fmt.Fprintf(&sb, "%s\n", st.statusMsg)
	}
	return sb.String()
}

func boolLabel(b bool) string {
	if b {
		return ansiGreen + ansiBold + "yes" + ansiReset
	}
	return ansiDim + "no" + ansiReset
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

// dashTunnels renders the saved tunnels and overlays their daemon
// status. Caller passes the already-sorted name slice (same slice
// the orchestrator put into st.rows) so the cursor index matches
// the rendered row index exactly.
//
// Status precedence: running > failed > stopped. A tunnel that's
// currently up is shown green even if a prior attempt errored; the
// error gets cleared on the next successful start anyway.
func dashTunnels(sb *strings.Builder, cfg *Config, names []string, st *uiState) {
	if len(names) == 0 {
		return
	}
	active, errs := loadTunnelStatuses()
	dashSectionCount(sb, "Tunnels", len(names))
	dashTableHeader(sb, "  NAME          TYPE     SPEC / STATE")
	for i, n := range names {
		def := cfg.Tunnels[n]
		status := dashStatus("stopped", ansiDim)
		extra := ""
		var errMsg string
		if a, ok := active[n]; ok {
			status = dashStatus("running", ansiGreen)
			extra = "  listen=" + a.Listen
		} else if msg, ok := errs[n]; ok {
			status = dashStatus("failed", ansiRed)
			errMsg = msg
		}
		flag := ""
		if def.Autostart {
			flag = " " + dashStatus("autostart", ansiCyan)
		}
		if extra != "" {
			extra = ansiDim + extra + ansiReset
		}
		marker := "   "
		selected := st != nil && st.isSelected("tunnel", i)
		if selected {
			marker = ansiBold + ansiYellow + " > " + ansiReset
		}
		row := fmt.Sprintf("%-12s  %-7s  %s  %s%s%s",
			dashName(n), ansiMagenta+def.Type+ansiReset, dashPath(def.Spec), status, extra, flag)
		if selected {
			fmt.Fprintf(sb, "%s%s%s%s\n", marker, ansiReverse, row, ansiReset)
		} else {
			fmt.Fprintf(sb, "%s%s\n", marker, row)
		}
		if errMsg != "" {
			// Indent under the row so it groups visually; truncate
			// to keep the table tight.
			line := truncOneLine(errMsg, 70)
			fmt.Fprintf(sb, "      %s%s%s\n", ansiRed, line, ansiReset)
		}
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
		selected := st != nil && st.isSelected("job", i)
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
	// Confirm popup renders elsewhere (centered box appended to the
	// dashboard); the footer only needs the regular key hints.
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
