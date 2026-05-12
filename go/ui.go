package main

import (
	"fmt"
	"os"
	"sort"
	"srv/internal/ansi"
	"srv/internal/jobs"
	"srv/internal/mcplog"
	"srv/internal/project"
	"srv/internal/srvtty"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// uiRow names a selectable row in the dashboard. The cursor is a
// single index into a flat list of uiRows assembled in display order
// (Tunnels first, then Jobs) so ↓/↑ walks visually top-down across
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
	focusPane    string     // "tunnel" / "job" / "mcp"; Tab / h / l cycles
	tunnelCursor int        // index within the tunnel window
	jobCursor    int        // index within the jobs window
	mcpCursor    int        // index within the mcp recent-tools window
	detailMode   bool       // showing the per-row detail panel
	confirm      *uiConfirm // non-nil = popup is up, awaiting Y/N
	statusMsg    string     // transient line in the footer (kill result, etc.)
	lastFrame    string
	prevLines    int
	forceRedraw  bool
	// needFullClear arms a \x1b[2J before the next frame write.
	// Set by updateDashboardWidth on resize so the new (smaller)
	// frame doesn't leave stripes of stale chars from the previous
	// (larger) frame visible in the corners.
	needFullClear bool
	// liveness is the result of the last remote "which job ids have
	// an .exit marker" sweep: jobID -> alive. Missing entries mean
	// "unknown" (e.g. profile unreachable) and are treated as alive
	// so we never hide a job whose status we couldn't probe.
	liveness      map[string]bool
	livenessFresh time.Time
	hiddenJobs    int // filtered-out exited-jobs count for the title hint
	// selectedProfile is the dashboard's notion of "which profile am I
	// looking at right now". Independent of sessions.json so a dashboard
	// open in one shell isn't yanked around by another shell's `srv use`.
	// Persists across runs via ui-state.json; press `p` to cycle.
	selectedProfile string
	statusSetAt     time.Time // wall-clock when statusMsg was last set
	// Snapshot of disk-backed and daemon-backed state. Refreshed on
	// tick or after a destructive action; every other iteration of
	// the render loop reads from these fields instead of re-doing
	// the I/O. Without this cache every poll ticks costs roughly:
	//
	//	~10ms  GetCwd / session.Touch (sessions.json read + write)
	//	~5ms   resolveProjectFile (cwd walk + stat)
	//	~5ms   panelDaemon daemonDial + status RPC
	//	~5ms   panelTunnels loadTunnelStatuses
	//	~5ms   panelTunnelDetail loadTunnelStatuses (again)
	//
	// On a 150ms poll that's ~20% CPU just to redraw "nothing
	// changed". With the cache idle redraws cost ~1ms (string build).
	snapCfg          *Config
	snapJobs         []*jobs.Record
	snapMCP          mcplog.Status
	snapDaemonResp   *daemonResponse
	snapTunnelActive map[string]tunnelInfo
	snapTunnelErrs   map[string]string
	snapProject      *project.File
	snapAt           time.Time
}

