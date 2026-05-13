package mcp

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/srvtty"
	"srv/internal/sshx"
	"srv/internal/streams"
	"strings"
	"sync"
	"time"
)

// boundedStreamResult is what runBoundedStream returns to its
// caller. Plain struct so callers can append their own summary
// lines / pick out fields for the structuredContent payload.
type boundedStreamResult struct {
	Text   string // text accumulated up to runTextMax
	Bytes  int    // total bytes captured (post-filter)
	Capped bool   // true if more output was dropped past runTextMax
	RunErr error  // RunStream's error -- typically os.ErrClosed when the timer ended the stream
}

// runBoundedStream is the shared body of follow-mode `tail` and
// `journal`: dial-time SSH client + remote command + follow_seconds
// budget + optional grep filter. Streams chunks to the client as
// notifications/progress, accumulates a capped text buffer for the
// final tool result, returns once the timer fires (and c.Close
// makes RunStream return) or the remote command ends on its own.
//
// Caller owns dialing and closing the *sshx.Client. The timer
// goroutine closes the client to unblock RunStream; that close
// races with the caller's own defer c.Close() and Go's sync.Once
// keeps it safe.
//
// filter == nil means "accept every chunk"; otherwise only chunks
// matching the regex are accumulated AND emitted via progress
// (filtering at the source is what keeps grep mandatory for
// follow-mode in the first place).
func runBoundedStream(c *sshx.Client, remoteCmd, cwd string, followSeconds int, filter *regexp.Regexp) boundedStreamResult {
	token := progressToken()
	timer := time.NewTimer(time.Duration(followSeconds) * time.Second)
	defer timer.Stop()
	stopOnce := sync.Once{}
	stop := func() { stopOnce.Do(func() { _ = c.Close() }) }
	go func() {
		<-timer.C
		stop()
	}()

	var capturedBytes int
	var capped bool
	var buf strings.Builder
	onChunk := func(_ sshx.StreamChunkKind, line string) {
		if filter != nil && !filter.MatchString(line) {
			return
		}
		// Accumulate up to the run-text cap; further chunks still
		// stream via progress (model sees them in real time) but
		// the final result text gets a truncation marker.
		if capturedBytes+len(line) <= runTextMax {
			buf.WriteString(line)
			capturedBytes += len(line)
		} else {
			capped = true
		}
		emitProgress(token, capturedBytes, line)
	}
	_, _, _, runErr := c.RunStream(remoteCmd, cwd, onChunk)
	return boundedStreamResult{
		Text:   buf.String(),
		Bytes:  capturedBytes,
		Capped: capped,
		RunErr: runErr,
	}
}

// handleTail follows a remote file for a bounded duration and
// streams new lines to the client via `notifications/progress`. The
// final tools/call response returns the full accumulated output
// (capped at runTextMax) plus structured metadata.
//
// Why not just use `run` with `tail -F`: synchronous `run` rejects
// long-blocking patterns including `tail -f` for the MCP timeout
// reason. `tail` here is the explicit, bounded-time version: stream
// for up to follow_seconds (max 60s), then return -- the model gets
// real-time progress mid-call AND a deterministic upper bound on
// the turn duration.
func handleTail(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	path, _ := args["path"].(string)
	if path == "" {
		return textErr("path is required")
	}
	lines := 50
	if v, ok := args["lines"].(float64); ok && v > 0 {
		lines = int(v)
	}
	// Hard cap on backfill -- a request for `lines: 100000` would
	// produce hundreds of KB of one-shot output regardless of follow
	// mode. Clamp transparently and tell the model.
	var linesClamped bool
	lines, linesClamped = clampLines(lines, 1000)

	// Default is one-shot (no follow). A no-arg `tail {path}` call
	// fetches the last `lines` of the file and returns. Models that
	// want a live follow opt in via follow_seconds AND a grep filter
	// -- the token-economy gate (below) enforces the pairing.
	follow := 0
	if v, ok := args["follow_seconds"].(float64); ok && v > 0 {
		follow = int(v)
	}
	// Hard cap on follow_seconds. The MCP per-tool timeout (Claude
	// Code default 60s) is the binding constraint; progress
	// notifications reset it but we still want a deterministic
	// ceiling.
	if follow > 60 {
		follow = 60
	}

	grep, _ := args["grep"].(string)

	// Token-economy gate: a long follow on a chatty log emits
	// megabytes of progress notifications regardless of the
	// final-result cap. We require `grep` whenever the caller asks
	// for more than a brief spot-check window. The CLI doesn't have
	// this constraint -- only MCP, since only there does volume
	// translate directly to tokens.
	if r := requireStreamFilter("tail", follow,
		[]string{grep},
		`{ path: "/var/log/app.log", follow_seconds: 30, grep: "ERROR|WARN" }`,
	); r != nil {
		return *r
	}

	var re *regexp.Regexp
	if grep != "" {
		r, err := regexp.Compile(grep)
		if err != nil {
			return textErr(fmt.Sprintf("bad regex %q: %v", grep, err))
		}
		re = r
	}

	_, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}

	c, err := sshx.Dial(prof)
	if err != nil {
		return textErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	remoteCmd := fmt.Sprintf("tail -F -n %d %s", lines, srvtty.ShQuotePath(path))

	res := runBoundedStream(c, remoteCmd, "", follow, re)
	// res.RunErr is expected when the timer-close ends the stream;
	// that's the normal exit path. Only surface as transport_error
	// when it's a different failure.
	text := res.Text
	if res.Capped {
		text += fmt.Sprintf("\n[output cap %d bytes; further lines streamed via progress only]\n", runTextMax)
	}
	text += fmt.Sprintf("\n[followed %s for %ds, %d bytes captured]", path, follow, res.Bytes)
	structured := map[string]any{
		"path":           path,
		"follow_seconds": follow,
		"bytes_captured": res.Bytes,
		"capped":         res.Capped,
		"lines_clamped":  linesClamped,
		"end_reason":     "timer",
	}
	if res.RunErr != nil && !errors.Is(res.RunErr, os.ErrClosed) {
		structured["transport_error"] = res.RunErr.Error()
	}
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		StructuredContent: structured,
	}
}

