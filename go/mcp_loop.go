package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// mcpMode is set true while the process is acting as a stdio MCP server.
// Other code (progress meters, prompts, anything that would render
// human-facing chrome to stderr) reads this to stay silent so it never
// leaks into the JSON-RPC response stream the model parses. Cannot be
// re-enabled from inside the MCP path -- the entire reason it exists.
var mcpMode bool

// cmdMcp is the stdio MCP server entry point. Reads JSON-RPC frames from
// stdin one line at a time, dispatches by method, writes responses to
// stdout. Logs lifecycle events to ~/.srv/mcp.log so disconnects can be
// post-mortem'd (the client doesn't surface why a session ended).
func cmdMcp(cfg *Config) error {
	mcpMode = true
	mcpLogf("start v=%s", Version)
	rd := bufio.NewReader(os.Stdin)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			// stdin EOF / pipe closed is the normal way Claude Code ends
			// an MCP session (between conversations, agent restart). The
			// log line distinguishes this from panic / write-error exits
			// so users debugging "why did mcp drop" can tell normal
			// lifecycle apart from real crashes.
			mcpLogf("exit reason=stdin-%s", classifyReadErr(err))
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
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "srv", "version": Version},
			}, nil))
		case "notifications/initialized":
			// no response for notifications
		case "ping":
			mcpSend(mcpResponse(req.ID, map[string]any{}, nil))
		case "tools/list":
			mcpSend(mcpResponse(req.ID, map[string]any{"tools": mcpToolDefs()}, nil))
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				mcpSend(mcpResponse(req.ID, nil, &jsonRPCError{
					Code:    -32602,
					Message: "invalid tools/call params: " + err.Error(),
				}))
				continue
			}
			args := p.Arguments
			if args == nil {
				args = map[string]any{}
			}
			cfg2, _ := LoadConfig()
			if cfg2 == nil {
				cfg2 = newConfig()
			}
			start := time.Now()
			res := safeMCPHandle(p.Name, args, cfg2)
			ok := "ok"
			if res.IsError {
				ok = "err"
			}
			mcpLogf("tool=%s dur=%.2fs %s", p.Name, time.Since(start).Seconds(), ok)
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
// logged to mcp.log first so a hard crash later (mcpSend BrokenPipe etc.)
// still leaves a trail. Stack would be ideal here but it inflates the
// log; the (tool, panic) pair is usually enough to localise via git
// blame.
func safeMCPHandle(name string, args map[string]any, cfg *Config) (res toolResult) {
	defer func() {
		if r := recover(); r != nil {
			mcpLogf("tool=%s panic=%v", name, r)
			res = toolResult{
				IsError: true,
				Content: []toolContent{{Type: "text", Text: fmt.Sprintf("panic: %v", r)}},
			}
		}
	}()
	return mcpHandleTool(name, args, cfg)
}
