package mcp

import (
	"fmt"
	"srv/internal/config"
	"srv/internal/jobs"
	"srv/internal/sshx"
)

// Token-economy bounds applied uniformly by handlers. Tuning each in
// isolation has consequences for the others, so they live together.
const (
	// runTextMax caps the combined stdout+stderr the `run` tool
	// returns to the MCP client. Beyond this, output is truncated
	// with a marker pointing the caller at remote-side filtering.
	//
	// The MCP client keeps every tool result in its conversation
	// history, so a single `cat /var/log/...` or `journalctl -n
	// 100000` permanently inflates the client's memory by the full
	// payload. 64 KiB is enough for typical command output while
	// drawing a hard line against runaway dumps.
	runTextMax            = 64 * 1024
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
// tool, capping the combined stdout+stderr at runTextMax. Returns
// (text, truncatedBytes); truncatedBytes is 0 when the output fit.
func buildRunText(res *sshx.RunCaptureResult, cwd string) (string, int) {
	text := res.Stdout
	if res.Stderr != "" {
		if text != "" {
			text += "\n--- stderr ---\n"
		}
		text += res.Stderr
	}
	truncated := 0
	if len(text) > runTextMax {
		truncated = len(text) - runTextMax
		text = text[:runTextMax] + fmt.Sprintf(
			"\n\n... [%d bytes truncated; pipe through head/tail/grep on the remote to slice the output] ...",
			truncated,
		)
	}
	// Don't spell out "exit 0" on success: Codex (and other MCP
	// clients with pattern-matching log analysis) read the word
	// "exit" as a failure signal even when followed by 0. Use
	// [ok cwd ...] for the success case; reserve [exit N cwd ...]
	// for the actually-non-zero codes where the word is accurate.
	if res.ExitCode == 0 {
		text += fmt.Sprintf("\n[ok cwd %s]", cwd)
	} else {
		text += fmt.Sprintf("\n[exit %d cwd %s]", res.ExitCode, cwd)
	}
	return text, truncated
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
