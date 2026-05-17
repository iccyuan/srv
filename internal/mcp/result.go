package mcp

import (
	"fmt"
	"maps"
	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/sshx"
)

// Token-economy bounds applied uniformly by handlers. Tuning each in
// isolation has consequences for the others, so they live together.
const (
	// ResultByteMax is the hard cap on a tool result's text payload,
	// applied uniformly by every handler that can produce variable-
	// length output (run, tail, journal, tail_log, wait_job log
	// body, run_group, ...). When a result would exceed this, the
	// handler returns oversizeResult instead of truncating -- the
	// model is expected to add a filter and retry rather than read a
	// truncated slice.
	//
	// The MCP client keeps every tool result in its conversation
	// history, so a single `cat /var/log/...` or `journalctl -n
	// 100000` permanently inflates the client's memory by the full
	// payload. 64 KiB is roomy enough that ordinary multi-screen
	// output (a build log, a config dump, a few hundred journal
	// lines) fits in one call, while still tripping the gate on
	// genuinely unbounded dumps.
	ResultByteMax         = 64 * 1024
	waitJobDefaultSeconds = 8
	waitJobMaxSeconds     = 15
)

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type toolResult struct {
	Content           []toolContent `json:"content"`
	IsError           bool          `json:"isError,omitempty"`
	StructuredContent any           `json:"structuredContent,omitempty"`
}

type toolContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// textErr wraps a plain string as an isError tool result. Used by
// every handler for pre-flight validation failures (missing args,
// profile not found, etc.).
func textErr(s string) toolResult {
	return toolResult{
		IsError: true,
		Content: []toolContent{{Type: "text", Text: s}},
	}
}

// detachedResult formats the "started detached job" response shared
// by `run background=true` and `detach`.
func detachedResult(rec *jobs.Record) toolResult {
	info := map[string]any{
		"job_id":    rec.ID,
		"status":    "running",
		"profile":   rec.Profile,
		"pid":       rec.Pid,
		"log":       rec.Log,
		"cwd":       rec.Cwd,
		"started":   rec.Started,
		"next_tool": "wait_job",
	}
	text := fmt.Sprintf(
		"started job %s pid=%d profile=%s\npoll with wait_job id=%s max_wait_seconds=%d",
		rec.ID, rec.Pid, rec.Profile, rec.ID, waitJobDefaultSeconds,
	)
	return toolResult{
		Content:           []toolContent{{Type: "text", Text: text}},
		StructuredContent: info,
	}
}

// resolveProfile is the per-handler wrapper around config.Resolve.
// Returns (name, profile, nil) on success or ("", nil, errResult)
// when resolution fails -- callers `return *errResult` to short-
// circuit. Replaces the 12-call repetition of config.Resolve +
// textErr across the registry.
func resolveProfile(cfg *config.Config, override string) (string, *config.Profile, *toolResult) {
	name, prof, err := config.Resolve(cfg, override)
	if err != nil {
		r := textErr(err.Error())
		return "", nil, &r
	}
	return name, prof, nil
}

// buildRunText assembles the textual payload returned by the `run`
// tool: stdout, optional `--- stderr ---` divider + stderr, plus a
// footer with cwd and exit code.
//
// Success renders as `[ok cwd ...]` rather than `exit 0 cwd ...` so
// MCP clients with pattern-matching log analysis (Codex et al.) don't
// read the word "exit" as a failure signal on every successful
// command.
//
// Does NOT truncate -- callers check len(text) against ResultByteMax
// and call oversizeResult when over.
func buildRunText(res *sshx.RunCaptureResult, cwd string) string {
	text := res.Stdout
	if res.Stderr != "" {
		if text != "" {
			text += "\n--- stderr ---\n"
		}
		text += res.Stderr
	}
	if res.ExitCode == 0 {
		text += fmt.Sprintf("\n[ok cwd %s]", cwd)
	} else {
		text += fmt.Sprintf("\n[exit %d cwd %s]", res.ExitCode, cwd)
	}
	return text
}

