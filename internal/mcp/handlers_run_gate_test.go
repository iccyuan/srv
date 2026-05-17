package mcp

import (
	"srv/internal/config"
	"strings"
	"testing"
)

// These are handler-level tests: they drive the real handleRunGroup /
// handleRun entry points (the functions wired into the MCP registry),
// not the gate predicates in isolation. group.Run validates membership
// up front and never dials when a group is missing, and every gate
// fires before any SSH, so the whole reject path is offline and
// platform-independent -- the gates are pure regex/string matching
// with no exec, no path separators, no runtime.GOOS branching, so the
// behaviour is identical on Linux, macOS, and any other Unix.

// structured pulls the StructuredContent of a result as a map, failing
// the test if it isn't one (every gate-reject result carries one).
func structured(t *testing.T, r toolResult) map[string]any {
	t.Helper()
	m, ok := r.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("StructuredContent is %T, want map[string]any (result text: %q)",
			r.StructuredContent, r.Content[0].Text)
	}
	return m
}

func resultText(r toolResult) string {
	if len(r.Content) == 0 {
		return ""
	}
	return r.Content[0].Text
}

// TestHandleRunGroup_RejectsUnfilteredSources is the regression test
// for the bug: run_group used to skip the rejectUnfiltered gate that
// `run` enforces, so an unbounded source fanned out across every group
// member -- the token blow-up multiplied by group size. Each pattern
// rejectUnfiltered knows about must now be caught through the
// run_group handler too.
func TestHandleRunGroup_RejectsUnfilteredSources(t *testing.T) {
	// NOTE: the gate matches sources in command position only (start
	// of string or after ; & |), so `sudo dmesg` / `sudo cat f` are
	// deliberately NOT listed -- that's a pre-existing blind spot
	// shared with `run`, not something run_group should reject extra.
	cases := []struct {
		cmd   string
		label string
	}{
		{"cat /var/log/syslog", "cat"},
		{"cat /etc/passwd", "cat"},
		{"dmesg", "dmesg"},
		{"journalctl", "journalctl"},
		{"journalctl --no-pager", "journalctl"},
		{"find /", "find"},
		{"find /var/log", "find"},
		// fan-out-specific shape: a per-host `cat` chained after a
		// harmless command still dumps a whole file on every member.
		{"hostname; cat /var/log/messages", "cat"},
	}
	for _, tc := range cases {
		// cfg is empty on purpose: the gate must fire BEFORE
		// group.Run, so the group never has to exist.
		r := handleRunGroup(map[string]any{
			"group":   "web",
			"command": tc.cmd,
		}, &config.Config{}, "")

		if !r.IsError {
			t.Errorf("%q: expected IsError, got success", tc.cmd)
			continue
		}
		if !strings.HasPrefix(resultText(r), "rejected:") {
			t.Errorf("%q: text %q does not start with \"rejected:\"", tc.cmd, resultText(r))
		}
		m := structured(t, r)
		if m["rejected_reason"] != "unbounded_output" {
			t.Errorf("%q: rejected_reason=%v, want unbounded_output", tc.cmd, m["rejected_reason"])
		}
		if m["pattern"] != tc.label {
			t.Errorf("%q: pattern=%v, want %q", tc.cmd, m["pattern"], tc.label)
		}
	}
}

// TestHandleRunGroup_AllowsBoundedSources verifies the gate does not
// over-reject: a downstream slicer / native limit / filter flag must
// pass the gate. Proof that it passed: the call reaches group.Run and
// fails with "group ... not found" (offline, no SSH) instead of the
// unbounded rejection. This mirrors every "OK" case the gate-unit
// tests cover, but exercised through the real handler.
func TestHandleRunGroup_AllowsBoundedSources(t *testing.T) {
	ok := []string{
		"cat /var/log/syslog | head -n 100",
		"cat foo | grep ERROR",
		"dmesg | tail -n 100",
		"dmesg | grep -i error",
		"journalctl -u nginx -n 100",
		"journalctl --since '10 min ago' -p err",
		"find /var/log -maxdepth 2 -name '*.log'",
		"find / -type f -newer /tmp/ref",
		"tail -n 50 /var/log/app.log",
		"head -n 20 /etc/hosts",
		"grep ERROR /var/log/app.log",
		"ls -la /var/log",
	}
	for _, cmd := range ok {
		r := handleRunGroup(map[string]any{
			"group":   "ghost-group",
			"command": cmd,
		}, &config.Config{}, "")

		// Passed the gate -> hit group resolution. Must NOT be the
		// unbounded rejection (which carries StructuredContent).
		if r.StructuredContent != nil {
			t.Errorf("%q: gate wrongly rejected (structured=%v)", cmd, r.StructuredContent)
			continue
		}
		if strings.HasPrefix(resultText(r), "rejected:") {
			t.Errorf("%q: gate wrongly rejected: %q", cmd, resultText(r))
			continue
		}
		if !strings.Contains(resultText(r), "not found") {
			t.Errorf("%q: expected to reach group resolution (\"not found\"), got %q",
				cmd, resultText(r))
		}
	}
}

