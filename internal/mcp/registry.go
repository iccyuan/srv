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
			Description: "Read or follow systemd journal on the remote. Mirrors journalctl's flag shape: `unit` (-u), `since`, `priority` (-p), `lines` (-n), `grep` (-g, server-side). Pass `follow_seconds` > 0 to stream new lines via `notifications/progress` for that many seconds (cap 60); leave 0 for a one-shot read. Use this in place of `run \"journalctl ...\"` so the bounded-follow case has a real tool surface instead of getting rejected as a long-blocking pattern.\n\nmacOS remotes auto-dispatch to the macOS unified-logging tools (`log show` / `log stream`) when the profile's platform is detected as darwin (`uname -s`, cached 24h under ~/.srv/cache/platform-<profile>.txt). The same args translate -- `unit` maps to a subsystem/process NSPredicate, `since` becomes `--last` (use durations like \"10m\"), `priority` maps to messageType / --info / --debug, `grep` becomes an NSPredicate MATCHES filter, `lines` is enforced via `| tail -n N`. Pass `prefer_log: true` to force the macOS path on a profile that hasn't been detected yet; pass `prefer_log: false` to force journalctl regardless of platform (useful for diagnostics, or for a Linux-in-darwin-container setup). Profile.Platform set explicitly (linux / darwin / other) skips detection.\n\nToken-economy gates (MCP only):\n  - ANY follow_seconds > 0 REQUIRES at least one of unit / since / priority / grep -- progress notifications during follow are unbounded by the result-text cap.\n  - `lines` is clamped to 2000.\n  - follow_seconds capped at 60s.\n  - Output exceeding 64 KiB is rejected (not truncated); narrow `unit` / `since` / `priority` / `grep`, or lower `lines` / `follow_seconds`, and retry.\n\nSibling tools (pick by source):\n  - `tail`      -> any remote file by path\n  - `tail_log`  -> output of a detached srv job (by job_id, not path)",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"unit":           strSchema("Service unit name (journalctl -u; on macOS with prefer_log, matches subsystem OR process)."),
					"since":          strSchema("Time window. journalctl: relative or absolute (\"10 min ago\"). macOS (prefer_log): duration like \"10m\" / \"1h\" passed to `log show --last`; ignored in follow mode (log stream is always live)."),
					"priority":       strSchema("Priority filter. journalctl: err / warning / info / debug. macOS (prefer_log): err/fault -> messageType predicate; info/debug -> --info/--debug; warning -> closest match via --info."),
					"lines":          intSchema(50, "Number of recent lines to fetch. journalctl: -n. macOS (prefer_log): appended as `| tail -n N` since `log` has no -n. Clamped to 2000."),
					"grep":           strSchema("Server-side regex filter. journalctl: -g. macOS (prefer_log): NSPredicate `eventMessage MATCHES \"RE\"`."),
					"follow_seconds": intSchema(0, "Follow for N seconds via progress notifications; 0 = one-shot. Capped at 60. On macOS with prefer_log, uses `log stream`."),
					"prefer_log":     boolSchema(false, "Dispatch to macOS unified logging (`log show` / `log stream`) instead of journalctl. Use on macOS hosts. See the description for the full arg mapping."),
					"profile":        strSchema(""),
				},
			},
		},
		handler: handleJournal,
	},
	{
		def: toolDef{
			Name:        "tail",
			Description: "Read the last N lines of a remote file. With follow_seconds > 0, also streams new lines via `notifications/progress` for that duration. Use the one-shot form for log spot-checks; use the follow form when you actually need to watch a log change mid-deploy.\n\nToken-economy gates (MCP only):\n  - ANY follow_seconds > 0 REQUIRES a `grep` regex. Even short follows can flood progress notifications; the 64 KiB final-result cap does NOT cap the progress stream.\n  - `lines` is clamped to 1000.\n  - follow_seconds capped at 60s.\n  - Output exceeding 64 KiB is rejected (not truncated); narrow the scope and retry.\n\nFor one-shot reads (default), no grep is required -- the `lines` cap is the bound.\n\n`grep` + `lines` semantics: grep is applied AFTER `tail -n lines`, i.e. it filters WITHIN the last N lines, not \"the last N matching lines.\" On a busy log a small `lines` can return nothing even when the file has many matches earlier -- raise `lines`, or use `run \"grep PATTERN file | tail -n N\"` for last-N-matching. (`journal`'s grep is server-side over the whole journal -- different semantics.)\n\nSibling tools (pick by source):\n  - `journal`   -> systemd unit logs (use this for any service log on a systemd host; never `tail /var/log/journal/...`)\n  - `tail_log`  -> output of a detached srv job (by job_id, not path)",
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
			Description: "Run a remote shell command. Three execution modes, picked automatically:\n  - `background: true`            -> detach, return job_id immediately; pair with wait_job. Required for >30s commands.\n  - client passes `_meta.progressToken` -> stream stdout/stderr as `notifications/progress` while the command runs (good for 20-90s builds/tests; progress keeps the per-tool timeout alive).\n  - neither                       -> synchronous, warm daemon pool (~200ms for short commands).\n\nREJECTED in non-background modes (use background=true instead):\n  - `sleep N` where N > 5\n  - `tail -f`, `watch`, `journalctl -f` and similar never-terminating patterns\n\nREJECTED as unbounded-output (token economy -- add a slicer):\n  - `cat <file>`           -> use `head -n N <file>` or `tail -n N <file>` or the `tail` MCP tool\n  - `dmesg`                -> pipe into `tail -n N` or `grep PATTERN`\n  - `journalctl` w/o flags -> use the `journal` MCP tool or add -u/--since/-p/-g/-n (macOS: pass prefer_log: true to journal)\n  - `find /` w/o flags     -> add -maxdepth N / -name PATTERN / -type / etc.\nDownstream limiters (`| head`, `| tail`, `| grep`, `| wc`, etc.) satisfy the gate. Streaming does NOT exempt the gate -- progress notifications add token cost on top of the final result, so the unbounded-source rule applies the same.\n\nOutput exceeding 64 KiB is rejected (not truncated). When streaming mode hits the cap mid-execution, the remote command is killed via SSH close and the call returns the oversize reject with `terminated_early: true`. Narrow with `head -n N` / `tail -n N` / `grep PATTERN` and retry.\n\nGotchas worth knowing before you bake them into a script:\n  - Tail-buffering in background jobs: `make 2>&1 | tail -n N` in a `background: true` script writes NOTHING to the job log until the upstream command exits -- the pipe buffers everything inside `tail`. wait_job / tail_log then look stuck mid-build. If you want mid-run visibility, drop the `| tail` (or use `| tee log` to keep both), and probe progress via filesystem signals (e.g. count built objects with `find ... -name '*.o' | wc -l`) rather than the log.\n  - zsh strict glob on macOS remotes: zsh aborts the whole command on an unmatched glob (`zsh: no matches found: foo-*`) even with `rm -f`. bash silently passes the literal through. For globs that may legitimately be empty, either name files explicitly, run via `bash -c '...'`, or prefix with `setopt no_nomatch;` inside the command. Same applies to any other zsh-default host (recent macOS, some Linux setups).",
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
			Description: "Sync local files to the remote. Selection by `mode`: git (changed tracked files), glob (`include` patterns; `*` is one level, `**` recurses), mtime (`since`), or list (explicit `files`). `files`/globs are resolved relative to `root` (the local sync root), not your shell cwd. NOT incremental: every selected file is re-tar'd and re-sent each call even if unchanged (no per-file skip) -- it's a push, not rsync.",
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
			Description: "Preview which remote files `sync delete=true` would remove. GIT-ONLY: it lists files git knows were deleted from the tracked set under `root` -- it does NOT diff arbitrary remote-vs-local trees. On a non-git `root` it returns \"would delete 0\" regardless of what the remote holds.",
			InputSchema: map[string]any{
				"type":       "object",
				"properties": map[string]any{"root": strSchema("Local root."), "remote_root": strSchema("Remote root."), "profile": strSchema("")},
			},
		},
		handler: handleSyncDeleteDryRun,
	},
	{
		def: toolDef{
			Name:        "sudo",
			Description: "Run a privileged command on the remote via `sudo -S`. Two boundaries make this safe enough to expose to an AI client (and neither is configurable):\n\n  1. Every call goes through MCP elicitation (interactive Allow/Deny shown to the human). A client that didn't advertise the `elicitation` capability is hard-denied -- there is NO `confirm` arg or any model-side way to bypass the human gate. Unlike the `run` tool's guard (which a model can `confirm: true` past at its own discretion), crossing the privilege boundary always asks a human.\n  2. Password sourcing has two modes, profile-configured:\n     - DEFAULT (recommended): cache-only. MCP only reads from the daemon's in-memory cache; a password enters that cache solely via the TTY-prompting `srv sudo` CLI. Run `srv sudo --cache-ttl 15m -P <profile> true` once from a terminal to seed the cache, then this tool can consume it for up to 15 minutes. On cache miss the response includes `structuredContent.cached: false` and the body text tells the user to seed from a terminal.\n     - OPT-IN (profile.enable_mcp_password_prompt = true): on cache miss, the tool sends a SECOND elicitation/create whose schema declares a `format:\"password\"` field. The client renders a masked input; the answer travels client UI -> srv -> daemon and is seeded into the cache (5 min TTL), then the sudo command proceeds in the same tool call. The model never sees the password in any tool result. Trade-off: removes the \"drop to a terminal\" friction at the cost of widening the prompt-injection surface (a malicious tool result that nudges the model into calling sudo will pop a password prompt). Enable only when that trade-off is acceptable for a given profile.\n\nFailure modes:\n  - Cache miss + opt-in off                 -> isError=true, structured `cached: false`, no SSH made.\n  - Cache miss + opt-in on + user cancels   -> isError=true, structured `cached: false`, no SSH made.\n  - Elicitation declined  -> isError=true, guard_denied=true.\n  - Elicitation unavailable (client without the capability) -> isError=true, guard_blocked=true.\n  - sudo exit 1 / wrong password -> isError=true, normal exit code surfaced; the bad cache entry is NOT auto-cleared (next call still hits the same cache; re-seed via CLI or opt-in prompt).\n\nOutput exceeding 64 KiB is rejected with the structured stub still flowing back; narrow with `| head -n N` / `| tail -n N` / `| grep PATTERN`.\n\nNo `confirm` arg and no `--no-cache` arg are exposed by design.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": strSchema("Remote shell command to run under sudo (without the `sudo` prefix; that's added by the wrapper)."),
					"profile": strSchema(""),
				},
				"required": []string{"command"},
			},
		},
		handler: handleSudo,
	},
	{
		def: toolDef{
			Name:        "run_group",
			Description: "Run the same remote command across every profile in a named group, in parallel. Returns one result per member with exit code, stdout/stderr, and duration. Use this when you'd otherwise have to loop `run` over N hosts (deploys, restarts, status checks). Synchronous: subject to the same 60s MCP per-tool cap as `run`, so keep the command short or run it via `detach` per-profile and then poll.\n\nOutput exceeding 64 KiB (combined across all members) is rejected (not truncated). Narrow the `group` membership, or run the command per-profile with a slicer (`| head -n N`, `| grep PATTERN`).",
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
			Name:        "prune_jobs",
			Description: "Delete FINISHED job records from the local ledger (running jobs are always kept). The MCP counterpart of `srv prune jobs`. Call this as your \"receipt\" once you've consumed a job's result via wait_job / tail_log -- it acknowledges and discards completed jobs so list_jobs stays small and trustworthy. No `id` = prune every finished record; `id` = prune just that one completed job (errors if it is still running -- kill_job it first). Reconciles against remote .exit markers first, so a job that finished since the last ledger touch is pruned by this same call.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":      strSchema("Optional: prune only this one finished job (full id or unambiguous prefix). Omit to prune all finished records."),
					"profile": strSchema(""),
				},
			},
		},
		handler: handlePruneJobs,
	},
	{
		def: toolDef{
			Name:        "tail_log",
			Description: "Read the last N lines of a detached job's log file (by job_id). Resolves the id to ~/.srv-jobs/<id>.log on the remote and runs `tail -n LINES` there. One-shot only -- use `wait_job` for the polling pattern that pairs with `detach` / `run background=true`.\n\nOutput exceeding 64 KiB is rejected (not truncated). Lower `lines`, or use `run \"grep PATTERN ~/.srv-jobs/<id>.log | head -n N\"` to filter directly.\n\nSibling tools (pick by source):\n  - `tail`     -> any remote file by path (with optional follow + grep)\n  - `journal`  -> systemd unit logs",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"id":    strSchema(""),
					"lines": intSchema(100, "Lines of log to fetch (tail -n). Default 100 sized to fit under the 64 KiB result cap for typical line widths."),
				},
				"required": []string{"id"},
			},
		},
		handler: handleTailLog,
	},
	{
		def: toolDef{
			Name:        "wait_job",
			Description: "Poll a detached job for completion, returning exit code + log tail when done. Designed to pair with `detach` or `run background=true`: long commands run in the background, and the model loops wait_job until status=completed. Defaults to short 8s polls and caps each call at 15s so Claude Code stays responsive. status=running means \"call wait_job again\"; status=completed means it's done and the local job record has been cleaned up.\n\nstructuredContent always carries `log_unchanged_seconds` (=now - log file mtime). When status=running and that number is large (>= 15s the hint string also says `log unchanged Ns`), the build's stdout has gone quiet -- common when the running command block-buffers its output (clang on a big translation unit, `make ... | tail -n N`), or when it's genuinely stuck. Don't just poll harder; probe progress via filesystem signals instead -- `find <build dir> -name '*.o' | wc -l`, output binary size, etc. log_unchanged_seconds = -1 means the marker wasn't parseable (older remote / stat fallback failed); treat as \"unknown, just keep polling.\"\n\nIf the response (status hint + log tail) exceeds 64 KiB, the response body is rejected but the structured fields (status, exit_code, log_unchanged_seconds) still flow back so the polling loop can advance. Lower `tail_lines`, or fetch the log separately with `tail_log` + smaller `lines`.",
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
