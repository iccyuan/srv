package mcp

import (
	"strings"
	"testing"
)

// lineBufferedFollow must line-buffer the follow command via stdbuf
// when available, with a plain fallback so macOS/*BSD/busybox (no
// stdbuf) keep working (degraded, not broken). Without line buffering
// a chatty tail -F / journalctl -f block-buffers to the SSH pipe and
// every captured line but the last is lost when the follow deadline
// hard-closes the connection -- the streaming data-loss bug.
func TestLineBufferedFollow(t *testing.T) {
	cmd := "tail -F -n 50 '/var/log/auth.log'"
	got := lineBufferedFollow(cmd)

	// Portability gate present.
	if !strings.Contains(got, "command -v stdbuf >/dev/null 2>&1") {
		t.Errorf("missing `command -v stdbuf` gate: %s", got)
	}
	// Linux/GNU path actually line-buffers the command.
	if !strings.Contains(got, "then stdbuf -oL "+cmd) {
		t.Errorf("stdbuf branch must line-buffer the exact command: %s", got)
	}
	// macOS/*BSD fallback still runs the command (degraded, no error).
	if !strings.Contains(got, "else "+cmd) {
		t.Errorf("fallback branch must run the plain command: %s", got)
	}
	// Single shell `if ... fi` so RunStream's shell wrap handles it.
	if !strings.HasPrefix(got, "if ") || !strings.HasSuffix(got, "; fi") {
		t.Errorf("must be one `if ...; fi` statement: %s", got)
	}
}
