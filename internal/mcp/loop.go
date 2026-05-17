package mcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"srv/internal/config"
	"srv/internal/i18n"
	"srv/internal/mcplog"
	"srv/internal/progress"
	"srv/internal/project"
	"strings"
	"time"
)

// currentProgressToken carries the per-request progress token from
// the loop down to whichever streaming handler is dispatched. A plain
// global is safe because the loop is strictly serial -- one tools/call
// at a time, set on entry, cleared on exit. Handlers that spawn
// goroutines snapshot it before returning so the goroutines see a
// stable value even if the next request blanks the global.
var currentProgressToken any

// progressToken returns the active request's progress token, or nil
// when none was supplied. Streaming-capable handlers call this to
// find out if the client asked for streaming.
func progressToken() any { return currentProgressToken }

// version is the srv build version, stashed by Run() at the
// package level so handlers (e.g. handleDoctor) can read it without
// it being threaded through every dispatch frame. The loop is
// strictly serial so the global is race-free.
var version string

// Run is the stdio MCP server entry point. Reads JSON-RPC frames from
// stdin one line at a time, dispatches by method, writes responses
// to stdout. Logs lifecycle events to ~/.srv/mcp.log so disconnects
// can be post-mortem'd (the client doesn't surface why a session
// ended).
//
// versionStr is embedded in the `initialize` reply's serverInfo and
// in the lifecycle log; callers pass the build's main.Version.
func Run(cfg *config.Config, versionStr string) error {
	version = versionStr
	i18n.SetMCPMode(true)
	project.SetSilent(true)
	progress.SetQuiet(true)
	mcplog.Logf("start v=%s", versionStr)
	rd := bufio.NewReader(os.Stdin)
	for {
		line, err := rd.ReadString('\n')
		if err != nil {
			// stdin EOF / pipe closed is the normal way Claude Code
			// ends an MCP session (between conversations, agent
			// restart). The log line distinguishes this from panic /
			// write-error exits so users debugging "why did mcp drop"
			// can tell normal lifecycle apart from real crashes.
			mcplog.Logf("exit reason=stdin-%s", classifyReadErr(err))
			return nil
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var req jsonRPCRequest
		if jerr := json.Unmarshal([]byte(line), &req); jerr != nil {
			send(response(nil, nil, &jsonRPCError{
				Code:    -32700,
				Message: "parse error: " + jerr.Error(),
			}))
			continue
		}
		switch req.Method {
		case "initialize":
			send(response(req.ID, map[string]any{
				"protocolVersion": protocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": "srv", "version": versionStr},
			}, nil))
		case "notifications/initialized":
			// no response for notifications
		case "ping":
			send(response(req.ID, map[string]any{}, nil))
		case "tools/list":
			send(response(req.ID, map[string]any{"tools": toolDefs()}, nil))
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
				Meta      struct {
					ProgressToken any `json:"progressToken"`
				} `json:"_meta"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				send(response(req.ID, nil, &jsonRPCError{
					Code:    -32602,
					Message: "invalid tools/call params: " + err.Error(),
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
			// after, so streaming tools can read it via progressToken().
			// Reset progressBytesCounter at the same boundary so the
			// per-call stats record captures only this call's stream.
			// Loop is serial so these globals are race-free; per-
			// handler goroutines that outlive the call must snapshot
			// the values themselves.
			currentProgressToken = p.Meta.ProgressToken
			progressBytesCounter = 0
			start := time.Now()
			res := safeHandle(p.Name, args, cfg2)
			currentProgressToken = nil
			dur := time.Since(start)
			ok := "ok"
			if res.IsError {
				ok = "err"
			}
			mcplog.Logf("tool=%s dur=%.2fs %s", p.Name, dur.Seconds(), ok)
			// Best-effort stats record. Sizes are JSON-marshaled to
			// match what actually went over the wire; bytes/4 is the
			// rough token estimate the CLI surfaces. Errors writing
			// the JSONL line are ignored -- stats are observability,
			// not authoritative state.
			argsJSON, _ := json.Marshal(args)
			resJSON, _ := json.Marshal(res)
			_ = mcplog.AppendCall(mcplog.Call{
				TS:            start,
				Tool:          p.Name,
				Cmd:           mcplog.DescribeArgs(p.Name, args),
				DurMs:         dur.Milliseconds(),
				InBytes:       len(argsJSON),
				OutBytes:      len(resJSON),
				ProgressBytes: progressBytesCounter,
				OK:            !res.IsError,
			})
			// Persist a full args+result replay record. Independent of
			// stats so users can disable one without losing the other.
			_ = appendReplay(replayEntry{
				TS:            start,
				Tool:          p.Name,
				Args:          args,
				Result:        res,
				DurMs:         dur.Milliseconds(),
				ProgressBytes: progressBytesCounter,
			})
			send(response(req.ID, res, nil))
		default:
			if req.ID != nil {
				send(response(req.ID, nil, &jsonRPCError{
					Code:    -32601,
					Message: "method not found: " + req.Method,
				}))
			}
		}
	}
}

// safeHandle isolates a tool handler from the request loop: any
// panic inside handle() is converted into an isError tool result so
// the MCP server stays alive for subsequent calls. The panic itself
// is logged to mcp.log first so a hard crash later (Send BrokenPipe
// etc.) still leaves a trail. Stack would be ideal here but it
// inflates the log; the (tool, panic) pair is usually enough to
// localise via git blame.
func safeHandle(name string, args map[string]any, cfg *config.Config) (res toolResult) {
	defer func() {
		if r := recover(); r != nil {
			mcplog.Logf("tool=%s panic=%v", name, r)
			res = toolResult{
				IsError: true,
				Content: []toolContent{{Type: "text", Text: fmt.Sprintf("panic: %v", r)}},
			}
		}
	}()
	return handle(name, args, cfg)
}
