// Package mcp implements the stdio MCP server: JSON-RPC 2.0 framing,
// the request loop, the tool registry, and ~25 named tool handlers
// that map Claude Code mcpTools/call requests onto srv operations.
//
// Entry point is Run. Main wires it from the `srv mcp` subcommand,
// injecting a Deps struct for the few callbacks that have to reach
// back into top-level helpers (check / doctor / diff / list_dir).
//
// The package layout splits along the seams that show up in everyday
// edits:
//
//	proto.go            JSON-RPC types, framing, send/response/progress
//	loop.go             Run() + safeHandle() + serial dispatch
//	registry.go         tool table, ToolDefs/Handle dispatchers, schemas
//	result.go           toolResult helpers + buildRunText + dependencies
//	gates.go            risky-pattern, sync-rejection, unbounded-output rules
//	handlers_*.go       handler implementations grouped by domain
package mcp

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// JSON-RPC 2.0 framing for the stdio MCP server: types, send/response
// helpers, and a small read-error classifier the loop uses to label
// stdin-EOF vs other failures in the diagnostic log.

const protocolVersion = "2024-11-05"

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      any           `json:"id"`
	Result  any           `json:"result,omitempty"`
	Error   *jsonRPCError `json:"error,omitempty"`
}

type jsonRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// sendMu serializes writes to stdout so concurrent emitters can't
// interleave bytes mid-frame. The synchronous tool-call path doesn't
// need it (one writer at a time), but the streaming run path has its
// stdout/stderr forwarder goroutines emitting progress notifications
// in parallel with whatever else, so every write goes through the
// mutex to keep frames atomic.
var sendMu sync.Mutex

// send marshals a response to stdout terminated with a newline (the
// stdio MCP framing). Recovers from BrokenPipe so a client that closes
// mid-write doesn't crash the loop.
func send(obj any) {
	b, err := json.Marshal(obj)
	if err != nil {
		return
	}
	defer func() { _ = recover() }() // BrokenPipe
	sendMu.Lock()
	defer sendMu.Unlock()
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
}

// notification is the JSON-RPC 2.0 shape for server-to-client
// notifications (no `id`, the client doesn't reply). Used today for
// `notifications/progress` so the streaming `run_stream` tool can
// push partial output before its final tool result lands.
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// progressParams is the payload of a `notifications/progress` message.
// `progressToken` echoes the value the client sent in tools/call meta;
// `progress` is a monotonic counter (we use byte count); `message` is
// the human-readable chunk -- for run_stream that's the streamed line.
type progressParams struct {
	ProgressToken any    `json:"progressToken"`
	Progress      int    `json:"progress"`
	Message       string `json:"message,omitempty"`
}

// emitProgress emits one `notifications/progress` to the client.
// Caller is responsible for monotonic `progress` -- we don't auto-
// increment. When token is nil (caller didn't pass _meta.progressToken)
// this is a no-op so streaming tools degrade gracefully under non-
// streaming clients.
func emitProgress(token any, progress int, message string) {
	if token == nil {
		return
	}
	send(notification{
		JSONRPC: "2.0",
		Method:  "notifications/progress",
		Params: progressParams{
			ProgressToken: token,
			Progress:      progress,
			Message:       message,
		},
	})
}

func response(id any, result any, errObj *jsonRPCError) jsonRPCResponse {
	r := jsonRPCResponse{JSONRPC: "2.0", ID: id}
	if errObj != nil {
		r.Error = errObj
	} else {
		r.Result = result
	}
	return r
}

// classifyReadErr reduces stdin Read errors to a short tag for the
// log. io.EOF is the boring "Claude closed stdin" case; anything else
// might hint at pipe-level breakage worth surfacing to the user.
func classifyReadErr(err error) string {
	if err == nil {
		return "nil"
	}
	if err.Error() == "EOF" {
		return "eof"
	}
	return "err:" + err.Error()
}

// guardBlocked builds a uniform error response when the session guard
// refuses an operation. Lives here next to other tool-result helpers
// because every guarded tool calls it.
func guardBlocked(tool, reason string) toolResult {
	text := fmt.Sprintf(
		"guard: %s blocked. %s\nRe-run with confirm=true if intentional, or disable via `srv guard off` (or unset SRV_GUARD).",
		tool, reason,
	)
	return toolResult{
		IsError: true,
		Content: []toolContent{{Type: "text", Text: text}},
		StructuredContent: map[string]any{
			"guard_blocked": true,
			"tool":          tool,
			"reason":        reason,
		},
	}
}

// jsonResult returns a tool result whose Content is a *compact* JSON
// rendering of `info`, with no separate StructuredContent. Both fields
// reach the MCP client; duplicating the same JSON in pretty-printed
// text AND a structured payload doubled the tokens many tools were
// spending on every call. Compact text is enough -- the model parses
// it fine and pretty-printing was costing ~30% extra whitespace tokens
// on top.
func jsonResult(info any) toolResult {
	b, _ := json.Marshal(info)
	return toolResult{Content: []toolContent{{Type: "text", Text: string(b)}}}
}
