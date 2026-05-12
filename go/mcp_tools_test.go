package main

import (
	"srv/internal/jobs"
	"strings"
	"testing"
)

// runRiskyMatch — high-confidence catches AND the false-positive cases
// the substring-based predecessor used to fire on.
func TestRunRiskyMatch_Catches(t *testing.T) {
	cases := []struct {
		cmd  string
		want string // pattern name; "" = should NOT match
	}{
		// canonical destructive forms
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
		{"rm foo.txt", ""},    // no -rf
		{"rm -r somedir", ""}, // no -f
		{"rm -f foo", ""},     // no -r
		{"git checkout -f main", ""},
		{"farm -rfast", ""}, // word-boundary protection
		{"docker run --rm -ti alpine sh", ""},
		{"ls /dev/sda", ""}, // listing isn't redirecting
	}
	for _, tc := range cases {
		got := runRiskyMatch(tc.cmd)
		if got != tc.want {
			t.Errorf("runRiskyMatch(%q) = %q, want %q", tc.cmd, got, tc.want)
		}
	}
}

// quoted strings shouldn't trigger -- this was the main pain point with
// the old substring matcher (echo "rm -rf" would flag).
func TestRunRiskyMatch_QuotedIsIgnored(t *testing.T) {
	cases := []string{
		`echo "rm -rf /tmp"`,
		`echo 'rm -rf /tmp'`,
		`grep "drop database" log.txt`,
		`printf '%s\n' "shutdown -h now"`,
		`echo "be careful with mkfs.ext4"`,
	}
	for _, c := range cases {
		if got := runRiskyMatch(c); got != "" {
			t.Errorf("runRiskyMatch(%q) = %q, want empty (string-literal)", c, got)
		}
	}
}

// command-then-quoted: real command at start, quoted noise after must
// still trigger. (Regression guard against an over-eager quote-stripping
// rewrite.)
func TestRunRiskyMatch_RealCommandWithQuotedNoise(t *testing.T) {
	cmd := `rm -rf /tmp/foo && echo "done"`
	if got := runRiskyMatch(cmd); got != "rm -rf" {
		t.Errorf("runRiskyMatch(%q) = %q, want rm -rf", cmd, got)
	}
}

func TestIsInsideQuotes(t *testing.T) {
	// pos points at the 'X' inside the string; we check whether that
	// offset is inside a quoted region.
	cases := []struct {
		s    string
		pos  int
		want bool
	}{
		{`echo X foo`, 5, false},
		{`echo "X" foo`, 6, true},
		{`echo 'X' foo`, 6, true},
		{`echo "a" X foo`, 9, false}, // outside the closed quote
		{`echo "a\"X" foo`, 8, true}, // escaped " stays inside double
		// Backslash inside single quotes is literal, so `'a\'` closes
		// the single-quoted region at the second '. X at pos 9 is
		// therefore outside any quote.
		{`echo 'a\'X bar`, 9, false},
	}
	for _, tc := range cases {
		if got := isInsideQuotes(tc.s, tc.pos); got != tc.want {
			t.Errorf("isInsideQuotes(%q, %d) = %v, want %v", tc.s, tc.pos, got, tc.want)
		}
	}
}

func TestRunRejectSync(t *testing.T) {
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
		{"tail -F app.log", false}, // -F is not -f / --follow
		{"tail -n 100 file", false},
		{"watch -n 1 ls", true},
		{"journalctl -u nginx -f", true},
		{"journalctl -u nginx --since 1h", false},
		{"ls -la", false},
		{"", false},
	}
	for _, tc := range cases {
		got := runRejectSync(tc.cmd)
		if (got != "") != tc.shouldFail {
			t.Errorf("runRejectSync(%q) = %q, shouldFail=%v", tc.cmd, got, tc.shouldFail)
		}
	}
}

func TestBuildMCPRunText_NoTruncation(t *testing.T) {
	res := &RunCaptureResult{
		Stdout:   "hello\nworld",
		Stderr:   "",
		ExitCode: 0,
	}
	text, truncated := buildMCPRunText(res, "/home/user")
	if truncated != 0 {
		t.Errorf("truncated = %d, want 0", truncated)
	}
	if !strings.Contains(text, "hello\nworld") {
		t.Errorf("missing stdout: %q", text)
	}
	if !strings.Contains(text, "[exit 0 cwd /home/user]") {
		t.Errorf("missing footer: %q", text)
	}
}

func TestBuildMCPRunText_StderrFenced(t *testing.T) {
	res := &RunCaptureResult{
		Stdout:   "out",
		Stderr:   "err",
		ExitCode: 1,
	}
	text, _ := buildMCPRunText(res, "/tmp")
	if !strings.Contains(text, "--- stderr ---") {
		t.Errorf("missing stderr fence: %q", text)
	}
	if !strings.Contains(text, "[exit 1 cwd /tmp]") {
		t.Errorf("missing footer: %q", text)
	}
}

func TestBuildMCPRunText_TruncatesAtCap(t *testing.T) {
	big := strings.Repeat("a", mcpRunTextMax+1234)
	res := &RunCaptureResult{Stdout: big, ExitCode: 0}
	text, truncated := buildMCPRunText(res, "/x")
	if truncated != 1234 {
		t.Errorf("truncated = %d, want 1234", truncated)
	}
	if !strings.Contains(text, "1234 bytes truncated") {
		t.Errorf("missing truncation marker: %q", text[:200])
	}
	if !strings.Contains(text, "[exit 0 cwd /x]") {
		t.Errorf("missing footer (truncated case): %q", text[len(text)-80:])
	}
}

func TestMcpDetachedResult(t *testing.T) {
	rec := &jobs.Record{
		ID:      "abc123",
		Profile: "prod",
		Pid:     42,
		Log:     "~/.srv-jobs/abc123.log",
		Cwd:     "/srv/app",
		Started: "2026-05-11T10:00:00Z",
	}
	r := rec
	tr := mcpDetachedResult(r)
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

func TestMcpToolRegistry_NoDriftBetweenDefsAndDispatch(t *testing.T) {
	defs := mcpToolDefs()
	if len(defs) != len(mcpToolMap) {
		t.Fatalf("defs (%d) and map (%d) disagree on tool count", len(defs), len(mcpToolMap))
	}
	for _, d := range defs {
		if _, ok := mcpToolMap[d.Name]; !ok {
			t.Errorf("tool %q in defs but not in map", d.Name)
		}
	}
}
