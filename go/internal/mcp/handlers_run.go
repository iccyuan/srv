package mcp

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/group"
	"srv/internal/remote"
	"srv/internal/sshx"
	"strings"
)

func handleRun(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return textErr("error: command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("run", cmd, confirm); blocked != nil {
		return *blocked
	}
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	background, _ := args["background"].(bool)
	if background {
		rec, err := remote.SpawnDetached(profName, prof, cmd)
		if err != nil {
			return textErr(err.Error())
		}
		return detachedResult(rec)
	}
	// Hard-reject sync calls that would block the MCP turn for too
	// long. Description tells the model what to do instead; this
	// catches the case where it ignored that and went with the
	// reflex sleep+poll pattern anyway.
	if why := rejectSync(cmd); why != "" {
		return textErr(rejectMessage(cmd, why))
	}
	// Token-economy gate: reject `cat <file>` / `dmesg` / unfiltered
	// `journalctl` / unfiltered `find /` and friends -- they have no
	// native upper bound, so even the 64 KiB result cap pays tokens
	// for the wrong slice. Model is told exactly what slicer to add.
	if label, msg := rejectUnfiltered(cmd); label != "" {
		return rejectUnfilteredMessage(label, msg)
	}
	cwd := config.GetCwd(profName, prof)
	res, _ := remote.RunCapture(prof, cwd, cmd)
	text, truncatedBytes := buildRunText(res, cwd)
	structured := map[string]any{
		"exit_code": res.ExitCode,
		"cwd":       cwd,
	}
	if truncatedBytes > 0 {
		structured["truncated_bytes"] = truncatedBytes
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           res.ExitCode != 0,
		StructuredContent: structured,
	}
}

// handleRunStream is the streaming variant of `run`. While the
// remote command executes, every line of stdout/stderr is pushed to
// the client as a `notifications/progress` notification tagged with
// the caller's progressToken. The final `tools/call` response still
// carries the full captured output (capped at runTextMax) and the
// exit code -- progress notifications are informational, not the
// authoritative output, so a client that ignores them still gets
// the same shape as `run`.
//
// Why this exists: the synchronous `run` tool is bound by the MCP
// per-tool timeout (Claude Code default 60s) -- a 30s build sits
// silent until completion and risks the "tools no longer available"
// red dot if it slips past the bound. Streaming keeps progress
// flowing so the client doesn't time out, and lets the model see
// partial output before the command finishes.
func handleRunStream(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return textErr("error: command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("run_stream", cmd, confirm); blocked != nil {
		return *blocked
	}
	// Same token-economy gate as plain `run` -- streaming makes the
	// unbounded-source problem worse, not better, since progress
	// notifications add their own token cost on top of the final
	// result.
	if label, msg := rejectUnfiltered(cmd); label != "" {
		return rejectUnfilteredMessage(label, msg)
	}
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := config.GetCwd(profName, prof)
	token := progressToken()

	// Direct dial -- the daemon's stream_run op exists but is wired
	// for CLI consumption (writes to stdout); routing through it
	// would require yet another adapter. Cold handshake hits the
	// same ~2.7s cost as any non-pooled tool, which is fine for an
	// explicitly-streaming call (the streaming masks the dial cost).
	c, err := sshx.Dial(prof)
	if err != nil {
		return textErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	// progress counter: byte-based, monotonic. Some MCP clients use
	// progress to drive a UI bar; bytes is meaningful enough without
	// knowing the unbounded total.
	var emitted int
	onChunk := func(_ sshx.StreamChunkKind, line string) {
		emitted += len(line)
		emitProgress(token, emitted, line)
	}

	exitCode, stdout, stderr, runErr := c.RunStream(cmd, cwd, onChunk)
	if runErr != nil {
		return textErr(fmt.Sprintf("stream run: %v", runErr))
	}

	res := &sshx.RunCaptureResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
		Cwd:      cwd,
	}
	text, truncatedBytes := buildRunText(res, cwd)
	structured := map[string]any{
		"exit_code":     exitCode,
		"cwd":           cwd,
		"bytes_emitted": emitted,
	}
	if truncatedBytes > 0 {
		structured["truncated_bytes"] = truncatedBytes
	}
	if token != nil {
		structured["streamed"] = true
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		IsError:           exitCode != 0,
		StructuredContent: structured,
	}
}

func handleDetach(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return textErr("command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("detach", cmd, confirm); blocked != nil {
		return *blocked
	}
	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	rec, err := remote.SpawnDetached(profName, prof, cmd)
	if err != nil {
		return textErr(err.Error())
	}
	return detachedResult(rec)
}

// handleRunGroup fans `command` out across every profile in a
// named group and returns one structured result per member.
//
// Why separate from `run`: mixing the fan-out shape into the `run`
// tool's schema would force callers to handle both "single result"
// and "array of results" depending on whether `group` was set.
// Keeping it separate gives both tools narrow, predictable response
// shapes.
func handleRunGroup(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	groupName, _ := args["group"].(string)
	if groupName == "" {
		return textErr("group is required")
	}
	cmd, _ := args["command"].(string)
	if cmd == "" {
		return textErr("command is required")
	}
	confirm, _ := args["confirm"].(bool)
	if blocked := guardCheckRisky("run_group", cmd, confirm); blocked != nil {
		return *blocked
	}
	// Long-blocking sync rejection applies fan-out too: a `sleep
	// 60` across 10 hosts wedges every connection in parallel for
	// the same 60s and still busts the MCP per-tool timeout.
	if why := rejectSync(cmd); why != "" {
		return textErr(rejectMessage(cmd, why))
	}
	results, err := group.Run(cfg, groupName, cmd)
	if err != nil {
		return textErr(err.Error())
	}

	// Build a compact text section per profile -- enough for the
	// model to read at a glance, with the full payload in structured
	// content.
	var sb strings.Builder
	maxExit, failed := 0, 0
	for _, r := range results {
		fmt.Fprintf(&sb, "=== %s [exit %d, %.1fs]", r.Profile, r.ExitCode, r.Duration)
		if r.Error != "" {
			fmt.Fprintf(&sb, " ERROR: %s", r.Error)
		}
		sb.WriteString(" ===\n")
		if r.Stdout != "" {
			sb.WriteString(r.Stdout)
			if !strings.HasSuffix(r.Stdout, "\n") {
				sb.WriteByte('\n')
			}
		}
		if r.Stderr != "" {
			sb.WriteString("--- stderr ---\n")
			sb.WriteString(r.Stderr)
			if !strings.HasSuffix(r.Stderr, "\n") {
				sb.WriteByte('\n')
			}
		}
		if r.ExitCode != 0 || r.Error != "" {
			failed++
			if r.ExitCode > maxExit {
				maxExit = r.ExitCode
			} else if r.ExitCode < 0 && maxExit == 0 {
				maxExit = 255
			}
		}
	}
	fmt.Fprintf(&sb, "\n%d profile(s), %d succeeded, %d failed.\n", len(results), len(results)-failed, failed)

	return toolResult{
		Content: []toolContent{{Type: "text", Text: sb.String()}},
		IsError: failed > 0,
		StructuredContent: map[string]any{
			"group":     groupName,
			"results":   results,
			"succeeded": len(results) - failed,
			"failed":    failed,
		},
	}
}
