package mcp

import (
	"strings"
	"testing"

	"srv/internal/config"
)

// run and detach must both reject an empty OR whitespace-only command
// (same contract). Regression: run used `if cmd == ""`, so a "   "
// command slipped through to a silent no-op while detach behaved
// differently. The check runs before any profile/SSH work, so a zero
// Config and no profile are fine.
func TestRunDetach_RejectBlankCommand(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t\n "} {
		r := handleRun(map[string]any{"command": blank}, &config.Config{}, "")
		if !r.IsError || len(r.Content) == 0 ||
			!strings.Contains(r.Content[0].Text, "command is required") {
			t.Errorf("handleRun(%q) = %+v; want 'command is required' error", blank, r)
		}
		d := handleDetach(map[string]any{"command": blank}, &config.Config{}, "")
		if !d.IsError || len(d.Content) == 0 ||
			!strings.Contains(d.Content[0].Text, "command is required") {
			t.Errorf("handleDetach(%q) = %+v; want 'command is required' error", blank, d)
		}
	}
}
