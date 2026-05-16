package mcp

import (
	"fmt"
	"srv/internal/config"
)

// toolHandler is the uniform handler signature. The dispatcher
// extracts profileOverride from args once and passes it explicitly
// so each handler doesn't repeat the extraction.
type toolHandler func(args map[string]any, cfg *config.Config, profileOverride string) toolResult

type tool struct {
	def     toolDef
	handler toolHandler
}

// tools is the registry. ONE source of truth: toolDefs() (advertised
// to the client via tools/list) and handle() (dispatch on tools/call)
// both read from this slice. Adding a tool means appending a single
// entry below; the def-list and the dispatch switch can never drift.
// Same OCP pattern as the CLI subcommand registry in commands.go.
var tools = []tool{
	{
		def: toolDef{
			Name:        "journal",
			Description: "Read or follow systemd journal on the remote. Mirrors journalctl's flag shape: `unit` (-u), `since`, `priority` (-p), `lines` (-n), `grep` (-g, server-side). Pass `follow_seconds` > 0 to stream new lines via `notifications/progress` for that many seconds (cap 60); leave 0 for a one-shot read. Use this in place of `run \"journalctl ...\"` so the bounded-follow case has a real tool surface instead of getting rejected as a long-blocking pattern.\n\nToken-economy gates (MCP only):\n  - ANY follow_seconds > 0 REQUIRES at least one of unit / since / priority / grep -- progress notifications during follow are unbounded by the result-text cap.\n  - `lines` is clamped to 2000.\n  - follow_seconds capped at 60s.\n  - Output exceeding 16 KiB is rejected (not truncated); narrow `unit` / `since` / `priority` / `grep`, or lower `lines` / `follow_seconds`, and retry.\n\nSibling tools (pick by source):\n  - `tail`      -> any remote file by path\n  - `tail_log`  -> output of a detached srv job (by job_id, not path)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"unit":           strSchema("Service unit name (passed to -u)."),
					"since":          strSchema("Relative or absolute time (e.g. \"10 min ago\")."),
					"priority":       strSchema("Priority filter (-p), e.g. err / warning / info."),
					"lines":          intSchema(50, "Number of recent lines to fetch (-n). Default 50 sized to fit under the 16 KiB result cap for typical line widths."),
					"grep":           strSchema("Server-side regex filter (-g)."),
					"follow_seconds": intSchema(0, "Follow for N seconds via progress notifications; 0 = one-shot. Capped at 60."),
					"profile":        strSchema(""),
				},
			},
		},
		handler: handleJournal,
	},
	{
		def: toolDef{
			Name:        "tail",
			Description: "Read the last N lines of a remote file. With follow_seconds > 0, also streams new lines via `notifications/progress` for that duration. Use the one-shot form for log spot-checks; use the follow form when you actually need to watch a log change mid-deploy.\n\nToken-economy gates (MCP only):\n  - ANY follow_seconds > 0 REQUIRES a `grep` regex. Even short follows can flood progress notifications; the 16 KiB final-result cap does NOT cap the progress stream.\n  - `lines` is clamped to 1000.\n  - follow_seconds capped at 60s.\n  - Output exceeding 16 KiB is rejected (not truncated); narrow the scope and retry.\n\nFor one-shot reads (default), no grep is required -- the `lines` cap is the bound.\n\n`grep` + `lines` semantics: grep is applied AFTER `tail -n lines`, i.e. it filters WITHIN the last N lines, not \"the last N matching lines.\" On a busy log a small `lines` can return nothing even when the file has many matches earlier -- raise `lines`, or use `run \"grep PATTERN file | tail -n N\"` for last-N-matching. (`journal`'s grep is server-side over the whole journal -- different semantics.)\n\nSibling tools (pick by source):\n  - `journal`   -> systemd unit logs (use this for any service log on a systemd host; never `tail /var/log/journal/...`)\n  - `tail_log`  -> output of a detached srv job (by job_id, not path)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":           strSchema("Remote file to follow."),
					"lines":          intSchema(50, "Lines to fetch (initial backfill, or full slice when not following). Clamped at 1000."),
					"follow_seconds": intSchema(0, "Follow for N seconds via progress notifications. 0 (default) = one-shot. ANY non-zero value REQUIRES a `grep` filter -- progress notifications are unbounded by the result-text cap. Capped at 60s."),
					"grep":           strSchema("Regex filter applied per line. Mandatory whenever follow_seconds > 0."),
					"profile":        strSchema(""),
				},
				"required": []string{"path"},
			},
		},
		handler: handleTail,
	},
	{
		def: toolDef{
			Name:        "run",
			Description: "Run a remote shell command. Three execution modes, picked automatically:\n  - `background: true`            -> detach, return job_id immediately; pair with wait_job. Required for >30s commands.\n  - client passes `_meta.progressToken` -> stream stdout/stderr as `notifications/progress` while the command runs (good for 20-90s builds/tests; progress keeps the per-tool timeout alive).\n  - neither                       -> synchronous, warm daemon pool (~200ms for short commands).\n\nREJECTED in non-background modes (use background=true instead):\n  - `sleep N` where N > 5\n  - `tail -f`, `watch`, `journalctl -f` and similar never-terminating patterns\n\nREJECTED as unbounded-output (token economy -- add a slicer):\n  - `cat <file>`           -> use `head -n N <file>` or `tail -n N <file>` or the `tail` MCP tool\n  - `dmesg`                -> pipe into `tail -n N` or `grep PATTERN`\n  - `journalctl` w/o flags -> use the `journal` MCP tool or add -u/--since/-p/-g/-n\n  - `find /` w/o flags     -> add -maxdepth N / -name PATTERN / -type / etc.\nDownstream limiters (`| head`, `| tail`, `| grep`, `| wc`, etc.) satisfy the gate. Streaming does NOT exempt the gate -- progress notifications add token cost on top of the final result, so the unbounded-source rule applies the same.\n\nOutput exceeding 16 KiB is rejected (not truncated). When streaming mode hits the cap mid-execution, the remote command is killed via SSH close and the call returns the oversize reject with `terminated_early: true`. Narrow with `head -n N` / `tail -n N` / `grep PATTERN` and retry.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command":    strSchema("Remote shell command."),
					"profile":    strSchema(""),
					"background": boolSchema(false, "Start as a detached job and return immediately. Required for long commands and for any sleep/wait/follow pattern."),
					"confirm":    boolSchema(false, "Required when guard is on AND command hits a high-risk pattern (rm -rf, dd, mkfs, drop ...)."),
				},
				"required": []string{"command"},
			},
		},
		handler: handleRun,
	},
	{
		def: toolDef{
			Name:        "cd",
			Description: "Set remote cwd.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":    strSchema("Path."),
					"profile": strSchema(""),
				},
				"required": []string{"path"},
			},
		},
		handler: handleCd,
	},
	{
		def: toolDef{
			Name:        "pwd",
			Description: "Get remote cwd.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handlePwd,
	},
	{
		def: toolDef{
			Name:        "use",
			Description: "Pin or clear profile.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"profile": strSchema(""),
					"clear":   boolSchema(false, ""),
				},
			},
		},
		handler: handleUse,
	},
	{
		def: toolDef{
			Name:        "status",
			Description: "Show active profile.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleStatus,
	},
	{
		def: toolDef{
			Name:        "list_profiles",
			Description: "List profiles.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: handleListProfiles,
	},
	{
		def: toolDef{
			Name:        "check",
			Description: "Probe SSH connectivity.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleCheck,
	},
	{
		def: toolDef{
			Name:        "doctor",
			Description: "Run local diagnostics.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleDoctor,
	},
	{
		def: toolDef{
			Name:        "daemon_status",
			Description: "Show daemon status.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		handler: handleDaemonStatus,
	},
	{
		def: toolDef{
			Name:        "env",
			Description: "Manage remote env.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"action":  map[string]any{"type": "string", "enum": []string{"list", "set", "unset", "clear"}, "default": "list"},
					"key":     strSchema("Env var name."),
					"value":   strSchema("Env var value."),
					"profile": strSchema(""),
				},
			},
		},
		handler: handleEnv,
	},
	{
		def: toolDef{
			Name:        "diff",
			Description: "Diff local vs remote file.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   strSchema("Local file."),
					"remote":  strSchema("Remote file."),
					"profile": strSchema(""),
				},
				"required": []string{"local"},
			},
		},
		handler: handleDiff,
	},
	{
		def: toolDef{
			Name:        "push",
			Description: "Upload file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"local":   strSchema(""),
					"remote":  strSchema("Remote path."),
					"profile": strSchema(""),
				},
				"required": []string{"local"},
			},
		},
		handler: handlePush,
	},
	{
		def: toolDef{
			Name:        "pull",
			Description: "Download file or directory.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote":    strSchema(""),
					"local":     strSchema("Local path."),
					"recursive": boolSchema(false, ""),
					"profile":   strSchema(""),
				},
				"required": []string{"remote"},
			},
		},
		handler: handlePull,
	},
	{
		def: toolDef{
			Name:        "sync",
			Description: "Sync local changes to remote.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"remote_root":  strSchema("Remote root."),
					"mode":         map[string]any{"type": "string", "enum": []string{"git", "mtime", "glob", "list"}},
					"git_scope":    map[string]any{"type": "string", "enum": []string{"all", "staged", "modified", "untracked"}, "default": "all"},
					"since":        strSchema("Duration, e.g. 2h."),
					"include":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"exclude":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"files":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"root":         strSchema(""),
					"dry_run":      boolSchema(false, ""),
					"delete":       boolSchema(false, ""),
					"yes":          boolSchema(false, ""),
					"delete_limit": intSchema(20, "Max deletes without yes=true."),
					"profile":      strSchema(""),
					"confirm":      boolSchema(false, "Required when guard is on AND delete=true (non-dry-run)."),
				},
			},
		},
		handler: handleSync,
	},
	{
		def: toolDef{
			Name:        "sync_delete_dry_run",
			Description: "Preview sync deletes.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"root": strSchema("Local root."), "remote_root": strSchema("Remote root."), "profile": strSchema("")},
			},
		},
		handler: handleSyncDeleteDryRun,
	},
	{
		def: toolDef{
			Name:        "run_group",
			Description: "Run the same remote command across every profile in a named group, in parallel. Returns one result per member with exit code, stdout/stderr, and duration. Use this when you'd otherwise have to loop `run` over N hosts (deploys, restarts, status checks). Synchronous: subject to the same 60s MCP per-tool cap as `run`, so keep the command short or run it via `detach` per-profile and then poll.\n\nOutput exceeding 16 KiB (combined across all members) is rejected (not truncated). Narrow the `group` membership, or run the command per-profile with a slicer (`| head -n N`, `| grep PATTERN`).",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"group":   strSchema("Group name as defined in config.groups."),
					"command": strSchema("Remote shell command to run on every member."),
					"confirm": boolSchema(false, "Required when guard is on AND command hits a high-risk pattern."),
				},
				"required": []string{"group", "command"},
			},
		},
		handler: handleRunGroup,
	},
	{
		def: toolDef{
			Name:        "detach",
			Description: "Start a remote command in the background and return its job_id immediately (sub-second). Pair with `wait_job` to block on completion in bounded chunks -- the recommended pattern for any command expected to take more than ~30s. The wrapper writes the user command's exit code to ~/.srv-jobs/<id>.exit when it finishes, which `wait_job` polls without keeping an SSH session open the whole time.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": strSchema(""),
					"profile": strSchema(""),
					"confirm": boolSchema(false, "Required when guard is on AND command hits a high-risk pattern."),
				},
				"required": []string{"command"},
			},
		},
		handler: handleDetach,
	},
	{
		def: toolDef{
			Name:        "list_jobs",
			Description: "List detached jobs.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"profile": strSchema("")},
			},
		},
		handler: handleListJobs,
	},
	{
		def: toolDef{
			Name:        "tail_log",
			Description: "Read the last N lines of a detached job's log file (by job_id). Resolves the id to ~/.srv-jobs/<id>.log on the remote and runs `tail -n LINES` there. One-shot only -- use `wait_job` for the polling pattern that pairs with `detach` / `run background=true`.\n\nOutput exceeding 16 KiB is rejected (not truncated). Lower `lines`, or use `run \"grep PATTERN ~/.srv-jobs/<id>.log | head -n N\"` to filter directly.\n\nSibling tools (pick by source):\n  - `tail`     -> any remote file by path (with optional follow + grep)\n  - `journal`  -> systemd unit logs",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    strSchema(""),
					"lines": intSchema(100, "Lines of log to fetch (tail -n). Default 100 sized to fit under the 16 KiB result cap for typical line widths."),
				},
				"required": []string{"id"},
			},
		},
		handler: handleTailLog,
	},
	{
		def: toolDef{
			Name:        "wait_job",
			Description: "Poll a detached job for completion, returning exit code + log tail when done. Designed to pair with `detach` or `run background=true`: long commands run in the background, and the model loops wait_job until status=completed. Defaults to short 8s polls and caps each call at 15s so Claude Code stays responsive. status=running means \"call wait_job again\"; status=completed means it's done and the local job record has been cleaned up.\n\nIf the response (status hint + log tail) exceeds 16 KiB, the response body is rejected but the structured fields (status, exit_code) still flow back so the polling loop can advance. Lower `tail_lines`, or fetch the log separately with `tail_log` + smaller `lines`.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":               strSchema("Job id from detach."),
					"max_wait_seconds": intSchema(waitJobDefaultSeconds, "Upper bound on this call's blocking time. Capped at 15 to keep the MCP UI responsive."),
					"tail_lines":       intSchema(50, "Lines of log to include in the response."),
				},
				"required": []string{"id"},
			},
		},
		handler: handleWaitJob,
	},
	{
		def: toolDef{
			Name:        "list_dir",
			Description: "List remote directory entries (subset of `ls -1Ap`). Use this instead of `run \"ls ...\"` for path discovery -- response is structured, ANSI-clean, and hits the warm daemon cache (sub-100ms on repeat). Pass an empty path for the active cwd. Dirs carry trailing '/'.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path":      strSchema("Remote path prefix. Empty = current cwd. Trailing '/' = list that directory; no trailing '/' = match entries whose name starts with the basename. E.g., '/etc/' lists /etc; '/etc/host' returns host*, hostname, hosts, hosts.allow."),
					"dirs_only": boolSchema(false, "Filter to directories only (entries ending in '/')."),
					"limit":     intSchema(500, "Maximum entries returned. Anything beyond gets dropped; truncated_count surfaces the cut so you know to query a deeper prefix."),
					"profile":   strSchema(""),
				},
			},
		},
		handler: handleListDir,
	},
	{
		def: toolDef{
			Name:        "kill_job",
			Description: "Signal detached job.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":     strSchema(""),
					"signal": map[string]any{"type": "string", "default": "TERM"},
				},
				"required": []string{"id"},
			},
		},
		handler: handleKillJob,
	},
}

// toolMap is built once from the registry so dispatch is O(1).
var toolMap map[string]*tool

func init() {
	toolMap = make(map[string]*tool, len(tools))
	for i := range tools {
		t := &tools[i]
		toolMap[t.def.Name] = t
	}
}

// toolDefs returns the slice of toolDef advertised on tools/list.
// Derived from the registry so the def-list and the dispatcher
// cannot drift -- both come from the same source.
func toolDefs() []toolDef {
	defs := make([]toolDef, 0, len(tools))
	for i := range tools {
		defs = append(defs, tools[i].def)
	}
	return defs
}

// handle dispatches a tools/call request through the registry.
// Unknown names return a textual error -- spec doesn't require a
// more structured "tool not found" form for that case.
func handle(name string, args map[string]any, cfg *config.Config) toolResult {
	profileOverride, _ := args["profile"].(string)
	if t, ok := toolMap[name]; ok {
		return t.handler(args, cfg, profileOverride)
	}
	return textErr(fmt.Sprintf("unknown tool %q", name))
}
