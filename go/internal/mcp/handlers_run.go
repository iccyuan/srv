package mcp

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/group"
	"srv/internal/remote"
	"srv/internal/sshx"
	"strings"
)

// handleRun dispatches the unified `run` tool across three modes:
//
//   - background=true  -> spawn detached job, return job_id immediately.
//   - progressToken set -> stream stdout/stderr chunks via
//     `notifications/progress`; cold-dials a
//     dedicated SSH session so the chunk callback
//     can fire per-line. Use for medium-length
//     (~20-90s) commands where progress keeps the
//     MCP per-tool timeout alive.
//   - neither           -> synchronous capture through the warm daemon
//     pool (~200ms for short commands).
//
// Same pre-flight gates apply to non-background modes: guard for
// destructive patterns, rejectSync for long-blocking shapes, and
// rejectUnfiltered for unbounded sources. Streaming does NOT exempt
// the gates -- progress notifications add their own token cost on top
// of the final result.
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
	// Sync-blocking patterns bust the MCP per-tool timeout even with
	// streaming (progress notifications reset the timeout, but the
	// conversation is held open forever with no upper bound). Route
	// to background=true for these.
	if why := rejectSync(cmd); why != "" {
		return textErr(rejectMessage(cmd, why))
	}
	// Token-economy gate: unbounded sources (cat <file>, bare dmesg,
	// unfiltered journalctl / find /) are rejected with an
	// educational message pointing at the right slicer. Same rule
	// for sync and stream paths -- progress chunks cost tokens too.
	if label, msg := rejectUnfiltered(cmd); label != "" {
		return rejectUnfilteredMessage(label, msg)
	}
	cwd := config.GetCwd(profName, prof)

	// Sync path: warm daemon pool when the client didn't ask for
	// streaming. Short commands (~200ms) skip the ~2.7s cold-dial
	// cost the streaming path pays.
	token := progressToken()
	if token == nil {
		res, _ := remote.RunCapture(prof, cwd, cmd)
		text := buildRunText(res, cwd)
		if len(text) > ResultByteMax {
			return oversizeResult("run", len(text), runOversizeHint,
				map[string]any{"exit_code": res.ExitCode, "cwd": cwd})
		}
		return toolResult{
			Content:           []toolContent{{Type: "text", Text: text}},
			IsError:           res.ExitCode != 0,
			StructuredContent: map[string]any{"exit_code": res.ExitCode, "cwd": cwd},
		}
	}

	// Streaming path -- the client passed _meta.progressToken on
	// tools/call. Direct dial here because the daemon's stream_run
	// op is wired for CLI consumption (writes to stdout); routing
	// through it would require yet another adapter. Cold handshake
	// hits the same ~2.7s cost as any non-pooled tool, which the
	// streaming call masks.
	c, err := sshx.Dial(prof)
	if err != nil {
		return textErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	// Byte-based monotonic progress counter; some MCP clients drive
	// a UI bar off it. Total is unbounded so this is just elapsed
	// bytes-so-far, not a percentage.
	//
	// Early-terminate when emitted > ResultByteMax: stop forwarding
	// progress (further chunks would burn tokens for output the
	// final result will reject anyway) and close the SSH session,
	// which kills the remote command via SIGHUP. The flag also
	// drives the post-RunStream branch below.
	var emitted int
	var oversize bool
	onChunk := func(_ sshx.StreamChunkKind, line string) {
		if oversize {
			return
		}
		emitted += len(line)
		emitProgress(token, emitted, line)
		if emitted > ResultByteMax {
			oversize = true
			// Close in a goroutine -- onChunk runs in the SSH read
			// loop; a synchronous c.Close() would wait for that
			// loop to drain and deadlock against itself.
			go func() { _ = c.Close() }()
		}
	}

	exitCode, stdout, stderr, runErr := c.RunStream(cmd, cwd, onChunk)
	// When `oversize` fired, we intentionally closed the session.
	// RunStream then typically returns os.ErrClosed or "exited
	// without exit status" -- both expected. Treat as the oversize
	// reject path and skip the normal error surface.
	if oversize {
		return oversizeResult("run", emitted, runOversizeHint,
			map[string]any{
				"cwd":              cwd,
				"bytes_emitted":    emitted,
				"streamed":         true,
				"terminated_early": true,
			})
	}
	if runErr != nil {
		return textErr(fmt.Sprintf("stream run: %v", runErr))
	}

	res := &sshx.RunCaptureResult{
		Stdout:   stdout,
		Stderr:   stderr,
		ExitCode: exitCode,
		Cwd:      cwd,
	}
	text := buildRunText(res, cwd)
	// Cap may still be hit at the final-assembly step (footer +
	// stderr divider push us over) even though early-terminate
	// didn't fire mid-stream. Same reject shape.
	if len(text) > ResultByteMax {
		return oversizeResult("run", len(text), runOversizeHint,
			map[string]any{"exit_code": exitCode, "cwd": cwd, "bytes_emitted": emitted, "streamed": true})
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		IsError: exitCode != 0,
		StructuredContent: map[string]any{
			"exit_code":     exitCode,
			"cwd":           cwd,
			"bytes_emitted": emitted,
			"streamed":      true,
		},
	}
}

// runOversizeHint is the filter guidance returned when `run` output
// exceeds ResultByteMax. Repeated verbatim by `run_group` is OK; the
// rejection helper just embeds whatever string is passed.
const runOversizeHint = "use `head -n N`, `tail -n N`, `grep PATTERN`, or pipe through `head|tail|grep|awk|sed|wc|cut|jq|sort|uniq` to slice the output"

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

	text := sb.String()
	// Cap applies to the aggregated text (N profiles' stdout/stderr
	// glued together). The per-profile `results` array in
	// structuredContent would also be too big to keep, so we drop
	// both and signal the count summary in `extra` for context.
	if len(text) > ResultByteMax {
		return oversizeResult("run_group", len(text),
			"narrow the `group` membership, or run the command per-profile with a slicer (`| head -n N`, `| grep PATTERN`)",
			map[string]any{
				"group":     groupName,
				"succeeded": len(results) - failed,
				"failed":    failed,
			})
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		IsError: failed > 0,
		StructuredContent: map[string]any{
			"group":     groupName,
			"results":   results,
			"succeeded": len(results) - failed,
			"failed":    failed,
		},
	}
}