// currentRow returns the uiRow under the cursor, or a zero uiRow
// when there's no selection. Centralised so renderers / handlers
// don't each duplicate the bounds check.
func (s *uiState) currentRow() uiRow {
	if s == nil || len(s.rows) == 0 {
		return uiRow{}
	}
	tunnels, jobs, mcp := countUIRows(s.rows)
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
	case "mcp":
		if s.mcpCursor >= 0 && s.mcpCursor < mcp {
			offset := tunnels + jobs + s.mcpCursor
			return uiRow{kind: "mcp", id: s.rows[offset].id, idx: s.mcpCursor}
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

func countUIRows(rows []uiRow) (tunnels, jobs, mcp int) {
	for _, r := range rows {
		switch r.kind {
		case "tunnel":
			tunnels++
		case "job":
			jobs++
		case "mcp":
			mcp++
		}
	}
	return tunnels, jobs, mcp
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
// Jobs are interactive: select with ↑/↓, k to kill the selected job
// (one-key Y confirmation). Everything else is read-only; no inline
// tunnel toggle / group edit because those have clean subcommand
// surfaces and would invite the undo / confirmation rabbit hole.
//
// Refresh policy: ticks every 2 seconds, but only writes to the
// terminal when the rendered content actually changed. That keeps
// the screen perfectly still on an idle dashboard (no per-tick
// flicker) while still picking up changes from `srv group set` etc.
// in another shell within ~2s. Disk-backed state (config, jobs, mcp
// log) is snapshotted so rapid cursor / pane switches don't re-read
// the files on every keystroke.
//
// Keys (dashboard mode):
//
//	q / Ctrl-C    exit
//	r             force a redraw
//	↑ / ↓         move cursor within the focused window
//	tab / h / l   switch between windows
//	p / P         cycle the active profile forward / backward
//	                (independent of the shell's `srv use` choice)
//	k             kill the selected job (arms a Y/N confirm)
func cmdUI(cfg *Config) error {
	if !srvtty.IsStdinTTY() {
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
	fmt.Fprint(os.Stderr, altScreenOn+ansi.Hide+clearScreen+cursorHome)
	defer fmt.Fprint(os.Stderr, ansi.Show+altScreenOff)

	kr := newKeyReader()
	st := &uiState{
		forceRedraw:     true,
		cursor:          0,
		liveness:        map[string]bool{},
		selectedProfile: pickInitialUIProfile(cfg),
	}
	const refreshEvery = 2 * time.Second
	// pollEvery is how often the loop wakes when no key is pressed.
	// Short enough that a terminal resize lands within ~50ms (Windows
	// has no SIGWINCH so we have to poll). Snapshot reads stay gated
	// by snapTTL and all per-render daemon/disk work now comes from
	// the cache, so the extra polls are essentially free (~1ms each).
	const pollEvery = 50 * time.Millisecond
	const livenessTTL = 10 * time.Second
	// snapTTL bounds how long the on-disk snapshot may be reused
	// between keystrokes. Matches refreshEvery so a 2s idle tick
	// naturally re-reads, while a flurry of cursor moves reuses the
	// snapshot and stays snappy.
	const snapTTL = refreshEvery

	for {
		// Poll terminal size every iteration (pollEvery ~= 50ms) so a
		// resize repaints the dashboard within a frame instead of
		// waiting for the next key press. On resize we also schedule
		// a full clearScreen for the next write to prevent stale
		// chars left over from the previous (wider/taller) frame.
		if updateDashboardWidth() {
			st.forceRedraw = true
			st.needFullClear = true
			st.lastFrame = "" // force diff check to detect "new"
		}

		// Refresh the snapshot only when the cache is older than
		// snapTTL. handleUIKey zeroes snapAt to force-invalidate after
		// destructive actions (kill / tunnel up-down) or on `r`; the
		// 2s idle tick falls through because snapAt was set < 2s ago.
		// Cursor and pane moves don't touch snapAt at all, so they
		// reuse the cached reads -- the visible latency the user
		// notices when MCP-switching.
		if st.snapCfg == nil || st.snapAt.IsZero() || time.Since(st.snapAt) > snapTTL {
			if fresh, _ := LoadConfig(); fresh != nil {
				st.snapCfg = fresh
			}
			st.snapJobs = currentJobs()
			st.snapMCP = mcplog.Read()
			// Drop tool calls whose origin PID isn't in ActivePIDs --
			// "history from a dead session" is noise; the user only
			// wants to see what currently-running MCP servers are
			// doing.
			if len(st.snapMCP.RecentTools) > 0 {
				kept := st.snapMCP.RecentTools[:0]
				for _, tc := range st.snapMCP.RecentTools {
					if mcplog.PidActive(tc.PID, st.snapMCP.ActivePIDs) {
						kept = append(kept, tc)
					}
				}
				st.snapMCP.RecentTools = kept
			}
			st.snapDaemonResp = fetchDaemonStatusForUI()
			st.snapTunnelActive, st.snapTunnelErrs = loadTunnelStatuses()
			st.snapProject = project.Resolve()
			st.snapAt = time.Now()
		}
		fresh := st.snapCfg
		if fresh == nil {
			fresh = cfg
		}
		// Keep selectedProfile valid against the freshest config: a
		// profile rename / removal elsewhere shouldn't leave the
		// dashboard pointing at a ghost.
		if st.selectedProfile == "" || fresh.Profiles[st.selectedProfile] == nil {
			st.selectedProfile = pickInitialUIProfile(fresh)
		}
		allJobs := st.snapJobs

		// Liveness: refresh from the remote whenever it's gone stale
		// (or whenever a forced redraw signals the user wants fresh
		// data). One SSH per profile, batched, so the cost is
		// bounded even with many jobs.
		if time.Since(st.livenessFresh) > livenessTTL || st.forceRedraw {
			// Wrap remoteExitMarkers as an ExitMarkerLister so the
			// jobs package stays decoupled from Config / SSH client.
			lister := func(profName string) (map[string]bool, bool) {
				prof, ok := fresh.Profiles[profName]
				if !ok {
					return nil, false
				}
				markers := remoteExitMarkers(prof)
				return markers, markers != nil
			}
			st.liveness = jobs.CheckLiveness(allJobs, lister)
			st.livenessFresh = time.Now()
		}

		// Filter to active jobs only. Exited rows ("`.exit` marker
		// present" — same semantics `wait_job` uses for "completed")
		// are hidden so the dashboard stays focused on what's
		// running; the full list lives in `srv jobs`.
		jobs := allJobs
		hidden := 0
		kept := allJobs[:0]
		for _, j := range allJobs {
			if alive, ok := st.liveness[j.ID]; ok && !alive {
				hidden++
				continue
			}
			kept = append(kept, j)
		}
		jobs = kept
		tunnelNames := sortedTunnelNames(fresh)
		st.hiddenJobs = hidden
		st.rows = buildSelectableRows(tunnelNames, jobs, st.snapMCP.RecentTools)
		clampCursor(st)

		// Detail is now part of the dashboard's right column,
		// updated live as the cursor moves. No fullscreen modal --
		// rendering always goes through renderDashboard.
		out := renderDashboard(fresh, jobs, tunnelNames, st)
		if st.forceRedraw || out != st.lastFrame {
			// Alt-screen redraw with per-line "clear to end-of-line"
			// (\x1b[K before each \n): each row self-wipes any chars
			// the previous frame left past its new endpoint, which is
			// what makes a shrunk frame after terminal resize look
			// clean instead of having phantom ╮s and ─s trailing off
			// the right side of every row. \x1b[J at the bottom
			// catches the case where the new frame has fewer rows
			// than the old one.
			//
			// On the first frame after resize we emit a full \x1b[2J
			// up-front: the per-line EL only handles trailing chars
			// on the new frame's rows, not the rows that don't exist
			// in the new frame at all.
			prefix := cursorHome
			if st.needFullClear {
				prefix = clearScreen + cursorHome
				st.needFullClear = false
			}
			flushed := strings.ReplaceAll(out, "\n", "\x1b[K\n")
			fmt.Fprint(os.Stderr, prefix+flushed+clearEnd)
			st.lastFrame = out
			st.forceRedraw = false
		}

		b, ok := kr.readWithTimeout(pollEvery)
		if !ok {
			// Poll tick: usually a no-op redraw (snapshot is cached,
			// frame matches lastFrame, nothing flushes). The two
			// things it actually does are detect a terminal resize
			// promptly and age out the transient statusMsg after
			// refreshEvery has elapsed since it was set.
			if st.statusMsg != "" && !st.statusSetAt.IsZero() && time.Since(st.statusSetAt) > refreshEvery {
				st.statusMsg = ""
				st.statusSetAt = time.Time{}
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
// shared cursor. Order matches the visual stack of the left column
// (tunnels -> jobs -> mcp recent), so a press of `j` walks the
// cursor visually downward without jumping between sections.
func buildSelectableRows(tunnels []string, jobs []*jobs.Record, mcpRecent []mcplog.ToolCall) []uiRow {
	rows := make([]uiRow, 0, len(tunnels)+len(jobs)+len(mcpRecent))
	for i, n := range tunnels {
		rows = append(rows, uiRow{kind: "tunnel", id: n, idx: i})
	}
	for i, j := range jobs {
		rows = append(rows, uiRow{kind: "job", id: j.ID, idx: i})
	}
	for i, tc := range mcpRecent {
		rows = append(rows, uiRow{kind: "mcp", id: tc.Name, idx: i})
	}
	return rows
}

// clampCursor keeps st.cursor in [0, len(rows)) when rows is non-
// empty, -1 otherwise. Called every tick because rows can shrink
// out from under the cursor (we killed a job; another shell did
// `srv tunnel remove`; an MCP log line rolled off the tail).
func clampCursor(st *uiState) {
	n := len(st.rows)
	if n == 0 {
		st.cursor = -1
		st.focusPane = ""
		st.tunnelCursor = 0
		st.jobCursor = 0
		st.mcpCursor = 0
		return
	}
	tunnels, jobs, mcp := countUIRows(st.rows)

	// First-time init / external caller dropped a raw st.cursor:
	// derive focusPane + the matching per-pane cursor from it so the
	// global cursor index keeps its position. Used by older callers
	// (and tests) that drive the state by setting just .cursor + .rows.
	if st.focusPane == "" {
		switch {
		case st.cursor >= 0 && st.cursor < tunnels:
			st.focusPane = "tunnel"
			st.tunnelCursor = st.cursor
		case st.cursor >= tunnels && st.cursor < tunnels+jobs:
			st.focusPane = "job"
			st.jobCursor = st.cursor - tunnels
		case st.cursor >= tunnels+jobs && st.cursor < n:
			st.focusPane = "mcp"
			st.mcpCursor = st.cursor - tunnels - jobs
		case st.cursor >= n:
			// Cursor past the end -- pin to the last selectable row.
			if mcp > 0 {
				st.focusPane = "mcp"
				st.mcpCursor = mcp - 1
			} else if jobs > 0 {
				st.focusPane = "job"
				st.jobCursor = jobs - 1
			} else if tunnels > 0 {
				st.focusPane = "tunnel"
				st.tunnelCursor = tunnels - 1
			}
		}
	}

	// If the focused pane has been emptied (e.g., user killed the
	// last job), pick the next non-empty pane in our preferred order.
	if (st.focusPane == "tunnel" && tunnels == 0) ||
		(st.focusPane == "job" && jobs == 0) ||
		(st.focusPane == "mcp" && mcp == 0) ||
		st.focusPane == "" {
		switch {
		case tunnels > 0:
			st.focusPane = "tunnel"
		case jobs > 0:
			st.focusPane = "job"
		case mcp > 0:
			st.focusPane = "mcp"
		default:
			st.focusPane = ""
		}
	}

	if st.tunnelCursor < 0 {
		st.tunnelCursor = 0
	}
	if st.jobCursor < 0 {
		st.jobCursor = 0
	}
	if st.mcpCursor < 0 {
		st.mcpCursor = 0
	}
	if tunnels > 0 && st.tunnelCursor >= tunnels {
		st.tunnelCursor = tunnels - 1
	}
	if jobs > 0 && st.jobCursor >= jobs {
		st.jobCursor = jobs - 1
	}
	if mcp > 0 && st.mcpCursor >= mcp {
		st.mcpCursor = mcp - 1
	}

	switch st.focusPane {
	case "tunnel":
		st.cursor = st.tunnelCursor
	case "job":
		st.cursor = tunnels + st.jobCursor
	case "mcp":
		st.cursor = tunnels + jobs + st.mcpCursor
	default:
		st.cursor = 0
	}
}

// currentJobs loads jobs.json and returns the slice (nil-safe).
// Pulled out so the main loop and the key handler share one source.
func currentJobs() []*jobs.Record {
	jf := jobs.Load()
	if jf == nil {
		return nil
	}
	return jf.Jobs
}

// focusNextPane cycles the focused pane forward through whatever
// non-empty panes exist: tunnel -> job -> mcp -> tunnel. Empty
// panes are skipped so Tab doesn't land in an empty section.
func focusNextPane(st *uiState) {
	cyclePane(st, +1)
}

// focusPrevPane is the reverse direction. h / Shift-Tab / ←.
func focusPrevPane(st *uiState) {
	cyclePane(st, -1)
}

func cyclePane(st *uiState, dir int) {
	tunnels, jobs, mcp := countUIRows(st.rows)
	order := []string{"tunnel", "job", "mcp"}
	avail := []string{}
	for _, p := range order {
		switch p {
		case "tunnel":
			if tunnels > 0 {
				avail = append(avail, p)
			}
		case "job":
			if jobs > 0 {
				avail = append(avail, p)
			}
		case "mcp":
			if mcp > 0 {
				avail = append(avail, p)
			}
		}
	}
	if len(avail) == 0 {
		return
	}
	cur := -1
	for i, p := range avail {
		if p == st.focusPane {
			cur = i
			break
		}
	}
	next := (cur + dir + len(avail)) % len(avail)
	st.focusPane = avail[next]
	clampCursor(st)
	st.forceRedraw = true
}

// moveFocusedRow moves the cursor in the focused pane by `delta`
// (1 = down, -1 = up). At pane boundaries the cursor automatically
// crosses into the next non-empty section (down past last row
// jumps to the top of the next pane; up past first row jumps to
// the bottom of the previous pane). Arrow keys therefore walk every
// selectable row across all three panes without the user having to
// press Tab.
func moveFocusedRow(st *uiState, delta int) {
	if len(st.rows) == 0 {
		return
	}
	tunnels, jobs, mcp := countUIRows(st.rows)
	order := []string{"tunnel", "job", "mcp"}
	sizes := map[string]int{"tunnel": tunnels, "job": jobs, "mcp": mcp}
	cursors := map[string]*int{
		"tunnel": &st.tunnelCursor,
		"job":    &st.jobCursor,
		"mcp":    &st.mcpCursor,
	}

	if st.focusPane == "" || sizes[st.focusPane] == 0 {
		// No focus or focus pane is empty -- jump to the first
		// non-empty pane in display order.
		for _, p := range order {
			if sizes[p] > 0 {
				st.focusPane = p
				*cursors[p] = 0
				break
			}
		}
		st.forceRedraw = true
		clampCursor(st)
		return
	}

	cur := cursors[st.focusPane]
	next := *cur + delta
	size := sizes[st.focusPane]
	if next >= 0 && next < size {
		*cur = next
		clampCursor(st)
		st.forceRedraw = true
		return
	}

	// Crossing a pane boundary -- find the next non-empty pane in
	// the requested direction.
	idx := indexOf(order, st.focusPane)
	step := 1
	if delta < 0 {
		step = -1
	}
	for i := 1; i <= len(order); i++ {
		p := order[(idx+i*step+len(order))%len(order)]
		if sizes[p] == 0 {
			continue
		}
		st.focusPane = p
		if step > 0 {
			*cursors[p] = 0
		} else {
			*cursors[p] = sizes[p] - 1
		}
		break
	}
	clampCursor(st)
	st.forceRedraw = true
}

func indexOf(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

// handleUIKey is the input dispatcher. Returns false when the user
// asked to exit (q / Ctrl-C in dashboard mode); true to keep the
// loop running. State mutations flow through st; side effects (the
// remote kill, tunnel up/down, status messages) are confined here.
//
// Precedence: an active confirmation popup eats every key until it
// resolves (Y = run, anything else = cancel), so a stray arrow press
// can't accidentally trigger a different action while a "kill?" is up.
func handleUIKey(b byte, st *uiState, jobs []*jobs.Record, tunnelNames []string, cfg *Config, kr *keyReader) bool {
	if st.confirm != nil {
		yes := b == 'y' || b == 'Y'
		action := st.confirm.action
		title := st.confirm.title
		st.confirm = nil
		st.forceRedraw = true
		if yes && action != nil {
			msg, err := action()
			if err != nil {
				st.statusMsg = ansi.Red + title + " failed: " + err.Error() + ansi.Reset
			} else {
				st.statusMsg = ansi.Green + title + ": " + msg + ansi.Reset
			}
			st.detailMode = false
			// Destructive action just touched jobs/tunnels on disk;
			// invalidate the snapshot so the next render reflects it
			// instead of waiting up to snapTTL.
			st.snapAt = time.Time{}
		} else {
			st.statusMsg = ansi.Dim + title + " cancelled" + ansi.Reset
		}
		st.statusSetAt = time.Now()
		return true
	}

	row := st.currentRow()

	// Detail is always visible on the right. Navigation is arrow-only;
	// `k` is reserved for the kill action so we don't shadow it with
	// vim-style up-movement.
	switch b {
	case 'q', '\x03': // q / Ctrl-C
		return false
	case 'r':
		st.forceRedraw = true
		st.snapAt = time.Time{} // manual refresh -- bypass snapTTL
	case 'p', 'P':
		dir := 1
		if b == 'P' {
			dir = -1
		}
		if next := cycleProfile(cfg, st.selectedProfile, dir); next != "" && next != st.selectedProfile {
			st.selectedProfile = next
			_ = saveUIPersistedState(&uiPersistedState{LastProfile: next})
			st.forceRedraw = true
		}
	case '\t', 'l':
		focusNextPane(st)
	case 'h':
		focusPrevPane(st)
	case 'k', ' ', 'x':
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
//	on a job row:    k = kill (TERM)
//	on a tunnel row: Space = toggle up/down,  x = remove
//
// Non-applicable combinations (Space on a job, k on a tunnel) are
// silently ignored -- the key hint in the footer already advertises
// which keys apply to which kind of row.
func armConfirmFor(st *uiState, row uiRow, jobs []*jobs.Record, tunnelNames []string, cfg *Config, key byte) {
	switch row.kind {
	case "job":
		if key != 'k' || row.idx < 0 || row.idx >= len(jobs) {
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
func uiKillJob(j *jobs.Record, cfg *Config) (string, error) {
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
	jf := jobs.Load()
	if jf != nil {
		kept := jf.Jobs[:0]
		for _, x := range jf.Jobs {
			if x.ID != j.ID {
				kept = append(kept, x)
			}
		}
		jf.Jobs = kept
		_ = jobs.Save(jf)
	}
	return out, nil
}

// redrawDashboard is a thin alias over the shared srvtty.RedrawInPlace
// helper. Kept as a named entry point so the dashboard's call site
// reads at the right level of abstraction (we're repainting a
// dashboard, not invoking a generic terminal helper).
func redrawDashboard(content string, prevLines int) {
	srvtty.RedrawInPlace(content, prevLines)
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
func renderDashboard(cfg *Config, jobs []*jobs.Record, tunnelNames []string, st *uiState) string {
	var sb strings.Builder
	panelHeader(&sb, st)
	panelActive(&sb, cfg, st)
	panelDaemon(&sb, st)

	// Reuse the snapshot the main loop already tailed -- one disk
	// read per snapTTL, not per redraw. Snapshot mode (st == nil) and
	// the legacy paths still hit disk because there's no cache to
	// inherit from.
	var mcpSnapshot mcplog.Status
	if st != nil {
		mcpSnapshot = st.snapMCP
	} else {
		mcpSnapshot = mcplog.Read()
	}

	if st == nil {
		panelTunnels(&sb, cfg, tunnelNames, st)
		panelJobs(&sb, jobs, st)
		panelMCP(&sb, mcpSnapshot, st)
	} else {
		leftW, rightW, gap := splitColumnsWidth(dashboardWidth)
		var leftBuf strings.Builder
		withDashboardWidth(leftW, func() {
			panelTunnels(&leftBuf, cfg, tunnelNames, st)
			panelJobs(&leftBuf, jobs, st)
			panelMCP(&leftBuf, mcpSnapshot, st)
		})
		var rightBuf strings.Builder
		withDashboardWidth(rightW, func() {
			panelDetail(&rightBuf, cfg, jobs, tunnelNames, mcpSnapshot, st)
		})
		writeSideBySide(&sb, leftBuf.String(), rightBuf.String(), leftW, gap)
	}

	panelGroups(&sb, cfg)
	panelFooter(&sb, st)
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
// `w`, then restores. Lets the existing panel* renderers (which
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
// empty line that each panel* writes for vertical separation
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
		return ansi.Cyan + ansi.Bold
	}
	return ansi.Dim
}

// boxTop / boxBottom / boxLine are the default-unfocused variants
// most panels use. Tunnels and Jobs (the only focusable panes) call
// the *Focused variants below and supply the active flag directly.
func boxTop(sb *strings.Builder, title string)    { boxTopFocused(sb, title, false) }
func boxBottom(sb *strings.Builder)               { boxBottomFocused(sb, false) }
func boxLine(sb *strings.Builder, content string) { boxLineFocused(sb, content, false) }

// boxTopWithHint draws the same panel header as boxTop but also
// embeds a dim right-aligned hint in the top border (typically
// "(p switch)" / "(space toggle)" / similar shortcut advertisements
// for the panel's primary action). Keeps key hints visually anchored
// to the affected panel without taking a content row.
func boxTopWithHint(sb *strings.Builder, title, hint string, focused bool) {
	border := boxColor(focused)
	label := ""
	if title != "" {
		t := strings.ToUpper(title)
		if focused {
			label = " " + ansi.Reset + ansi.Bold + ansi.Yellow + "▸ " + t + ansi.Reset + border + " "
		} else {
			label = " " + ansi.Reset + ansi.Bold + ansi.Cyan + t + ansi.Reset + border + " "
		}
	}
	hintLabel := ""
	if hint != "" {
		hintLabel = " " + ansi.Reset + ansi.Dim + hint + ansi.Reset + border + " "
	}
	labelVis := visualWidth(label)
	hintVis := visualWidth(hintLabel)
	remain := dashboardWidth - 2 - labelVis - hintVis
	if remain < 0 {
		remain = 0
	}
	left := 2
	if remain < left {
		left = remain
	}
	right := remain - left
	fmt.Fprintf(sb, "%s╭%s%s%s%s%s╮%s\n",
		border,
		strings.Repeat("─", left),
		label,
		strings.Repeat("─", right),
		hintLabel,
		border, ansi.Reset)
}

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
			label = " " + ansi.Reset + ansi.Bold + ansi.Yellow + "▸ " + t + ansi.Reset + border + " "
		} else {
			label = " " + ansi.Reset + ansi.Bold + ansi.Cyan + t + ansi.Reset + border + " "
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
		border, ansi.Reset)
}

func boxBottomFocused(sb *strings.Builder, focused bool) {
	border := boxColor(focused)
	fmt.Fprintf(sb, "%s╰%s╯%s\n", border, strings.Repeat("─", dashboardWidth-2), ansi.Reset)
}

func boxLineFocused(sb *strings.Builder, content string, focused bool) {
	border := boxColor(focused)
	fmt.Fprintf(sb, "%s│%s %s %s│%s\n",
		border, ansi.Reset, padAnsiRight(content, dashboardContentWidth), border, ansi.Reset)
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

func kvLine(key, value string) string {
	return ansi.Dim + fmt.Sprintf("%-9s", strings.ToUpper(key)+":") + ansi.Reset + " " + value
}

func kvPair(leftLabel, leftValue, rightLabel, rightValue string) string {
	left := kvLine(leftLabel, leftValue)
	right := kvLine(rightLabel, rightValue)
	spaces := dashboardContentWidth - visualWidth(left) - visualWidth(right)
	if spaces < 2 {
		spaces = 2
	}
	return left + strings.Repeat(" ", spaces) + right
}

func panelHeader(sb *strings.Builder, st *uiState) {
	boxTop(sb, "srv")
	boxLine(sb, ansi.Bold+ansi.Magenta+"SRV UI"+ansi.Reset+"  "+ansi.Dim+"windowed control dashboard"+ansi.Reset)
	if st == nil {
		boxLine(sb, ansi.Dim+"snapshot mode (no tty)"+ansi.Reset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	boxLine(sb, fmt.Sprintf("keys: %sq%s quit  %sr%s redraw  %stab/h/l%s window  %s↑/↓%s row  %sp%s profile  %sk%s kill",
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset))
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// panelActive renders the "what am I looking at" header as a single
// inline row: profile · target · cwd · pinned. Compact form so a
// long hostname or project path doesn't push us onto a second line
// that the terminal then visually wraps; each section truncates
// inline when the running total approaches dashboardContentWidth.
// The "p switch" key hint lives in the box title so the content row
// stays free for the actual data.
func panelActive(sb *strings.Builder, cfg *Config, st *uiState) {
	boxTopWithHint(sb, "active", "p switch", false)
	name, prof := lookupSelectedProfile(cfg, st)
	if prof == nil {
		boxLine(sb, ansi.Dim+"no profile selected"+ansi.Reset)
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
	var pf *project.File
	if st != nil {
		pf = st.snapProject
	} else {
		pf = project.Resolve()
	}
	cwd := uiCwd(prof, pf)
	parts := []string{
		ansi.Yellow + ansi.Bold + name + ansi.Reset,
		ansi.Cyan + target + ansi.Reset,
	}
	if cwd != "" {
		parts = append(parts, dashPath(cwd))
	}
	if pf != nil {
		parts = append(parts, ansi.Dim+"pinned "+ansi.Reset+dashPath(pf.Path))
	}
	boxLine(sb, fitInlineParts(parts, "", dashboardContentWidth))
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// uiCwd is the dashboard's cwd resolver. Deliberately does NOT
// consult sessions.json -- the UI is decoupled from the shell that
// launched it (per "ui 不和 shell 绑定"). session.Touch's read+write
// also dominated the per-render cost, so skipping it makes profile
// switches feel instant. Order of precedence: $SRV_CWD env > pinned
// project file > profile.default_cwd.
func uiCwd(prof *Profile, pf *project.File) string {
	if env := os.Getenv("SRV_CWD"); env != "" {
		return env
	}
	if pf != nil && pf.Cwd != "" {
		return pf.Cwd
	}
	if prof != nil {
		return prof.GetDefaultCwd()
	}
	return ""
}

// lookupSelectedProfile reads the dashboard's chosen profile from
// st.selectedProfile (preferred) and falls back to the first
// available cfg profile -- the dashboard is no longer pegged to
// sessions.json so we never call ResolveProfile here.
func lookupSelectedProfile(cfg *Config, st *uiState) (string, *Profile) {
	if cfg == nil {
		return "", nil
	}
	if st != nil && st.selectedProfile != "" {
		if p, ok := cfg.Profiles[st.selectedProfile]; ok {
			p.Name = st.selectedProfile
			return st.selectedProfile, p
		}
	}
	for _, n := range sortedProfileNames(cfg) {
		p := cfg.Profiles[n]
		p.Name = n
		return n, p
	}
	return "", nil
}

// fitInlineParts joins `parts` with a dim middot separator, appends
// `tail` (right-aligned), and truncates the join from the right when
// it overflows `width`. Used by the single-line panels so a long path
// or profile-list shrinks instead of wrapping.
func fitInlineParts(parts []string, tail string, width int) string {
	sep := ansi.Dim + " · " + ansi.Reset
	body := strings.Join(parts, sep)
	tailW := visualWidth(tail)
	bodyW := visualWidth(body)
	avail := width - tailW - 1
	if avail < 10 {
		avail = 10
	}
	if bodyW > avail {
		body = ellipsisAnsiRight(body, avail)
		bodyW = visualWidth(body)
	}
	pad := width - bodyW - tailW
	if pad < 1 {
		pad = 1
	}
	return body + strings.Repeat(" ", pad) + tail
}

// ellipsisAnsiRight clips an ANSI-bearing string to visual width
// `w`, replacing the last three visible cells with "..." when it
// has to cut. Falls back to a plain-width truncation when the
// string has no escape sequences -- the common case.
func ellipsisAnsiRight(s string, w int) string {
	if visualWidth(s) <= w {
		return s
	}
	if w <= 3 {
		return strings.Repeat(".", w)
	}
	// Walk visible cells, copying bytes verbatim and stopping when we
	// hit the budget. ANSI escapes don't count toward the budget.
	var out strings.Builder
	seen := 0
	inEscape := false
	target := w - 3
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inEscape {
			out.WriteByte(c)
			if (c >= 0x40 && c <= 0x7e) || c == 'm' {
				inEscape = false
			}
			continue
		}
		if c == 0x1b {
			out.WriteByte(c)
			inEscape = true
			continue
		}
		if seen >= target {
			break
		}
		out.WriteByte(c)
		seen++
	}
	out.WriteString("...")
	out.WriteString(ansi.Reset)
	return out.String()
}

// panelDaemon renders daemon state from the cached snapshot -- never
// dials inline. The main loop refreshes st.snapDaemonResp at most
// once per snapTTL (2s); without that, each render fired a fresh
// daemonDial+status RPC, which at the 150ms poll interval meant
// 6-7 socket round-trips per second.
func panelDaemon(sb *strings.Builder, st *uiState) {
	boxTop(sb, "daemon")
	var resp *daemonResponse
	if st != nil {
		resp = st.snapDaemonResp
	} else {
		resp = fetchDaemonStatusForUI()
	}
	if resp == nil {
		boxLine(sb, dashStatus("stopped", ansi.Dim))
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	parts := []string{
		dashStatus("running", ansi.Green),
		ansi.Dim + "up " + ansi.Reset + fmtDuration(time.Duration(resp.Uptime)*time.Second),
		ansi.Dim + "pooled " + ansi.Reset + strconv.Itoa(len(resp.Profiles)),
	}
	if len(resp.Profiles) > 0 {
		parts = append(parts, ansi.Cyan+strings.Join(resp.Profiles, ", ")+ansi.Reset)
	}
	boxLine(sb, fitInlineParts(parts, "", dashboardContentWidth))
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// fetchDaemonStatusForUI does a single status RPC and returns the
// response, or nil if the daemon isn't reachable. Used by the main
// loop's snapshot refresh and by the snapshot-mode (st==nil) fallback.
func fetchDaemonStatusForUI() *daemonResponse {
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		return nil
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "status"}, time.Second)
	if err != nil || resp == nil || !resp.OK {
		return nil
	}
	return resp
}

func panelGroups(sb *strings.Builder, cfg *Config) {
	if len(cfg.Groups) == 0 {
		return
	}
	boxTop(sb, fmt.Sprintf("groups %d", len(cfg.Groups)))
	boxLine(sb, ansi.Dim+"NAME          SIZE  MEMBERS"+ansi.Reset)
	boxLine(sb, ansi.Dim+strings.Repeat("-", dashboardContentWidth)+ansi.Reset)
	names := make([]string, 0, len(cfg.Groups))
	for n := range cfg.Groups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		members := cfg.Groups[n]
		boxLine(sb, fmt.Sprintf("%-12s  %s%2d%s  %s",
			dashName(n), ansi.Magenta+ansi.Bold, len(members), ansi.Reset, ansi.Cyan+fitPlain(strings.Join(members, ", "), 56)+ansi.Reset))
	}
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func panelTunnels(sb *strings.Builder, cfg *Config, names []string, st *uiState) {
	if len(names) == 0 {
		return
	}
	focused := st != nil && st.focusPane == "tunnel"
	var active map[string]tunnelInfo
	var errs map[string]string
	if st != nil {
		active = st.snapTunnelActive
		errs = st.snapTunnelErrs
	} else {
		active, errs = loadTunnelStatuses()
	}
	title := fmt.Sprintf("tunnels %d", len(names))
	boxTopFocused(sb, title, focused)
	boxLineFocused(sb, ansi.Dim+"  NAME          TYPE     SPEC / STATE"+ansi.Reset, focused)
	boxLineFocused(sb, ansi.Dim+strings.Repeat("-", dashboardContentWidth)+ansi.Reset, focused)
	for i, n := range names {
		def := cfg.Tunnels[n]
		status := dashStatus("stopped", ansi.Dim)
		extra := ""
		errMsg := ""
		if a, ok := active[n]; ok {
			status = dashStatus("running", ansi.Green)
			extra = " listen=" + a.Listen
		} else if msg, ok := errs[n]; ok {
			status = dashStatus("failed", ansi.Red)
			errMsg = msg
		}
		flag := ""
		if def.Autostart {
			flag = " " + dashStatus("autostart", ansi.Cyan)
		}
		marker := "  "
		row := fmt.Sprintf("%s%-12s  %-7s  %s  %s%s%s",
			marker, dashName(n), ansi.Magenta+def.Type+ansi.Reset, dashPath(fitPlain(def.Spec, 32)), status, ansi.Dim+extra+ansi.Reset, flag)
		selected := st != nil && st.isSelected("tunnel", i)
		if selected {
			row = ansi.Yellow + ansi.Bold + "> " + ansi.Reset + ansi.Reverse + row[2:] + ansi.Reset
		}
		boxLineFocused(sb, row, focused)
		if errMsg != "" {
			boxLineFocused(sb, "    "+ansi.Red+fitPlain(errMsg, 76)+ansi.Reset, focused)
		}
	}
	boxBottomFocused(sb, focused)
	fmt.Fprintln(sb)
}

func panelJobs(sb *strings.Builder, jobs []*jobs.Record, st *uiState) {
	hidden := 0
	if st != nil {
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
		boxLineFocused(sb, ansi.Dim+"nothing running; `srv jobs` lists completed entries"+ansi.Reset, focused)
		boxBottomFocused(sb, focused)
		fmt.Fprintln(sb)
		return
	}
	boxLineFocused(sb, ansi.Dim+"  ID            PROFILE     PID       AGE       COMMAND"+ansi.Reset, focused)
	boxLineFocused(sb, ansi.Dim+strings.Repeat("-", dashboardContentWidth)+ansi.Reset, focused)
	for i, j := range jobs {
		cmd := fitPlain(j.Cmd, 42)
		started := j.Started
		if t, ok := parseISOLike(j.Started); ok {
			started = fmtDuration(time.Since(t)) + " ago"
		}
		row := fmt.Sprintf("  %-12s  %-10s  %-8d  %-8s  %s",
			dashName(truncID(j.ID)), ansi.Cyan+j.Profile+ansi.Reset, j.Pid, dashMeta(started), cmd)
		if st != nil && st.isSelected("job", i) {
			row = ansi.Yellow + ansi.Bold + "> " + ansi.Reset + ansi.Reverse + row[2:] + ansi.Reset
		}
		boxLineFocused(sb, row, focused)
	}
	boxBottomFocused(sb, focused)
	fmt.Fprintln(sb)
}

// panelDetail is the right-column panel: a live view of whatever the
// cursor currently highlights. Updates automatically on every
// arrow / Tab event so the user doesn't have to press Enter to
// "open" a row -- ranger / mutt / lazygit pattern. Falls back to a
// hint when nothing is selected.
func panelDetail(sb *strings.Builder, cfg *Config, jobs []*jobs.Record, tunnelNames []string, mcp mcplog.Status, st *uiState) {
	row := st.currentRow()
	switch row.kind {
	case "tunnel":
		if row.idx >= 0 && row.idx < len(tunnelNames) {
			panelTunnelDetail(sb, tunnelNames[row.idx], cfg, st)
			return
		}
	case "job":
		if row.idx >= 0 && row.idx < len(jobs) {
			panelJobDetail(sb, jobs[row.idx])
			return
		}
	case "mcp":
		if row.idx >= 0 && row.idx < len(mcp.RecentTools) {
			panelMCPDetail(sb, mcp.RecentTools[row.idx], mcp)
			return
		}
	}
	boxTop(sb, "detail")
	boxLine(sb, ansi.Dim+"(no row selected -- move cursor with ↑/↓)"+ansi.Reset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// panelMCPDetail renders one MCP tool call in the right column:
// the parsed fields (name / dur / ok-or-err / when) plus the server
// PID that handled it. We cross-reference against mcp.ActivePIDs so
// the row reads "12345 (alive)" if that MCP server is still running,
// or "12345 (previous session)" if it has since exited -- useful
// when debugging "which Claude Code instance issued this call".
func panelMCPDetail(sb *strings.Builder, tc mcplog.ToolCall, mcp mcplog.Status) {
	boxTop(sb, "mcp call detail")
	boxLine(sb, kvLine("tool", ansi.Yellow+ansi.Bold+tc.Name+ansi.Reset))
	boxLine(sb, kvLine("duration", ansi.Magenta+tc.Dur+ansi.Reset))
	status := dashStatus("ok", ansi.Green)
	if !tc.OK {
		status = dashStatus("err", ansi.Red)
	}
	boxLine(sb, kvLine("result", status))
	boxLine(sb, kvLine("when", tc.When.Format("2006-01-02 15:04:05")+dashMeta(" ("+fmtDuration(time.Since(tc.When))+" ago)")))

	pidLabel := strconv.Itoa(tc.PID)
	if tc.PID == 0 {
		pidLabel = dashMeta("(unknown)")
	} else if mcplog.PidActive(tc.PID, mcp.ActivePIDs) {
		pidLabel = ansi.Green + ansi.Bold + pidLabel + ansi.Reset + dashMeta(" (alive)")
	} else {
		pidLabel = pidLabel + dashMeta(" (previous session)")
	}
	boxLine(sb, kvLine("server pid", pidLabel))

	boxLine(sb, "")
	boxLine(sb, ansi.Dim+"raw log: ~/.srv/mcp.log"+ansi.Reset)
	boxLine(sb, ansi.Dim+"(read-only -- no actions available here)"+ansi.Reset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// pidIsActive moved to srv/internal/mcplog as mcplog.PidActive --
// reproducing the helper here is no longer necessary now that the
// caller already imports the same package.

// panelJobDetail renders job details fit for the right column.
// Same fields as the old full-screen renderJobDetail, just laid out
// against the narrower width.
func panelJobDetail(sb *strings.Builder, j *jobs.Record) {
	boxTop(sb, "job detail")
	boxLine(sb, kvLine("id", dashName(j.ID)))
	boxLine(sb, kvLine("profile", ansi.Cyan+j.Profile+ansi.Reset))
	boxLine(sb, kvLine("pid", strconv.Itoa(j.Pid)))
	started := j.Started
	if t, ok := parseISOLike(j.Started); ok {
		started = j.Started + dashMeta(" ("+fmtDuration(time.Since(t))+" ago)")
	}
	boxLine(sb, kvLine("started", started))
	if j.Cwd != "" {
		boxLine(sb, kvLine("cwd", dashPath(fitPlain(j.Cwd, dashboardContentWidth-10))))
	}
	if j.Log != "" {
		boxLine(sb, kvLine("log", dashPath(fitPlain(j.Log, dashboardContentWidth-10))))
	}
	boxLine(sb, "")
	boxLine(sb, ansi.Dim+"COMMAND:"+ansi.Reset)
	for _, line := range wrapText(j.Cmd, dashboardContentWidth-2) {
		boxLine(sb, "  "+line)
	}
	boxLine(sb, "")
	boxLine(sb, ansi.Dim+"press "+ansi.Yellow+ansi.Bold+"k"+ansi.Reset+ansi.Dim+" to kill"+ansi.Reset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// panelTunnelDetail renders tunnel details for the right
// column. Surfaces last-attempt errors prominently -- that's the
// info the user most wants when something looks "stopped" but they
// expected "running".
func panelTunnelDetail(sb *strings.Builder, name string, cfg *Config, st *uiState) {
	def := cfg.Tunnels[name]
	if def == nil {
		boxTop(sb, "tunnel detail")
		boxLine(sb, ansi.Red+"tunnel "+name+" not found in config"+ansi.Reset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	boxTop(sb, "tunnel detail")
	boxLine(sb, kvLine("name", dashName(name)))
	boxLine(sb, kvLine("type", ansi.Magenta+def.Type+ansi.Reset))
	boxLine(sb, kvLine("spec", dashPath(def.Spec)))
	boxLine(sb, kvLine("profile", ansi.Cyan+tunnelProfileLabel(def)+ansi.Reset))
	boxLine(sb, kvLine("autostart", boolLabel(def.Autostart)))
	var active map[string]tunnelInfo
	var errs map[string]string
	if st != nil {
		active = st.snapTunnelActive
		errs = st.snapTunnelErrs
	} else {
		active, errs = loadTunnelStatuses()
	}
	if a, ok := active[name]; ok {
		boxLine(sb, kvLine("state", dashStatus("running", ansi.Green)))
		boxLine(sb, kvLine("listen", dashPath(a.Listen)))
	} else if msg, ok := errs[name]; ok {
		boxLine(sb, kvLine("state", dashStatus("failed", ansi.Red)))
		boxLine(sb, "")
		boxLine(sb, ansi.Red+ansi.Bold+"ERROR:"+ansi.Reset)
		for _, line := range wrapText(msg, dashboardContentWidth-2) {
			boxLine(sb, "  "+ansi.Red+line+ansi.Reset)
		}
	} else {
		boxLine(sb, kvLine("state", dashStatus("stopped", ansi.Dim)))
	}
	boxLine(sb, "")
	boxLine(sb, ansi.Dim+"press "+ansi.Yellow+ansi.Bold+"Space"+ansi.Reset+ansi.Dim+" up/down, "+ansi.Yellow+ansi.Bold+"x"+ansi.Reset+ansi.Dim+" remove"+ansi.Reset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// panelMCP renders the MCP panel: header row with daemon state +
// optional active PIDs, then the recent tool-call list (selectable
// row by row -- focused-pane visuals + cursor matching same shape
// as Tunnels / Jobs).
func panelMCP(sb *strings.Builder, mcp mcplog.Status, st *uiState) {
	if !mcp.LogExists {
		return
	}
	focused := st != nil && st.focusPane == "mcp"
	boxTopFocused(sb, fmt.Sprintf("mcp %d", len(mcp.RecentTools)), focused)
	if len(mcp.ActivePIDs) == 0 {
		boxLineFocused(sb, kvPair("state", dashStatus("idle", ansi.Dim), "last", fmtDuration(time.Since(mcp.LastActive))+" ago"), focused)
	} else {
		pids := make([]string, 0, len(mcp.ActivePIDs))
		for _, p := range mcp.ActivePIDs {
			pids = append(pids, strconv.Itoa(p))
		}
		boxLineFocused(sb, kvPair("state", dashStatus("running", ansi.Green), "pids", strings.Join(pids, ", ")), focused)
	}
	if len(mcp.RecentTools) > 0 {
		boxLineFocused(sb, ansi.Dim+"  TOOL                  DUR      STATE    AGE"+ansi.Reset, focused)
		boxLineFocused(sb, ansi.Dim+strings.Repeat("-", dashboardContentWidth)+ansi.Reset, focused)
		for i, tc := range mcp.RecentTools {
			status := dashStatus("ok", ansi.Green)
			if !tc.OK {
				status = dashStatus("err", ansi.Red)
			}
			row := fmt.Sprintf("  %-20s  %-7s  %-7s  %s",
				ansi.Yellow+tc.Name+ansi.Reset, ansi.Magenta+tc.Dur+ansi.Reset, status,
				dashMeta(fmtDuration(time.Since(tc.When))+" ago"))
			if st != nil && st.isSelected("mcp", i) {
				row = ansi.Yellow + ansi.Bold + "> " + ansi.Reset + ansi.Reverse + row[2:] + ansi.Reset
			}
			boxLineFocused(sb, row, focused)
		}
	}
	boxBottomFocused(sb, focused)
	fmt.Fprintln(sb)
}

func panelFooter(sb *strings.Builder, st *uiState) {
	boxTop(sb, "help")
	if st == nil {
		boxLine(sb, ansi.Dim+"snapshot complete"+ansi.Reset)
	} else if st.statusMsg != "" {
		boxLine(sb, st.statusMsg)
	} else {
		focus := st.focusPane
		if focus == "" {
			focus = "none"
		}
		boxLine(sb, kvPair("focus", ansi.Yellow+ansi.Bold+strings.ToUpper(focus)+ansi.Reset, "mode", "window navigation"))
		switch focus {
		case "tunnel":
			boxLine(sb, "actions: "+ansi.Yellow+"↑/↓"+ansi.Reset+" move  "+ansi.Yellow+"space"+ansi.Reset+" up/down  "+ansi.Yellow+"x"+ansi.Reset+" remove  "+ansi.Yellow+"tab"+ansi.Reset+" next window  "+ansi.Yellow+"q"+ansi.Reset+" quit")
		case "job":
			boxLine(sb, "actions: "+ansi.Yellow+"↑/↓"+ansi.Reset+" move  "+ansi.Yellow+"k"+ansi.Reset+" kill  "+ansi.Yellow+"tab"+ansi.Reset+" next window  "+ansi.Yellow+"q"+ansi.Reset+" quit")
		case "mcp":
			boxLine(sb, "actions: "+ansi.Yellow+"↑/↓"+ansi.Reset+" move  "+ansi.Dim+"(read-only)"+ansi.Reset+"  "+ansi.Yellow+"tab"+ansi.Reset+" next window  "+ansi.Yellow+"q"+ansi.Reset+" quit")
		default:
			boxLine(sb, "actions: "+ansi.Yellow+"tab"+ansi.Reset+" choose window  "+ansi.Yellow+"r"+ansi.Reset+" refresh  "+ansi.Yellow+"q"+ansi.Reset+" quit")
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
	fmt.Fprintf(sb, "%s%s%s\n", ansi.Bold+ansi.Red, top, ansi.Reset)
	fmt.Fprintf(sb, "%s│%s %s%s%s%s%s│%s\n",
		ansi.Bold+ansi.Red, ansi.Reset,
		ansi.Bold+ansi.Red, c.title, ansi.Reset,
		strings.Repeat(" ", max(0, width-3-visualWidth(c.title))),
		ansi.Bold+ansi.Red, ansi.Reset)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		ansi.Bold+ansi.Red, ansi.Reset,
		strings.Repeat(" ", width-2),
		ansi.Bold+ansi.Red, ansi.Reset)
	for _, line := range c.body {
		pad := max(0, width-3-visualWidth(line))
		fmt.Fprintf(sb, "%s│%s %s%s%s│%s\n",
			ansi.Bold+ansi.Red, ansi.Reset,
			line, strings.Repeat(" ", pad),
			ansi.Bold+ansi.Red, ansi.Reset)
	}
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		ansi.Bold+ansi.Red, ansi.Reset,
		strings.Repeat(" ", width-2),
		ansi.Bold+ansi.Red, ansi.Reset)
	choice := ansi.Yellow + ansi.Bold + "[Y]" + ansi.Reset + " confirm    " +
		ansi.Yellow + ansi.Bold + "[N/Esc]" + ansi.Reset + " cancel"
	pad := max(0, width-3-visualWidth("[Y] confirm    [N/Esc] cancel"))
	fmt.Fprintf(sb, "%s│%s %s%s%s│%s\n",
		ansi.Bold+ansi.Red, ansi.Reset,
		choice, strings.Repeat(" ", pad),
		ansi.Bold+ansi.Red, ansi.Reset)
	fmt.Fprintf(sb, "%s%s%s\n", ansi.Bold+ansi.Red, bot, ansi.Reset)
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
// field of the jobs.Record plus the local references the user might
// need to act on it (`srv logs`, `srv kill`).
func renderJobDetail(j *jobs.Record, st *uiState) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "%sJOB DETAIL%s  %s%s%s\n",
		ansi.Bold+ansi.Magenta, ansi.Reset, ansi.Dim, j.ID, ansi.Reset)
	fmt.Fprintf(&sb, "%s%s%s\n\n", ansi.Dim, dashboardRule, ansi.Reset)

	dashField(&sb, "id", dashName(j.ID))
	dashField(&sb, "profile", ansi.Cyan+j.Profile+ansi.Reset)
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
	fmt.Fprintf(&sb, "  %sCOMMAND:%s\n", ansi.Dim, ansi.Reset)
	// Wrap the command across multiple lines so a long pipeline
	// stays visible without horizontal scrolling.
	for _, line := range wrapText(j.Cmd, 76) {
		fmt.Fprintf(&sb, "    %s\n", line)
	}
	fmt.Fprintln(&sb)

	fmt.Fprintf(&sb, "%s%s%s\n", ansi.Dim, dashboardRule, ansi.Reset)
	if st != nil && st.confirm != nil {
		renderConfirmPopup(&sb, st.confirm)
		return sb.String()
	}
	fmt.Fprintf(&sb, "Keys: %sq%s back   %sk%s kill   %ssrv logs %s -f%s tails remotely\n",
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Dim, j.ID, ansi.Reset)
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
		ansi.Bold+ansi.Magenta, ansi.Reset, ansi.Dim, name, ansi.Reset)
	fmt.Fprintf(&sb, "%s%s%s\n\n", ansi.Dim, dashboardRule, ansi.Reset)
	dashField(&sb, "name", dashName(name))
	dashField(&sb, "type", ansi.Magenta+def.Type+ansi.Reset)
	dashField(&sb, "spec", dashPath(def.Spec))
	dashField(&sb, "profile", ansi.Cyan+tunnelProfileLabel(def)+ansi.Reset)
	dashField(&sb, "autostart", boolLabel(def.Autostart))
	active, errs := loadTunnelStatuses()
	if a, ok := active[name]; ok {
		dashField(&sb, "state", dashStatus("running", ansi.Green))
		dashField(&sb, "listen", dashPath(a.Listen))
	} else if msg, ok := errs[name]; ok {
		dashField(&sb, "state", dashStatus("failed", ansi.Red))
		// Errors can be wordy ("dial profile X: ssh: handshake
		// failed: connect to ... timeout"). Show on its own block
		// rather than squeezing into one field row.
		fmt.Fprintln(&sb)
		fmt.Fprintf(&sb, "  %sERROR:%s\n", ansi.Red+ansi.Bold, ansi.Reset)
		for _, line := range wrapText(msg, 72) {
			fmt.Fprintf(&sb, "    %s%s%s\n", ansi.Red, line, ansi.Reset)
		}
	} else {
		dashField(&sb, "state", dashStatus("stopped", ansi.Dim))
	}
	fmt.Fprintln(&sb)
	fmt.Fprintf(&sb, "%s%s%s\n", ansi.Dim, dashboardRule, ansi.Reset)
	if st != nil && st.confirm != nil {
		renderConfirmPopup(&sb, st.confirm)
		return sb.String()
	}
	fmt.Fprintf(&sb, "Keys: %sq%s back   %sSpace%s toggle up/down   %sx%s remove\n",
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset)
	if st != nil && st.statusMsg != "" {
		fmt.Fprintf(&sb, "%s\n", st.statusMsg)
	}
	return sb.String()
}

func boolLabel(b bool) string {
	if b {
		return ansi.Green + ansi.Bold + "yes" + ansi.Reset
	}
	return ansi.Dim + "no" + ansi.Reset
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
var dashboardHeight = 24

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

// updateDashboardWidth re-reads srvtty.Size() and updates the
// package-level width / height vars. Returns true if either axis
// changed -- the caller uses it to schedule a full clearScreen for
// the next frame so a shrink doesn't leave stale chars behind, and
// to bump forceRedraw so the new frame is flushed even when the
// snapshot data itself is unchanged.
func updateDashboardWidth() bool {
	w, h := srvtty.Size()
	if w <= 0 {
		return false
	}
	if w < dashboardMinWidth {
		w = dashboardMinWidth
	}
	if w > dashboardMaxWidth {
		w = dashboardMaxWidth
	}
	if h <= 0 {
		h = dashboardHeight
	}
	if w == dashboardWidth && h == dashboardHeight {
		return false
	}
	dashboardWidth = w
	dashboardContentWidth = w - 4
	dashboardHeight = h
	return true
}

const dashboardRule = "========================================================================================"
const dashboardSubRule = "----------------------------------------------------------------------------------------"

func dashHeader(sb *strings.Builder, st *uiState) {
	boxTop(sb, "srv")
	boxLine(sb, ansi.Bold+ansi.Magenta+"SRV UI"+ansi.Reset+"  "+ansi.Dim+"current-shell control dashboard"+ansi.Reset)
	if st == nil {
		// Non-TTY snapshot mode: no interactive keys to advertise.
		boxLine(sb, ansi.Dim+"snapshot mode (no tty)"+ansi.Reset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	fmt.Fprintf(sb,
		"Keys: %sq%s quit  %sr%s redraw  %s↑/↓%s select  %sk%s kill\n\n",
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
	)
}

func dashSection(sb *strings.Builder, title string) {
	fmt.Fprintf(sb, "%s== %s ==%s\n", ansi.Bold+ansi.Cyan, strings.ToUpper(title), ansi.Reset)
}

func dashSectionCount(sb *strings.Builder, title string, count int) {
	fmt.Fprintf(sb, "%s== %s %s(%d)%s ==%s\n",
		ansi.Bold+ansi.Cyan, strings.ToUpper(title), ansi.Dim, count, ansi.Reset+ansi.Bold+ansi.Cyan, ansi.Reset)
}

func dashField(sb *strings.Builder, key, value string) {
	fmt.Fprintf(sb, "  %-10s %s\n", strings.ToUpper(key)+":", value)
}

func dashStatus(label, color string) string {
	return color + ansi.Bold + "[" + strings.ToUpper(label) + "]" + ansi.Reset
}

func dashName(s string) string {
	return ansi.Yellow + ansi.Bold + s + ansi.Reset
}

func dashMeta(s string) string {
	if s == "" {
		return ""
	}
	return ansi.Dim + s + ansi.Reset
}

func dashPath(s string) string {
	return ansi.Green + s + ansi.Reset
}

func dashTableHeader(sb *strings.Builder, cols ...string) {
	fmt.Fprint(sb, "  ")
	for i, col := range cols {
		if i > 0 {
			fmt.Fprint(sb, "  ")
		}
		fmt.Fprintf(sb, "%s%s%s", ansi.Dim, col, ansi.Reset)
	}
	fmt.Fprintln(sb)
	fmt.Fprintf(sb, "  %s%s%s\n", ansi.Dim, dashboardSubRule, ansi.Reset)
}

func dashActive(sb *strings.Builder, cfg *Config) {
	dashSection(sb, "Active")
	name, prof, err := ResolveProfile(cfg, "")
	if err != nil {
		dashField(sb, "state", dashStatus("no profile", ansi.Dim))
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
	dashField(sb, "target", ansi.Cyan+target+ansi.Reset)
	cwd := GetCwd(name, prof)
	dashField(sb, "cwd", dashPath(cwd))
	if pf := project.Resolve(); pf != nil {
		dashField(sb, "pinned", dashPath(pf.Path))
	}
	fmt.Fprintln(sb)
}

func dashDaemon(sb *strings.Builder) {
	dashSection(sb, "Daemon")
	conn := daemonDial(300 * time.Millisecond)
	if conn == nil {
		dashField(sb, "state", dashStatus("stopped", ansi.Dim))
		fmt.Fprintln(sb)
		return
	}
	defer conn.Close()
	resp, err := daemonCall(conn, daemonRequest{Op: "status"}, time.Second)
	if err != nil || resp == nil || !resp.OK {
		dashField(sb, "state", dashStatus("unreachable", ansi.Red))
		fmt.Fprintln(sb)
		return
	}
	dashField(sb, "state", dashStatus("running", ansi.Green))
	dashField(sb, "uptime", fmtDuration(time.Duration(resp.Uptime)*time.Second))
	dashField(sb, "pooled", strconv.Itoa(len(resp.Profiles)))
	if len(resp.Profiles) > 0 {
		dashField(sb, "profiles", ansi.Cyan+strings.Join(resp.Profiles, ", ")+ansi.Reset)
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
			dashName(n), ansi.Magenta+ansi.Bold, len(members), ansi.Reset, ansi.Cyan+strings.Join(members, ", ")+ansi.Reset)
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
		status := dashStatus("stopped", ansi.Dim)
		extra := ""
		var errMsg string
		if a, ok := active[n]; ok {
			status = dashStatus("running", ansi.Green)
			extra = "  listen=" + a.Listen
		} else if msg, ok := errs[n]; ok {
			status = dashStatus("failed", ansi.Red)
			errMsg = msg
		}
		flag := ""
		if def.Autostart {
			flag = " " + dashStatus("autostart", ansi.Cyan)
		}
		if extra != "" {
			extra = ansi.Dim + extra + ansi.Reset
		}
		marker := "   "
		selected := st != nil && st.isSelected("tunnel", i)
		if selected {
			marker = ansi.Bold + ansi.Yellow + " > " + ansi.Reset
		}
		row := fmt.Sprintf("%-12s  %-7s  %s  %s%s%s",
			dashName(n), ansi.Magenta+def.Type+ansi.Reset, dashPath(def.Spec), status, extra, flag)
		if selected {
			fmt.Fprintf(sb, "%s%s%s%s\n", marker, ansi.Reverse, row, ansi.Reset)
		} else {
			fmt.Fprintf(sb, "%s%s\n", marker, row)
		}
		if errMsg != "" {
			// Indent under the row so it groups visually; truncate
			// to keep the table tight.
			line := truncOneLine(errMsg, 70)
			fmt.Fprintf(sb, "      %s%s%s\n", ansi.Red, line, ansi.Reset)
		}
	}
	fmt.Fprintln(sb)
}

// dashJobs renders the jobs table. When `st` is non-nil and the
// caller has a valid selection (st.selectedJob in range), the
// matching row gets a `>` marker + reverse video so the user can
// see what their next k will target.
//
// If the active filter (default) hid completed jobs, the section
// header gets a "(N hidden)" tail so the user knows to consult
// `srv jobs` for the full list.
func dashJobs(sb *strings.Builder, jobs []*jobs.Record, st *uiState) {
	hidden := 0
	if st != nil {
		hidden = st.hiddenJobs
	}
	if len(jobs) == 0 && hidden == 0 {
		return
	}
	if hidden > 0 {
		fmt.Fprintf(sb, "%s== JOBS %s(%d, %d completed hidden -- see %ssrv jobs%s%s)%s ==%s\n",
			ansi.Bold+ansi.Cyan, ansi.Dim, len(jobs), hidden,
			ansi.Yellow+ansi.Bold, ansi.Reset+ansi.Dim, "",
			ansi.Reset+ansi.Bold+ansi.Cyan, ansi.Reset)
	} else {
		dashSectionCount(sb, "Jobs", len(jobs))
	}
	if len(jobs) == 0 {
		fmt.Fprintf(sb, "  %s(nothing running)%s\n\n", ansi.Dim, ansi.Reset)
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
			marker = ansi.Bold + ansi.Yellow + " > " + ansi.Reset
		}
		row := fmt.Sprintf("%-12s  %-10s  %-8d  %-8s  %s",
			dashName(truncID(j.ID)), ansi.Cyan+j.Profile+ansi.Reset, j.Pid, dashMeta(started), cmd)
		if selected {
			// Reverse-video the row content so the selection is
			// readable on terminals that drop the cursor marker.
			fmt.Fprintf(sb, "%s%s%s%s\n", marker, ansi.Reverse, row, ansi.Reset)
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
	st := mcplog.Read()
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
			dashField(sb, "state", dashStatus("idle", ansi.Dim))
			dashField(sb, "last", fmtDuration(time.Since(st.LastActive))+" ago")
		} else {
			dashField(sb, "state", dashStatus("idle", ansi.Dim))
		}
	} else {
		pids := make([]string, 0, len(st.ActivePIDs))
		for _, p := range st.ActivePIDs {
			pids = append(pids, strconv.Itoa(p))
		}
		dashField(sb, "state", dashStatus("running", ansi.Green))
		dashField(sb, "pids", strings.Join(pids, ", "))
	}
	if len(st.RecentTools) > 0 {
		fmt.Fprintln(sb)
		dashTableHeader(sb, "TOOL                  DUR      STATE    AGE")
		for _, tc := range st.RecentTools {
			status := dashStatus("ok", ansi.Green)
			if !tc.OK {
				status = dashStatus("err", ansi.Red)
			}
			age := dashMeta(fmtDuration(time.Since(tc.When)) + " ago")
			fmt.Fprintf(sb, "  %-20s  %-7s  %-7s  %s\n", ansi.Yellow+tc.Name+ansi.Reset, ansi.Magenta+tc.Dur+ansi.Reset, status, age)
		}
	}
	fmt.Fprintln(sb)
}

func dashFooter(sb *strings.Builder, st *uiState) {
	fmt.Fprintf(sb, "%s%s%s\n", ansi.Dim, dashboardRule, ansi.Reset)
	if st == nil {
		fmt.Fprintf(sb, "%ssnapshot complete%s\n", ansi.Dim, ansi.Reset)
		return
	}
	// Confirm popup renders elsewhere (centered box appended to the
	// dashboard); the footer only needs the regular key hints.
	fmt.Fprintf(sb, "Keys: %sq%s quit  %sr%s redraw  %s↑/↓%s move  %sk%s kill\n",
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
	)
	if st.statusMsg != "" {
		fmt.Fprintf(sb, "%s\n", st.statusMsg)
	}
}

// parseISOLike accepts the timestamp formats srv writes -- srvutil.NowISO()
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
	// srvutil.NowISO() writes local wall-clock without a tz suffix. Parse in
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
