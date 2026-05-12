package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"srv/internal/config"
	"srv/internal/mcplog"
	"srv/internal/progress"
	"srv/internal/project"
	"strings"
	"time"

	"srv/internal/i18n"
)

// mcpMode is set true while the process is acting as a stdio MCP server.
// Other code (progress meters, prompts, anything that would render
// human-facing chrome to stderr) reads this to stay silent so it never
// leaks into the JSON-RPC response stream the model parses. Cannot be
// re-enabled from inside the MCP path -- the entire reason it exists.
var mcpMode bool

// currentProgressToken carries the per-request progress token from the
// loop down to whichever streaming handler is dispatched. Plain global
// is safe because the loop is strictly serial -- one mcpTools/call at a
// time, set on entry, cleared on exit. Handlers that spawn goroutines
// snapshot it before returning so the goroutines see a stable value
// even if the next request blanks the global.
var currentProgressToken any

// currentProgressTokenFn returns whatever was stamped onto the running
// request. Streaming-capable mcpTools call this to find out if the client
// asked for streaming (non-nil) or wants the synchronous shape (nil).
func currentProgressTokenFn() any { return currentProgressToken }

// Run is the stdio MCP server entry point. Reads JSON-RPC frames from
// stdin one line at a time, dispatches by method, writes responses to
// stdout. Logs lifecycle events to ~/.srv/mcp.log so disconnects can be
// post-mortem'd (the client doesn't surface why a session ended).
func cmdMcp(cfg *config.Config) error {
	mcpMode = true
	i18n.SetMCPMode(true)
	project.SetSilent(true)
	progress.SetQuiet(true)
	mcplog.Logf("start v=%s", Version)
	rd := bufio.NewReader(os.Stdin)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			// stdin EOF / pipe closed is the normal way Claude Code ends
			// an MCP session (between conversations, agent restart). The
			// log line distinguishes this from panic / write-error exits
			// so users debugging "why did mcp drop" can tell normal
			// lifecycle apart from real crashes.
			mcplog.Logf("exit reason=stdin-%s", classifyReadErr(err))
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req jsonRPCRequest
		if jerr := json.Unmarshal([]byte(line), &req); jerr != nil {
			mcpSend(mcpResponse(nil, nil, &jsonRPCError{
				Code:    -32700,
				Message: "parse error: " + jerr.Error(),
			}))
			continue
		}
		switch req.Method {
		case "initialize":
			mcpSend(mcpResponse(req.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"mcpTools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "srv", "version": Version},
			}, nil))
		case "notifications/initialized":
			// no response for notifications
		case "ping":
			mcpSend(mcpResponse(req.ID, map[string]any{}, nil))
		case "mcpTools/list":
			mcpSend(mcpResponse(req.ID, map[string]any{"mcpTools": mcpToolDefs()}, nil))
		case "mcpTools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
				Meta      struct {
					ProgressToken any `json:"progressToken"`
				} `json:"_meta"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				mcpSend(mcpResponse(req.ID, nil, &jsonRPCError{
					Code:    -32602,
					Message: "invalid mcpTools/call params: " + err.Error(),
				}))
				continue
			}
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			cfg2, _ := config.Load()
			if cfg2 == nil {
				cfg2 = config.New()
			}
			// Stash the progress token before dispatch and clear it
			// after, so streaming mcpTools can read it via
			// currentProgressTokenFn(). Loop is serial so the global is
			// race-free; per-handler goroutines that outlive the call
			// must snapshot the value themselves.
			currentProgressToken = p.Meta.ProgressToken
			start := time.Now()
			res := safeMCPHandle(p.Name, args, cfg2)
			currentProgressToken = nil
			ok := "ok"
			if res.IsError {
				ok = "err"
			}
			mcplog.Logf("tool=%s dur=%.2fs %s", p.Name, time.Since(start).Seconds(), ok)
			mcpSend(mcpResponse(req.ID, res, nil))
		default:
			if req.ID != nil {
				mcpSend(mcpResponse(req.ID, nil, &jsonRPCError{
					Code:    -32601,
					Message: "method not found: " + req.Method,
				}))
			}
		}
	}
}

// safeMCPHandle isolates a tool handler from the request loop: any panic
// inside mcpHandleTool is converted into an isError tool result so the
// MCP server stays alive for subsequent calls. The panic itself is
// logged to mcp.log first so a hard crash later (Send BrokenPipe etc.)
// still leaves a trail. Stack would be ideal here but it inflates the
// log; the (tool, panic) pair is usually enough to localise via git
// blame.
func safeMCPHandle(name string, args map[string]any, cfg *config.Config) (res toolResult) {
	defer func() {
		if r := recover(); r != nil {
			mcplog.Logf("tool=%s panic=%v", name, r)
			res = toolResult{
				IsError: true,
				Content: []toolContent{{Type: "text", Text: fmt.Sprintf("panic: %v", r)}},
			}
		}
	}()
	return mcpHandleTool(name, args, cfg)
}
