package mcp

import (
	"strings"
	"testing"
)

func TestIsMeaningfulFilter(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"", false},
		{"   ", false},
		{"\t\n", false},
		{".*", false},
		{".+", false},
		{".", false},
		{"[\\s\\S]*", false},
		{"ERROR", true},
		{"foo.bar", true},
		{"^WARN", true},
		{"a", true},
		{"  ERROR  ", true},
		{".*ERROR.*", true},
	}
	for _, c := range cases {
		if got := isMeaningfulFilter(c.in); got != c.want {
			t.Errorf("isMeaningfulFilter(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestRequireStreamFilter_NoFollow(t *testing.T) {
	r := requireStreamFilter("tail", 0, []string{""}, "(example)")
	if r != nil {
		t.Errorf("one-shot call shouldn't gate; got rejection")
	}
}

func TestRequireStreamFilter_AnyFollowWithoutFilterRejected(t *testing.T) {
	for _, follow := range []int{1, 3, 5, 10, 30, 60} {
		r := requireStreamFilter("tail", follow, []string{""}, "(example)")
		if r == nil {
			t.Errorf("follow=%d with empty filter should reject", follow)
		}
	}
}

func TestRequireStreamFilter_FollowWithFilterAllowed(t *testing.T) {
	for _, follow := range []int{1, 30, 60} {
		r := requireStreamFilter("tail", follow, []string{"ERROR"}, "(example)")
		if r != nil {
			t.Errorf("follow=%d with real filter should pass; got reject", follow)
		}
	}
}

func TestRequireStreamFilter_RejectionShape(t *testing.T) {
	r := requireStreamFilter("tail", 30, []string{""}, `{ path: "x", grep: "ERROR" }`)
	if r == nil {
		t.Fatal("expected rejection")
	}
	if !r.IsError {
		t.Error("rejection should be IsError=true")
	}
	if len(r.Content) == 0 ||
		!strings.Contains(r.Content[0].Text, "requires at least one output filter") {
		t.Errorf("missing standard rejection message: %+v", r.Content)
	}
	if !strings.Contains(r.Content[0].Text, `grep: "ERROR"`) {
		t.Error("rejection should include the caller-supplied hint example")
	}
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent not a map: %T", r.StructuredContent)
	}
	if sc["rejected_reason"] != "unbounded_streaming" {
		t.Errorf("rejected_reason=%v", sc["rejected_reason"])
	}
}

func TestRequireStreamFilter_BypassPatternRejected(t *testing.T) {
	for _, follow := range []int{1, 30} {
		r := requireStreamFilter("tail", follow, []string{".*"}, "(example)")
		if r == nil {
			t.Errorf("follow=%d with bypass pattern '.*' should still reject", follow)
		}
	}
}

func TestRequireStreamFilter_AnyOneFilterPasses(t *testing.T) {
	r := requireStreamFilter("journal", 30,
		[]string{"", "10 min ago", "", ""}, "(example)")
	if r != nil {
		t.Errorf("any non-empty filter should pass; got reject")
	}
}

func TestRequireStreamFilter_AllEmptyRejected(t *testing.T) {
	r := requireStreamFilter("journal", 30,
		[]string{"", "", "", ""}, "(example)")
	if r == nil {
		t.Error("all-empty filters with long follow should reject")
	}
}

func TestClampLines(t *testing.T) {
	cases := []struct {
		asked, max, want int
		clamped          bool
	}{
		{0, 1000, 0, false},
		{50, 1000, 50, false},
		{1000, 1000, 1000, false},
		{1001, 1000, 1000, true},
		{1_000_000, 2000, 2000, true},
	}
	for _, c := range cases {
		got, cl := clampLines(c.asked, c.max)
		if got != c.want || cl != c.clamped {
			t.Errorf("clampLines(%d, %d) = (%d, %v), want (%d, %v)",
				c.asked, c.max, got, cl, c.want, c.clamped)
		}
	}
}

func TestRejectUnfiltered_RejectsBareCat(t *testing.T) {
	label, msg := rejectUnfiltered("cat /var/log/syslog")
	if label != "cat" {
		t.Errorf("label=%q, want cat", label)
	}
	if !strings.Contains(msg, "head -n") {
		t.Errorf("message should suggest head -n: %q", msg)
	}
}

func TestRejectUnfiltered_CatWithPipeAllowed(t *testing.T) {
	cases := []string{
		"cat /var/log/syslog | head -n 100",
		"cat foo | grep ERROR",
		"cat foo | wc -l",
		"cat foo | tail",
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label != "" {
			t.Errorf("expected pass: %q got reject %q", c, label)
		}
	}
}

func TestRejectUnfiltered_HeadAlwaysOK(t *testing.T) {
	cases := []string{
		"head /var/log/syslog",
		"head -n 50 file",
		"head -c 1024 file",
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label != "" {
			t.Errorf("head should pass: %q got reject %q", c, label)
		}
	}
}

func TestRejectUnfiltered_RejectsDmesg(t *testing.T) {
	label, _ := rejectUnfiltered("dmesg")
	if label != "dmesg" {
		t.Errorf("dmesg should reject, got label=%q", label)
	}
}

func TestRejectUnfiltered_DmesgWithFilterOK(t *testing.T) {
	cases := []string{
		"dmesg | tail -n 100",
		"dmesg | grep error",
		"dmesg | wc -l",
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label != "" {
			t.Errorf("piped dmesg should pass: %q got %q", c, label)
		}
	}
}

func TestRejectUnfiltered_BareJournalctlRejected(t *testing.T) {
	label, msg := rejectUnfiltered("journalctl")
	if label != "journalctl" {
		t.Errorf("bare journalctl should reject, got %q", label)
	}
	if !strings.Contains(msg, "journal { unit:") {
		t.Errorf("should suggest the MCP tool: %q", msg)
	}
}

func TestRejectUnfiltered_JournalctlWithFilterOK(t *testing.T) {
	cases := []string{
		"journalctl -u nginx",
		"journalctl --since '10 min ago'",
		"journalctl -p err",
		"journalctl -n 100",
		"journalctl -g ERROR",
		"journalctl -k",
		"journalctl -f -u nginx",
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label != "" {
			t.Errorf("filtered journalctl should pass: %q got %q", c, label)
		}
	}
}

func TestRejectUnfiltered_BareFindRejected(t *testing.T) {
	label, _ := rejectUnfiltered("find /")
	if label != "find" {
		t.Errorf("bare find / should reject, got %q", label)
	}
}

func TestRejectUnfiltered_FindWithFilterOK(t *testing.T) {
	cases := []string{
		"find /var/log -name '*.log'",
		"find / -maxdepth 2",
		"find /etc -type f",
		"find /home -newer /tmp/marker",
		"find / -mtime -1",
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label != "" {
			t.Errorf("filtered find should pass: %q got %q", c, label)
		}
	}
}

func TestRejectUnfiltered_FindRelativePathNotChecked(t *testing.T) {
	if label, _ := rejectUnfiltered("find ."); label != "" {
		t.Errorf("find . should pass, got reject %q", label)
	}
}

func TestRejectUnfiltered_QuotedContentDoesntFalsePositive(t *testing.T) {
	cases := []string{
		`echo "cat /etc/passwd"`,
		`echo 'dmesg'`,
		`grep "journalctl" /tmp/foo`,
		`logger "cat is in this quoted string"`,
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label != "" {
			t.Errorf("quoted content should not reject: %q got %q", c, label)
		}
	}
}

func TestRejectUnfiltered_PipeChainsCount(t *testing.T) {
	cases := []string{
		"cat foo | grep -i error | tail -n 50",
		"dmesg | grep -i error | head -n 20",
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label != "" {
			t.Errorf("pipe chain with limiter should pass: %q got %q", c, label)
		}
	}
}

func TestRejectUnfiltered_AfterSemicolonStillCaught(t *testing.T) {
	cases := []string{
		"cd /tmp && cat foo",
		"true; cat /etc/hosts",
		"echo hi || dmesg",
	}
	for _, c := range cases {
		if label, _ := rejectUnfiltered(c); label == "" {
			t.Errorf("post-separator unbounded cmd should reject: %q", c)
		}
	}
}

func TestStripShellQuotedContent(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{`cat /etc/passwd`, `cat /etc/passwd`},
		{`echo "cat /etc/passwd"`, `echo "               "`},
		{`echo 'cat foo'`, `echo '       '`},
		{`grep "x" file`, `grep " " file`},
		{`echo "a\"b"`, `echo "  "`},
	}
	for _, c := range cases {
		got := stripShellQuotedContent(c.in)
		if got != c.want {
			t.Errorf("strip(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRejectUnfilteredMessage_StructuredShape(t *testing.T) {
	r := rejectUnfilteredMessage("cat", "use head -n N instead")
	if !r.IsError {
		t.Error("rejection should be IsError=true")
	}
	sc, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent not a map: %T", r.StructuredContent)
	}
	if sc["rejected_reason"] != "unbounded_output" {
		t.Errorf("rejected_reason=%v", sc["rejected_reason"])
	}
	if sc["pattern"] != "cat" {
		t.Errorf("pattern=%v", sc["pattern"])
	}
}
