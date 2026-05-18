package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"srv/internal/mcplog"
	"strings"
)

// MCP elicitation: putting a human in the loop when the guard fires.
//
// The guard used to hard-deny a high-risk command and tell the *model*
// to re-issue with confirm=true -- i.e. the model, not the human,
// decided whether to bypass. This file adds the missing human gate:
// when the client declared the `elicitation` capability we send it an
// `elicitation/create` request describing the command and honour the
// human's Allow/Deny answer. Clients that did NOT advertise the
// capability still get the original hard-deny (the documented degrade
// path) -- we never silently let a risky command through just because
// the round-trip wasn't available.
//
// Transport note: the request loop in loop.go is strictly serial and
// runs each tool handler in the read goroutine. A naive "send the
// request, wait for the reply" would deadlock -- nothing else reads
// stdin while the handler is blocked. Instead elicitConfirm reuses the
// SAME *bufio.Reader the loop reads from (stdinReader) and pumps it
// inline until the matching reply arrives, handling the handful of
// frames a serial client can legitimately interleave (ping; cancelled
// / other notifications). This keeps the "one thing at a time, plain
// globals are race-free" invariant the rest of the package relies on.

// stdinReader is the loop's buffered stdin reader, published so the
// elicitation round-trip can pump it inline (see the transport note
// above). Set once by Run before the loop starts; the serial loop
// makes the plain global race-free, same as version /
// currentProgressToken.
var stdinReader *bufio.Reader

// clientElicitation records whether the peer advertised the
// `elicitation` capability in its initialize params. Elicitation is a
// *client* capability in MCP -- the server doesn't advertise it; it
// just checks the client can be asked before sending a request. False
// (the default, and the value under any client that didn't declare it)
// routes the guard back to its original hard-deny.
var clientElicitation bool

// elicitSeq generates ids for server->client requests. Rendered as
// the string "srv-elicit-N" so it can never collide with a
// client-chosen request id (the client picks its own id space).
var elicitSeq int

// elicitFnForTests, when set, replaces the real transport round-trip
// so unit tests can drive the guard's allow/deny branches without a
// live MCP peer. Mirrors the guardConfigForTests seam.
var elicitFnForTests func(prompt string) (allow bool, asked bool)

// elicitConfirm asks the human, via the MCP client, to allow or deny
// an operation. Returns (allow, asked):
//
//	asked=false -> the client could not be asked (no elicitation
//	               capability, transport gone, or a JSON-RPC error
//	               back). Caller must fall back to hard-deny; we never
//	               treat "couldn't ask" as "allowed".
//	asked=true  -> the human answered. allow=true means Accept; a
//	               Decline or Cancel comes back allow=false.
func elicitConfirm(prompt string) (allow bool, asked bool) {
	if elicitFnForTests != nil {
		return elicitFnForTests(prompt)
	}
	if !clientElicitation || stdinReader == nil {
		return false, false
	}

	elicitSeq++
	id := fmt.Sprintf("srv-elicit-%d", elicitSeq)

	// requestedSchema is a no-property object: we only need a binary
	// Accept/Decline, which MCP conveys via the response `action`, not
	// via form content. An empty-properties object schema is the
	// minimal valid shape and renders as a plain confirm in clients.
	send(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "elicitation/create",
		"params": map[string]any{
			"message": prompt,
			"requestedSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	})
	mcplog.Logf("elicit id=%s sent", id)

	want := []byte(`"` + id + `"`)
	for {
		line, err := stdinReader.ReadString('\n')
		if err != nil {
			// Pipe closed mid-elicitation: the session is ending. Don't
			// invent an answer -- degrade to hard-deny. The outer loop's
			// next read sees the same EOF and exits cleanly.
			mcplog.Logf("elicit id=%s read err=%s", id, classifyReadErr(err))
			return false, false
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var f struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  *jsonRPCError   `json:"error"`
		}
		if json.Unmarshal([]byte(line), &f) != nil {
			continue
		}

		// A response (no method) carrying our id is the answer.
		if f.Method == "" && bytes.Equal(bytes.TrimSpace(f.ID), want) {
			if f.Error != nil {
				mcplog.Logf("elicit id=%s error=%d %s", id, f.Error.Code, f.Error.Message)
				return false, false
			}
			var res struct {
				Action string `json:"action"`
			}
			_ = json.Unmarshal(f.Result, &res)
			a := strings.ToLower(strings.TrimSpace(res.Action))
			mcplog.Logf("elicit id=%s action=%s", id, a)
			// Only an explicit accept allows. decline / cancel /
			// anything unrecognised is treated as "do not proceed" --
			// the safe default for a guard.
			return a == "accept", true
		}

		// Frames a serial client can legitimately interleave while we
		// wait for its answer:
		switch {
		case f.Method == "ping":
			send(response(rawID(f.ID), map[string]any{}, nil))
		case f.Method != "" && len(f.ID) > 0 && string(bytes.TrimSpace(f.ID)) != "null":
			// An unexpected request (shouldn't happen: the client is
			// awaiting our tools/call result). Answer with an error so
			// the client doesn't hang; keep waiting for our reply.
			mcplog.Logf("elicit id=%s unexpected request method=%s", id, f.Method)
			send(response(rawID(f.ID), nil, &jsonRPCError{
				Code:    -32603,
				Message: "srv: busy awaiting elicitation response",
			}))
		default:
			// Notification (no id) -- e.g. notifications/cancelled.
			// Nothing to reply; keep pumping.
		}
	}
}

// rawID decodes a json.RawMessage id back into the any the response
// helper expects, preserving string-vs-number so the echoed id matches
// what the client sent.
func rawID(raw json.RawMessage) any {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	return v
}