// TestHandleRunGroup_RejectSyncParity locks in that the long-blocking
// sync gate is enforced for the fan-out path too (a `sleep 30` across
// a group wedges every connection in parallel).
func TestHandleRunGroup_RejectSyncParity(t *testing.T) {
	r := handleRunGroup(map[string]any{
		"group":   "web",
		"command": "sleep 30",
	}, &config.Config{}, "")
	if !r.IsError || !strings.Contains(resultText(r), "rejected:") {
		t.Fatalf("sleep 30 not rejected by run_group: %q", resultText(r))
	}
	if !strings.Contains(resultText(r), "background: true") {
		t.Errorf("rejectSync message should point at the background pattern, got %q", resultText(r))
	}
}

// TestHandleRunGroup_GuardParity verifies the destructive-command
// guard is enforced through run_group (tool name surfaces as
// "run_group"), and that confirm=true bypasses it -- same contract as
// `run`.
func TestHandleRunGroup_GuardParity(t *testing.T) {
	t.Setenv("SRV_GUARD", "1")
	// Use an empty config as the guard rule source so the test does
	// not depend on ~/.srv/config.json; defaults (incl. "rm -rf")
	// apply when Guard is nil.
	SetGuardConfigForTests(&config.Config{})
	t.Cleanup(func() { SetGuardConfigForTests(nil) })

	blocked := handleRunGroup(map[string]any{
		"group":   "web",
		"command": "rm -rf /tmp/x",
	}, &config.Config{}, "")
	if !blocked.IsError {
		t.Fatal("rm -rf not guard-blocked through run_group")
	}
	m := structured(t, blocked)
	if m["guard_blocked"] != true {
		t.Errorf("guard_blocked=%v, want true", m["guard_blocked"])
	}
	if m["tool"] != "run_group" {
		t.Errorf("tool=%v, want run_group", m["tool"])
	}

	// confirm=true must bypass the guard and reach group resolution.
	ok := handleRunGroup(map[string]any{
		"group":   "ghost-group",
		"command": "rm -rf /tmp/x",
		"confirm": true,
	}, &config.Config{}, "")
	if ok.StructuredContent != nil || !strings.Contains(resultText(ok), "not found") {
		t.Errorf("confirm=true should bypass guard and reach group resolution, got %q (structured=%v)",
			resultText(ok), ok.StructuredContent)
	}
}

func TestHandleRunGroup_MissingArgs(t *testing.T) {
	if r := handleRunGroup(map[string]any{"command": "ls"}, &config.Config{}, ""); !r.IsError ||
		!strings.Contains(resultText(r), "group is required") {
		t.Errorf("missing group: got %q", resultText(r))
	}
	if r := handleRunGroup(map[string]any{"group": "web"}, &config.Config{}, ""); !r.IsError ||
		!strings.Contains(resultText(r), "command is required") {
		t.Errorf("missing command: got %q", resultText(r))
	}
}

// TestHandleRun_UnfilteredGatedOnStreamingPath covers the streaming
// concern: `run`'s rejectUnfiltered gate sits BEFORE the
// progressToken() sync/stream split, so a client that asked for
// streaming (progress token present) is gated identically to a sync
// call -- the unbounded source is rejected before any SSH dial, never
// streamed chunk-by-chunk. We assert the reject still fires with the
// progress token set, proving the streaming path inherits the gate.
func TestHandleRun_UnfilteredGatedOnStreamingPath(t *testing.T) {
	cfg := &config.Config{
		Profiles: map[string]*config.Profile{
			"t": {Host: "127.0.0.1", User: "x"},
		},
	}

	// Snapshot + restore the package-global progress token so we
	// don't leak streaming state into other tests in this binary.
	prev := currentProgressToken
	t.Cleanup(func() { currentProgressToken = prev })

	for _, streaming := range []bool{false, true} {
		if streaming {
			currentProgressToken = "progress-tok-1"
		} else {
			currentProgressToken = nil
		}
		// profileOverride="t" makes config.Resolve deterministic
		// (no session/env/project lookup) so the test is offline.
		r := handleRun(map[string]any{"command": "cat /etc/passwd"}, cfg, "t")
		if !r.IsError || !strings.HasPrefix(resultText(r), "rejected:") {
			t.Fatalf("streaming=%v: cat not rejected by run: %q", streaming, resultText(r))
		}
		m := structured(t, r)
		if m["rejected_reason"] != "unbounded_output" || m["pattern"] != "cat" {
			t.Errorf("streaming=%v: structured=%v, want unbounded_output/cat", streaming, m)
		}
	}
}
