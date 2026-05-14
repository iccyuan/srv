package ui

import (
	"srv/internal/ansi"
	"srv/internal/config"
	"srv/internal/daemon"
	"srv/internal/jobs"
	"srv/internal/mcplog"
	"strings"
	"testing"
	"time"
)

func TestFmtDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{-time.Second, "0s"},
		{8 * time.Second, "8s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m"},
		{15 * time.Minute, "15m"},
		{59 * time.Minute, "59m"},
		{time.Hour, "1h"},
		{2*time.Hour + 15*time.Minute, "2h 15m"},
		{23 * time.Hour, "23h"},
		{24 * time.Hour, "1d"},
		{3 * 24 * time.Hour, "3d"},
	}
	for _, c := range cases {
		if got := fmtDuration(c.d); got != c.want {
			t.Errorf("fmtDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestParseISOLike(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"not a time", false},
		// srvutil.NowISO() output -- no timezone.
		{"2026-05-10T23:25:16", true},
		// RFC3339 with Z.
		{"2026-05-10T23:25:16Z", true},
		// RFC3339 with explicit offset.
		{"2026-05-10T23:25:16+08:00", true},
	}
	for _, c := range cases {
		_, ok := parseISOLike(c.in)
		if ok != c.want {
			t.Errorf("parseISOLike(%q) ok=%v, want %v", c.in, ok, c.want)
		}
	}
	// Sanity: parsing srvutil.NowISO() and computing time.Since should give a
	// near-zero duration, not "57 years ago" (would happen if we
	// parsed local time as UTC).
	if t1, ok := parseISOLike("2026-05-11T00:00:00"); ok {
		if t1.Year() != 2026 {
			t.Errorf("parsed year=%d, want 2026", t1.Year())
		}
	}
}

func TestClampCursor(t *testing.T) {
	mkRows := func(n int) []uiRow {
		rs := make([]uiRow, n)
		for i := range rs {
			rs[i] = uiRow{kind: "job", id: "x", idx: i}
		}
		return rs
	}
	cases := []struct {
		name    string
		initial int
		n       int
		want    int
	}{
		{"no rows forces -1", 0, 0, -1},
		{"no rows forces -1 from positive", 5, 0, -1},
		{"negative becomes 0", -1, 3, 0},
		{"in range untouched", 2, 5, 2},
		{"past end clamps to n-1", 9, 3, 2},
		{"single row: 0 stays 0", 0, 1, 0},
		{"single row: high clamps to 0", 5, 1, 0},
	}
	for _, c := range cases {
		st := &uiState{cursor: c.initial, rows: mkRows(c.n)}
		clampCursor(st)
		if st.cursor != c.want {
			t.Errorf("%s: got %d, want %d", c.name, st.cursor, c.want)
		}
	}
}

func TestBuildSelectableRows_OrderTunnelsBeforeJobs(t *testing.T) {
	tunnels := []string{"db", "web"}
	jobs := []*jobs.Record{
		{ID: "j1", Profile: "p"},
		{ID: "j2", Profile: "p"},
	}
	rows := buildSelectableRows(tunnels, jobs, nil)
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	want := []uiRow{
		{kind: "tunnel", id: "db", idx: 0},
		{kind: "tunnel", id: "web", idx: 1},
		{kind: "job", id: "j1", idx: 0},
		{kind: "job", id: "j2", idx: 1},
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("row %d: got %+v, want %+v", i, rows[i], w)
		}
	}
}