// handleJournal exposes journalctl to the MCP server. Bounded
// duration (same idea as `tail`): follow_seconds defaults to 0,
// caps at 60s. Always non-follow if follow_seconds=0.
//
// Token-economy gates:
//   - lines is clamped to 2000 to bound one-shot reads (the
//     underlying journal can be GBs).
//   - follow_seconds > 0 requires at least one of unit/since/priority/
//     grep so a chatty system journal can't flood progress
//     notifications. The CLI counterpart has no such gate -- this
//     is purely an MCP rule.
func handleJournal(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
	unit, _ := args["unit"].(string)
	since, _ := args["since"].(string)
	priority, _ := args["priority"].(string)
	lines := 100
	if v, ok := args["lines"].(float64); ok && v >= 0 {
		lines = int(v)
	}
	lines, linesClamped := clampLines(lines, 2000)
	grep, _ := args["grep"].(string)
	follow := 0
	if v, ok := args["follow_seconds"].(float64); ok && v > 0 {
		follow = int(v)
	}
	if follow > 60 {
		follow = 60
	}

	// Token-economy gate. A bare `journalctl -f` taps the whole
	// system log and floods within seconds. unit / since / priority
	// / grep each meaningfully constrain the firehose; any one is
	// enough.
	if r := requireStreamFilter("journal", follow,
		[]string{unit, since, priority, grep},
		`{ Unit: "nginx.service", follow_seconds: 30 }`,
	); r != nil {
		return *r
	}

	profName, prof, errResult := resolveProfile(cfg, profileOverride)
	if errResult != nil {
		return *errResult
	}
	cwd := config.GetCwd(profName, prof)

	jc := streams.JournalCmd{
		Unit: unit, Since: since, Priority: priority, Lines: lines, Grep: grep,
		Follow: follow > 0,
	}
	remoteCmd := jc.ToRemoteCommand()

	if follow == 0 {
		res, _ := remote.RunCapture(prof, cwd, remoteCmd)
		text, truncatedBytes := buildRunText(res, cwd)
		structured := map[string]any{
			"exit_code":     res.ExitCode,
			"cwd":           cwd,
			"unit":          unit,
			"lines_clamped": linesClamped,
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

	// Follow mode: bounded tail-style streaming. Same shape as the
	// `tail` MCP tool -- dial direct, time-out via client close,
	// stream chunks via progress notifications.
	c, err := sshx.Dial(prof)
	if err != nil {
		return textErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	// journalctl's `-g` flag already applies grep server-side, so
	// the follow-mode chunk filter is nil here; the same gate that
	// requires SOME filter argument has already passed by this
	// point (unit / since / priority / grep).
	res := runBoundedStream(c, remoteCmd, cwd, follow, nil)

	text := res.Text
	if res.Capped {
		text += fmt.Sprintf("\n[output cap %d bytes; further lines streamed via progress only]\n", runTextMax)
	}
	text += fmt.Sprintf("\n[followed journal on %s for %ds, %d bytes captured]", profName, follow, res.Bytes)
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{
			"unit":           unit,
			"follow_seconds": follow,
			"bytes_captured": res.Bytes,
			"capped":         res.Capped,
			"lines_clamped":  linesClamped,
		},
	}
}
