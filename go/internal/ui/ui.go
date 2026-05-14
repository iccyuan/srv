package ui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"srv/internal/ansi"
	"srv/internal/clierr"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/history"
	"srv/internal/jobs"
	"srv/internal/mcplog"
	"srv/internal/project"
	"srv/internal/remote"
	"srv/internal/srvpath"
	"srv/internal/srvtty"
	"srv/internal/tunnel"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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
	detailScroll int        // first visible body row in the right-side DETAIL panel
	detailMode   bool       // showing the per-row detail panel
	confirm      *uiConfirm // non-nil = popup is up, awaiting Y/N
	statusMsg    string     // transient line in the footer (kill result, etc.)
	demoMode     bool       // true when rendering/handling the built-in demo dataset
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
	hiddenJobs    int       // filtered-out exited-jobs count for the title hint
	statusSetAt   time.Time // wall-clock when statusMsg was last set
	// Snapshot of disk-backed and daemon-backed state. Refreshed on
	// tick or after a destructive action; every other iteration of
	// the render loop reads from these fields instead of re-doing
	// the I/O. Without this cache every poll ticks costs roughly:
	//
	//	~10ms  config.GetCwd / session.Touch (sessions.json read + write)
	//	~5ms   resolveProjectFile (cwd walk + stat)
	//	~5ms   panelDaemon daemon.DialSock + status RPC
	//	~5ms   panelTunnels loadTunnelStatuses
	//	~5ms   panelTunnelDetail loadTunnelStatuses (again)
	//
	// On a 150ms poll that's ~20% CPU just to redraw "nothing
	// changed". With the cache idle redraws cost ~1ms (string build).
	snapCfg          *config.Config
	snapJobs         []*jobs.Record
	snapMCP          mcplog.Status
	snapDaemonResp   *daemon.Response
	snapTunnelActive map[string]daemon.TunnelInfo
	snapTunnelErrs   map[string]string
	snapAt           time.Time
	// helpVisible toggles the `?` help overlay. While true every key
	// dismisses the overlay rather than triggering its normal action.
	helpVisible bool
	// historyVisible toggles the `H` history overlay (recent CLI
	// commands from internal/history). Up/Down scroll within the
	// popup; Esc / any other key closes.
	historyVisible bool
	historyScroll  int
	// filterMode + filterQuery implement the `/` job-filter modal.
	// In filterMode every printable byte appends to filterQuery,
	// backspace pops one rune, Enter commits, Esc clears and exits.
	// Outside filterMode a non-empty filterQuery still filters the
	// JOBS panel so the user can keep typing-then-navigating.
	filterMode  bool
	filterQuery string
	// jobLog holds the most recently fetched job log preview. The L
	// key on a job row spawns a goroutine that drops the result on
	// jobLogCh; the main loop drains it on each idle tick. Only one
	// fetch is in flight at a time -- subsequent L presses overwrite.
	jobLog   *uiJobLog
	jobLogCh chan *uiJobLog
}

// uiJobLog is the in-memory cache of a job's last-N log lines. Lives
// on uiState.jobLog; nil when the user has not pressed L yet.
type uiJobLog struct {
	jobID     string
	lines     []string
	err       string
	fetchedAt time.Time
	fetching  bool
}

