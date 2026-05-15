package mcp

import (
	"srv/internal/jobs"
	"srv/internal/sshx"
	"strings"
	"testing"
)

// riskyMatch -- high-confidence catches AND the false-positive cases
// the substring-based predecessor used to fire on.
func TestRiskyMatch_Catches(t *testing.T) {
	cases := []struct {
		cmd  string
		want string // pattern name; "" = should NOT match
	}{
		{"rm -rf /tmp/foo", "rm -rf"},
		{"rm -fr /tmp/foo", "rm -rf"},
		{"rm -Rf /var", "rm -rf"},
		{"rm --recursive --force /etc", "rm -rf"},
		{"rm --force --recursive /", "rm -rf"},
		{"rm -r --force somedir", "rm -rf"},
		{"sudo rm -rf /", "rm -rf"},
		{"cd /tmp && rm -rf cache", "rm -rf"},

		{"dd of=/dev/sda bs=1M", "dd of=/..."},
		{"dd if=/dev/zero of=foo", "dd of=/..."},
		{"dd if=/dev/urandom of=/dev/sdb", "dd of=/..."},

		{"mkfs.ext4 /dev/sdb1", "mkfs"},
		{"mkfs /dev/sdb", "mkfs"},

		{"shutdown -h now", "shutdown"},
		{"sudo reboot", "reboot"},
		{"halt -p", "halt"},
		{"poweroff", "poweroff"},

		{"DROP DATABASE prod", "drop database"},
		{"drop table users", "drop database"},
		{"TRUNCATE TABLE sessions", "truncate table"},
		{"truncate --size=0 file", "truncate table"},

		{":> /etc/passwd", ":>/"},
		{": > /home/user/.bashrc", ":>/"},

		{"chattr -i /etc/shadow", "chattr -i"},
		{"echo x > /dev/sda1", "> /dev/disk"},
		{"cat foo > /dev/nvme0n1", "> /dev/disk"},

		// safe / unrelated commands
		{"ls -la", ""},
		{"echo hello", ""},
		{"rm foo.txt", ""},
		{"rm -r somedir", ""},
		{"rm -f foo", ""},
		{"git checkout -f main", ""},
		{"farm -rfast", ""},
		{"docker run --rm -ti alpine sh", ""},
		{"ls /dev/sda", ""},
	}
	for _, tc := range cases {
		got := riskyMatch(tc.cmd)
		if got != tc.want {
			t.Errorf("riskyMatch(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// Quoted strings shouldn't trigger -- this was the main pain point
// with the old substring matcher (echo "rm -rf" would flag).
func TestRiskyMatch_QuotedIsIgnored(t *testing.T) {
	cases := []string{
		`echo "rm -rf /tmp"`,
		`echo 'rm -rf /tmp'`,
		`grep "drop database" log.txt`,
		`printf '%s\n' "shutdown -h now"`,
		`echo "be careful with mkfs.ext4"`,
	}
	for _, c := range cases {
		if got := riskyMatch(c); got != "" {
			t.Errorf("riskyMatch(%q) = %q, want empty (string-literal)", c, got)
		}
	}
}

// Real command at start, quoted noise after must still trigger.
func TestRiskyMatch_RealCommandWithQuotedNoise(t *testing.T) {
	cmd := `rm -rf /tmp/foo && echo "done"`
	if got := riskyMatch(cmd); got != "rm -rf" {
		t.Errorf("riskyMatch(%q) = %q, want rm -rf", cmd, got)
	}
}

// Command substitution inside double quotes still executes -- the
// guard must trip on the inner risky pattern even though the outer
// argument is double-quoted. Was a false-negative until codePositions
// learned to track $(...) and backticks.
func TestRiskyMatch_CmdSubstitutionInQuotes(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		// $(...) inside double quotes: shell evaluates, so trip.
		{`echo "$(rm -rf /tmp/foo)"`, "rm -rf"},
		{`x="$(mkfs.ext4 /dev/sdb)"; echo done`, "mkfs"},
		// Backticks inside double quotes: same story.
		{"echo \"`rm -rf /tmp/foo`\"", "rm -rf"},
		// Backticks at command position.
		{"x=`shutdown -h now`", "shutdown"},
		// Single quotes ARE literal, even with $(...) inside.
		{`echo '$(rm -rf foo)'`, ""},
		// Nested $() depth -- innermost trips the gate.
		{`echo $(echo $(reboot))`, "reboot"},
	}
	for _, c := range cases {
		if got := riskyMatch(c.cmd); got != c.want {
			t.Errorf("riskyMatch(%q) = %q, want %q", c.cmd, got, c.want)
		}
	}
}

func TestIsInsideQuotes(t *testing.T) {
	cases := []struct {
		s    string
		pos  int
		want bool
	}{
		{`echo X foo`, 5, false},
		{`echo "X" foo`, 6, true},
		{`echo 'X' foo`, 6, true},
		{`echo "a" X foo`, 9, false},
		{`echo "a\"X" foo`, 8, true},
		{`echo 'a\'X bar`, 9, false},
	}
	for _, tc := range cases {
		if got := isInsideQuotes(tc.s, tc.pos); got != tc.want {
			t.Errorf("isInsideQuotes(%q, %d) = %v, want %v", tc.s, tc.pos, got, tc.want)
		}
	}
}

func TestRejectSync(t *testing.T) {
	cases := []struct {
		cmd        string
		shouldFail bool
	}{
		{"sleep 1", false},
		{"sleep 5", false},
		{"sleep 6", true},
		{"sleep 30", true},
		{"sleep 12.5", true},
		{"tail -f /var/log/syslog", true},
		{"tail -F app.log", false},
		{"tail -n 100 file", false},
		{"watch -n 1 ls", true},
		{"journalctl -u nginx -f", true},
		{"journalctl -u nginx --since 1h", false},
		{"ls -la", false},
		{"", false},
	}
	for _, tc := range cases {
		got := rejectSync(tc.cmd)
		if (got != "") != tc.shouldFail {
			t.Errorf("rejectSync(%q) = %q, shouldFail=%v", tc.cmd, got, tc.shouldFail)
		}
	}
}

func TestBuildRunText_NoTruncation(t *testing.T) {
	res := &sshx.RunCaptureResult{
		Stdout:   "hello\nworld",
		Stderr:   "",
		ExitCode: 0,
	}
	text, truncated := buildRunText(res, "/home/user")
	if truncated != 0 {
		t.Errorf("truncated = %d, want 0", truncated)
	}
	if !strings.Contains(text, "hello\nworld") {
		t.Errorf("missing stdout: %q", text)
	}
	// ExitCode 0 now renders as "ok" rather than "exit 0" so MCP
	// clients with pattern-matching log analysis don't read the
	// word "exit" as a failure signal on every successful command.
	if !strings.Contains(text, "[ok cwd /home/user]") {
		t.Errorf("missing footer: %q", text)
	}
}

func TestBuildRunText_StderrFenced(t *testing.T) {
	res := &sshx.RunCaptureResult{
		Stdout:   "out",
		Stderr:   "err",
		ExitCode: 1,
	}
	text, _ := buildRunText(res, "/tmp")
	if !strings.Contains(text, "--- stderr ---") {
		t.Errorf("missing stderr fence: %q", text)
	}
	if !strings.Contains(text, "[exit 1 cwd /tmp]") {
		t.Errorf("missing footer: %q", text)
	}
}

func TestBuildRunText_TruncatesAtCap(t *testing.T) {
	big := strings.Repeat("a", runTextMax+1234)
	res := &sshx.RunCaptureResult{Stdout: big, ExitCode: 0}
	text, truncated := buildRunText(res, "/x")
	if truncated != 1234 {
		t.Errorf("truncated = %d, want 1234", truncated)
	}
	if !strings.Contains(text, "1234 bytes truncated") {
		t.Errorf("missing truncation marker: %q", text[:200])
	}
	if !strings.Contains(text, "[ok cwd /x]") {
		t.Errorf("missing footer (truncated case): %q", text[len(text)-80:])
	}
}

func TestDetachedResult(t *testing.T) {
	rec := &jobs.Record{
		ID:      "abc123",
		Profile: "prod",
		Pid:     42,
		Log:     "~/.srv-jobs/abc123.log",
		Cwd:     "/srv/app",
		Started: "2026-05-11T10:00:00Z",
	}
	r := rec
	tr := detachedResult(r)
	if tr.IsError {
		t.Errorf("IsError = true, want false")
	}
	if len(tr.Content) == 0 || !strings.Contains(tr.Content[0].Text, "abc123") {
		t.Errorf("text content missing job id: %+v", tr.Content)
	}
	info, ok := tr.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent not map[string]any: %T", tr.StructuredContent)
	}
	if info["job_id"] != "abc123" {
		t.Errorf("job_id mismatch: %v", info["job_id"])
	}
	if info["status"] != "running" {
		t.Errorf("status = %v, want running", info["status"])
	}
	if info["next_tool"] != "wait_job" {
		t.Errorf("next_tool = %v, want wait_job", info["next_tool"])
	}
}

func TestRegistry_NoDriftBetweenDefsAndDispatch(t *testing.T) {
	defs := toolDefs()
	if len(defs) != len(toolMap) {
		t.Fatalf("defs (%d) and map (%d) disagree on tool count", len(defs), len(toolMap))
	}
	for _, d := range defs {
		if _, ok := toolMap[d.Name]; !ok {
			t.Errorf("tool %q in defs but not in map", d.Name)
		}
	}
}
