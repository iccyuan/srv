package main

import (
	"encoding/json"
	"fmt"
	"os"
)

// JSON-RPC 2.0 framing for the stdio MCP server: types, send/response
// helpers, and a small read-error classifier the loop uses to label
// stdin-EOF vs other failures in the diagnostic log.

const mcpProtocolVersion = "2024-11-05"

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

// mcpSend marshals a response to stdout terminated with a newline (the
// stdio MCP framing). Recovers from BrokenPipe so a client that closes
// mid-write doesn't crash the loop.
func mcpSend(obj any) {
	b, err := json.Marshal(obj)
	if err != nil {
		return
	}
	defer func() { _ = recover() }() // BrokenPipe
	os.Stdout.Write(b)
	os.Stdout.Write([]byte("\n"))
}

func mcpResponse(id any, result any, errObj *jsonRPCError) jsonRPCResponse {
	r := jsonRPCResponse{JSONRPC: "2.0", ID: id}
	if errObj != nil {
		r.Error = errObj
	} else {
		r.Result = result
	}
	return r
}

// classifyReadErr reduces stdin Read errors to a short tag for the log.
// io.EOF is the boring "Claude closed stdin" case; anything else might
// hint at pipe-level breakage worth surfacing to the user.
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

// mcpJSONResult returns a tool result whose Content is a *compact* JSON
// rendering of `info`, with no separate StructuredContent. Both fields
// reach the MCP client; duplicating the same JSON in pretty-printed text
// AND a structured payload doubled the tokens many tools were spending
// on every call. Compact text is enough -- the model parses it fine and
// pretty-printing was costing ~30% extra whitespace tokens on top.
func mcpJSONResult(info any) toolResult {
	b, _ := json.Marshal(info)
	return toolResult{Content: []toolContent{{Type: "text", Text: string(b)}}}
}
