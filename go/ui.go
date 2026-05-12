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
	cursor       int        // index into rows; -1 when rows is empty
	rows         []uiRow    // selectable rows in display order
	focusPane    string     // "tunnel" or "job"; Tab / h / l switches panes
	tunnelCursor int        // index within the tunnel window
	jobCursor    int        // index within the jobs window
	detailMode   bool       // showing the per-row detail panel
	confirm      *uiConfirm // non-nil = popup is up, awaiting Y/N
	showAll      bool       // include completed (`.exit` marker present) jobs
	statusMsg    string     // transient line in the footer (kill result, etc.)
	lastFrame    string
	prevLines    int
	forceRedraw  bool
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
	if s == nil || len(s.rows) == 0 {
		return uiRow{}
	}
	tunnels, jobs := countUIRows(s.rows)
	switch s.focusPane {
	case "tunnel":
		if s.tunnelCursor >= 0 && s.tunnelCursor < tunnels {
			return uiRow{kind: "tunnel", id: s.rows[s.tunnelCursor].id, idx: s.tunnelCursor}
		}
	case "job":
		if s.jobCursor >= 0 && s.jobCursor < jobs {
			offset := tunnels + s.jobCursor
			return uiRow{kind: "job", id: s.rows[offset].id, idx: s.jobCursor}
		}
	}
	if s.cursor < 0 || s.cursor >= len(s.rows) {
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

func countUIRows(rows []uiRow) (tunnels, jobs int) {
	for _, r := range rows {
		switch r.kind {
		case "tunnel":
			tunnels++
		case "job":
			jobs++
		}
	}
	return tunnels, jobs
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

	// Width follows the live terminal so the panels actually fill the
	// window instead of sitting at the hard-coded 88. We re-read on
	// every redraw cycle in case the user resized.
	updateDashboardWidth()

	// Enter the alternate screen buffer (xterm extension `?1049`).
	// The user's previous shell content is preserved underneath and
	// restored when we exit, so `srv ui` feels like top / htop /
	// rustnet: type the command, the whole window becomes the UI;
	// quit, the shell is exactly as you left it. Cursor stays hidden
	// for the duration.
	fmt.Fprint(os.Stderr, altScreenOn+ansiHide+clearScreen+cursorHome)
	defer fmt.Fprint(os.Stderr, ansiShow+altScreenOff)

	kr := newKeyReader()
	st := &uiState{forceRedraw: true, cursor: 0, liveness: map[string]bool{}}
	const refreshEvery = 2 * time.Second
	const livenessTTL = 10 * time.Second

	for {
		// Track terminal width so a resize lands cleanly on the next
		// tick. We don't try to repaint mid-frame -- the user can hit
		// `r` if they want it sooner, but a 2s tick is usually fast
		// enough that they won't notice the lag.
		if updateDashboardWidth() {
			st.forceRedraw = true
		}

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

		// Detail is now part of the dashboard's right column,
		// updated live as the cursor moves. No fullscreen modal --
		// rendering always goes through renderDashboard.
		out := renderDashboard(fresh, jobs, tunnelNames, st)
		if st.forceRedraw || out != st.lastFrame {
			// Alt-screen redraw: home the cursor, write the new
			// frame, then clear from cursor to end-of-screen. The
			// terminal cell buffer keeps the pixels of unchanged
			// rows from the previous frame already in place, so
			// only the actually-different bytes flicker. No
			// scrollback pollution -- when the user quits we
			// `?1049l` back to the original shell.
			fmt.Fprint(os.Stderr, cursorHome+out+clearEnd)
			st.lastFrame = out
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
		st.focusPane = ""
		st.tunnelCursor = 0
		st.jobCursor = 0
		return
	}
	tunnels, jobs := countUIRows(st.rows)
	if st.focusPane == "" {
		switch {
		case st.cursor >= 0 && st.cursor < tunnels:
			st.focusPane = "tunnel"
			st.tunnelCursor = st.cursor
		case st.cursor >= tunnels && st.cursor < n:
			st.focusPane = "job"
			st.jobCursor = st.cursor - tunnels
		case st.cursor >= n && n > 0:
			last := st.rows[n-1]
			st.focusPane = last.kind
			if last.kind == "tunnel" {
				st.tunnelCursor = tunnels - 1
			} else {
				st.jobCursor = jobs - 1
			}
		case tunnels > 0:
			st.focusPane = "tunnel"
		default:
			st.focusPane = "job"
		}
	}
	if tunnels == 0 && st.focusPane == "tunnel" {
		st.focusPane = "job"
	}
	if jobs == 0 && st.focusPane == "job" {
		st.focusPane = "tunnel"
	}
	if st.tunnelCursor < 0 {
		st.tunnelCursor = 0
	}
	if st.jobCursor < 0 {
		st.jobCursor = 0
	}
	if tunnels > 0 && st.tunnelCursor >= tunnels {
		st.tunnelCursor = tunnels - 1
	}
	if jobs > 0 && st.jobCursor >= jobs {
		st.jobCursor = jobs - 1
	}
	switch st.focusPane {
	case "tunnel":
		st.cursor = st.tunnelCursor
	case "job":
		st.cursor = tunnels + st.jobCursor
	default:
		st.cursor = 0
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

func focusNextPane(st *uiState) {
	tunnels, jobs := countUIRows(st.rows)
	switch st.focusPane {
	case "tunnel":
		if jobs > 0 {
			st.focusPane = "job"
		}
	case "job":
		if tunnels > 0 {
			st.focusPane = "tunnel"
		}
	default:
		if tunnels > 0 {
			st.focusPane = "tunnel"
		} else if jobs > 0 {
			st.focusPane = "job"
		}
	}
	clampCursor(st)
	st.forceRedraw = true
}

func focusPrevPane(st *uiState) {
	focusNextPane(st)
}

// moveFocusedRow moves the cursor in the focused pane by `delta`
// (1 = down, -1 = up). At pane boundaries the cursor automatically
// crosses into the next section (down past last tunnel jumps to
// first job, up past first job jumps to last tunnel), so the user
// can navigate every selectable row with j/k or arrow keys alone --
// Tab is then just a fast-cross shortcut, not the only way.
func moveFocusedRow(st *uiState, delta int) {
	tunnels, jobs := countUIRows(st.rows)
	if tunnels+jobs == 0 {
		return
	}
	switch st.focusPane {
	case "tunnel":
		next := st.tunnelCursor + delta
		if next >= tunnels && jobs > 0 {
			st.focusPane = "job"
			st.jobCursor = 0
		} else if next < 0 && jobs > 0 {
			st.focusPane = "job"
			st.jobCursor = jobs - 1
		} else {
			st.tunnelCursor = next
		}
	case "job":
		next := st.jobCursor + delta
		if next >= jobs && tunnels > 0 {
			st.focusPane = "tunnel"
			st.tunnelCursor = 0
		} else if next < 0 && tunnels > 0 {
			st.focusPane = "tunnel"
			st.tunnelCursor = tunnels - 1
		} else {
			st.jobCursor = next
		}
	default:
		// No focus set -- pick whichever pane has rows.
		if tunnels > 0 {
			st.focusPane = "tunnel"
			st.tunnelCursor = 0
		} else {
			st.focusPane = "job"
			st.jobCursor = 0
		}
	}
	clampCursor(st)
	st.forceRedraw = true
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

	// Detail is always visible on the right -- Enter no longer opens
	// a modal, it's just a no-op now (or could be wired to trigger
	// the "default action" per row in future). Keep the key sink so
	// stray ⏎ presses don't fall through to other handlers.
	switch b {
	case 'q', '\x03': // q / Ctrl-C
		return false
	case 'r':
		st.forceRedraw = true
	case 'a':
		st.showAll = !st.showAll
		st.focusPane = "job"
		st.jobCursor = 0
		st.forceRedraw = true
	case '\t', 'l':
		focusNextPane(st)
	case 'h':
		focusPrevPane(st)
	case 'j':
		moveFocusedRow(st, 1)
	case 'k':
		moveFocusedRow(st, -1)
	case '\r', '\n':
		// No-op: detail is already visible in the right column,
		// nothing to "open". Sink the key so it doesn't drop into
		// the catch-all default of other handlers.
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
			moveFocusedRow(st, -1)
		case 'B':
			moveFocusedRow(st, 1)
		case 'C':
			focusNextPane(st)
		case 'D':
			focusPrevPane(st)
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
// renderDashboard composes the full-screen layout: status panels on
// top (full width), tunnels + jobs in a left column with the detail
// panel auto-tracking the cursor on the right (ranger / mutt
// idiom), MCP / groups / help underneath (full width again).
//
// Snapshot mode (st == nil) skips the side-by-side -- a piped
// output of stacked boxes is more useful than ascii-art columns.
func renderDashboard(cfg *Config, jobs []*JobRecord, tunnelNames []string, st *uiState) string {
	var sb strings.Builder
	btopHeader(&sb, st)
	btopActive(&sb, cfg)
	btopDaemon(&sb)

	if st == nil {
		btopTunnels(&sb, cfg, tunnelNames, st)
		btopJobs(&sb, jobs, st)
	} else {
		leftW, rightW, gap := splitColumnsWidth(dashboardWidth)
		var leftBuf strings.Builder
		withDashboardWidth(leftW, func() {
			btopTunnels(&leftBuf, cfg, tunnelNames, st)
			btopJobs(&leftBuf, jobs, st)
		})
		var rightBuf strings.Builder
		withDashboardWidth(rightW, func() {
			btopDetail(&rightBuf, cfg, jobs, tunnelNames, st)
		})
		writeSideBySide(&sb, leftBuf.String(), rightBuf.String(), leftW, gap)
	}

	btopMCP(&sb)
	btopGroups(&sb, cfg)
	btopFooter(&sb, st)
	if st != nil && st.confirm != nil {
		renderConfirmPopup(&sb, st.confirm)
	}
	return sb.String()
}

// splitColumnsWidth divides a full-width row into left/right
// columns with a one-cell gap. Both halves get a floor so the
// tunnel rows / detail panel don't end up unreadably narrow on a
// 60-col window.
func splitColumnsWidth(total int) (left, right, gap int) {
	gap = 1
	left = (total - gap) / 2
	if left < 32 {
		left = 32
	}
	right = total - left - gap
	if right < 28 {
		right = 28
	}
	return left, right, gap
}

// withDashboardWidth runs `fn` with the package-level
// dashboardWidth / dashboardContentWidth temporarily clamped to
// `w`, then restores. Lets the existing btop* renderers (which
// read those globals) draw a smaller box for one column without
// having to thread `width` through every helper signature.
func withDashboardWidth(w int, fn func()) {
	if w < 20 {
		w = 20
	}
	savedW, savedC := dashboardWidth, dashboardContentWidth
	dashboardWidth = w
	dashboardContentWidth = w - 4
	defer func() {
		dashboardWidth = savedW
		dashboardContentWidth = savedC
	}()
	fn()
}

// writeSideBySide zips two pre-rendered column blocks line-by-line,
// padding the shorter side with blanks so the result stays
// grid-aligned. visualWidth handles ANSI sequences so a coloured row
// of N visible chars still occupies N cells.
func writeSideBySide(sb *strings.Builder, left, right string, leftWidth, gap int) {
	lLines := splitDashboardLines(left)
	rLines := splitDashboardLines(right)
	n := len(lLines)
	if len(rLines) > n {
		n = len(rLines)
	}
	gapStr := strings.Repeat(" ", gap)
	blankLeft := strings.Repeat(" ", leftWidth)
	for i := 0; i < n; i++ {
		var l, r string
		if i < len(lLines) {
			l = lLines[i]
			pad := leftWidth - visualWidth(l)
			if pad > 0 {
				l += strings.Repeat(" ", pad)
			}
		} else {
			l = blankLeft
		}
		if i < len(rLines) {
			r = rLines[i]
		}
		fmt.Fprintf(sb, "%s%s%s\n", l, gapStr, r)
	}
}

// splitDashboardLines splits panel output and drops the trailing
// empty line that each btop* writes for vertical separation
// (irrelevant when stitching columns together).
func splitDashboardLines(s string) []string {
	if s == "" {
		return nil
	}
	lines := strings.Split(s, "\n")
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// boxColor returns the ANSI escape used for a panel's border.
// Focused panels get bright cyan + bold, every other panel stays
// dim gray. Single switch-by-bool so a theme rework touches one
// spot rather than every box* call site.
func boxColor(focused bool) string {
	if focused {
		return ansiCyan + ansiBold
	}
	return ansiDim
}

// boxTop / boxBottom / boxLine are the default-unfocused variants
// most panels use. Tunnels and Jobs (the only focusable panes) call
// the *Focused variants below and supply the active flag directly.
func boxTop(sb *strings.Builder, title string)    { boxTopFocused(sb, title, false) }
func boxBottom(sb *strings.Builder)               { boxBottomFocused(sb, false) }
func boxLine(sb *strings.Builder, content string) { boxLineFocused(sb, content, false) }

// boxTopFocused draws a rounded-corner panel header with the title
// embedded near the top-left, rustnet / bottom -style. Focused panels
// get a "▸ " arrow prefix on the title plus a bright cyan border so
// the active pane is unambiguous on screen.
func boxTopFocused(sb *strings.Builder, title string, focused bool) {
	border := boxColor(focused)
	label := ""
	if title != "" {
		t := strings.ToUpper(title)
		if focused {
			label = " " + ansiReset + ansiBold + ansiYellow + "▸ " + t + ansiReset + border + " "
		} else {
			label = " " + ansiReset + ansiBold + ansiCyan + t + ansiReset + border + " "
		}
	}
	labelVis := visualWidth(label)
	remain := dashboardWidth - 2 - labelVis
	if remain < 0 {
		remain = 0
	}
	left := 2
	if remain < left {
		left = remain
	}
	right := remain - left
	fmt.Fprintf(sb, "%s╭%s%s%s%s╮%s\n",
		border,
		strings.Repeat("─", left),
		label,
		strings.Repeat("─", right),
		border, ansiReset)
}

func boxBottomFocused(sb *strings.Builder, focused bool) {
	border := boxColor(focused)
	fmt.Fprintf(sb, "%s╰%s╯%s\n", border, strings.Repeat("─", dashboardWidth-2), ansiReset)
}

func boxLineFocused(sb *strings.Builder, content string, focused bool) {
	border := boxColor(focused)
	fmt.Fprintf(sb, "%s│%s %s %s│%s\n",
		border, ansiReset, padAnsiRight(content, dashboardContentWidth), border, ansiReset)
}

func padAnsiRight(s string, width int) string {
	pad := width - visualWidth(s)
	if pad < 0 {
		pad = 0
	}
	return s + strings.Repeat(" ", pad)
}

func fitPlain(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}
	if width <= 3 {
		return s[:width]
	}
	return s[:width-3] + "..."
}

func btopKV(key, value string) string {
	return ansiDim + fmt.Sprintf("%-9s", strings.ToUpper(key)+":") + ansiReset + " " + value
}

func btopPair(leftLabel, leftValue, rightLabel, rightValue string) string {
	left := btopKV(leftLabel, leftValue)
	right := btopKV(rightLabel, rightValue)
	spaces := dashboardContentWidth - visualWidth(left) - visualWidth(right)
	if spaces < 2 {
		spaces = 2
	}
	return left + strings.Repeat(" ", spaces) + right
}

func btopHeader(sb *strings.Builder, st *uiState) {
	boxTop(sb, "srv")
	boxLine(sb, ansiBold+ansiMagenta+"SRV UI"+ansiReset+"  "+ansiDim+"windowed control dashboard"+ansiReset)
	if st == nil {
		boxLine(sb, ansiDim+"snapshot mode (no tty)"+ansiReset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	mode := "active only"
	if st.showAll {
		mode = "all jobs"
	}
	boxLine(sb, fmt.Sprintf("keys: %sq%s quit  %sr%s redraw  %stab/h/l%s window  %sj/k%s row  %senter%s detail  %sa%s jobs %s",
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiYellow+ansiBold, ansiReset,
		ansiDim+"("+mode+")"+ansiReset))
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func btopActive(sb *strings.Builder, cfg *Config) {
	boxTop(sb, "active")
	name, prof, err := ResolveProfile(cfg, "")
	if err != nil {
		boxLine(sb, btopKV("state", dashStatus("no profile", ansiDim)))
		boxBottom(sb)
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
	boxLine(sb, btopPair("profile", dashName(name), "target", ansiCyan+target+ansiReset))
	boxLine(sb, btopKV("cwd", dashPath(GetCwd(name, prof))))
	if pf := resolveProjectFile(); pf != nil {
		boxLine(sb, btopKV("pinned", dashPath(pf.Path)))
	}
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func btopDaemon(sb *strings.Builder) {
	boxTop(sb, "daemon")
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		boxLine(sb, btopKV("state", dashStatus("stopped", ansiDim)))
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "status"}, time.Second)
	if err != nil || resp == nil || !resp.OK {
		boxLine(sb, btopKV("state", dashStatus("unreachable", ansiRed)))
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	boxLine(sb, btopPair("state", dashStatus("running", ansiGreen), "uptime", fmtDuration(time.Duration(resp.Uptime)*time.Second)))
	boxLine(sb, btopPair("pooled", strconv.Itoa(len(resp.Profiles)), "profiles", ansiCyan+fitPlain(strings.Join(resp.Profiles, ", "), 42)+ansiReset))
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func btopGroups(sb *strings.Builder, cfg *Config) {
	if len(cfg.Groups) == 0 {
		return
	}
	boxTop(sb, fmt.Sprintf("groups %d", len(cfg.Groups)))
	boxLine(sb, ansiDim+"NAME          SIZE  MEMBERS"+ansiReset)
	boxLine(sb, ansiDim+strings.Repeat("-", dashboardContentWidth)+ansiReset)
	names := make([]string, 0, len(cfg.Groups))
	for n := range cfg.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		members := cfg.Groups[n]
		boxLine(sb, fmt.Sprintf("%-12s  %s%2d%s  %s",
			dashName(n), ansiMagenta+ansiBold, len(members), ansiReset, ansiCyan+fitPlain(strings.Join(members, ", "), 56)+ansiReset))
	}
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func btopTunnels(sb *strings.Builder, cfg *Config, names []string, st *uiState) {
	if len(names) == 0 {
		return
	}
	focused := st != nil && st.focusPane == "tunnel"
	active, errs := loadTunnelStatuses()
	title := fmt.Sprintf("tunnels %d", len(names))
	boxTopFocused(sb, title, focused)
	boxLineFocused(sb, ansiDim+"  NAME          TYPE     SPEC / STATE"+ansiReset, focused)
	boxLineFocused(sb, ansiDim+strings.Repeat("-", dashboardContentWidth)+ansiReset, focused)
	for i, n := range names {
		def := cfg.Tunnels[n]
		status := dashStatus("stopped", ansiDim)
		extra := ""
		errMsg := ""
		if a, ok := active[n]; ok {
			status = dashStatus("running", ansiGreen)
			extra = " listen=" + a.Listen
		} else if msg, ok := errs[n]; ok {
			status = dashStatus("failed", ansiRed)
			errMsg = msg
		}
		flag := ""
		if def.Autostart {
			flag = " " + dashStatus("autostart", ansiCyan)
		}
		marker := "  "
		row := fmt.Sprintf("%s%-12s  %-7s  %s  %s%s%s",
			marker, dashName(n), ansiMagenta+def.Type+ansiReset, dashPath(fitPlain(def.Spec, 32)), status, ansiDim+extra+ansiReset, flag)
		selected := st != nil && st.isSelected("tunnel", i)
		if selected {
			row = ansiYellow + ansiBold + "> " + ansiReset + ansiReverse + row[2:] + ansiReset
		}
		boxLineFocused(sb, row, focused)
		if errMsg != "" {
			boxLineFocused(sb, "    "+ansiRed+fitPlain(errMsg, 76)+ansiReset, focused)
		}
	}
	boxBottomFocused(sb, focused)
	fmt.Fprintln(sb)
}

func btopJobs(sb *strings.Builder, jobs []*JobRecord, st *uiState) {
	hidden := 0
	if st != nil && !st.showAll {
		hidden = st.hiddenJobs
	}
	if len(jobs) == 0 && hidden == 0 {
		return
	}
	focused := st != nil && st.focusPane == "job"
	title := fmt.Sprintf("jobs %d", len(jobs))
	if hidden > 0 {
		title = fmt.Sprintf("jobs %d + %d hidden", len(jobs), hidden)
	}
	boxTopFocused(sb, title, focused)
	if len(jobs) == 0 {
		boxLineFocused(sb, ansiDim+"nothing running; press a to show completed jobs"+ansiReset, focused)
		boxBottomFocused(sb, focused)
		fmt.Fprintln(sb)
		return
	}
	boxLineFocused(sb, ansiDim+"  ID            PROFILE     PID       AGE       COMMAND"+ansiReset, focused)
	boxLineFocused(sb, ansiDim+strings.Repeat("-", dashboardContentWidth)+ansiReset, focused)
	for i, j := range jobs {
		cmd := fitPlain(j.Cmd, 42)
		started := j.Started
		if t, ok := parseISOLike(j.Started); ok {
			started = fmtDuration(time.Since(t)) + " ago"
		}
		row := fmt.Sprintf("  %-12s  %-10s  %-8d  %-8s  %s",
			dashName(truncID(j.ID)), ansiCyan+j.Profile+ansiReset, j.Pid, dashMeta(started), cmd)
		if st != nil && st.isSelected("job", i) {
			row = ansiYellow + ansiBold + "> " + ansiReset + ansiReverse + row[2:] + ansiReset
		}
		boxLineFocused(sb, row, focused)
	}
	boxBottomFocused(sb, focused)
	fmt.Fprintln(sb)
}

// btopDetail is the right-column panel: a live view of whatever the
// cursor currently highlights. Updates automatically on every j/k /
// arrow / Tab event so the user doesn't have to press Enter to "open"
// a row -- ranger / mutt / lazygit pattern. Falls back to a hint when
// nothing is selected (empty tunnels + empty jobs).
func btopDetail(sb *strings.Builder, cfg *Config, jobs []*JobRecord, tunnelNames []string, st *uiState) {
	row := st.currentRow()
	switch row.kind {
	case "tunnel":
		if row.idx >= 0 && row.idx < len(tunnelNames) {
			btopTunnelDetailColumn(sb, tunnelNames[row.idx], cfg)
			return
		}
	case "job":
		if row.idx >= 0 && row.idx < len(jobs) {
			btopJobDetailColumn(sb, jobs[row.idx])
			return
		}
	}
	boxTop(sb, "detail")
	boxLine(sb, ansiDim+"(no row selected -- move cursor with j/k or arrow keys)"+ansiReset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// btopJobDetailColumn renders job details fit for the right column.
// Same fields as the old full-screen renderJobDetail, just laid out
// against the narrower width.
func btopJobDetailColumn(sb *strings.Builder, j *JobRecord) {
	boxTop(sb, "job detail")
	boxLine(sb, btopKV("id", dashName(j.ID)))
	boxLine(sb, btopKV("profile", ansiCyan+j.Profile+ansiReset))
	boxLine(sb, btopKV("pid", strconv.Itoa(j.Pid)))
	started := j.Started
	if t, ok := parseISOLike(j.Started); ok {
		started = j.Started + dashMeta(" ("+fmtDuration(time.Since(t))+" ago)")
	}
	boxLine(sb, btopKV("started", started))
	if j.Cwd != "" {
		boxLine(sb, btopKV("cwd", dashPath(fitPlain(j.Cwd, dashboardContentWidth-10))))
	}
	if j.Log != "" {
		boxLine(sb, btopKV("log", dashPath(fitPlain(j.Log, dashboardContentWidth-10))))
	}
	boxLine(sb, "")
	boxLine(sb, ansiDim+"COMMAND:"+ansiReset)
	for _, line := range wrapText(j.Cmd, dashboardContentWidth-2) {
		boxLine(sb, "  "+line)
	}
	boxLine(sb, "")
	boxLine(sb, ansiDim+"press "+ansiYellow+ansiBold+"K"+ansiReset+ansiDim+" to kill"+ansiReset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// btopTunnelDetailColumn renders tunnel details for the right
// column. Surfaces last-attempt errors prominently -- that's the
// info the user most wants when something looks "stopped" but they
// expected "running".
func btopTunnelDetailColumn(sb *strings.Builder, name string, cfg *Config) {
	def := cfg.Tunnels[name]
	if def == nil {
		boxTop(sb, "tunnel detail")
		boxLine(sb, ansiRed+"tunnel "+name+" not found in config"+ansiReset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	boxTop(sb, "tunnel detail")
	boxLine(sb, btopKV("name", dashName(name)))
	boxLine(sb, btopKV("type", ansiMagenta+def.Type+ansiReset))
	boxLine(sb, btopKV("spec", dashPath(def.Spec)))
	boxLine(sb, btopKV("profile", ansiCyan+tunnelProfileLabel(def)+ansiReset))
	boxLine(sb, btopKV("autostart", boolLabel(def.Autostart)))
	active, errs := loadTunnelStatuses()
	if a, ok := active[name]; ok {
		boxLine(sb, btopKV("state", dashStatus("running", ansiGreen)))
		boxLine(sb, btopKV("listen", dashPath(a.Listen)))
	} else if msg, ok := errs[name]; ok {
		boxLine(sb, btopKV("state", dashStatus("failed", ansiRed)))
		boxLine(sb, "")
		boxLine(sb, ansiRed+ansiBold+"ERROR:"+ansiReset)
		for _, line := range wrapText(msg, dashboardContentWidth-2) {
			boxLine(sb, "  "+ansiRed+line+ansiReset)
		}
	} else {
		boxLine(sb, btopKV("state", dashStatus("stopped", ansiDim)))
	}
	boxLine(sb, "")
	boxLine(sb, ansiDim+"press "+ansiYellow+ansiBold+"Space"+ansiReset+ansiDim+" up/down, "+ansiYellow+ansiBold+"x"+ansiReset+ansiDim+" remove"+ansiReset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func btopMCP(sb *strings.Builder) {
	st := readMCPStatus()
	if !st.LogExists {
		return
	}
	boxTop(sb, "mcp")
	if len(st.ActivePIDs) == 0 {
		boxLine(sb, btopPair("state", dashStatus("idle", ansiDim), "last", fmtDuration(time.Since(st.LastActive))+" ago"))
	} else {
		pids := make([]string, 0, len(st.ActivePIDs))
		for _, p := range st.ActivePIDs {
			pids = append(pids, strconv.Itoa(p))
		}
		boxLine(sb, btopPair("state", dashStatus("running", ansiGreen), "pids", strings.Join(pids, ", ")))
	}
	if len(st.RecentTools) > 0 {
		boxLine(sb, ansiDim+"TOOL                  DUR      STATE    AGE"+ansiReset)
		boxLine(sb, ansiDim+strings.Repeat("-", dashboardContentWidth)+ansiReset)
		for _, tc := range st.RecentTools {
			status := dashStatus("ok", ansiGreen)
			if !tc.OK {
				status = dashStatus("err", ansiRed)
			}
			boxLine(sb, fmt.Sprintf("%-20s  %-7s  %-7s  %s",
				ansiYellow+tc.Name+ansiReset, ansiMagenta+tc.Dur+ansiReset, status, dashMeta(fmtDuration(time.Since(tc.When))+" ago")))
		}
	}
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func btopFooter(sb *strings.Builder, st *uiState) {
	boxTop(sb, "help")
	if st == nil {
		boxLine(sb, ansiDim+"snapshot complete"+ansiReset)
	} else if st.statusMsg != "" {
		boxLine(sb, st.statusMsg)
	} else {
		focus := st.focusPane
		if focus == "" {
			focus = "none"
		}
		boxLine(sb, btopPair("focus", ansiYellow+ansiBold+strings.ToUpper(focus)+ansiReset, "mode", "window navigation"))
		switch focus {
		case "tunnel":
			boxLine(sb, "actions: "+ansiYellow+"j/k"+ansiReset+" move  "+ansiYellow+"enter"+ansiReset+" details  "+ansiYellow+"space"+ansiReset+" up/down  "+ansiYellow+"x"+ansiReset+" remove  "+ansiYellow+"tab"+ansiReset+" next window")
		case "job":
			boxLine(sb, "actions: "+ansiYellow+"j/k"+ansiReset+" move  "+ansiYellow+"enter"+ansiReset+" details  "+ansiYellow+"K"+ansiReset+" kill  "+ansiYellow+"a"+ansiReset+" show all  "+ansiYellow+"tab"+ansiReset+" next window")
		default:
			boxLine(sb, "actions: "+ansiYellow+"tab"+ansiReset+" choose window  "+ansiYellow+"r"+ansiReset+" refresh  "+ansiYellow+"q"+ansiReset+" quit")
		}
	}
	boxBottom(sb)
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

// dashboardWidth / dashboardContentWidth follow the terminal's
// actual column count once cmdUI starts (set by updateDashboardWidth
// each redraw). The hard-coded defaults take over only in non-TTY
// snapshot mode where no terminal size is reported.
var dashboardWidth = 88
var dashboardContentWidth = dashboardWidth - 4

// Alt-screen / cursor-control ANSI sequences. Same xterm extensions
// every TUI app uses (top / htop / btop / vim / rustnet).
const (
	altScreenOn  = "\x1b[?1049h"
	altScreenOff = "\x1b[?1049l"
	cursorHome   = "\x1b[H"
	clearScreen  = "\x1b[2J"
	clearEnd     = "\x1b[J"
)

// dashboardMinWidth / dashboardMaxWidth bound the auto-detected
// terminal width. Below the minimum, panel borders + the inner
// content collide; above the maximum, lines get embarrassingly
// sparse on ultra-wide monitors.
const (
	dashboardMinWidth = 60
	dashboardMaxWidth = 200
)

// updateDashboardWidth re-reads terminalSize() and updates the
// package-level width vars. Returns true if the width actually
// changed (caller can use it to force a full redraw on resize).
func updateDashboardWidth() bool {
	w, _ := terminalSize()
	if w <= 0 {
		return false
	}
	if w < dashboardMinWidth {
		w = dashboardMinWidth
	}
	if w > dashboardMaxWidth {
		w = dashboardMaxWidth
	}
	if w == dashboardWidth {
		return false
	}
	dashboardWidth = w
	dashboardContentWidth = w - 4
	return true
}

const dashboardRule = "========================================================================================"
const dashboardSubRule = "----------------------------------------------------------------------------------------"

func dashHeader(sb *strings.Builder, st *uiState) {
	boxTop(sb, "srv")
	boxLine(sb, ansiBold+ansiMagenta+"SRV UI"+ansiReset+"  "+ansiDim+"current-shell control dashboard"+ansiReset)
	if st == nil {
		// Non-TTY snapshot mode: no interactive keys to advertise.
		boxLine(sb, ansiDim+"snapshot mode (no tty)"+ansiReset)
		boxBottom(sb)
		fmt.Fprintln(sb)
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