func TestVisualWidth_StripsAnsi(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"\x1b[31mhello\x1b[0m", 5},
		{"\x1b[1;33mYES\x1b[0m   no", 8}, // YES + 3 spaces + no = 8 visible cols
		{"美国备用", 8},
		{"\x1b[2m美国\x1b[0m svc", 8},
	}
	for _, c := range cases {
		if got := visualWidth(c.in); got != c.want {
			t.Errorf("visualWidth(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestFitPlainUsesDisplayWidth(t *testing.T) {
	got := fitPlain("美国备用服务", 7)
	if visualWidth(got) > 7 {
		t.Fatalf("fitPlain width=%d, want <= 7: %q", visualWidth(got), got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("fitPlain should add ellipsis, got %q", got)
	}
}

func TestVisibleListRangeTracksCursor(t *testing.T) {
	cases := []struct {
		total, cursor, limit int
		wantStart, wantEnd   int
	}{
		{3, 0, 5, 0, 3},
		{10, 0, 5, 0, 5},
		{10, 4, 5, 2, 7},
		{10, 9, 5, 5, 10},
		{10, -1, 5, 0, 5},
		{10, 99, 5, 5, 10},
	}
	for _, c := range cases {
		start, end := visibleListRange(c.total, c.cursor, c.limit)
		if start != c.wantStart || end != c.wantEnd {
			t.Errorf("visibleListRange(%d,%d,%d)=(%d,%d), want (%d,%d)",
				c.total, c.cursor, c.limit, start, end, c.wantStart, c.wantEnd)
		}
	}
}

func TestPanelJobsShowsScrollableWindow(t *testing.T) {
	js := make([]*jobs.Record, 9)
	for i := range js {
		js[i] = &jobs.Record{
			ID:      "job-000" + string(rune('0'+i)),
			Profile: "prod",
			Pid:     1000 + i,
			Started: "2026-05-13T10:00:00",
			Cmd:     "cmd-" + string(rune('0'+i)),
		}
	}
	st := &uiState{
		rows:      buildSelectableRows(nil, js, nil),
		focusPane: "job",
		jobCursor: 7,
	}
	clampCursor(st)

	withDashboardWidth(96, func() {
		var sb strings.Builder
		panelJobs(&sb, js, st)
		out := sb.String()
		assertDashboardLineWidths(t, out, 96)
		if !strings.Contains(out, "SHOWING 5-9/9") {
			t.Fatalf("jobs panel missing range hint: %q", out)
		}
		if strings.Contains(out, "COMMAND") || strings.Contains(out, "cmd-") {
			t.Fatalf("jobs panel should not render command column: %q", out)
		}
		for _, want := range []string{"job-0004", "job-0005", "job-0006", "job-0007", "job-0008"} {
			if !strings.Contains(out, want) {
				t.Fatalf("jobs panel missing visible row %q: %q", want, out)
			}
		}
		for _, hidden := range []string{"job-0000", "job-0001", "job-0002", "job-0003"} {
			if strings.Contains(out, hidden) {
				t.Fatalf("jobs panel included hidden row %q: %q", hidden, out)
			}
		}
	})
}

func TestMoveFocusedRowWrapsWithinPane(t *testing.T) {
	rows := buildSelectableRows([]string{"tun"}, []*jobs.Record{{ID: "job-a"}, {ID: "job-b"}}, []mcplog.ToolCall{{Name: "tool-a"}})
	st := &uiState{rows: rows, focusPane: "job", jobCursor: 1}
	clampCursor(st)

	moveFocusedRow(st, 1)
	if st.focusPane != "job" || st.jobCursor != 0 {
		t.Fatalf("down should wrap inside jobs pane, got focus=%q jobCursor=%d", st.focusPane, st.jobCursor)
	}
	moveFocusedRow(st, -1)
	if st.focusPane != "job" || st.jobCursor != 1 {
		t.Fatalf("up should wrap inside jobs pane, got focus=%q jobCursor=%d", st.focusPane, st.jobCursor)
	}
}

func TestTunnelActionsArmConfirmationsInUI(t *testing.T) {
	cfg := &config.Config{
		Tunnels: map[string]*config.TunnelDef{
			"db": {Type: "local", Spec: "15432:localhost:5432"},
		},
	}
	st := &uiState{
		rows:         buildSelectableRows([]string{"db"}, nil, nil),
		focusPane:    "tunnel",
		tunnelCursor: 0,
	}
	clampCursor(st)
	row := st.currentRow()

	armConfirmFor(st, row, nil, []string{"db"}, cfg, ' ')
	if st.confirm != nil {
		if !strings.Contains(st.confirm.title, "tunnel ") || !strings.Contains(st.confirm.title, " db") {
			t.Fatalf("space should arm tunnel up/down confirmation, got %q", st.confirm.title)
		}
	} else {
		t.Fatalf("space should arm tunnel up/down confirmation")
	}

	st.confirm = nil
	armConfirmFor(st, row, nil, []string{"db"}, cfg, 'x')
	if st.confirm == nil {
		t.Fatalf("x should arm tunnel remove confirmation")
	}
	if !strings.Contains(st.confirm.title, "remove tunnel db") {
		t.Fatalf("x should arm tunnel remove confirmation, got %q", st.confirm.title)
	}
}

func TestDemoActionsArmAndAcknowledgeWithoutRealSideEffects(t *testing.T) {
	cfg, js, mcp := demoDashboardData(nil)
	tunnelNames := sortedTunnelNames(cfg)
	st := &uiState{
		rows:             buildSelectableRows(tunnelNames, js, mcp.RecentTools),
		focusPane:        "tunnel",
		tunnelCursor:     0,
		demoMode:         true,
		src:              NewDemoSource(nil),
		snapTunnelActive: map[string]daemon.TunnelInfo{},
	}
	clampCursor(st)

	armConfirmFor(st, st.currentRow(), js, tunnelNames, cfg, ' ')
	if st.confirm == nil {
		t.Fatalf("demo space should arm tunnel confirmation")
	}
	msg, err := st.confirm.action()
	if err != nil {
		t.Fatalf("demo tunnel action should not touch real daemon: %v", err)
	}
	if !strings.Contains(msg, "demo tunnel") {
		t.Fatalf("demo tunnel action returned %q", msg)
	}

	st.confirm = nil
	st.focusPane = "job"
	st.jobCursor = 0
	clampCursor(st)
	armConfirmFor(st, st.currentRow(), js, tunnelNames, cfg, 'k')
	if st.confirm == nil {
		t.Fatalf("demo k should arm job confirmation")
	}
	msg, err = st.confirm.action()
	if err != nil {
		t.Fatalf("demo job action should not touch real remote process: %v", err)
	}
	if !strings.Contains(msg, "demo kill") {
		t.Fatalf("demo job action returned %q", msg)
	}
}

func TestConfirmPopupIsVisibleInClippedDashboard(t *testing.T) {
	cfg, js, mcp := demoDashboardData(nil)
	tunnelNames := sortedTunnelNames(cfg)
	st := &uiState{
		rows:      buildSelectableRows(tunnelNames, js, mcp.RecentTools),
		focusPane: "job",
		jobCursor: 0,
		confirm: &uiConfirm{
			title: "kill demo-job-0001",
			body:  []string{"demo confirmation"},
		},
		snapMCP: mcp,
	}
	clampCursor(st)

	withDashboardWidth(96, func() {
		out := renderDashboardWithMCP(cfg, js, tunnelNames, st, mcp)
		frame := altScreenFrame(out, 16)
		if !strings.Contains(frame, "kill demo-job-0001") || !strings.Contains(frame, "[Y]") {
			t.Fatalf("clipped frame should include confirmation popup: %q", frame)
		}
	})
}

func TestDetailPanelScrollsWithinWindow(t *testing.T) {
	j := &jobs.Record{
		ID:      "job-long-detail",
		Profile: "prod",
		Pid:     4321,
		Started: "2026-05-13T10:00:00",
		Cmd:     strings.Repeat("very-long-command-segment ", 20),
	}
	st := &uiState{
		rows:         buildSelectableRows(nil, []*jobs.Record{j}, nil),
		focusPane:    "job",
		jobCursor:    0,
		detailScroll: 5,
	}
	clampCursor(st)

	withDashboardWidth(50, func() {
		var sb strings.Builder
		renderStretchedDetail(&sb, &config.Config{}, []*jobs.Record{j}, nil, mcplog.Status{}, st, 10)
		out := sb.String()
		lines := splitDashboardLines(out)
		if len(lines) != 10 {
			t.Fatalf("detail lines=%d, want 10: %q", len(lines), out)
		}
		if !strings.Contains(out, "detail scroll") {
			t.Fatalf("detail should show scroll hint: %q", out)
		}
		if !strings.Contains(lines[len(lines)-1], "╰") {
			t.Fatalf("detail should keep bottom border as final line: %q", out)
		}
		assertDashboardLineWidths(t, out, 50)
	})
}

func TestDemoHeaderAndStatusPanel(t *testing.T) {
	st := &uiState{demoMode: true, statusMsg: ansi.Green + "ok" + ansi.Reset}
	withDashboardWidth(96, func() {
		var header strings.Builder
		panelHeader(&header, st)
		if !strings.Contains(header.String(), "DEMO") {
			t.Fatalf("demo header missing marker: %q", header.String())
		}
		var status strings.Builder
		panelStatus(&status, st)
		if !strings.Contains(status.String(), "STATUS") || !strings.Contains(status.String(), "ok") {
			t.Fatalf("status panel missing message: %q", status.String())
		}
		var footer strings.Builder
		panelFooter(&footer, st)
		if strings.Contains(footer.String(), "ok") {
			t.Fatalf("footer should not contain transient status: %q", footer.String())
		}
	})
}

func TestConfirmColorReflectsAction(t *testing.T) {
	if got := confirmColor("tunnel up db"); got != ansi.Bold+ansi.Green {
		t.Fatalf("tunnel up color=%q", got)
	}
	if got := confirmColor("tunnel down db"); got != ansi.Bold+ansi.Yellow {
		t.Fatalf("tunnel down color=%q", got)
	}
	if got := confirmColor("kill job"); got != ansi.Bold+ansi.Red {
		t.Fatalf("kill color=%q", got)
	}
}

func TestPanelMCPShowsScrollableWindow(t *testing.T) {
	now := time.Now()
	tools := make([]mcplog.ToolCall, 9)
	for i := range tools {
		tools[i] = mcplog.ToolCall{
			When: now.Add(-time.Duration(i) * time.Minute),
			Name: "tool-" + string(rune('0'+i)),
			Dur:  "1.0s",
			OK:   true,
			PID:  4242,
		}
	}
	st := &uiState{
		rows:      buildSelectableRows(nil, nil, tools),
		focusPane: "mcp",
		mcpCursor: 7,
	}
	clampCursor(st)
	mcp := mcplog.Status{
		LogExists:   true,
		ActivePIDs:  []int{4242},
		LastActive:  now,
		RecentTools: tools,
	}

	withDashboardWidth(96, func() {
		var sb strings.Builder
		panelMCP(&sb, mcp, st)
		out := sb.String()
		assertDashboardLineWidths(t, out, 96)
		if !strings.Contains(out, "showing 5-9/9") {
			t.Fatalf("mcp panel missing range hint: %q", out)
		}
		for _, want := range []string{"tool-4", "tool-5", "tool-6", "tool-7", "tool-8"} {
			if !strings.Contains(out, want) {
				t.Fatalf("mcp panel missing visible row %q: %q", want, out)
			}
		}
		for _, hidden := range []string{"tool-0", "tool-1", "tool-2", "tool-3"} {
			if strings.Contains(out, hidden) {
				t.Fatalf("mcp panel included hidden row %q: %q", hidden, out)
			}
		}
	})
}

func TestDemoDashboardDataIncludesJobsAndMCP(t *testing.T) {
	cfg, js, mcp := demoDashboardData(nil)
	if cfg == nil || len(cfg.Profiles) == 0 {
		t.Fatal("demo config should include profiles")
	}
	if len(js) < dashboardListRows+1 {
		t.Fatalf("demo jobs=%d, want more than visible list rows", len(js))
	}
	if len(mcp.RecentTools) < dashboardListRows+1 {
		t.Fatalf("demo mcp tools=%d, want more than visible list rows", len(mcp.RecentTools))
	}
	if !mcp.LogExists || len(mcp.ActivePIDs) == 0 {
		t.Fatalf("demo mcp should look active: %+v", mcp)
	}
}

func TestRenderDashboardWidthsMixedLocale(t *testing.T) {
	cfg := &config.Config{
		DefaultProfile: "美国备用",
		Profiles: map[string]*config.Profile{
			"美国备用":  {Host: "backup.example"},
			"美国服务":  {Host: "svc.example"},
			"tokyo": {Host: "tokyo.example"},
		},
		Groups: map[string][]string{
			"亚洲组": {"美国备用", "tokyo"},
		},
		Tunnels: map[string]*config.TunnelDef{
			"数据库": {Type: "local", Spec: "15432:数据库.internal:5432", Profile: "美国备用"},
		},
	}
	js := []*jobs.Record{
		{ID: "202605130001", Profile: "美国备用", Pid: 1234, Started: "2026-05-13T10:00:00", Cmd: "echo hello && sleep 30"},
	}
	st := &uiState{
		cursor:    0,
		rows:      buildSelectableRows(sortedTunnelNames(cfg), js, nil),
		focusPane: "job",
	}
	for _, width := range []int{60, 72, 96, 120} {
		withDashboardWidth(width, func() {
			out := renderDashboard(cfg, js, sortedTunnelNames(cfg), st)
			assertDashboardLineWidths(t, out, width)
			frame := altScreenFrame(out, 12)
			if strings.HasSuffix(frame, "\n") {
				t.Fatalf("altScreenFrame(%d) ended with newline", width)
			}
			if got := len(splitDashboardLines(frame)); got > 12 {
				t.Fatalf("altScreenFrame(%d) lines=%d, want <= 12", width, got)
			}
		})
	}
}

func assertDashboardLineWidths(t *testing.T, out string, width int) {
	t.Helper()
	for i, line := range splitDashboardLines(out) {
		got := visualWidth(line)
		if got > width {
			t.Fatalf("line %d width=%d exceeds %d: %q", i, got, width, line)
		}
	}
}

func TestWriteSideBySidePadsMissingRightWithSpaces(t *testing.T) {
	var sb strings.Builder
	writeSideBySide(&sb, "left\nleft\n", "", 4, 8, 1)

	lines := splitDashboardLines(sb.String())
	if len(lines) != 2 {
		t.Fatalf("line count=%d, want 2: %q", len(lines), sb.String())
	}
	for i, line := range lines {
		if got := visualWidth(line); got != 13 {
			t.Errorf("line %d width=%d, want 13: %q", i, got, line)
		}
		// A column whose box already closed with `╰─╯` (or, in this test,
		// a column that produced no content at all) should not be padded
		// with floating `│ │` walls below it. Plain spaces only.
		if got := strings.Count(line, "│"); got != 0 {
			t.Errorf("line %d should pad missing right with plain spaces, got %d │: %q", i, got, line)
		}
	}
}

func TestWrapText(t *testing.T) {
	cases := []struct {
		in    string
		width int
		want  []string
	}{
		// Short string -- one line.
		{"short", 20, []string{"short"}},
		// Exact width -- one line.
		{"abcde", 5, []string{"abcde"}},
		// Two words that fit on one line.
		{"foo bar", 20, []string{"foo bar"}},
		// Two-word wrap.
		{"foo bar", 5, []string{"foo", "bar"}},
		// Token longer than width -- emitted unbroken.
		{"verylongword", 5, []string{"verylongword"}},
		// Empty input passes through.
		{"", 10, []string{""}},
		// Width <= 0 -- no wrapping.
		{"a b c d", 0, []string{"a b c d"}},
	}
	for _, c := range cases {
		got := wrapText(c.in, c.width)
		if len(got) != len(c.want) {
			t.Errorf("wrapText(%q, %d): %v, want %v", c.in, c.width, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("wrapText(%q, %d)[%d]=%q, want %q", c.in, c.width, i, got[i], c.want[i])
			}
		}
	}
}

func TestFilterJobsSubstringIgnoresCase(t *testing.T) {
	js := []*jobs.Record{
		{ID: "build-api", Profile: "prod", Cmd: "npm run build"},
		{ID: "test-api", Profile: "stage", Cmd: "go test ./..."},
		{ID: "deploy-web", Profile: "prod", Cmd: "bash deploy.sh"},
	}
	cases := []struct {
		query string
		want  []string
	}{
		{"", []string{"build-api", "test-api", "deploy-web"}},
		{"API", []string{"build-api", "test-api"}},
		{"prod", []string{"build-api", "deploy-web"}},
		{"deploy.sh", []string{"deploy-web"}},
		{"nothing", nil},
	}
	for _, c := range cases {
		got := filterJobs(js, c.query)
		ids := make([]string, len(got))
		for i, j := range got {
			ids[i] = j.ID
		}
		if len(ids) != len(c.want) {
			t.Fatalf("filterJobs(%q): %v, want %v", c.query, ids, c.want)
		}
		for i := range ids {
			if ids[i] != c.want[i] {
				t.Errorf("filterJobs(%q)[%d]=%q, want %q", c.query, i, ids[i], c.want[i])
			}
		}
	}
}

func TestFilterModeAppendsAndBackspaces(t *testing.T) {
	st := &uiState{filterMode: true, filterQuery: ""}
	handleFilterKey('a', st, nil)
	handleFilterKey('p', st, nil)
	handleFilterKey('i', st, nil)
	if st.filterQuery != "api" {
		t.Fatalf("filterQuery=%q, want %q", st.filterQuery, "api")
	}
	handleFilterKey('\x7f', st, nil) // Backspace
	if st.filterQuery != "ap" {
		t.Fatalf("after backspace filterQuery=%q, want %q", st.filterQuery, "ap")
	}
	handleFilterKey('\r', st, nil) // Enter commits
	if st.filterMode {
		t.Fatalf("Enter should exit filter mode")
	}
	if st.filterQuery != "ap" {
		t.Fatalf("Enter should preserve query, got %q", st.filterQuery)
	}
}

func TestPersistedUIStateRoundtrip(t *testing.T) {
	src := persistedUIState{
		FocusPane:    "job",
		Cursor:       3,
		TunnelCursor: 1,
		JobCursor:    2,
		McpCursor:    4,
	}
	st := &uiState{}
	applyPersistedState(st, src)
	if st.focusPane != "job" || st.cursor != 3 || st.jobCursor != 2 || st.mcpCursor != 4 || st.tunnelCursor != 1 {
		t.Fatalf("applyPersistedState mismatch: %+v", st)
	}
}

func TestHelpOverlayRendersForFocus(t *testing.T) {
	st := &uiState{focusPane: "job"}
	withDashboardWidth(96, func() {
		var sb strings.Builder
		renderHelpPopup(&sb, st)
		out := sb.String()
		for _, want := range []string{"keyboard shortcuts", "k", "L", "/", "?"} {
			if !strings.Contains(out, want) {
				t.Fatalf("help popup missing %q: %q", want, out)
			}
		}
	})
}

func TestJobLogPreviewAppearsInDetail(t *testing.T) {
	j := &jobs.Record{ID: "job-X", Profile: "p", Pid: 1, Started: "2026-05-13T10:00:00", Cmd: "echo hi"}
	st := &uiState{
		jobLog: &uiJobLog{
			jobID:     "job-X",
			lines:     []string{"line one", "line two"},
			fetchedAt: time.Now(),
		},
	}
	withDashboardWidth(60, func() {
		var sb strings.Builder
		panelJobDetail(&sb, "job detail", j, st)
		out := sb.String()
		if !strings.Contains(out, "line one") || !strings.Contains(out, "line two") {
			t.Fatalf("log lines missing from detail: %q", out)
		}
		if !strings.Contains(out, "LOG") {
			t.Fatalf("log header missing: %q", out)
		}
	})
}

func TestJobLogPreviewOnlyForMatchingJob(t *testing.T) {
	j := &jobs.Record{ID: "job-A", Profile: "p", Pid: 1, Started: "2026-05-13T10:00:00", Cmd: "echo hi"}
	st := &uiState{
		jobLog: &uiJobLog{
			jobID: "job-B",
			lines: []string{"other job's log"},
		},
	}
	withDashboardWidth(60, func() {
		var sb strings.Builder
		panelJobDetail(&sb, "job detail", j, st)
		out := sb.String()
		if strings.Contains(out, "other job's log") {
			t.Fatalf("log preview leaked to wrong job: %q", out)
		}
	})
}

func TestPanelJobsFilterEmptyShowsHint(t *testing.T) {
	st := &uiState{
		focusPane:   "job",
		filterMode:  false,
		filterQuery: "nothing-matches",
		rows:        nil,
	}
	withDashboardWidth(60, func() {
		var sb strings.Builder
		panelJobs(&sb, nil, st)
		out := sb.String()
		if !strings.Contains(out, "no jobs match filter") {
			t.Fatalf("filtered empty panel should show hint: %q", out)
		}
		// boxTop upper-cases the title text, so the query echoes back
		// in uppercase too.
		if !strings.Contains(strings.ToLower(out), "/nothing-matches") {
			t.Fatalf("title should reflect active query: %q", out)
		}
	})
}

func TestTruncID(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", ""},
		{"short", "short"},
		{"12345678", "12345678"},
		{"123456789", "12345678"},
		{"abcdef-fedcba-aaaa", "abcdef-f"},
	}
	for _, c := range cases {
		if got := truncID(c.in); got != c.want {
			t.Errorf("truncID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
