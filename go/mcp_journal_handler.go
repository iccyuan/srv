package main

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/remote"
	"srv/internal/sshx"
	"strings"
	"time"

	"srv/internal/streams"
)

// handleMCPJournal exposes journal to the MCP server. Bounded
// duration (same idea as `tail`): follow_seconds defaults to 30s,
// caps at 60s. Always non-follow if follow_seconds=0.
//
// Token-economy gates:
//   - lines is clamped to 2000 to bound one-shot reads (the underlying
//     journal can be GBs).
//   - follow_seconds > 5 requires at least one of unit/since/priority/
//     grep so a chatty system journal can't flood progress
//     notifications. The CLI counterpart has no such gate -- this is
//     purely an MCP rule.
func handleMCPJournal(args map[string]any, cfg *config.Config, profileOverride string) toolResult {
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

	// Token-economy gate. A bare `journalctl -f` taps the whole system
	// log and floods within seconds. unit / since / priority / grep
	// each meaningfully constrain the firehose; any one is enough.
	if r := requireStreamFilter("journal", follow,
		[]string{unit, since, priority, grep},
		`{ Unit: "nginx.service", follow_seconds: 30 }`,
	); r != nil {
		return *r
	}

	profName, prof, errResult := resolveMCPProfile(cfg, profileOverride)
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
		text, truncatedBytes := buildMCPRunText(res, cwd)
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
		return mcpTextErr(fmt.Sprintf("dial: %v", err))
	}
	defer c.Close()

	token := currentProgressTokenFn()
	timer := time.NewTimer(time.Duration(follow) * time.Second)
	defer timer.Stop()
	go func() {
		<-timer.C
		_ = c.Close()
	}()

	var buf strings.Builder
	var captured int
	var capped bool
	onChunk := func(_ sshx.StreamChunkKind, line string) {
		if captured+len(line) <= mcpRunTextMax {
			buf.WriteString(line)
			captured += len(line)
		} else {
			capped = true
		}
		mcpProgress(token, captured, line)
	}
	_, _, _, _ = c.RunStream(remoteCmd, cwd, onChunk)

	text := buf.String()
	if capped {
		text += fmt.Sprintf("\n[output cap %d bytes; further lines streamed via progress only]\n", mcpRunTextMax)
	}
	text += fmt.Sprintf("\n[followed journal on %s for %ds, %d bytes captured]", profName, follow, captured)
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{
			"unit":           unit,
			"follow_seconds": follow,
			"bytes_captured": captured,
			"capped":         capped,
			"lines_clamped":  linesClamped,
		},
	}
}