// payloadResult wraps a text payload as a Content-only tool result.
//
// NO StructuredContent, deliberately: an MCP client (Claude Code)
// surfaces structuredContent in place of the text block whenever it
// is present and isError is false. For every tool whose entire value
// IS the text -- command output, log lines, file content, sync
// previews -- attaching a structured metadata stub silently hides
// the payload and leaves the model only that stub. Keeping these
// Content-only is the exact contract jsonResult already uses for the
// structured tools, for the same token reason; metadata that the
// model genuinely needs (clamp notices, transport errors) is folded
// into the text by the caller instead.
//
// isError still drives the client's error styling and the stats OK
// flag; when true the text surfaces regardless.
func payloadResult(text string, isError bool) toolResult {
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		IsError: isError,
	}
}

// runResult builds the final `run` tool result from a captured
// execution.
//
// Content-only on purpose -- NO StructuredContent on the success
// path. MCP clients (Claude Code) surface `structuredContent` in
// place of the text block whenever it is present and isError is
// false, which would hide the command's actual stdout -- the entire
// point of `run`. (On a non-zero exit isError=true and the text is
// shown, but a successful command's output would silently vanish,
// leaving the model only a byte count.) The `[ok cwd X]` / `[exit N
// cwd X]` footer baked in by buildRunText already carries exit code
// and cwd, so nothing the model needs is lost. This mirrors the
// Content-only contract of jsonResult and its token rationale.
//
// Oversize still routes to oversizeResult, which sets isError=true
// (so its text DOES surface) and carries retry metadata plus `extra`
// (e.g. bytes_emitted / streamed from the streaming path) in
// structuredContent.
func runResult(res *sshx.RunCaptureResult, cwd string, extra map[string]any) toolResult {
	text := buildRunText(res, cwd)
	if len(text) > ResultByteMax {
		ex := map[string]any{"exit_code": res.ExitCode, "cwd": cwd}
		maps.Copy(ex, extra)
		return oversizeResult("run", len(text), runOversizeHint, ex)
	}
	return toolResult{
		Content: []toolContent{{Type: "text", Text: text}},
		IsError: res.ExitCode != 0,
	}
}

// oversizeResult is the unified rejection when a tool's text payload
// exceeds ResultByteMax. The body is intentionally not echoed -- the
// model is expected to add a filter and retry, not to read a sliced
// fragment that may have lost the relevant lines. `hint` is the
// tool-specific instruction on what filter to add; `extra` is merged
// into structuredContent so authoritative metadata (exit_code, job
// status, ...) flows back even when the body is dropped.
func oversizeResult(tool string, gotBytes int, hint string, extra map[string]any) toolResult {
	msg := fmt.Sprintf(
		"rejected: %s output is %d bytes (cap %d). Output not returned -- narrow the scope and retry:\n%s",
		tool, gotBytes, ResultByteMax, hint,
	)
	sc := map[string]any{
		"rejected_reason": "oversize_output",
		"bytes_returned":  gotBytes,
		"cap_bytes":       ResultByteMax,
	}
	maps.Copy(sc, extra)
	r := textErr(msg)
	r.StructuredContent = sc
	return r
}

// strSchema builds a string-type JSON schema fragment. Empty desc
// maps to a bare {"type": "string"} -- shaving "description":"" off
// every passthrough field keeps the tools/list payload compact.
func strSchema(desc string) map[string]any {
	if desc == "" {
		return map[string]any{"type": "string"}
	}
	return map[string]any{"type": "string", "description": desc}
}

func boolSchema(def bool, desc string) map[string]any {
	out := map[string]any{"type": "boolean", "default": def}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

func intSchema(def int, desc string) map[string]any {
	out := map[string]any{"type": "integer", "default": def}
	if desc != "" {
		out["description"] = desc
	}
	return out
}

// clampLines bounds the user-supplied `lines` value to `max` and
// signals via the second return whether clamping happened.
func clampLines(asked, max int) (int, bool) {
	if asked > max {
		return max, true
	}
	return asked, false
}