// persistedUIState is the JSON shape written to ~/.srv/ui-state.json.
// Only the fields whose value the user might reasonably want carried
// across `srv ui` invocations are stored -- the cached snapshots,
// transient popups, and goroutine handles are session-local.
type persistedUIState struct {
	FocusPane    string `json:"focus_pane,omitempty"`
	Cursor       int    `json:"cursor,omitempty"`
	TunnelCursor int    `json:"tunnel_cursor,omitempty"`
	JobCursor    int    `json:"job_cursor,omitempty"`
	McpCursor    int    `json:"mcp_cursor,omitempty"`
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

// Cmd is `srv ui` -- a one-screen dashboard showing the bits of srv
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
//	k             kill the selected job (arms a Y/N confirm)
func Cmd(args []string, cfg *config.Config) error {
	demo := uiDemoMode(args)
	if !srvtty.IsStdinTTY() {
		// Without a TTY there's no way to read keys; degrade to a
		// one-shot print of the snapshot so `srv ui | less` still
		// works (or piped into a script). Jobs are still listed --
		// just without the selection markers and key hints.
		if demo {
			demoCfg, demoJobs, demoMCP := demoDashboardData(cfg)
			fmt.Print(renderDashboardWithMCP(demoCfg, demoJobs, sortedTunnelNames(demoCfg), nil, demoMCP))
			return nil
		}
		fmt.Print(renderDashboard(cfg, currentJobs(), sortedTunnelNames(cfg), nil))
		return nil
	}
	fd := int(os.Stdin.Fd())
	state, err := term.MakeRaw(fd)
	if err != nil {
		return clierr.Errf(1, "tty raw mode: %v", err)
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

	kr := srvtty.NewKeyReader()
	st := &uiState{
		forceRedraw: true,
		cursor:      0,
		liveness:    map[string]bool{},
		demoMode:    demo,
		jobLogCh:    make(chan *uiJobLog, 4),
	}
	// Persisted cursor / focus is loaded only outside demo mode. Demo
	// is meant to be a clean reproducible screenshot; leaking last
	// session's "selected MCP row 7" into demo screenshots would be
	// surprising.
	if !demo {
		applyPersistedState(st, loadUIState())
		defer saveUIState(st)
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
		if demo {
			demoCfg, demoJobs, demoMCP := demoDashboardData(cfg)
			st.snapCfg = demoCfg
			st.snapJobs = demoJobs
			st.snapMCP = demoMCP
			st.snapDaemonResp = &daemon.Response{OK: true, Profiles: []string{"美国备用", "tokyo-demo"}, Uptime: int64(2 * time.Hour / time.Second)}
			st.snapTunnelActive = map[string]daemon.TunnelInfo{}
			st.snapTunnelErrs = map[string]string{}
			st.snapAt = time.Now()
		} else if st.snapCfg == nil || st.snapAt.IsZero() || time.Since(st.snapAt) > snapTTL {
			if fresh, _ := config.Load(); fresh != nil {
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
			st.snapTunnelActive, st.snapTunnelErrs = tunnel.LoadStatuses()
			st.snapAt = time.Now()
		}
		fresh := st.snapCfg
		if fresh == nil {
			fresh = cfg
		}
		allJobs := st.snapJobs

		// Liveness: refresh from the remote whenever it's gone stale.
		// One SSH per profile, batched, so the cost is bounded even
		// with many jobs. Manual `r` zeroes livenessFresh below so the
		// probe re-runs; resize-triggered forceRedraw deliberately does
		// NOT re-probe — otherwise a window resize would block the
		// repaint on SSH round-trips and the dashboard would freeze on
		// the old (clipped) frame for seconds.
		if demo {
			st.liveness = map[string]bool{}
			for _, j := range allJobs {
				st.liveness[j.ID] = true
			}
			st.livenessFresh = time.Now()
		} else if time.Since(st.livenessFresh) > livenessTTL {
			// Wrap remoteExitMarkers as an ExitMarkerLister so the
			// jobs package stays decoupled from Config / SSH client.
			lister := func(profName string) (map[string]bool, bool) {
				prof, ok := fresh.Profiles[profName]
				if !ok {
					return nil, false
				}
				capture := func(cmd string) (string, int, bool) {
					res, err := remote.RunCapture(prof, "", cmd)
					if err != nil || res == nil {
						return "", 0, false
					}
					return res.Stdout, res.ExitCode, true
				}
				markers := jobs.RemoteExitMarkers(capture)
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
		// `/` filter narrows the JOBS list to rows whose ID or Cmd
		// contains the query substring (case-insensitive). Applied
		// here so st.rows / cursor bookkeeping below see the filtered
		// view directly -- panelJobs then renders exactly what's
		// selectable.
		jobs = filterJobs(jobs, st.filterQuery)
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
			fmt.Fprint(os.Stderr, prefix+altScreenFrame(out, dashboardHeight)+clearEnd)
			st.lastFrame = out
			st.forceRedraw = false
		}

		b, ok := kr.ReadWithTimeout(pollEvery)
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
			if drainJobLogCh(st) {
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
func sortedTunnelNames(cfg *config.Config) []string {
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
// loadUIState reads ~/.srv/ui-state.json and returns the persisted
// pane / cursor positions. A missing or malformed file is silently
// treated as "start from defaults" -- the user shouldn't have to
// delete the file just to recover from a bad write.
func loadUIState() persistedUIState {
	data, err := os.ReadFile(srvpath.UIState())
	if err != nil {
		return persistedUIState{}
	}
	var s persistedUIState
	if json.Unmarshal(data, &s) != nil {
		return persistedUIState{}
	}
	return s
}

// saveUIState writes the per-pane cursor positions back to
// ~/.srv/ui-state.json. Errors are swallowed -- the UI shouldn't fail
// to exit just because the state file is on a read-only filesystem
// or the disk is full. The state is purely a navigation convenience.
func saveUIState(st *uiState) {
	if st == nil {
		return
	}
	s := persistedUIState{
		FocusPane:    st.focusPane,
		Cursor:       st.cursor,
		TunnelCursor: st.tunnelCursor,
		JobCursor:    st.jobCursor,
		McpCursor:    st.mcpCursor,
	}
	data, err := json.Marshal(s)
	if err != nil {
		return
	}
	path := srvpath.UIState()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(path, data, 0o644)
}

// applyPersistedState seeds st with the saved cursor positions. Bounds
// are not validated here -- clampCursor runs immediately after on the
// first frame and brings stale indices back into range.
func applyPersistedState(st *uiState, s persistedUIState) {
	if s.FocusPane != "" {
		st.focusPane = s.FocusPane
	}
	if s.Cursor != 0 {
		st.cursor = s.Cursor
	}
	st.tunnelCursor = s.TunnelCursor
	st.jobCursor = s.JobCursor
	st.mcpCursor = s.McpCursor
}

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
	if st.focusPane != avail[next] {
		st.detailScroll = 0
	}
	st.focusPane = avail[next]
	clampCursor(st)
	st.forceRedraw = true
}

// moveFocusedRow moves the cursor inside the focused pane only.
// Arrow keys wrap within that pane; Tab / h / l are the only keys
// that switch panes.
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
	size := sizes[st.focusPane]
	prev := *cur
	if size > 0 {
		*cur = (*cur + delta + size) % size
	}
	if *cur != prev {
		st.detailScroll = 0
	}
	clampCursor(st)
	st.forceRedraw = true
}

func scrollDetail(st *uiState, delta int) {
	st.detailScroll += delta
	if st.detailScroll < 0 {
		st.detailScroll = 0
	}
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
// Precedence (top wins):
//   - help overlay: any key dismisses it (so `?` is a true toggle).
//   - filter input mode: every byte feeds the filter buffer.
//   - confirmation popup: only Y / N / Esc are meaningful.
//   - normal dashboard navigation.
func handleUIKey(b byte, st *uiState, jobs []*jobs.Record, tunnelNames []string, cfg *config.Config, kr *srvtty.KeyReader) bool {
	if st.helpVisible {
		// Any key dismisses the help screen. Ctrl-C / q still quit so
		// the user isn't trapped if they hit the overlay by accident
		// and want to abort.
		st.helpVisible = false
		st.forceRedraw = true
		if b == 'q' || b == '\x03' {
			return false
		}
		return true
	}
	if st.historyVisible {
		// Any key dismisses; q / Ctrl-C still quits. Up/Down scroll the
		// list when there's more than a screenful.
		switch b {
		case 'q', '\x03':
			return false
		case 0x1b: // Esc / arrow prefix
			// Drain a possible arrow-key sequence so it doesn't leak
			// into the next render. Plain Esc closes the overlay.
			if next, ok := kr.ReadWithTimeout(20 * time.Millisecond); ok && next == '[' {
				if dir, ok := kr.ReadWithTimeout(20 * time.Millisecond); ok {
					switch dir {
					case 'A':
						if st.historyScroll > 0 {
							st.historyScroll--
						}
					case 'B':
						st.historyScroll++
					}
					st.forceRedraw = true
					return true
				}
			}
			st.historyVisible = false
			st.forceRedraw = true
			return true
		case 'j':
			st.historyScroll++
			st.forceRedraw = true
			return true
		case 'k':
			if st.historyScroll > 0 {
				st.historyScroll--
			}
			st.forceRedraw = true
			return true
		}
		st.historyVisible = false
		st.forceRedraw = true
		return true
	}
	if st.filterMode {
		return handleFilterKey(b, st, kr)
	}
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
		st.snapAt = time.Time{}        // manual refresh -- bypass snapTTL
		st.livenessFresh = time.Time{} // also re-probe SSH liveness
	case '?':
		st.helpVisible = true
		st.forceRedraw = true
	case 'H':
		// Capital H reserved for the history overlay so lowercase 'h'
		// keeps its established "move focus left" meaning.
		st.historyVisible = true
		st.historyScroll = 0
		st.forceRedraw = true
	case '/':
		st.filterMode = true
		st.forceRedraw = true
	case 'L':
		armJobLogFetch(st, row, jobs, cfg)
	case '\x15': // Ctrl-U
		scrollDetail(st, -5)
	case '\x04': // Ctrl-D
		scrollDetail(st, 5)
	case '\t', 'l':
		focusNextPane(st)
	case 'h':
		focusPrevPane(st)
	case 'k', ' ', 'x':
		armConfirmFor(st, row, jobs, tunnelNames, cfg, b)
	case 0x1b: // ESC -- possibly an arrow-key sequence
		b2, ok := kr.ReadWithTimeout(80 * time.Millisecond)
		if !ok {
			return false // bare ESC = quit
		}
		if b2 != '[' {
			return true
		}
		b3, ok := kr.ReadWithTimeout(20 * time.Millisecond)
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
		case '5', '6':
			if _, ok := kr.ReadWithTimeout(20 * time.Millisecond); ok {
				if b3 == '5' {
					scrollDetail(st, -5)
				} else {
					scrollDetail(st, 5)
				}
			}
		}
	}
	return true
}

// handleFilterKey routes keys while the `/` filter modal is active.
// Enter commits the query, Esc clears+exits, Backspace pops a rune,
// any other printable byte appends. Arrow keys / Tab fall through to
// nav: the user usually types a query then walks the filtered list,
// so we don't want them to first press Enter every time.
func handleFilterKey(b byte, st *uiState, kr *srvtty.KeyReader) bool {
	switch b {
	case '\r', '\n':
		// Commit: leave filter mode but keep the query so the list
		// stays narrowed while the user navigates.
		st.filterMode = false
		st.forceRedraw = true
	case '\x1b': // Esc
		// On ESC we have to disambiguate "bare Esc = clear filter" vs
		// "Esc [ A = up arrow that bubbles into nav". Same 80ms probe
		// the main handler uses.
		b2, ok := kr.ReadWithTimeout(80 * time.Millisecond)
		if !ok {
			st.filterMode = false
			st.filterQuery = ""
			st.forceRedraw = true
			return true
		}
		if b2 == '[' {
			// Swallow the rest of the CSI sequence so it doesn't get
			// re-injected as nav while we're still in filter mode.
			kr.ReadWithTimeout(20 * time.Millisecond)
			return true
		}
		st.filterMode = false
		st.filterQuery = ""
		st.forceRedraw = true
	case '\x7f', '\b':
		if r, size := lastRune(st.filterQuery); size > 0 {
			_ = r
			st.filterQuery = st.filterQuery[:len(st.filterQuery)-size]
			st.forceRedraw = true
		}
	default:
		if b >= 0x20 && b < 0x7f {
			st.filterQuery += string(rune(b))
			st.forceRedraw = true
		}
	}
	return true
}

func lastRune(s string) (rune, int) {
	if s == "" {
		return 0, 0
	}
	r, size := utf8.DecodeLastRuneInString(s)
	return r, size
}

// armJobLogFetch kicks off an asynchronous fetch of the selected job's
// last N log lines. The goroutine sends the populated uiJobLog onto
// st.jobLogCh; the main loop drains the channel each idle tick.
//
// Synchronously fetching here would block the UI for the duration of
// the SSH round-trip (~100ms with a warm daemon pool, multiple seconds
// when re-dialling cold). The user pressing L on the wrong row
// shouldn't freeze the dashboard.
func armJobLogFetch(st *uiState, row uiRow, js []*jobs.Record, cfg *config.Config) {
	if row.kind != "job" || row.idx < 0 || row.idx >= len(js) {
		return
	}
	j := js[row.idx]
	st.jobLog = &uiJobLog{jobID: j.ID, fetching: true}
	st.forceRedraw = true
	if st.demoMode {
		// Inject a few fake lines so the panel layout is exercisable
		// in screenshots / tests without dialling a real remote.
		ch := st.jobLogCh
		go func() {
			lines := []string{
				"[demo] " + j.Cmd,
				"[demo] starting...",
				"[demo] ok 1/5",
				"[demo] ok 2/5",
				"[demo] (live fetch suppressed in demo mode)",
			}
			ch <- &uiJobLog{jobID: j.ID, lines: lines, fetchedAt: time.Now()}
		}()
		return
	}
	if j.Log == "" {
		ch := st.jobLogCh
		go func() {
			ch <- &uiJobLog{jobID: j.ID, err: "no log path recorded for this job", fetchedAt: time.Now()}
		}()
		return
	}
	profile := j.Profile
	logPath := j.Log
	cmd := "tail -n 200 " + shellQuote(logPath)
	ch := st.jobLogCh
	go func() {
		res, ok := daemon.TryRunCapture(profile, "", cmd)
		out := &uiJobLog{jobID: j.ID, fetchedAt: time.Now()}
		if !ok {
			out.err = "daemon unavailable"
			ch <- out
			return
		}
		body := res.Stdout
		if body == "" {
			body = res.Stderr
		}
		body = strings.TrimRight(body, "\n")
		if body == "" {
			out.err = "log is empty"
		} else {
			out.lines = strings.Split(body, "\n")
		}
		ch <- out
	}()
}

// shellQuote wraps s in single quotes so it survives unquoted shell
// interpretation. Embedded single quotes are escaped with the
// `'\”` idiom; we don't try to handle the (impossible) case of a
// path containing a literal single quote AND a backslash run that
// would need word-splitting.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// drainJobLogCh non-blockingly pulls any pending log results into
// st.jobLog. Called by the main loop's idle tick so a slow fetch
// doesn't have to coincide with a keypress to land on screen.
func drainJobLogCh(st *uiState) bool {
	if st == nil || st.jobLogCh == nil {
		return false
	}
	got := false
	for {
		select {
		case res := <-st.jobLogCh:
			if res == nil {
				continue
			}
			// Only adopt the result when it matches the currently
			// requested job. The user may have moved on by the time
			// the SSH call returned; honouring a stale result would
			// flash the wrong log into the panel.
			if st.jobLog != nil && st.jobLog.jobID == res.jobID {
				st.jobLog = res
				got = true
			}
		default:
			return got
		}
	}
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
func armConfirmFor(st *uiState, row uiRow, jobs []*jobs.Record, tunnelNames []string, cfg *config.Config, key byte) {
	switch row.kind {
	case "job":
		if key != 'k' || row.idx < 0 || row.idx >= len(jobs) {
			return
		}
		j := jobs[row.idx]
		action := func() (string, error) { return uiKillJob(j, cfg) }
		if st.demoMode {
			action = func() (string, error) { return "demo kill acknowledged; no process changed", nil }
		}
		st.confirm = &uiConfirm{
			title: "kill " + j.ID,
			body: []string{
				j.ID + "  (" + j.Profile + ", pid " + strconv.Itoa(j.Pid) + ")",
				truncOneLine(j.Cmd, 60),
				"",
				"Send SIGTERM to the remote pid and drop the local jobs.json entry.",
			},
			action: action,
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
		active := tunnel.LoadActive()
		if st.demoMode {
			active = st.snapTunnelActive
		}
		_, isUp := active[name]
		switch key {
		case ' ':
			action := func() (string, error) { return uiTunnelDown(name) }
			if st.demoMode {
				action = func() (string, error) { return "demo tunnel stopped; no daemon changed", nil }
			}
			if isUp {
				st.confirm = &uiConfirm{
					title: "tunnel down " + name,
					body: []string{
						name + "  (" + def.Type + " " + def.Spec + ")",
						"",
						"Stop the daemon-hosted listener. Existing connections drop.",
					},
					action: action,
				}
			} else {
				action := func() (string, error) { return uiTunnelUp(name) }
				if st.demoMode {
					action = func() (string, error) { return "demo tunnel started; no daemon changed", nil }
				}
				st.confirm = &uiConfirm{
					title: "tunnel up " + name,
					body: []string{
						name + "  (" + def.Type + " " + def.Spec + ", profile " + tunnelProfileLabel(def) + ")",
						"",
						"Bring the tunnel up via the daemon.",
					},
					action: action,
				}
			}
			st.forceRedraw = true
		case 'x':
			extra := ""
			if isUp {
				extra = " The currently-running tunnel will be stopped first."
			}
			action := func() (string, error) { return uiTunnelRemove(name, cfg) }
			if st.demoMode {
				action = func() (string, error) { return "demo tunnel removed; no config changed", nil }
			}
			st.confirm = &uiConfirm{
				title: "remove tunnel " + name,
				body: []string{
					name + "  (" + def.Type + " " + def.Spec + ")",
					"",
					"Delete the saved definition from config." + extra,
				},
				action: action,
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
func tunnelProfileLabel(def *config.TunnelDef) string {
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
	if !daemon.Ensure() {
		return "", fmt.Errorf("daemon unavailable")
	}
	conn := daemon.DialSock(2 * time.Second)
	if conn == nil {
		return "", fmt.Errorf("daemon unreachable")
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_up", Name: name}, 10*time.Second)
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
	conn := daemon.DialSock(2 * time.Second)
	if conn == nil {
		return "", fmt.Errorf("daemon not running")
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "tunnel_down", Name: name}, 5*time.Second)
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
// removal on their next config.Load.
func uiTunnelRemove(name string, cfg *config.Config) (string, error) {
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
	if err := config.Save(cfg); err != nil {
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
func uiKillJob(j *jobs.Record, cfg *config.Config) (string, error) {
	prof, ok := cfg.Profiles[j.Profile]
	if !ok {
		return "", fmt.Errorf("profile %q not found", j.Profile)
	}
	cmd := fmt.Sprintf("kill -TERM %d 2>/dev/null && echo killed || echo 'no such pid'", j.Pid)
	res, err := remote.RunCapture(prof, "", cmd)
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

func altScreenFrame(content string, height int) string {
	lines := splitDashboardLines(content)
	if height > 0 && len(lines) > height {
		lines = lines[:height]
	}
	if len(lines) == 0 {
		return ""
	}
	return strings.Join(lines, "\x1b[K\n") + "\x1b[K"
}

func uiDemoMode(args []string) bool {
	if os.Getenv("SRV_UI_DEMO") == "1" {
		return true
	}
	for _, a := range args {
		switch a {
		case "demo", "--demo":
			return true
		}
	}
	return false
}

func demoDashboardData(base *config.Config) (*config.Config, []*jobs.Record, mcplog.Status) {
	cfg := config.New()
	if base != nil {
		cfg.Lang = base.Lang
		cfg.Hints = base.Hints
	}
	cfg.DefaultProfile = "美国备用"
	cfg.Profiles = map[string]*config.Profile{
		"美国备用":       {Host: "backup.example.com", User: "deploy", DefaultCwd: "/srv/demo"},
		"美国服务":       {Host: "service.example.com", User: "ops", DefaultCwd: "/data/app"},
		"tokyo-demo": {Host: "tokyo.example.com", User: "dev", DefaultCwd: "/workspace"},
	}
	cfg.Groups = map[string][]string{
		"演示组": {"美国备用", "美国服务", "tokyo-demo"},
	}
	cfg.Tunnels = map[string]*config.TunnelDef{
		"数据库": {Type: "local", Spec: "15432:db.internal:5432", Profile: "美国备用"},
		"监控":  {Type: "remote", Spec: "19090:127.0.0.1:9090", Profile: "tokyo-demo", Autostart: true},
	}

	now := time.Now()
	demoJobs := []*jobs.Record{
		{ID: "demo-job-0001", Profile: "美国备用", Pid: 31001, Started: now.Add(-42 * time.Minute).Format("2006-01-02T15:04:05"), Cmd: "npm run build -- --watch"},
		{ID: "demo-job-0002", Profile: "美国服务", Pid: 31002, Started: now.Add(-37 * time.Minute).Format("2006-01-02T15:04:05"), Cmd: "python worker.py --queue default"},
		{ID: "demo-job-0003", Profile: "tokyo-demo", Pid: 31003, Started: now.Add(-29 * time.Minute).Format("2006-01-02T15:04:05"), Cmd: "go test ./..."},
		{ID: "demo-job-0004", Profile: "美国备用", Pid: 31004, Started: now.Add(-21 * time.Minute).Format("2006-01-02T15:04:05"), Cmd: "tail -f /var/log/app.log"},
		{ID: "demo-job-0005", Profile: "美国服务", Pid: 31005, Started: now.Add(-13 * time.Minute).Format("2006-01-02T15:04:05"), Cmd: "docker compose up api"},
		{ID: "demo-job-0006", Profile: "tokyo-demo", Pid: 31006, Started: now.Add(-8 * time.Minute).Format("2006-01-02T15:04:05"), Cmd: "bash scripts/deploy.sh staging"},
		{ID: "demo-job-0007", Profile: "美国备用", Pid: 31007, Started: now.Add(-3 * time.Minute).Format("2006-01-02T15:04:05"), Cmd: "sleep 300"},
	}

	tools := []mcplog.ToolCall{
		{When: now.Add(-16 * time.Minute), Name: "read_file", Dur: "42ms", OK: true, PID: 4242},
		{When: now.Add(-14 * time.Minute), Name: "search", Dur: "118ms", OK: true, PID: 4242},
		{When: now.Add(-12 * time.Minute), Name: "go_test", Dur: "1.8s", OK: true, PID: 4242},
		{When: now.Add(-10 * time.Minute), Name: "shell", Dur: "250ms", OK: true, PID: 4242},
		{When: now.Add(-8 * time.Minute), Name: "apply_patch", Dur: "96ms", OK: true, PID: 4242},
		{When: now.Add(-6 * time.Minute), Name: "ui_snapshot", Dur: "77ms", OK: true, PID: 4242},
		{When: now.Add(-4 * time.Minute), Name: "build", Dur: "2.4s", OK: true, PID: 4242},
		{When: now.Add(-2 * time.Minute), Name: "mcp_error_demo", Dur: "30ms", OK: false, PID: 4242},
	}
	demoMCP := mcplog.Status{
		LogPath:     "demo://srv-ui",
		LogExists:   true,
		ActivePIDs:  []int{4242},
		LastActive:  now.Add(-2 * time.Minute),
		RecentTools: tools,
	}
	return cfg, demoJobs, demoMCP
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
func renderDashboard(cfg *config.Config, jobs []*jobs.Record, tunnelNames []string, st *uiState) string {
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
	return renderDashboardWithMCP(cfg, jobs, tunnelNames, st, mcpSnapshot)
}

func renderDashboardWithMCP(cfg *config.Config, jobs []*jobs.Record, tunnelNames []string, st *uiState, mcpSnapshot mcplog.Status) string {
	var sb strings.Builder
	panelHeader(&sb, st)
	panelProfiles(&sb, cfg, st)
	if st == nil || dashboardWidth < dashboardTwoColumnMinWidth {
		panelOverview(&sb, cfg, jobs, tunnelNames, mcpSnapshot, st)
		panelDaemon(&sb, st)
		panelTunnels(&sb, cfg, tunnelNames, st)
		panelJobs(&sb, jobs, st)
		panelMCP(&sb, mcpSnapshot, st)
		if st != nil {
			panelStatus(&sb, st)
			panelFooter(&sb, st)
		}
	} else {
		leftW, rightW, gap := splitColumnsWidth(dashboardWidth)
		// The DETAIL panel's `╰─╯` is anchored to the bottom of the
		// JOBS panel: rendering the left column in two stages lets us
		// measure exactly how tall OVERVIEW + DAEMON + TUNNELS + JOBS
		// are together, and target the detail panel to that height so
		// the two columns close on the same row. MCP + FOOTER follow
		// below on the left while the right side is just plain blank
		// space (writeSideBySide pads with spaces, no floating walls).
		var leftTop, leftBot strings.Builder
		withDashboardWidth(leftW, func() {
			panelOverview(&leftTop, cfg, jobs, tunnelNames, mcpSnapshot, st)
			panelDaemon(&leftTop, st)
			panelTunnels(&leftTop, cfg, tunnelNames, st)
			panelJobs(&leftTop, jobs, st)
			panelMCP(&leftBot, mcpSnapshot, st)
			panelStatus(&leftBot, st)
			panelFooter(&leftBot, st)
		})
		topLines := splitDashboardLines(leftTop.String())
		// DETAIL is anchored exactly to leftTop: its `╭─╮` lines up
		// with OVERVIEW's `╭─╮` (both start on the same row of the
		// side-by-side block) and its `╰─╯` lines up with JOBS' `╰─╯`
		// (both close on the last row of leftTop). On terminals too
		// short to fit leftTop, the bottom edges land below the visible
		// frame together -- that's a terminal-size problem, not a
		// layout one.
		var rightBuf strings.Builder
		withDashboardWidth(rightW, func() {
			renderStretchedDetail(&rightBuf, cfg, jobs, tunnelNames, mcpSnapshot, st, len(topLines))
		})
		writeSideBySide(&sb, leftTop.String(), rightBuf.String(), leftW, rightW, gap)
		// MCP + FOOTER stack underneath the side-by-side block, at the
		// left column's width. The right side of those rows stays
		// genuinely empty so the panels naturally hang off the bottom
		// of the now-closed DETAIL column without re-opening any
		// border walls.
		sb.WriteString(leftBot.String())
	}

	panelGroups(&sb, cfg)
	if st == nil {
		panelFooter(&sb, st)
	}
	out := sb.String()
	if st != nil && st.historyVisible {
		return overlayHistoryPopup(out, st)
	}
	if st != nil && st.helpVisible {
		return overlayHelpPopup(out, st)
	}
	if st != nil && st.confirm != nil {
		return overlayConfirmPopup(out, st.confirm)
	}
	return out
}

// overlayHelpPopup renders the `?` help screen as a centred overlay.
// Same transparent-outside-the-box scheme overlayConfirmPopup uses:
// cells to the left / right of the help box are preserved from the
// underlying dashboard so the user sees what the help is talking
// about.
func overlayHelpPopup(content string, st *uiState) string {
	lines := splitDashboardLines(content)
	visibleHeight := len(lines)
	if dashboardHeight > 0 {
		visibleHeight = dashboardHeight
	}
	if visibleHeight < 1 {
		visibleHeight = 1
	}
	for len(lines) < visibleHeight {
		lines = append(lines, strings.Repeat(" ", dashboardWidth))
	}

	var popup strings.Builder
	renderHelpPopup(&popup, st)
	popupLines := splitDashboardLines(popup.String())
	if len(popupLines) == 0 {
		return content
	}
	if len(popupLines) > visibleHeight {
		popupLines = popupLines[:visibleHeight]
	}
	start := (visibleHeight - len(popupLines)) / 2
	if start < 0 {
		start = 0
	}
	for i, line := range popupLines {
		lines[start+i] = overlayLineSegment(lines[start+i], line, dashboardWidth)
	}
	return strings.Join(lines, "\n") + "\n"
}

// overlayHistoryPopup renders the H key's history overlay using the
// same shape as overlayHelpPopup. Reads from internal/history live so
// the user sees commands they just ran.
func overlayHistoryPopup(content string, st *uiState) string {
	lines := splitDashboardLines(content)
	visibleHeight := len(lines)
	if dashboardHeight > 0 {
		visibleHeight = dashboardHeight
	}
	if visibleHeight < 1 {
		visibleHeight = 1
	}
	for len(lines) < visibleHeight {
		lines = append(lines, strings.Repeat(" ", dashboardWidth))
	}

	var popup strings.Builder
	renderHistoryPopup(&popup, st)
	popupLines := splitDashboardLines(popup.String())
	if len(popupLines) == 0 {
		return content
	}
	if len(popupLines) > visibleHeight {
		popupLines = popupLines[:visibleHeight]
	}
	start := (visibleHeight - len(popupLines)) / 2
	if start < 0 {
		start = 0
	}
	for i, line := range popupLines {
		lines[start+i] = overlayLineSegment(lines[start+i], line, dashboardWidth)
	}
	return strings.Join(lines, "\n") + "\n"
}

func overlayConfirmPopup(content string, c *uiConfirm) string {
	lines := splitDashboardLines(content)
	visibleHeight := len(lines)
	if dashboardHeight > 0 {
		visibleHeight = dashboardHeight
	}
	if visibleHeight < 1 {
		visibleHeight = 1
	}
	for len(lines) < visibleHeight {
		lines = append(lines, strings.Repeat(" ", dashboardWidth))
	}

	var popup strings.Builder
	renderConfirmPopup(&popup, c)
	popupLines := splitDashboardLines(popup.String())
	if len(popupLines) == 0 {
		return content
	}
	if len(popupLines) > visibleHeight {
		popupLines = popupLines[:visibleHeight]
	}

	start := (visibleHeight - len(popupLines)) / 2
	if start < 0 {
		start = 0
	}
	for i, line := range popupLines {
		lines[start+i] = overlayLineSegment(lines[start+i], line, dashboardWidth)
	}
	return strings.Join(lines, "\n") + "\n"
}

func overlayLineSegment(base, popup string, width int) string {
	if width <= 0 {
		return popup
	}
	popupWidth := visualWidth(popup)
	if popupWidth >= width {
		return popup
	}
	left := (width - popupWidth) / 2
	rightStart := left + popupWidth
	return ansiLeft(base, left) + ansi.Reset + popup + ansi.Reset + ansiRight(base, rightStart, width)
}

func centerAnsiContent(content string, width int) string {
	if width <= 0 {
		return content
	}
	contentWidth := visualWidth(content)
	if contentWidth >= width {
		return content
	}
	left := (width - contentWidth) / 2
	right := width - contentWidth - left
	return strings.Repeat(" ", left) + content + strings.Repeat(" ", right)
}

func ansiLeft(s string, width int) string {
	return ansiSlice(s, 0, width)
}

func ansiRight(s string, start, width int) string {
	if start >= width {
		return ""
	}
	right := ansiSlice(s, start, width)
	if visualWidth(right) < width-start {
		right += strings.Repeat(" ", width-start-visualWidth(right))
	}
	return right
}

func ansiSlice(s string, start, end int) string {
	if end <= start {
		return ""
	}
	var out strings.Builder
	col := 0
	inEsc := false
	sawEsc := false
	for i := 0; i < len(s); {
		c := s[i]
		if inEsc {
			// Always copy escape bytes, even when the slice has not
			// started yet. SGR state is sticky -- if the base line
			// opened with `\x1b[2m` (dim) at column 0 and we slice
			// columns 80-95, the slice's visible cells were rendered
			// in dim on the original line. Dropping the dim escape
			// would resurface the underlying default-color cells next
			// to the popup, which is exactly the visible "the row to
			// the right of the popup brightened" regression.
			out.WriteByte(c)
			i++
			// Only `m` ends an SGR sequence; the rest of 0x40-0x7e
			// includes the CSI introducer `[` (0x5b), which would
			// wrongly terminate the escape and cause the parameter
			// bytes (`2` etc.) and the final `m` to be counted as
			// visible content -- leaving the sliced string two
			// columns short of the requested width and silently
			// padded with trailing spaces.
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		if c == 0x1b {
			out.WriteByte(c)
			sawEsc = true
			inEsc = true
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		rw := runeDisplayWidth(r)
		next := col + rw
		if next > start && col < end {
			if col >= start && next <= end {
				out.WriteString(s[i : i+size])
			} else {
				out.WriteString(strings.Repeat(" ", min(rw, end-start)))
			}
		}
		col = next
		i += size
		if col >= end {
			break
		}
	}
	if got := visualWidth(out.String()); got < end-start {
		out.WriteString(strings.Repeat(" ", end-start-got))
	}
	// Close any open SGR state so the trailing cells (and the next
	// line, via the terminal's persistent SGR) render in default
	// attributes.
	if sawEsc {
		out.WriteString(ansi.Reset)
	}
	return out.String()
}

// splitColumnsWidth divides a full-width row into left/right
// columns with a one-cell gap. Both halves get a floor so the
// tunnel rows / detail panel don't end up unreadably narrow on a
// 60-col window.
func splitColumnsWidth(total int) (left, right, gap int) {
	gap = 1
	left = (total - gap) / 2
	right = total - left - gap
	if left < dashboardColumnMinWidth {
		left = dashboardColumnMinWidth
		right = total - left - gap
	}
	if right < dashboardColumnMinWidth {
		right = dashboardColumnMinWidth
		left = total - right - gap
	}
	if left < 20 {
		left = 20
	}
	if right < 20 {
		right = 20
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
// of N visible chars still occupies N cells. Padding rows on either
// side are plain spaces so a column whose box has already closed with
// `╰─╯` is not visually reopened with floating `│ │` walls below it.
func writeSideBySide(sb *strings.Builder, left, right string, leftWidth, rightWidth, gap int) {
	lLines := splitDashboardLines(left)
	rLines := splitDashboardLines(right)
	n := len(lLines)
	if len(rLines) > n {
		n = len(rLines)
	}
	gapStr := strings.Repeat(" ", gap)
	blankLeft := strings.Repeat(" ", leftWidth)
	blankRight := strings.Repeat(" ", rightWidth)
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
			pad := rightWidth - visualWidth(r)
			if pad > 0 {
				r += strings.Repeat(" ", pad)
			}
		} else {
			r = blankRight
		}
		fmt.Fprintf(sb, "%s%s%s\n", l, gapStr, r)
	}
}

// renderStretchedDetail draws panelDetail at exactly `targetHeight`
// rows (excluding the trailing blank). The box's `╭─╮` top sits on
// row 0 and its `╰─╯` bottom on row targetHeight-1, so the closing
// border is guaranteed to land within the dashboard's visible frame
// regardless of how tall the natural detail content is.
//
// Three regimes:
//
//   - Natural height == targetHeight: emit unchanged.
//   - Natural height <  targetHeight: keep the top + body, insert
//     blank `│ │` rows between the last body row and the bottom, and
//     drop the bottom in place. This is the common case in a 2-column
//     layout where the left column is taller than the detail panel.
//   - Natural height >  targetHeight: keep the top, the first
//     (targetHeight-2) body rows, then the bottom -- trailing content
//     is truncated rather than spilling past the visible frame and
//     hiding the closing border.
//
// Caller must invoke this inside withDashboardWidth(rightW, …) so the
// padding boxLine emits at the right column's width.
func renderStretchedDetail(sb *strings.Builder, cfg *config.Config, jobs []*jobs.Record, tunnelNames []string, mcp mcplog.Status, st *uiState, targetHeight int) {
	if targetHeight < 3 {
		targetHeight = 3
	}
	var buf strings.Builder
	panelDetail(&buf, cfg, jobs, tunnelNames, mcp, st)
	lines := splitDashboardLines(buf.String())
	bottomIdx := -1
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.Contains(lines[i], "╰") {
			bottomIdx = i
			break
		}
	}
	if bottomIdx < 0 {
		sb.WriteString(buf.String())
		return
	}
	// Body rows live between lines[0] (top) and lines[bottomIdx] (bot).
	bodyMax := targetHeight - 2
	if bodyMax < 0 {
		bodyMax = 0
	}
	body := lines[1:bottomIdx]
	hidden := 0
	if len(body) > bodyMax {
		maxStart := len(body) - bodyMax
		if st != nil {
			if st.detailScroll > maxStart {
				st.detailScroll = maxStart
			}
			if st.detailScroll < 0 {
				st.detailScroll = 0
			}
			start := st.detailScroll
			body = body[start : start+bodyMax]
			hidden = len(lines[1:bottomIdx]) - len(body)
		} else {
			body = body[:bodyMax]
			hidden = len(lines[1:bottomIdx]) - len(body)
		}
	}
	if hidden > 0 && len(body) > 0 {
		body[len(body)-1] = detailScrollHint(st, hidden)
	}
	sb.WriteString(lines[0])
	sb.WriteByte('\n')
	for _, line := range body {
		sb.WriteString(line)
		sb.WriteByte('\n')
	}
	for pad := bodyMax - len(body); pad > 0; pad-- {
		boxLine(sb, "")
	}
	sb.WriteString(lines[bottomIdx])
	sb.WriteString("\n\n")
}

func detailScrollHint(st *uiState, hidden int) string {
	offset := 0
	if st != nil {
		offset = st.detailScroll
	}
	return boxedMetaLine(fmt.Sprintf("detail scroll %d, %d hidden", offset, hidden))
}

func boxedMetaLine(msg string) string {
	return fmt.Sprintf("%s│%s %s %s│%s",
		boxColor(false), ansi.Reset, padAnsiRight(dashMeta(msg), dashboardContentWidth), boxColor(false), ansi.Reset)
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

// boxTopWithDimSuffix renders a panel header where a dim suffix is
// appended directly after the bright title (e.g. "PROFILES 2  ● 1
// active · ○ 1 idle" with the second half dim). Going through boxTop
// would not work: boxTop applies strings.ToUpper to its whole title,
// which would mangle the ANSI escape bytes that carry the dim
// formatting. So the uppercasing and styling happen here instead.
func boxTopWithDimSuffix(sb *strings.Builder, title, suffix string) {
	border := boxColor(false)
	label := " " + ansi.Reset + ansi.Bold + ansi.Cyan + strings.ToUpper(title) + ansi.Reset
	if suffix != "" {
		label += "  " + ansi.Dim + strings.ToUpper(suffix) + ansi.Reset
	}
	label += border + " "
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
	// Truncate overflowing content so the closing │ lands exactly at
	// dashboardWidth. Without this, a long header in the narrow
	// split-column layout would push past the box's right edge,
	// terminal would wrap the line, and the right column on the next
	// row would visually lose its border.
	if visualWidth(content) > dashboardContentWidth {
		content = ellipsisAnsiRight(content, dashboardContentWidth)
	}
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
	if width <= 0 || visualWidth(s) <= width {
		return s
	}
	if width <= 3 {
		return truncatePlainDisplay(s, width)
	}
	return truncatePlainDisplay(s, width-3) + "..."
}

func truncatePlainDisplay(s string, width int) string {
	if width <= 0 {
		return ""
	}
	var out strings.Builder
	used := 0
	for _, r := range s {
		rw := runeDisplayWidth(r)
		if used+rw > width {
			break
		}
		out.WriteRune(r)
		used += rw
	}
	return out.String()
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
	// Default label for the interactive case. Snapshot (st == nil)
	// is shown below as "snapshot mode (no tty)", so the upper-right
	// "live terminal view" tag would contradict it -- swap to a
	// neutral label in that case.
	mode := ansi.Dim + "live terminal view" + ansi.Reset
	switch {
	case st == nil:
		mode = ansi.Dim + "one-shot snapshot" + ansi.Reset
	case st.demoMode:
		mode = ansi.Yellow + ansi.Bold + "DEMO" + ansi.Reset + ansi.Dim + " simulated data" + ansi.Reset
	}
	boxLine(sb, fitInlineParts(
		[]string{
			ansi.Bold + ansi.Magenta + "SRV UI" + ansi.Reset,
			ansi.Dim + "remote control dashboard" + ansi.Reset,
		},
		mode,
		dashboardContentWidth,
	))
	if st == nil {
		boxLine(sb, ansi.Dim+"snapshot mode (no tty)"+ansi.Reset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	boxLine(sb, fmt.Sprintf("keys  %stab/h/l%s pane   %s↑/↓%s row   %sCtrl-U/D%s detail   %s/%s filter   %s?%s help   %sq%s quit",
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset,
		ansi.Yellow+ansi.Bold, ansi.Reset))
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// panelProfiles lists every configured profile and marks each one as
// active (●, daemon currently has a pooled SSH connection) or idle
// (○, no live connection). Multi-column grid so even a wallet of 30+
// profiles stays within ~6 rows; overflow collapses to a "... N more"
// footer so the dashboard's height stays bounded.
func panelProfiles(sb *strings.Builder, cfg *config.Config, st *uiState) {
	names := profileNamesSorted(cfg)
	if len(names) == 0 {
		boxTop(sb, "profiles")
		boxLine(sb, ansi.Dim+"no profiles configured"+ansi.Reset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}

	active := map[string]bool{}
	if st != nil && st.snapDaemonResp != nil {
		for _, p := range st.snapDaemonResp.Profiles {
			active[p] = true
		}
	}
	activeCount := 0
	for _, n := range names {
		if active[n] {
			activeCount++
		}
	}

	boxTop(sb, fmt.Sprintf("profiles %d", len(names)))
	boxLine(sb, fmt.Sprintf("%sactive%s %d    %sidle%s %d",
		ansi.Dim, ansi.Reset, activeCount,
		ansi.Dim, ansi.Reset, len(names)-activeCount))

	maxName := 0
	for _, n := range names {
		if w := visualWidth(n); w > maxName {
			maxName = w
		}
	}
	const marker = 2 // "● " / "○ "
	const gap = 2
	cellW := marker + maxName + gap
	if cellW < 14 {
		cellW = 14
	}
	cols := dashboardContentWidth / cellW
	if cols < 1 {
		cols = 1
	}

	// Cap visible rows so a wallet of 50 profiles can't push the rest
	// of the dashboard off-screen. 4 rows × cols cells is usually
	// plenty; anything past that collapses to a "... N more" line.
	const maxRows = 4
	totalRows := (len(names) + cols - 1) / cols
	rows := totalRows
	if rows > maxRows {
		rows = maxRows
	}
	visible := rows * cols
	if visible > len(names) {
		visible = len(names)
	}

	for r := 0; r < rows; r++ {
		var line strings.Builder
		for c := 0; c < cols; c++ {
			idx := r*cols + c
			if idx >= visible {
				break
			}
			n := names[idx]
			var dot, label string
			if active[n] {
				dot = ansi.Green + ansi.Bold + "● " + ansi.Reset
				label = ansi.Yellow + ansi.Bold + n + ansi.Reset
			} else {
				dot = ansi.Dim + "○ " + ansi.Reset
				label = ansi.Dim + n + ansi.Reset
			}
			cell := dot + padAnsiRight(label, cellW-marker)
			line.WriteString(cell)
		}
		boxLine(sb, strings.TrimRight(line.String(), " "))
	}
	if visible < len(names) {
		boxLine(sb, ansi.Dim+fmt.Sprintf("... %d more (use `srv ls` to view all)", len(names)-visible)+ansi.Reset)
	}
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// profileNamesSorted returns the cfg.Profiles keys alphabetically.
// Kept local to ui.go so the dashboard has a stable enumeration
// independent of any caller's iteration order.
func profileNamesSorted(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Profiles))
	for n := range cfg.Profiles {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
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
	for i := 0; i < len(s); {
		c := s[i]
		if inEscape {
			out.WriteByte(c)
			i++
			// Only `m` ends an SGR sequence -- see the matching note
			// in ansiSlice. Treating the CSI introducer `[` (0x5b) as
			// an escape terminator here would make the parameter
			// bytes and the final `m` count toward the cell budget,
			// causing the truncation to fire two cells too early and
			// chop a `\x1b[0m` reset in half (the user-visible symptom
			// is the selected TUNNEL row's SPEC shrinking to "...").
			if c == 'm' {
				inEscape = false
			}
			continue
		}
		if c == 0x1b {
			out.WriteByte(c)
			inEscape = true
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 0 {
			break
		}
		rw := runeDisplayWidth(r)
		if seen+rw > target {
			break
		}
		out.WriteString(s[i : i+size])
		seen += rw
		i += size
	}
	out.WriteString("...")
	out.WriteString(ansi.Reset)
	return out.String()
}

// panelDaemon renders daemon state from the cached snapshot -- never
// dials inline. The main loop refreshes st.snapDaemonResp at most
// once per snapTTL (2s); without that, each render fired a fresh
// daemon.DialSock+status RPC, which at the 150ms poll interval meant
// 6-7 socket round-trips per second.
func panelDaemon(sb *strings.Builder, st *uiState) {
	boxTop(sb, "daemon")
	var resp *daemon.Response
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

func panelOverview(sb *strings.Builder, cfg *config.Config, jobs []*jobs.Record, tunnelNames []string, mcp mcplog.Status, st *uiState) {
	profiles := 0
	defaultProfile := ""
	groups := 0
	if cfg != nil {
		profiles = len(cfg.Profiles)
		defaultProfile = cfg.DefaultProfile
		groups = len(cfg.Groups)
	}
	if defaultProfile == "" {
		defaultProfile = "-"
	}
	focus := "snapshot"
	if st != nil {
		focus = st.focusPane
		if focus == "" {
			focus = "none"
		}
		if st.demoMode {
			focus += " demo"
		}
	}
	hidden := 0
	if st != nil {
		hidden = st.hiddenJobs
	}
	boxTopWithDimSuffix(sb, "overview", fmt.Sprintf("focus %s", strings.ToUpper(focus)))
	boxLine(sb, compactOverviewLine(
		overviewItem("profiles", strconv.Itoa(profiles)),
		overviewItem("default", defaultProfile),
	))
	boxLine(sb, compactOverviewLine(
		overviewItem("groups", strconv.Itoa(groups)),
		overviewItem("tunnels", strconv.Itoa(len(tunnelNames))),
		overviewItem("jobs", fmt.Sprintf("%d visible / %d hidden", len(jobs), hidden)),
	))
	if mcp.LogExists {
		boxLine(sb, overviewItem("mcp", fmt.Sprintf("%d recent", len(mcp.RecentTools))))
	} else {
		boxLine(sb, overviewItem("mcp", dashMeta("no log yet")))
	}
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func overviewItem(label, value string) string {
	return ansi.Dim + strings.ToUpper(label) + ":" + ansi.Reset + " " + value
}

func compactOverviewLine(parts ...string) string {
	return strings.Join(parts, "    ")
}

// fetchDaemonStatusForUI does a single status RPC and returns the
// response, or nil if the daemon isn't reachable. Used by the main
// loop's snapshot refresh and by the snapshot-mode (st==nil) fallback.
func fetchDaemonStatusForUI() *daemon.Response {
	conn := daemon.DialSock(300 * time.Millisecond)
	if conn == nil {
		return nil
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "status"}, time.Second)
	if err != nil || resp == nil || !resp.OK {
		return nil
	}
	return resp
}

func panelGroups(sb *strings.Builder, cfg *config.Config) {
	if len(cfg.Groups) == 0 {
		// Empty-state card so `srv ui` keeps a stable panel set
		// regardless of what's configured -- same reasoning as
		// panelTunnels above. The hint points at the command that
		// would populate this view.
		boxTop(sb, "groups 0")
		boxLine(sb, ansi.Dim+"no profile groups  (try: srv group set <name> <profile...>)"+ansi.Reset)
		boxBottom(sb)
		fmt.Fprintln(sb)
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

func panelTunnels(sb *strings.Builder, cfg *config.Config, names []string, st *uiState) {
	focused := st != nil && st.focusPane == "tunnel"
	if len(names) == 0 {
		// Render an empty-state card instead of vanishing. Without
		// this, `srv ui` and `srv ui demo` produce dashboards with a
		// different panel count -- a user with no saved tunnels
		// sees JOBS pushed up where TUNNELS would be, which doesn't
		// match any screenshot or demo. Keeping the box visible
		// also gives the user a discoverability hint (`srv tunnel
		// add ...`) right where they'd look for tunnels.
		boxTopWithHint(sb, "tunnels 0", "space toggle  x remove", focused)
		boxLineFocused(sb, ansi.Dim+"no saved tunnels  (try: srv tunnel add <name> <port>)"+ansi.Reset, focused)
		boxBottomFocused(sb, focused)
		fmt.Fprintln(sb)
		return
	}
	var active map[string]daemon.TunnelInfo
	var errs map[string]string
	if st != nil {
		active = st.snapTunnelActive
		errs = st.snapTunnelErrs
	} else {
		active, errs = tunnel.LoadStatuses()
	}
	title := fmt.Sprintf("tunnels %d", len(names))
	if st != nil && len(names) > 0 {
		title += fmt.Sprintf("  %d/%d", st.tunnelCursor+1, len(names))
	}
	boxTopWithHint(sb, title, "space toggle  x remove", focused)
	nameW := 12
	typeW := 7
	specW := dashboardContentWidth - nameW - typeW - 22
	if specW < 10 {
		specW = 10
	}
	if specW > 44 {
		specW = 44
	}
	boxLineFocused(sb, ansi.Dim+"  "+padAnsiRight("NAME", nameW)+"  "+padAnsiRight("TYPE", typeW)+"  SPEC / STATE"+ansi.Reset, focused)
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
		row := "  " +
			padAnsiRight(dashName(fitPlain(n, nameW)), nameW) + "  " +
			padAnsiRight(ansi.Magenta+fitPlain(def.Type, typeW)+ansi.Reset, typeW) + "  " +
			dashPath(fitPlain(def.Spec, specW)) + "  " +
			status + ansi.Dim + extra + ansi.Reset + flag
		// Selection only swaps the leading "  " gutter for a yellow
		// `> ` cursor. We intentionally do NOT wrap the rest of the
		// row in `ansi.Reverse` -- the columns carry their own colour
		// coding (magenta type, cyan spec, green/red state) and
		// inverting them turns the row into an unreadable rainbow.
		if st != nil && st.isSelected("tunnel", i) {
			row = ansi.Yellow + ansi.Bold + "> " + ansi.Reset + row[2:]
		}
		boxLineFocused(sb, row, focused)
		if errMsg != "" {
			boxLineFocused(sb, "    "+ansi.Red+fitPlain(errMsg, max(8, dashboardContentWidth-4))+ansi.Reset, focused)
		}
	}
	boxBottomFocused(sb, focused)
	fmt.Fprintln(sb)
}

// filterJobs returns the subset of `js` whose ID or Cmd contains
// `query` (case-insensitive substring). An empty query returns the
// slice unchanged so the no-filter path doesn't allocate.
func filterJobs(js []*jobs.Record, query string) []*jobs.Record {
	q := strings.TrimSpace(strings.ToLower(query))
	if q == "" {
		return js
	}
	out := make([]*jobs.Record, 0, len(js))
	for _, j := range js {
		if strings.Contains(strings.ToLower(j.ID), q) ||
			strings.Contains(strings.ToLower(j.Cmd), q) ||
			strings.Contains(strings.ToLower(j.Profile), q) {
			out = append(out, j)
		}
	}
	return out
}

func panelJobs(sb *strings.Builder, jobs []*jobs.Record, st *uiState) {
	hidden := 0
	if st != nil {
		hidden = st.hiddenJobs
	}
	focused := st != nil && st.focusPane == "job"
	if len(jobs) == 0 && hidden == 0 && (st == nil || st.filterQuery == "") {
		// Render an empty-state card so the panel layout stays
		// stable regardless of detached-job state. The hint mirrors
		// what `srv jobs` shows when nothing is running.
		boxTopWithHint(sb, "jobs 0", "k kill", focused)
		boxLineFocused(sb, ansi.Dim+"nothing running  (try: srv -d <cmd> to detach one)"+ansi.Reset, focused)
		boxBottomFocused(sb, focused)
		fmt.Fprintln(sb)
		return
	}
	title := fmt.Sprintf("jobs %d", len(jobs))
	if st != nil && len(jobs) > 0 {
		title += fmt.Sprintf("  %d/%d", st.jobCursor+1, len(jobs))
	}
	if hidden > 0 {
		title = fmt.Sprintf("jobs %d + %d hidden", len(jobs), hidden)
	}
	if st != nil && st.filterQuery != "" {
		q := st.filterQuery
		if st.filterMode {
			q += "▏" // a cursor glyph; the modal is currently capturing input
		}
		title += "  /" + q
	}
	start, end := 0, len(jobs)
	if st != nil {
		start, end = visibleListRange(len(jobs), st.jobCursor, dashboardListRows)
		if end-start < len(jobs) {
			title += fmt.Sprintf("  showing %d-%d/%d", start+1, end, len(jobs))
		}
	}
	boxTopWithHint(sb, title, "k kill", focused)
	if len(jobs) == 0 {
		msg := ansi.Dim + "nothing running; `srv jobs` lists completed entries" + ansi.Reset
		if st != nil && st.filterQuery != "" {
			msg = ansi.Dim + "no jobs match filter -- press Esc to clear" + ansi.Reset
		}
		boxLineFocused(sb, msg, focused)
		boxBottomFocused(sb, focused)
		fmt.Fprintln(sb)
		return
	}
	boxLineFocused(sb, ansi.Dim+"  ID            PROFILE       PID       AGE"+ansi.Reset, focused)
	boxLineFocused(sb, ansi.Dim+strings.Repeat("-", dashboardContentWidth)+ansi.Reset, focused)
	for i := start; i < end; i++ {
		j := jobs[i]
		started := j.Started
		if t, ok := parseISOLike(j.Started); ok {
			started = fmtDuration(time.Since(t)) + " ago"
		}
		row := "  " +
			padAnsiRight(dashName(truncID(j.ID)), 12) + "  " +
			padAnsiRight(ansi.Cyan+fitPlain(j.Profile, 12)+ansi.Reset, 12) + "  " +
			padAnsiRight(strconv.Itoa(j.Pid), 8) + "  " +
			padAnsiRight(dashMeta(started), 8)
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
func panelDetail(sb *strings.Builder, cfg *config.Config, jobs []*jobs.Record, tunnelNames []string, mcp mcplog.Status, st *uiState) {
	row := st.currentRow()
	switch row.kind {
	case "tunnel":
		if row.idx >= 0 && row.idx < len(tunnelNames) {
			panelTunnelDetail(sb, fmt.Sprintf("tunnel detail  %d/%d", row.idx+1, len(tunnelNames)), tunnelNames[row.idx], cfg, st)
			return
		}
	case "job":
		if row.idx >= 0 && row.idx < len(jobs) {
			panelJobDetail(sb, fmt.Sprintf("job detail  %d/%d", row.idx+1, len(jobs)), jobs[row.idx], st)
			return
		}
	case "mcp":
		if row.idx >= 0 && row.idx < len(mcp.RecentTools) {
			panelMCPDetail(sb, fmt.Sprintf("mcp call detail  %d/%d", row.idx+1, len(mcp.RecentTools)), mcp.RecentTools[row.idx], mcp)
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
func panelMCPDetail(sb *strings.Builder, title string, tc mcplog.ToolCall, mcp mcplog.Status) {
	boxTop(sb, title)
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
// against the narrower width. When the user has hit `L` on this job,
// the last N log lines are appended below the COMMAND block so the
// user can eyeball recent output without exiting the UI.
func panelJobDetail(sb *strings.Builder, title string, j *jobs.Record, st *uiState) {
	boxTop(sb, title)
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
	// Log preview block. Rendered only when the user pressed L on
	// this same job -- otherwise we keep the panel compact so the
	// JOBS-bottom anchor doesn't drift around as the user navigates.
	if st != nil && st.jobLog != nil && st.jobLog.jobID == j.ID {
		boxLine(sb, "")
		hdr := ansi.Dim + "LOG  " + ansi.Reset + ansi.Dim + "(last " + strconv.Itoa(len(st.jobLog.lines)) + " lines)" + ansi.Reset
		if st.jobLog.fetching {
			hdr = ansi.Dim + "LOG  fetching..." + ansi.Reset
		} else if !st.jobLog.fetchedAt.IsZero() {
			hdr += dashMeta("  " + fmtDuration(time.Since(st.jobLog.fetchedAt)) + " ago")
		}
		boxLine(sb, hdr)
		switch {
		case st.jobLog.err != "":
			boxLine(sb, "  "+ansi.Red+st.jobLog.err+ansi.Reset)
		case st.jobLog.fetching:
			boxLine(sb, "  "+ansi.Dim+"loading remote log..."+ansi.Reset)
		default:
			// Render the *last* lines that fit so the freshest output
			// is what the user sees. The DETAIL panel is height-bounded
			// to leftTop -- letting an oversize log push the panel
			// taller would break the JOBS-bottom alignment we just
			// fixed.
			for _, line := range st.jobLog.lines {
				boxLine(sb, "  "+fitPlain(line, dashboardContentWidth-2))
			}
		}
	}
	boxLine(sb, "")
	boxLine(sb, ansi.Dim+"press "+ansi.Yellow+ansi.Bold+"k"+ansi.Reset+ansi.Dim+" to kill   "+ansi.Yellow+ansi.Bold+"L"+ansi.Reset+ansi.Dim+" log preview"+ansi.Reset)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

// panelTunnelDetail renders tunnel details for the right
// column. Surfaces last-attempt errors prominently -- that's the
// info the user most wants when something looks "stopped" but they
// expected "running".
func panelTunnelDetail(sb *strings.Builder, title, name string, cfg *config.Config, st *uiState) {
	def := cfg.Tunnels[name]
	if def == nil {
		boxTop(sb, title)
		boxLine(sb, ansi.Red+"tunnel "+name+" not found in config"+ansi.Reset)
		boxBottom(sb)
		fmt.Fprintln(sb)
		return
	}
	boxTop(sb, title)
	boxLine(sb, kvLine("name", dashName(name)))
	boxLine(sb, kvLine("type", ansi.Magenta+def.Type+ansi.Reset))
	boxLine(sb, kvLine("spec", dashPath(def.Spec)))
	boxLine(sb, kvLine("profile", ansi.Cyan+tunnelProfileLabel(def)+ansi.Reset))
	boxLine(sb, kvLine("autostart", boolLabel(def.Autostart)))
	var active map[string]daemon.TunnelInfo
	var errs map[string]string
	if st != nil {
		active = st.snapTunnelActive
		errs = st.snapTunnelErrs
	} else {
		active, errs = tunnel.LoadStatuses()
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
	title := fmt.Sprintf("mcp %d", len(mcp.RecentTools))
	if st != nil && len(mcp.RecentTools) > 0 {
		title += fmt.Sprintf("  %d/%d", st.mcpCursor+1, len(mcp.RecentTools))
	}
	boxTopWithHint(sb, title, "read-only", focused)
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
		toolW := dashboardContentWidth - 30
		if toolW < 10 {
			toolW = 10
		}
		if toolW > 20 {
			toolW = 20
		}
		start, end := 0, len(mcp.RecentTools)
		if st != nil {
			start, end = visibleListRange(len(mcp.RecentTools), st.mcpCursor, dashboardListRows)
			if end-start < len(mcp.RecentTools) {
				boxLineFocused(sb, dashMeta(fmt.Sprintf("showing %d-%d/%d", start+1, end, len(mcp.RecentTools))), focused)
			}
		}
		for i := start; i < end; i++ {
			tc := mcp.RecentTools[i]
			status := dashStatus("ok", ansi.Green)
			if !tc.OK {
				status = dashStatus("err", ansi.Red)
			}
			row := "  " +
				padAnsiRight(ansi.Yellow+fitPlain(tc.Name, toolW)+ansi.Reset, toolW) + "  " +
				padAnsiRight(ansi.Magenta+tc.Dur+ansi.Reset, 7) + "  " +
				padAnsiRight(status, 7) + "  " +
				dashMeta(fmtDuration(time.Since(tc.When))+" ago")
			if st != nil && st.isSelected("mcp", i) {
				row = ansi.Yellow + ansi.Bold + "> " + ansi.Reset + ansi.Reverse + row[2:] + ansi.Reset
			}
			boxLineFocused(sb, row, focused)
		}
	}
	boxBottomFocused(sb, focused)
	fmt.Fprintln(sb)
}

func visibleListRange(total, cursor, limit int) (start, end int) {
	if total <= 0 {
		return 0, 0
	}
	if limit <= 0 || limit >= total {
		return 0, total
	}
	if cursor < 0 {
		cursor = 0
	}
	if cursor >= total {
		cursor = total - 1
	}
	start = cursor - limit/2
	if start < 0 {
		start = 0
	}
	if start+limit > total {
		start = total - limit
	}
	return start, start + limit
}

func panelStatus(sb *strings.Builder, st *uiState) {
	if st == nil || st.statusMsg == "" {
		return
	}
	boxTop(sb, "status")
	boxLine(sb, st.statusMsg)
	boxBottom(sb)
	fmt.Fprintln(sb)
}

func panelFooter(sb *strings.Builder, st *uiState) {
	boxTop(sb, "keys")
	if st == nil {
		boxLine(sb, ansi.Dim+"snapshot complete"+ansi.Reset)
	} else {
		focus := st.focusPane
		if focus == "" {
			focus = "none"
		}
		boxLine(sb, kvPair("focus", ansi.Yellow+ansi.Bold+strings.ToUpper(focus)+ansi.Reset, "mode", "live dashboard"))
		switch focus {
		case "tunnel":
			boxLine(sb, keyHelp("↑/↓ move", "space up/down", "x remove", "tab next", "? help"))
		case "job":
			boxLine(sb, keyHelp("↑/↓ move", "k kill", "L log", "/ filter", "tab next", "? help"))
		case "mcp":
			boxLine(sb, keyHelp("↑/↓ move", "read-only", "tab next", "? help"))
		default:
			boxLine(sb, keyHelp("tab choose pane", "? help", "q quit"))
		}
	}
	boxBottom(sb)
}

func keyHelp(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		fields := strings.Fields(p)
		if len(fields) == 0 {
			continue
		}
		key := fields[0]
		desc := strings.TrimSpace(strings.TrimPrefix(p, key))
		if key == "read-only" {
			out = append(out, ansi.Dim+"read-only"+ansi.Reset)
		} else if desc == "" {
			out = append(out, ansi.Yellow+ansi.Bold+key+ansi.Reset)
		} else {
			out = append(out, ansi.Yellow+ansi.Bold+key+ansi.Reset+" "+desc)
		}
	}
	return strings.Join(out, ansi.Dim+"  ·  "+ansi.Reset)
}

// renderHelpPopup draws the `?` help cheatsheet. Same box shape as
// the confirmation popup so the user has a single visual vocabulary
// for "modal thing on top of the dashboard". Content is grouped by
// pane so the relevant keys for the current focus are first.
func renderHelpPopup(sb *strings.Builder, st *uiState) {
	rows := helpRows(st)
	width := 0
	for _, r := range rows {
		if w := visualWidth(r) + 4; w > width {
			width = w
		}
	}
	title := "keyboard shortcuts"
	if w := visualWidth(title) + 6; w > width {
		width = w
	}
	if width < 40 {
		width = 40
	}
	if width > dashboardWidth {
		width = dashboardWidth
	}
	color := ansi.Bold + ansi.Cyan
	top := "┌" + strings.Repeat("─", width-2) + "┐"
	bot := "└" + strings.Repeat("─", width-2) + "┘"
	fmt.Fprintf(sb, "%s%s%s\n", color, top, ansi.Reset)
	titleCell := centerAnsiContent(color+title+ansi.Reset, width-2)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, titleCell, color, ansi.Reset)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, strings.Repeat(" ", width-2), color, ansi.Reset)
	for _, r := range rows {
		// Help rows already carry colour; left-pad to 2 spaces so the
		// box wall doesn't kiss the first glyph.
		padded := "  " + r
		cell := padded + strings.Repeat(" ", max(0, width-2-visualWidth(padded)))
		fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, cell, color, ansi.Reset)
	}
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, strings.Repeat(" ", width-2), color, ansi.Reset)
	hint := ansi.Dim + "press any key to close" + ansi.Reset
	cell := centerAnsiContent(hint, width-2)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, cell, color, ansi.Reset)
	fmt.Fprintf(sb, "%s%s%s\n", color, bot, ansi.Reset)
}

// renderHistoryPopup draws a panel showing the last N CLI commands
// recorded by internal/history. Read-only -- the model can't run from
// here (no per-row action wiring), but having the panel beside the
// other dashboards lets the user see "what did I just do" without
// flipping shells. Up/Down (or j/k) scrolls when the list exceeds the
// popup height.
func renderHistoryPopup(sb *strings.Builder, st *uiState) {
	const popupHeight = 18
	width := dashboardWidth - 4
	if width < 60 {
		width = 60
	}
	if width > dashboardWidth {
		width = dashboardWidth
	}
	color := ansi.Bold + ansi.Cyan
	top := "┌" + strings.Repeat("─", width-2) + "┐"
	bot := "└" + strings.Repeat("─", width-2) + "┘"
	fmt.Fprintf(sb, "%s%s%s\n", color, top, ansi.Reset)
	title := "command history (recent first)"
	titleCell := centerAnsiContent(color+title+ansi.Reset, width-2)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, titleCell, color, ansi.Reset)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, strings.Repeat(" ", width-2), color, ansi.Reset)

	entries, err := history.ReadAll()
	rows := []string{}
	if err != nil {
		rows = []string{ansi.Red + "history read error: " + err.Error() + ansi.Reset}
	} else if len(entries) == 0 {
		rows = []string{ansi.Dim + "(no history yet -- run a remote command via srv)" + ansi.Reset}
	} else {
		// Newest first.
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			mark := " "
			markColor := ansi.Green
			if e.Exit != 0 {
				mark = "!"
				markColor = ansi.Red
			}
			when := e.Time
			if len(when) > 19 {
				when = when[:19]
			}
			cmd := e.Cmd
			maxCmd := width - 32
			if maxCmd < 10 {
				maxCmd = 10
			}
			if len(cmd) > maxCmd {
				cmd = cmd[:maxCmd-3] + "..."
			}
			rows = append(rows, fmt.Sprintf("%s%s%s %s  %s%s%s  %s",
				markColor, mark, ansi.Reset,
				when,
				ansi.Dim, e.Profile, ansi.Reset,
				cmd))
		}
	}

	// Scroll window.
	visibleRows := popupHeight - 6 // 2 border + title + spacer + bottom hint + bot
	if visibleRows < 4 {
		visibleRows = 4
	}
	scroll := st.historyScroll
	if scroll > len(rows)-visibleRows {
		scroll = len(rows) - visibleRows
	}
	if scroll < 0 {
		scroll = 0
	}
	st.historyScroll = scroll
	end := scroll + visibleRows
	if end > len(rows) {
		end = len(rows)
	}
	window := rows[scroll:end]
	for _, r := range window {
		padded := "  " + r
		cell := padded + strings.Repeat(" ", max(0, width-2-visualWidth(padded)))
		fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, cell, color, ansi.Reset)
	}
	// Pad with blank rows so the popup keeps a stable height.
	for i := len(window); i < visibleRows; i++ {
		fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, strings.Repeat(" ", width-2), color, ansi.Reset)
	}
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, strings.Repeat(" ", width-2), color, ansi.Reset)
	hint := ansi.Dim + "↑/↓ or j/k scroll  ·  any other key closes  ·  q quits" + ansi.Reset
	cell := centerAnsiContent(hint, width-2)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n", color, ansi.Reset, cell, color, ansi.Reset)
	fmt.Fprintf(sb, "%s%s%s\n", color, bot, ansi.Reset)
}

// helpRows returns the cheatsheet lines, ordered with the rows most
// relevant to the current focus first.
func helpRows(st *uiState) []string {
	yk := func(k, desc string) string {
		return ansi.Yellow + ansi.Bold + k + ansi.Reset + "  " + desc
	}
	global := []string{
		yk("tab/h/l", "switch focused pane"),
		yk("↑/↓", "move cursor within pane"),
		yk("Ctrl-U/D", "scroll DETAIL panel"),
		yk("r", "force refresh snapshot"),
		yk("/", "filter JOBS by id / cmd"),
		yk("H", "history overlay (recent CLI commands)"),
		yk("?", "this help screen"),
		yk("q / Ctrl-C", "quit"),
	}
	focus := st.focusPane
	if focus == "" {
		focus = "(none)"
	}
	var perPane []string
	switch focus {
	case "tunnel":
		perPane = []string{
			ansi.Dim + "tunnel pane:" + ansi.Reset,
			yk("space", "toggle tunnel up / down"),
			yk("x", "remove tunnel definition"),
		}
	case "job":
		perPane = []string{
			ansi.Dim + "job pane:" + ansi.Reset,
			yk("k", "send SIGTERM to selected job"),
			yk("L", "preview last log lines in DETAIL"),
		}
	case "mcp":
		perPane = []string{
			ansi.Dim + "mcp pane:" + ansi.Reset,
			ansi.Dim + "read-only -- no row actions" + ansi.Reset,
		}
	default:
		perPane = []string{
			ansi.Dim + "no pane focused -- press tab" + ansi.Reset,
		}
	}
	rows := append([]string{ansi.Dim + "global:" + ansi.Reset}, global...)
	rows = append(rows, "")
	rows = append(rows, perPane...)
	return rows
}

// renderConfirmPopup draws the action title and explanatory body
// lines; overlayConfirmPopup places the resulting box over the
// already-rendered dashboard. Every emitted line starts directly with
// the red border glyph so the overlay only overwrites cells *inside*
// the red box -- anything to the left / right of the popup remains
// the underlying dashboard content.
func renderConfirmPopup(sb *strings.Builder, c *uiConfirm) {
	width := min(64, max(28, dashboardWidth))
	for _, line := range c.body {
		if w := visualWidth(line) + 4; w > width {
			width = w
		}
	}
	if w := visualWidth(c.title) + 6; w > width {
		width = w
	}
	if width > dashboardWidth {
		width = dashboardWidth
	}
	if width < 28 {
		width = 28
	}
	top := "┌" + strings.Repeat("─", width-2) + "┐"
	bot := "└" + strings.Repeat("─", width-2) + "┘"
	color := confirmColor(c.title)
	title := fitPlain(c.title, max(8, width-4))
	fmt.Fprintf(sb, "%s%s%s\n", color, top, ansi.Reset)
	titleCell := centerAnsiContent(color+title+ansi.Reset, width-2)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		color, ansi.Reset,
		titleCell,
		color, ansi.Reset)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		color, ansi.Reset,
		strings.Repeat(" ", width-2),
		color, ansi.Reset)
	for _, line := range c.body {
		line = fitPlain(line, max(8, width-4))
		lineCell := centerAnsiContent(line, width-2)
		fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
			color, ansi.Reset,
			lineCell,
			color, ansi.Reset)
	}
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		color, ansi.Reset,
		strings.Repeat(" ", width-2),
		color, ansi.Reset)
	choice := ansi.Yellow + ansi.Bold + "[Y]" + ansi.Reset + " confirm    " +
		ansi.Yellow + ansi.Bold + "[N/Esc]" + ansi.Reset + " cancel"
	choiceWidth := visualWidth("[Y] confirm    [N/Esc] cancel")
	if choiceWidth > width-3 {
		choice = ansi.Yellow + ansi.Bold + "[Y]" + ansi.Reset + " ok  " +
			ansi.Yellow + ansi.Bold + "[N]" + ansi.Reset + " cancel"
		choiceWidth = visualWidth("[Y] ok  [N] cancel")
	}
	choiceCell := centerAnsiContent(choice, width-2)
	fmt.Fprintf(sb, "%s│%s%s%s│%s\n",
		color, ansi.Reset,
		choiceCell,
		color, ansi.Reset)
	fmt.Fprintf(sb, "%s%s%s\n", color, bot, ansi.Reset)
}

func confirmColor(title string) string {
	switch {
	case strings.HasPrefix(title, "tunnel up "):
		return ansi.Bold + ansi.Green
	case strings.HasPrefix(title, "tunnel down "):
		return ansi.Bold + ansi.Yellow
	default:
		return ansi.Bold + ansi.Red
	}
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
		w += runeDisplayWidth(r)
	}
	return w
}

func runeDisplayWidth(r rune) int {
	if r == 0 {
		return 0
	}
	if r < 32 || (r >= 0x7f && r < 0xa0) {
		return 0
	}
	if isWideRune(r) {
		return 2
	}
	return 1
}

func isWideRune(r rune) bool {
	return (r >= 0x1100 && r <= 0x115f) ||
		(r >= 0x2329 && r <= 0x232a) ||
		(r >= 0x2e80 && r <= 0xa4cf) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe10 && r <= 0xfe19) ||
		(r >= 0xfe30 && r <= 0xfe6f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff) ||
		(r >= 0x20000 && r <= 0x3fffd)
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
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
func renderTunnelDetail(name string, cfg *config.Config, st *uiState) string {
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
	active, errs := tunnel.LoadStatuses()
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
// actual column count once Cmd starts (set by updateDashboardWidth
// each redraw). The hard-coded defaults take over only in non-TTY
// snapshot mode where no terminal size is reported.
var dashboardWidth = 87
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
	dashboardMinWidth          = 60
	dashboardMaxWidth          = 200
	dashboardTwoColumnMinWidth = 96
	dashboardColumnMinWidth    = 44
	dashboardListRows          = 5
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
	// Keep the frame one cell away from the terminal's hard right edge.
	// Several terminals auto-wrap immediately after a glyph is written in
	// the last column; our per-line erase then lands on the next row and
	// makes the right border look missing. A one-cell gutter keeps the
	// closing vertical rule visible.
	if w > dashboardMinWidth {
		w--
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

func dashActive(sb *strings.Builder, cfg *config.Config) {
	dashSection(sb, "Active")
	name, prof, err := config.Resolve(cfg, "")
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
	cwd := config.GetCwd(name, prof)
	dashField(sb, "cwd", dashPath(cwd))
	if pf := project.Resolve(); pf != nil {
		dashField(sb, "pinned", dashPath(pf.Path))
	}
	fmt.Fprintln(sb)
}

func dashDaemon(sb *strings.Builder) {
	dashSection(sb, "Daemon")
	conn := daemon.DialSock(300 * time.Millisecond)
	if conn == nil {
		dashField(sb, "state", dashStatus("stopped", ansi.Dim))
		fmt.Fprintln(sb)
		return
	}
	defer conn.Close()
	resp, err := daemon.Call(conn, daemon.Request{Op: "status"}, time.Second)
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

func dashGroups(sb *strings.Builder, cfg *config.Config) {
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
func dashTunnels(sb *strings.Builder, cfg *config.Config, names []string, st *uiState) {
	if len(names) == 0 {
		return
	}
	active, errs := tunnel.LoadStatuses()
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
